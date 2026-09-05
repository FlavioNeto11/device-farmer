package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Handler builds the routing table once and returns it.
//
// Routing uses net/http's pattern mux (Go 1.22+): the method is part of the
// pattern and path variables are read with r.PathValue. No third-party router
// is involved, which keeps the dependency list at the three modules go.mod
// already names.
//
// Authorisation is applied at registration, not inside handlers. Every route
// below is wrapped in requireRole with the level it needs, so a new handler
// cannot be added without a level being chosen for it — the failure mode where
// somebody adds a destructive endpoint and forgets the check is not reachable
// from this table.
func (s *Server) Handler() http.Handler {
	s.handlerOnce.Do(func() { s.handler = s.buildHandler() })
	return s.handler
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated infrastructure routes. A liveness probe that needs a
	// credential is a liveness probe that fails when the credential rotates.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /metrics", s.metricsHandler())

	tenant := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.requireRole(RoleTenant, h))
	}
	// operator gates everything that can take a device away from a running job
	// or disturb hardware somebody else is using. Each of these writes
	// farm.audit_log with the caller's name.
	operator := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.requireRole(RoleOperator, h))
	}

	// What this deployment can actually do, observed rather than declared.
	//
	// Gated at TENANT, which is the lowest role there is, and that is the whole
	// design: under an open authenticator the stub hands every caller operator,
	// so the debugging case this route exists for — an operator working on a
	// farm whose authentication is the broken thing — still reaches it. The
	// moment FARM_API_TOKENS is set, the same line starts asking for a
	// credential.
	//
	// It was registered bare, and that is the wrong shape for this particular
	// payload: it inventories the host agents, the schema, the fleet size and,
	// in the open case, a labelled warning that anyone reaching this port can
	// revoke leases and power-cycle slots. A route that hands a scanner the
	// reconnaissance AND the notice that nothing will stop them is not a
	// liveness probe, and unlike healthz it would have stayed open after the
	// authentication gap it describes was closed.
	tenant("GET /api/v1/capabilities", s.handleCapabilities)

	// Job specs. Reading the vocabulary needs no privilege: a client that
	// cannot ask what steps this server accepts has to hard-code them, and
	// then a schema change breaks it silently instead of loudly.
	tenant("POST /api/v1/specs/validate", s.handleSpecValidate)
	tenant("GET /api/v1/specs/kinds", s.handleSpecKinds)
	tenant("GET /api/v1/specs/resets", s.handleSpecResets)

	// Routes contributed by the parent, mounted before the catch-all UI so a
	// more specific pattern still wins. This is the seam for subsystems the
	// server does not own — the artifact store, which needs a blob backend the
	// server has no opinion about.
	for _, mount := range s.extraRoutes {
		mount(s, mux)
	}

	// Fleet and devices.
	tenant("GET /api/v1/fleet", s.handleFleet)
	tenant("GET /api/v1/devices/{id}", s.handleDevice)
	operator("POST /api/v1/devices/{id}/exec", s.handleDeviceExec)
	// reslot and rebrand change where a device is addressed and what the
	// phone says it is. Both refuse while the device holds a live lease.
	operator("POST /api/v1/devices/{id}/reslot", s.handleDeviceReslot)
	operator("POST /api/v1/devices/{id}/rebrand", s.handleDeviceRebrand)

	// Physical topology and host administration.
	tenant("GET /api/v1/topology", s.handleTopology)
	tenant("GET /api/v1/hosts", s.handleHosts)
	operator("POST /api/v1/hosts/{id}/drain", s.handleHostDrain)
	operator("POST /api/v1/hosts/{id}/undrain", s.handleHostUndrain)
	tenant("GET /api/v1/slots", s.handleSlotList)
	operator("POST /api/v1/slots", s.handleSlotRegister)
	operator("POST /api/v1/slots/{id}/label", s.handleSlotLabel)
	operator("POST /api/v1/slots/{id}/power", s.handleSlotPower)

	// Work.
	tenant("GET /api/v1/jobs", s.handleJobList)
	tenant("POST /api/v1/jobs", s.handleJobCreate)
	tenant("GET /api/v1/jobs/{id}", s.handleJobGet)
	tenant("POST /api/v1/jobs/{id}/cancel", s.handleJobCancel)

	// Leases. renew and release are the hot path; revoke is the human one.
	tenant("GET /api/v1/leases", s.handleLeaseList)
	tenant("POST /api/v1/leases/acquire", s.handleLeaseAcquire)
	tenant("POST /api/v1/leases/{id}/renew", s.handleLeaseRenew)
	tenant("POST /api/v1/leases/{id}/release", s.handleLeaseRelease)
	operator("POST /api/v1/leases/{id}/revoke", s.handleLeaseRevoke)

	// Recovery, quarantine and bulk work.
	tenant("GET /api/v1/recovery", s.handleRecovery)
	operator("POST /api/v1/quarantines/{id}/close", s.handleQuarantineClose)
	tenant("GET /api/v1/bulk", s.handleBulkList)
	operator("POST /api/v1/bulk", s.handleBulkCreate)
	tenant("GET /api/v1/bulk/{id}", s.handleBulkGet)

	// Audit and live updates.
	tenant("GET /api/v1/events", s.handleEvents)
	tenant("GET /api/v1/stream", s.handleStream)

	// An unmatched /api/v1/ path gets the JSON envelope rather than the
	// dashboard's HTML, so a client with a typo in a URL reads an error it can
	// parse. It is authenticated for the same reason the rest of the API is:
	// an unauthenticated 404 map is a free inventory of the control plane.
	// A catch-all under /api/v1/ matches every path in the prefix, so Go's own
	// method-mismatch handling never ran and DELETE /api/v1/fleet answered 404
	// — telling a client the resource does not exist when the resource is fine
	// and the verb is wrong. apiFallback answers 405 with an Allow header for a
	// known path and leaves a genuinely unknown path at 404.
	mux.Handle("/api/v1/", s.apiFallback(mux))

	// The dashboard. The parent supplies it; this package only holds the hook.
	mux.HandleFunc("/", s.serveUI)

	// instrument is OUTSIDE recoverer on purpose: a panic recovered below still
	// produces an access log line, a 500 in the request counter, and a
	// balanced in-flight gauge. The other order loses all three for exactly the
	// requests worth knowing about.
	return s.instrument(s.recoverer(mux))
}

