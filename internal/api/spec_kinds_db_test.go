package api

// The step vocabulary this server PUBLISHES must describe the steps it can
// actually run.
//
// GET /api/v1/specs/kinds reads farm.step_kinds and hands the description
// column to whoever asked — `ctl kinds`, the Docs tab, anything a tenant
// writes. router.go leaves the route unprivileged precisely so a client can
// hard-code nothing and ask, which makes a wrong row the control plane lying
// about itself to the clients that behaved best.
//
// test/assertions_v21.sql pins what those rows SAY. This file pins something
// SQL cannot reach: that the identifiers inside them still name things this
// build accepts, in the step they are written under.
//
// A description is prose and cannot be diffed against a struct. Its
// identifiers can, and every drift worth catching shows up as one:
//
//   - wait_for describes two forms and names them by the fields that select
//     between them. Rename jobspec.WaitFor.Handle and the row keeps offering a
//     "handle" key the decoder no longer accepts.
//   - it names the kind a handle belongs to. Rename shell_detached and the row
//     points at a step nobody offers.
//   - and no row may offer a key that belongs to a DIFFERENT payload. That is
//     not a pedantic check: jobspec decodes with DisallowUnknownFields, so an
//     author who believes such a row has their whole spec refused at parse
//     time. The near miss this rule was written for is wait_for telling authors
//     to judge "against expect_exit" — which reads perfectly, and names a field
//     of the shell payload and of nothing else. The key that actually decides a
//     detached run's verdict is the spec-level default_expect_exit, because
//     runner.detachedExpectExit resolves it from an EMPTY shell.
//
// A token that names nothing in the contract is ignored on purpose. These
// descriptions are about Android, and a truthful one may well mention
// sys.boot_completed; a rule that failed on device-side names would be a test
// blocking correct prose.
//
// Needs DATABASE_URL pointing at a MIGRATED database and skips without one.
// Nothing is written; the only query is the endpoint's own SELECT.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/jobspec"
)

// specKindsPath is the route these tests ask, spelled as router.go registers
// it — a client asks by URL, not by handler.
const specKindsPath = "/api/v1/specs/kinds"

// payloadTypes is one value per step kind, in the vocabulary's order.
//
// Spelled out because jobspec.KindInfo deliberately carries no payload type:
// the database's row is about behaviour, not about Go. The order is CHECKED
// against jobspec.Kinds() rather than trusted, so a merge that duplicated one
// entry and dropped another — which keeps the length right — cannot silently
// widen or narrow what the descriptions are read against.
var payloadTypes = []jobspec.Payload{
	jobspec.Push{}, jobspec.Install{}, jobspec.Uninstall{}, jobspec.Shell{},
	jobspec.ShellDetached{}, jobspec.WaitFor{}, jobspec.Pull{}, jobspec.Assert{},
	jobspec.Reset{}, jobspec.Sleep{},
}

// snakeToken matches the shape a wire key takes: lowercase words joined by
// underscores. It is a candidate, not a verdict — only tokens that turn out to
// be real keys of some other step are reported.
var snakeToken = regexp.MustCompile(`[a-z][a-z0-9]*(?:_[a-z0-9]+)+`)

