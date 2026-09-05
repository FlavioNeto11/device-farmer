package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/flaviopadilha/device-farmer/internal/lease"
)

// APIError is the one error shape this package emits. Every non-2xx response
// body is {"error": APIError} and nothing else, so a client can parse a failure
// without first deciding which of several error dialects it received.
//
// Code is the machine-readable half and is the field callers branch on. It is
// deliberately separate from the HTTP status because two of this API's most
// important failures share a status with unrelated conditions: 409 is both
// "no capacity" (an ordinary scheduling outcome) and "this power cycle would
// disturb a live lease" (a refusal a human must read).
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Detail carries structured context — the offending lease, the permitted
	// values, the constraint that fired. It exists so the dashboard can explain
	// a refusal instead of rendering a dead button.
	Detail any `json:"detail,omitempty"`
}

type errorBody struct {
	Error APIError `json:"error"`
}

// Error codes. These are part of the API contract: a holder decides between
// "abort the job" and "retry the request" by reading CodeFenced against
// CodeTransient, so neither name may change without a version bump.
const (
	CodeBadRequest      = "bad_request"
	CodeInvalidJSON     = "invalid_json"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeUnauthenticated = "unauthenticated"
	CodeForbidden       = "forbidden"
	CodeInternal        = "internal"
	CodeUnavailable     = "unavailable"
	CodeTimeout         = "timeout"
	CodeClientClosed    = "client_closed"

	// CodeFenced accompanies 410 Gone from renew and means the lease is gone:
	// terminal, unrecoverable, abort the job. It is returned if and only if
	// farm.lease_renew returned zero rows.
	CodeFenced = "fenced"

	// CodeTransient accompanies 503 and means the database did not answer. The
	// lease is untouched, no deadline moved, and the caller must retry with
	// backoff. Conflating this with CodeFenced is STF #663 with a different
	// trigger, which is why they are different codes and different statuses.
	CodeTransient = "transient"

	// CodeNoCapacity accompanies 409 from acquire. Not an error: no healthy,
	// enabled, unleased device matched. Re-queue and try again.
	CodeNoCapacity = "no_capacity"

	// CodeCheckViolation accompanies 400 when Postgres refused a write against
	// a CHECK constraint — most importantly a release reason outside the seven
	// the schema permits.
	CodeCheckViolation = "check_violation"

	// CodeDisruptionRefused accompanies 409 when an operator action would
	// disturb a device whose live lease forbids that blast radius.
	CodeDisruptionRefused = "disruption_refused"

	// CodeADBError accompanies 502 when the host's ADB server could not be
	// reached or spoke nonsense. It says nothing about any lease.
	CodeADBError = "adb_error"

	// CodeHostAgent accompanies 502 when the farmd-node agent on a host could
	// not be reached, or was reached and the hardware rung it attempted did
	// not bring the device back. The detail says which. Like CodeADBError it
	// says nothing about any lease: every lease in the power domain keeps its
	// device, its fence and its deadline.
	CodeHostAgent = "host_agent"

	// CodeUINotMounted accompanies 404 at "/" when the binary was built or
	// wired without the dashboard handler.
	CodeUINotMounted = "ui_not_mounted"
)

// statusClientClosed is nginx's non-standard 499. Used when the caller
// disconnected: it keeps "the client went away" out of the 5xx rate that pages
// a human.
const statusClientClosed = 499

// permittedReleaseReasons is the vocabulary of farm.leases.release_reason,
// taken from internal/lease so this list cannot drift from the constants the
// rest of the binary uses. It is echoed back in the 400 body when a release is
// refused, because the useful half of that refusal is the list of words that
// would have worked.
var permittedReleaseReasons = []string{
	string(lease.ReasonCompleted),
	string(lease.ReasonFailed),
	string(lease.ReasonJobCancelled),
	string(lease.ReasonMaxRuntime),
	string(lease.ReasonOperatorRevoked),
	string(lease.ReasonHolderExpired),
	string(lease.ReasonDeviceRetired),
}