// serveUI dispatches to the mounted dashboard, or explains its absence.
func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
	if ui := s.uiHandler(); ui != nil {
		ui.ServeHTTP(w, r)
		return
	}
	writeError(w, http.StatusNotFound, CodeUINotMounted,
		"no dashboard is mounted on this instance; the API itself is at /api/v1",
		map[string]string{"api_base": "/api/v1", "health": "/healthz", "metrics": "/metrics"})
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, CodeNotFound,
		"no such API route: "+r.Method+" "+r.URL.Path, nil)
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// recoverer turns a panic into a 500 with the standard envelope, and keeps the
// process alive.
//
// A panic in a dashboard query must not take down the listener that renewals
// arrive on. The stack is logged; the client is told nothing beyond "internal
// error", because a stack trace in a response body is an information leak.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the documented way for a handler to
			// abandon a response; it is not a bug and must not be logged as one.
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			s.log.ErrorContext(r.Context(), "panic serving request",
				"method", r.Method, "path", r.URL.Path, "panic", rec)
			writeError(w, http.StatusInternalServerError, CodeInternal, "internal error", nil)
		}()
		next.ServeHTTP(w, r)
	})
}

// streamPath is the one route whose response is long-lived by design. It is
// matched by path rather than by pattern because the pattern is not known until
// the mux has run, and the decision below has to be made before that.
const streamPath = "/api/v1/stream"

// instrument records metrics, assigns a request id, and writes the access log.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = newRequestID()
		}
		w.Header().Set("X-Request-Id", reqID)

		// The slot is filled in by requireRole, which runs inside the mux on a
		// derived request whose context this one cannot see.
		slot := &identitySlot{}
		r = r.WithContext(withIdentitySlot(r.Context(), slot))

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		// Event streams are excluded from the in-flight gauge. They are open
		// for hours on purpose, and counting them would make one wall-mounted
		// dashboard look like permanent saturation on the gauge an operator
		// reads to decide whether the API is overloaded.
		if r.URL.Path != streamPath {
			s.metrics.inFlight.Inc()
			defer s.metrics.inFlight.Dec()
		}
		next.ServeHTTP(rec, r)

		elapsed := time.Since(start)
		route := routeLabel(r)
		s.metrics.requests.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
		s.metrics.duration.WithLabelValues(route, r.Method).Observe(elapsed.Seconds())

		// The event stream is long-lived by design; logging its completion at
		// info with a multi-hour duration would look like a stall in every log
		// search. It is logged at debug instead.
		level := slog.LevelInfo
		if r.URL.Path == streamPath {
			level = slog.LevelDebug
		}

		args := []any{
			"method", r.Method,
			"route", route,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.written,
			"duration_ms", elapsed.Milliseconds(),
			"request_id", reqID,
		}
		if id, ok := slot.get(); ok {
			args = append(args, "subject", id.Subject, "role", string(id.Role))
		}
		s.log.Log(r.Context(), level, "request", args...)
	})
}