// publishedKinds asks the endpoint the way a client does, once.
func publishedKinds(t *testing.T) map[string]string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		t.Skip("no DATABASE_URL; the published vocabulary lives in the database")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	s, err := New(&config.Config{}, pool,
		WithAuthenticator(bearerFor(t, "op-token:operator:alice")),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// New takes a background context that only Shutdown cancels. There is no
	// http.Server here, so this is the whole of it.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})

	req := httptest.NewRequest(http.MethodGet, specKindsPath, nil)
	req.Header.Set("Authorization", "Bearer op-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", specKindsPath, rec.Code, rec.Body.String())
	}

	var body struct {
		Kinds []struct {
			Kind        string `json:"kind"`
			Description string `json:"description"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := make(map[string]string, len(body.Kinds))
	for _, k := range body.Kinds {
		out[k.Kind] = strings.ToLower(k.Description)
	}
	if len(out) != len(jobspec.Kinds()) {
		t.Fatalf("the endpoint published %d kinds for a %d-kind vocabulary", len(out), len(jobspec.Kinds()))
	}
	return out
}

// jsonTag returns the wire name of a struct field, by its GO name.
//
// Looking the field up by name is the point: the test names WaitFor.Handle, so
// removing or renaming that field fails here — with a message about the
// description — at the moment the change is made, rather than leaving the
// database offering a key the decoder stopped accepting.
func jsonTag(t *testing.T, p jobspec.Payload, field string) string {
	t.Helper()
	rt := reflect.TypeOf(p)
	f, ok := rt.FieldByName(field)
	if !ok {
		t.Fatalf("%s has no field %s any more. farm.step_kinds describes that field to every "+
			"client that asks; correct the %q row in a new migration, and test/assertions_v21.sql "+
			"with it, before removing it here", rt, field, p.Kind())
	}
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "" {
		t.Fatalf("%s.%s carries no json name, so no description can refer to it", rt, field)
	}
	return name
}

// payloadKeys is every wire key of one payload, including the ones omitempty
// hides when they are zero.
func payloadKeys(p jobspec.Payload) map[string]bool {
	out := map[string]bool{}
	rt := reflect.TypeOf(p)
	for i := 0; i < rt.NumField(); i++ {
		if name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ","); name != "" {
			out[name] = true
		}
	}
	return out
}

// envelopeKeys is every wire key a spec carries ABOVE its payloads.
//
// Taken from the encoder rather than from reflection, because jobspec.Step
// carries no json tags at all: its wire shape comes from an unexported
// envelope, so marshalling a fully populated value is the only way to learn
// those names without restating them here and being wrong later.
func envelopeKeys(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	step := jobspec.Step{
		ID:              "id",
		Timeout:         jobspec.Duration(time.Second),
		ContinueOnError: true,
		Payload:         jobspec.Sleep{Duration: jobspec.Duration(time.Second)},
	}
	for _, v := range []any{
		jobspec.Spec{
			Version:           jobspec.SpecVersion,
			DefaultTimeout:    jobspec.Duration(time.Second),
			DefaultExpectExit: []int{0},
			Steps:             []jobspec.Step{step},
		},
		step,
	} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %T: %v", v, err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("decode %T: %v", v, err)
		}
		for k := range obj {
			out[k] = true
		}
	}
	return out
}

// TestPublishedStepVocabulary reads what the endpoint answers against the
// contract this binary implements. One fixture, because the answer is the same
// for every check and each one costs a pool, a server and a metric registry.
func TestPublishedStepVocabulary(t *testing.T) {
	kinds := publishedKinds(t)

	// ------------------------------------------------------------------
	// The defect 00021 fixed: wait_for has two forms that conclude
	// different things — a probe form that says only that its own command
	// exited zero, and a handle form that reads a shell_detached run's
	// published status and judges it. The row described the first for as
	// long as both existed, so a client that asked was told about the form
	// that cannot express what it wanted. The probe an author writes
	// instead, `test -f …/soak.result`, goes true for a 137 exactly as
	// eagerly as for a 0.
	// ------------------------------------------------------------------
	t.Run("wait_for names both of its forms", func(t *testing.T) {
		desc := kinds[string(jobspec.KindWaitFor)]
		if desc == "" {
			t.Fatalf("the endpoint published no description for %s", jobspec.KindWaitFor)
		}
		for _, field := range []string{"Probe", "Handle"} {
			tag := jsonTag(t, jobspec.WaitFor{}, field)
			if !strings.Contains(desc, tag) {
				t.Errorf("the published description of %s does not name %q, one of the two forms "+
					"the step has; a client building a spec from this answer cannot discover the "+
					"other one:\n  %s", jobspec.KindWaitFor, tag, desc)
			}
		}
		// The handle form is meaningless without the step that declares a
		// handle. Spelled with the constant, so renaming the kind fails here
		// rather than leaving the row pointing at a step nobody offers.
		if !strings.Contains(desc, string(jobspec.KindShellDetached)) {
			t.Errorf("the published description of %s names a handle without naming %s, the only "+
				"step that declares one:\n  %s", jobspec.KindWaitFor, jobspec.KindShellDetached, desc)
		}
	})

	// ------------------------------------------------------------------
	// No row may offer a key that belongs to a different step.
	// ------------------------------------------------------------------
	t.Run("no row offers another step's key", func(t *testing.T) {
		if len(payloadTypes) != len(jobspec.Kinds()) {
			t.Fatalf("payloadTypes covers %d kinds, the vocabulary has %d; a new kind needs an "+
				"entry here so its description is read against its payload",
				len(payloadTypes), len(jobspec.Kinds()))
		}
		own := make(map[string]map[string]bool, len(payloadTypes))
		anyPayload := map[string]bool{}
		for i, p := range payloadTypes {
			if want := jobspec.Kinds()[i].Kind; p.Kind() != want {
				t.Fatalf("payloadTypes[%d] is %s where the vocabulary has %s; the list is read "+
					"positionally and no longer says what it claims", i, p.Kind(), want)
			}
			keys := payloadKeys(p)
			own[string(p.Kind())] = keys
			for k := range keys {
				anyPayload[k] = true
			}
		}

		// Legal in any row: the spec- and step-level keys, and the name of
		// every kind — a description that cross-references another step is
		// exactly what wait_for and shell_detached need to do.
		shared := envelopeKeys(t)
		for _, info := range jobspec.Kinds() {
			shared[string(info.Kind)] = true
		}

		for kind, desc := range kinds {
			var foreign []string
			for _, tok := range snakeToken.FindAllString(desc, -1) {
				if anyPayload[tok] && !own[kind][tok] && !shared[tok] {
					foreign = append(foreign, tok)
				}
			}
			if len(foreign) > 0 {
				sort.Strings(foreign)
				t.Errorf("the published description of %s offers %v, which no %s payload accepts; "+
					"jobspec decodes with DisallowUnknownFields, so an author who believes this row "+
					"has the whole spec refused at parse time:\n  %s", kind, foreign, kind, desc)
			}
		}
	})
}