// writeJSON encodes v and writes it with status.
//
// The value is marshalled into a buffer before a single byte of the response is
// committed: encoding straight into the ResponseWriter would emit a 200 header
// followed by half a body when marshalling fails midway, and the client would
// see a truncated success.
func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		h := w.Header()
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"response encoding failed"}}`))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
	_, _ = w.Write([]byte("\n"))
}

// writeError emits the error envelope.
func writeError(w http.ResponseWriter, status int, code, message string, detail any) {
	writeJSON(w, status, errorBody{Error: APIError{Code: code, Message: message, Detail: detail}})
}

// fail maps err to a status and writes the envelope, logging server-side
// failures with the operation that produced them.
//
// op is a short verb phrase ("list fleet", "renew lease") and appears only in
// the log, never in the body: an error message handed to a client should say
// what to do about it, not where in the server it came from.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, op string, err error) {
	status, apiErr := classifyError(err)

	switch {
	case status == statusClientClosed:
		// The caller hung up. Writing is pointless and logging at error level
		// would make a user pressing Escape look like an outage — but the
		// outcome is still recorded, so an abandoned request is not counted as
		// the 200 the recorder starts at.
		noteClientGone(w)
		s.log.DebugContext(r.Context(), "request abandoned by client",
			"op", op, "path", r.URL.Path, "err", err)
		return
	case status >= 500:
		s.log.ErrorContext(r.Context(), "request failed",
			"op", op, "path", r.URL.Path, "status", status, "code", apiErr.Code, "err", err)
	default:
		s.log.InfoContext(r.Context(), "request rejected",
			"op", op, "path", r.URL.Path, "status", status, "code", apiErr.Code, "err", err)
	}

	if status == http.StatusServiceUnavailable {
		// A transient database failure is retryable, and saying so in a header
		// keeps well-behaved clients from hot-looping on a Postgres failover.
		w.Header().Set("Retry-After", "2")
	}
	writeError(w, status, apiErr.Code, apiErr.Message, apiErr.Detail)
}

// classifyError maps an error from pgx, from internal/lease, or from the
// standard library onto an HTTP status and an error envelope.
//
// The load-bearing case is SQLSTATE 23514. A check violation is ALWAYS a 400:
// the canonical instance is a holder trying to release a lease with a
// connectivity-flavoured reason such as "device_offline", which the schema has
// no word for. That is the caller sending a value the domain does not contain,
// not the server breaking, and reporting it as a 500 would both mislead the
// caller and pollute the error budget that pages a human.
func classifyError(err error) (int, APIError) {
	switch {
	case err == nil:
		return http.StatusInternalServerError, APIError{Code: CodeInternal, Message: "unspecified failure"}

	case errors.Is(err, context.Canceled):
		return statusClientClosed, APIError{Code: CodeClientClosed, Message: "request cancelled by the client"}

	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, APIError{
			Code:    CodeTimeout,
			Message: "the operation did not finish within its deadline",
		}

	case errors.Is(err, lease.ErrFenced):
		return http.StatusGone, APIError{
			Code: CodeFenced,
			Message: "this lease is no longer yours: abort the job, close every ADB socket, " +
				"and write nothing further to the device",
		}

	case errors.Is(err, lease.ErrNoCapacity):
		return http.StatusConflict, APIError{
			Code: CodeNoCapacity,
			Message: "no healthy, enabled, unleased device in the pool matched this job; " +
				"this is an ordinary scheduling outcome, re-queue and retry",
		}

	case errors.Is(err, lease.ErrJobNotFound):
		return http.StatusNotFound, APIError{Code: CodeNotFound, Message: "job not found"}

	case errors.Is(err, pgx.ErrNoRows):
		return http.StatusNotFound, APIError{Code: CodeNotFound, Message: "not found"}
	}

	// internal/lease already classified a check violation for us and knows
	// which reason was refused, so prefer its typed error over the raw pg one.
	var cv *lease.CheckViolationError
	if errors.As(err, &cv) {
		return http.StatusBadRequest, releaseReasonRefusal(string(cv.Reason), cv.Constraint, cv.Message)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return classifyPgError(pgErr)
	}

	// pgx reports an unreachable database as a *pgconn.ConnectError or a bare
	// net error. Neither proves anything about any lease, so both are 503:
	// retryable, and never a fencing signal.
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return http.StatusServiceUnavailable, APIError{
			Code:    CodeTransient,
			Message: "the database is not reachable right now; retry with backoff. No lease was touched.",
		}
	}

	return http.StatusInternalServerError, APIError{Code: CodeInternal, Message: "internal error"}
}

// classifyPgError maps a SQLSTATE onto a status. Every case here exists
// because the corresponding condition is reachable from a request body; the
// default is the honest 500.
func classifyPgError(pgErr *pgconn.PgError) (int, APIError) {
	switch pgErr.Code {
	case "23514": // check_violation
		return http.StatusBadRequest, releaseReasonRefusal("", pgErr.ConstraintName, pgErr.Message)

	case "23505": // unique_violation
		// The partial unique indexes on farm.leases are what actually prevent a
		// double grant, so this is a race the caller lost, not corruption.
		return http.StatusConflict, APIError{
			Code:    CodeConflict,
			Message: "that write conflicts with a row that already exists",
			Detail:  map[string]string{"constraint": pgErr.ConstraintName, "message": pgErr.Message},
		}

	case "23503": // foreign_key_violation
		return http.StatusBadRequest, APIError{
			Code:    CodeBadRequest,
			Message: "a referenced row does not exist (pool, queue, tenant, device or slot)",
			Detail:  map[string]string{"constraint": pgErr.ConstraintName, "message": pgErr.Message},
		}

	case "23502": // not_null_violation
		return http.StatusBadRequest, APIError{
			Code:    CodeBadRequest,
			Message: "a required field was missing: " + pgErr.ColumnName,
		}

	case "22P02", "22003", "22007", "22008": // malformed text, numeric or time literal
		return http.StatusBadRequest, APIError{
			Code:    CodeBadRequest,
			Message: "a value in the request is not in a form the database accepts: " + pgErr.Message,
		}

	case "P0002": // no_data_found, raised by farm.lease_acquire for an unknown job
		return http.StatusNotFound, APIError{Code: CodeNotFound, Message: pgErr.Message}

	case "42501": // insufficient_privilege — the reaper/watchdog role firewall
		return http.StatusForbidden, APIError{
			Code:    CodeForbidden,
			Message: "the database refused this operation to this role: " + pgErr.Message,
		}

	case "40001", "40P01", "55P03", "57P03": // serialization, deadlock, lock_not_available, cannot_connect_now
		return http.StatusServiceUnavailable, APIError{
			Code:    CodeTransient,
			Message: "the database could not complete this right now; retry with backoff. No lease was touched.",
		}

	case "57014": // query_canceled (statement_timeout)
		return http.StatusGatewayTimeout, APIError{
			Code:    CodeTimeout,
			Message: "the database cancelled the query before it finished",
		}

	case "53300", "53400": // too_many_connections, configuration_limit_exceeded
		return http.StatusServiceUnavailable, APIError{
			Code:    CodeTransient,
			Message: "the database is out of capacity right now; retry with backoff. No lease was touched.",
		}
	}

	return http.StatusInternalServerError, APIError{Code: CodeInternal, Message: "internal database error"}
}

// releaseReasonRefusal builds the 400 body for a check violation.
//
// When the violation came from the release_reason constraint — by far the most
// likely one to be triggered by a request body — the message names the actual
// problem and lists the seven words that would have worked. Anything else that
// trips a CHECK still lands here as a 400 rather than a 500, because a CHECK
// constraint firing means the request asked for a state the schema forbids.
func releaseReasonRefusal(reason, constraint, message string) APIError {
	looksLikeReleaseReason := reason != "" ||
		strings.Contains(constraint, "release_reason") ||
		strings.Contains(message, "release_reason")

	if looksLikeReleaseReason {
		detail := map[string]any{
			"permitted_release_reasons": permittedReleaseReasons,
			"constraint":                constraint,
			"database_message":          message,
		}
		if reason != "" {
			detail["rejected_reason"] = reason
		}
		return APIError{
			Code: CodeCheckViolation,
			Message: "the release reason is not a permitted value. farm.leases.release_reason " +
				"has no word for connectivity — a socket error, a probe timeout or a device " +
				"going offline cannot end a lease — so the database refused the write and the " +
				"lease is untouched.",
			Detail: detail,
		}
	}

	return APIError{
		Code:    CodeCheckViolation,
		Message: fmt.Sprintf("the database refused this value against constraint %q: %s", constraint, message),
		Detail:  map[string]string{"constraint": constraint, "database_message": message},
	}
}