// routeLabel is the metric label for this request. r.Pattern is the pattern the
// mux matched, so cardinality is bounded by the routing table rather than by
// whatever ids clients happen to send.
func routeLabel(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return "unmatched"
}

// statusRecorder captures the status and byte count for logging and metrics.
//
// Unwrap is what keeps Server-Sent Events working: http.ResponseController
// finds the underlying ResponseWriter through it, so Flush still reaches the
// real connection through this wrapper.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (rec *statusRecorder) WriteHeader(code int) {
	if rec.wrote {
		return
	}
	rec.status = code
	rec.wrote = true
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if !rec.wrote {
		rec.wrote = true
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.written += int64(n)
	return n, err
}

// Unwrap exposes the wrapped writer to http.ResponseController (Flush, deadlines).
func (rec *statusRecorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// noteStatus records an outcome for a request that will never write a response.
func (rec *statusRecorder) noteStatus(code int) {
	if !rec.wrote {
		rec.status = code
	}
}

// noteClientGone marks a request the caller abandoned before anything was
// written.
//
// The recorder optimistically starts at 200, so without this an abandoned
// request is counted as a SUCCESS in farm_api_requests_total. On the renewal
// route that is the worst possible lie: a holder whose pod died mid-renewal
// would be indistinguishable, in the one series that matters, from a holder
// whose lease was renewed.
func noteClientGone(w http.ResponseWriter) {
	if rec, ok := w.(*statusRecorder); ok {
		rec.noteStatus(statusClientClosed)
	}
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A request id is diagnostic only; a nameless request is better than a
		// failed one.
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Request helpers
// ---------------------------------------------------------------------------

// decodeJSON reads a JSON request body into dst.
//
// Unknown fields are an error on purpose. The alternative — silently ignoring
// them — turns a typo like "max_runtime_sec" into a job with NO maximum
// runtime, which is the difference between a lease that ends on the clock the
// user wrote down and one that runs until a human notices. An empty body is
// accepted and leaves dst at its zero value, so per-field validation produces
// the specific message rather than "unexpected end of JSON input".
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	// A second JSON value in one body means the client is confused about the
	// protocol; accepting the first and ignoring the rest hides that.
	if dec.More() {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

// badJSON writes the 400 for a body that could not be decoded.
func badJSON(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, CodeBadRequest,
			"request body exceeds "+strconv.Itoa(maxRequestBody)+" bytes", nil)
		return
	}
	writeError(w, http.StatusBadRequest, CodeInvalidJSON,
		"the request body is not valid JSON for this endpoint: "+err.Error(), nil)
}

// badRequest writes a 400 naming the field at fault.
func badRequest(w http.ResponseWriter, message string, detail any) {
	writeError(w, http.StatusBadRequest, CodeBadRequest, message, detail)
}

// queryInt reads a bounded integer query parameter, clamping rather than
// failing: a dashboard asking for limit=100000 should get the maximum page, not
// an error dialog.
func queryInt(r *http.Request, key string, def, minVal, maxVal int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < minVal {
		return minVal
	}
	if n > maxVal {
		return maxVal
	}
	return n
}

// queryString reads and trims a query parameter.
func queryString(r *http.Request, key string) string {
	return strings.TrimSpace(r.URL.Query().Get(key))
}

// pathInt64 reads an integer path variable such as a slot or quarantine id.
func pathInt64(r *http.Request, key string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(r.PathValue(key)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// looksLikeUUID reports whether s has the canonical 8-4-4-4-12 shape.
//
// It is a request-shape check, not a validation: the database is still the
// authority, and a well-formed uuid that does not exist yields a 404 from
// there. Its job is to turn an obviously wrong path variable into a 400 with a
// clear message instead of SQLSTATE 22P02.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// secondsPtr converts a non-positive "unset" seconds value into nil, so an
// omitted duration reaches Postgres as NULL rather than as an interval of zero.
func secondsPtr(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}
