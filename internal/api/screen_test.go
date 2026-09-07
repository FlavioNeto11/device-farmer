package api

// The interactive-control routes, the half that needs no database.
//
// Two kinds of test live here. The first is the guard: a screen is the only
// thing in this API whose response cannot be masked by tenant, so the assertion
// that it is operator-only has to survive somebody widening it. That one reads
// router.go as text, because the property is about the registration and not
// about any handler's behaviour.
//
// The second is about what a refusal SAYS. This path has five distinct ways to
// say no — unconfigured, fenced-without-a-certificate, already open, at the cap,
// and the server did not start — and each sends an operator somewhere different.
// A 503 that does not name the variable to set is a support ticket; a 409 that
// does not distinguish "retry elsewhere" from "never retry this" is a client that
// hammers a phone. So the messages are asserted, not just the statuses.
//
// The streaming tests are in screen_db_test.go: they need a device row, and a
// device row needs a migrated database.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/artifacts"
	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/screen"
)

// ---------------------------------------------------------------------------
// The guard
// ---------------------------------------------------------------------------

// TestScreenRoutesAreOperatorOnly is modelled on TestBulkReadsAreOperatorOnly
// and exists for the same reason, only more so.
//
// Every tenant-readable route in this package narrows its answer by tenant_id,
// and the masking works by nilling named fields — see fleet.go. A framebuffer has
// no named fields. There is nothing to mask: the picture is whatever is on the
// phone, which may be another tenant's login screen, another tenant's test data,
// or another tenant's customer's name on a support call.
//
// So there is no correct tenant variant of this route, and the mutation this test
// exists to catch is not malice. It is somebody on a Friday letting tenants see
// "their own" devices, which is a sentence that cannot be enforced against a
// bitmap.
//
// Falsify: change either registration in router.go from operator( to tenant(.
func TestScreenRoutesAreOperatorOnly(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}
	for _, route := range []string{
		"GET /api/v1/devices/{id}/screen",
		"POST /api/v1/devices/{id}/input",
	} {
		if !strings.Contains(string(src), `operator("`+route+`"`) {
			t.Errorf("%s is not registered with operator(...). A framebuffer cannot be masked "+
				"by tenant: there are no named fields to nil, and the picture is whatever is "+
				"on the phone.", route)
		}
		if strings.Contains(string(src), `tenant("`+route+`"`) {
			t.Errorf("%s is registered with tenant(...). Every tenant-readable route here "+
				"narrows by tenant_id and this one cannot.", route)
		}
	}
}

// TestTheScreenStreamIsExcludedFromTheInFlightGauge.
//
// The gauge is what an operator reads to decide whether the api is overloaded. A
// screen left open on a wall display is in-flight for hours by design, so four
// phones on a dashboard would pin it at four permanently — the gauge would report
// saturation on a farm doing nothing, and then report exactly the same thing when
// it really was saturated.
//
// The event stream already had this exclusion; adding a second long-lived route
// without extending it is the bug. isLongLived is where both now live.
//
// Falsify: make isLongLived return p == streamPath only.
func TestTheScreenStreamIsExcludedFromTheInFlightGauge(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/api/v1/stream", true},
		{"/api/v1/devices/df-abc/screen", true},
		{"/api/v1/devices/9f1c0d2e-0000-0000-0000-000000000000/screen", true},

		// And the things that merely look like it must NOT be excluded, or a
		// sloppy predicate silently stops counting real requests.
		{"/api/v1/devices/df-abc", false},
		{"/api/v1/devices/df-abc/input", false},
		{"/api/v1/devices/df-abc/exec", false},
		{"/api/v1/fleet", false},
		{"/api/v1/screen", false},
		{"/screen", false},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if got := isLongLived(r); got != tc.want {
			t.Errorf("isLongLived(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Configuration refusals
// ---------------------------------------------------------------------------

// screenServer builds a Server whose database is never reached.
//
// The pool is lazy — pgxpool.New does not connect until a query runs — and every
// case below is refused by screenUnavailable, which runs BEFORE lookupDevice.
// That ordering is the thing being relied on, and it is also the right ordering
// on its own terms: a farm with no screen server pinned does not need a database
// round trip to find that out.
func screenServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/unreachable?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	s, err := New(cfg, pool,
		WithAuthenticator(NewAllowAll(slog.New(slog.DiscardHandler), "tester")),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		// A stub store, because none of these cases is about the artifact
		// store; the case that IS about it builds a Server without one.
		WithScreenArtifacts(stubScreenArtifacts{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.screens.Close() })
	return s
}

// TestAFarmWithNoScreenServerNamesTheVariablesToSet.
//
// This is the refusal almost every reader of this feature meets first, because it
// is the default: a farm that upgraded into this code has pinned no jar. A 503
// that says "unavailable" and stops is a support ticket. One that names
// FARM_SCREEN_SERVER_SHA and FARM_SCREEN_SERVER_VERSION is a deploy.
//
// Falsify: drop the remedy from screenUnavailable's first case.
func TestAFarmWithNoScreenServerNamesTheVariablesToSet(t *testing.T) {
	srv := screenServer(t, &config.Config{})

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/devices/dev-1/screen", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/devices/dev-1/input",
			strings.NewReader(`{"session":"x","events":[{"type":"key","action":"down","keycode":4}]}`)),
	} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status %d, want 503\n%s", req.Method, req.URL.Path, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		for _, want := range []string{
			config.EnvScreenServerSHA, config.EnvScreenServerVersion, faultConfiguration,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s %s: the refusal does not mention %s, so an operator is not told "+
					"what to set:\n%s", req.Method, req.URL.Path, want, body)
			}
		}
	}
}

// TestAFencedFarmWithNoControlCertificateSaysSoRatherThanFailingToDial.
//
// On a farm with no fence proxy the admission preamble is inert and a plain dial
// is what every other ADB client in this process already does. On a FENCED farm
// the proxy requires mTLS, so a screen with no control certificate fails the
// handshake — and a handshake failure surfacing as a dial error sends an operator
// to look at the network, which is the one place the problem is not.
//
// Falsify: delete the FenceClient.Enabled() && !FenceControl.Enabled() case from
// screenUnavailable.
func TestAFencedFarmWithNoControlCertificateSaysSoRatherThanFailingToDial(t *testing.T) {
	cfg := screenOnConfig()
	cfg.FenceClient = fenceClientOn()
	srv := screenServer(t, cfg)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/devices/dev-1/screen", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{config.EnvFenceControlCert, config.EnvFenceControlKey} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not name %s:\n%s", want, body)
		}
	}
}

// TestAnUnfencedFarmIsNotRefusedForHavingNoControlCertificate is the other half,
// and it is the half that makes the demo work at all.
//
// A farm with no fence proxy anywhere dials ADB in the clear, exactly as exec
// does today. Requiring a control certificate there would make the feature
// unreachable on every evaluation install, for no gain: there is no proxy to
// present it to.
//
// Falsify: make screenUnavailable require FenceControl.Enabled()
// unconditionally.
func TestAnUnfencedFarmIsNotRefusedForHavingNoControlCertificate(t *testing.T) {
	srv := screenServer(t, screenOnConfig())
	if reason, _ := srv.screenUnavailable(); reason != "" {
		t.Errorf("an unfenced farm with a pinned jar reports the screen path unavailable: %s\n"+
			"There is no proxy to present a certificate to, and exec dials the same way.", reason)
	}
}

// TestATenantTokenCannotOpenAScreen is the runtime half of the guard above. The
// text assertion catches the registration changing; this catches the gate itself
// not working.
func TestATenantTokenCannotOpenAScreen(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/unreachable?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	s, err := New(screenOnConfig(), pool,
		WithAuthenticator(bearerFor(t, "t-token:tenant:ci:acme")),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.screens.Close() })

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/devices/dev-1/screen", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/devices/dev-1/input", strings.NewReader(`{}`)),
	} {
		req.Header.Set("Authorization", "Bearer t-token")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: a tenant token got %d, want 403\n%s",
				req.Method, req.URL.Path, rec.Code, rec.Body)
		}
	}
}

// TestInputForASessionThisReplicaDoesNotHoldIsAConflictNotANotFound.
//
// With more than one api replica a session lives in exactly one of them, so a 404
// would read as "that session is over" when it is running perfectly well next
// door — and the client would tear down a working screen. A 409 naming
// session_not_here, with a counter behind it, is how an operator discovers they
// need api.service.sessionAffinity. stream_ticket.go makes the same argument for
// mint and redeem.
//
// Falsify: return 404 / CodeNotFound from the Lookup failure in
// handleDeviceInput.
func TestInputForASessionThisReplicaDoesNotHoldIsAConflictNotANotFound(t *testing.T) {
	srv := screenServer(t, screenOnConfig())

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/devices/dev-1/input",
		strings.NewReader(`{"session":"deadbeef","events":[{"type":"key","action":"down","keycode":4}]}`)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409; a 404 here reads as \"the session is over\" when it is "+
			"alive in another replica\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"session_not_here", "sessionAffinity"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, body)
		}
	}
}

// TestTheInputBatchIsBounded. A batch coalesces one gesture into one round trip.
// It is not a channel for bulk work, and an unbounded one spends a phone's input
// queue from a single request.
func TestTheInputBatchIsBounded(t *testing.T) {
	srv := screenServer(t, screenOnConfig())

	events := make([]map[string]any, maxInputBatch+1)
	for i := range events {
		events[i] = map[string]any{"type": "touch", "action": "move", "x": 1, "y": 1}
	}
	body, err := json.Marshal(map[string]any{"session": "x", "events": events})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/devices/dev-1/input", strings.NewReader(string(body))))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a batch of %d was accepted with status %d\n%s", len(events), rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------------------
// Decoding input
// ---------------------------------------------------------------------------

// TestAnUnknownEventTypeIsRefusedRatherThanDropped.
//
// THIS IS THE ONE THAT MATTERS about decoding. A batch is down-move-up. If an
// unknown event in the middle were skipped, the caller would be told
// "accepted: 3" for a gesture that was never completed — and the phone would be
// left behaving as though somebody is still holding the screen, with nothing in
// the response saying so.
//
// Falsify: in decodeInputEvents, `continue` on the default case instead of
// returning an error.
func TestAnUnknownEventTypeIsRefusedRatherThanDropped(t *testing.T) {
	for _, events := range [][]inputEvent{
		{{Type: "touch", Action: "down", X: 1, Y: 1}, {Type: "wiggle"}, {Type: "touch", Action: "up", X: 1, Y: 1}},
		{{Type: ""}},
		{{Type: "touch", Action: "sideways"}},
		{{Type: "touch"}},
		{{Type: "key", Action: "sideways"}},
		{{Type: "key"}},
	} {
		if _, err := decodeInputEvents(events); err == nil {
			t.Errorf("decodeInputEvents accepted %+v; an event silently dropped from a "+
				"down-move-up gesture leaves a finger down that nothing will lift", events)
		}
	}
}

// TestARefusedEventNamesItsPositionInTheBatch. "event 2 of 3" is the difference
// between a caller fixing one event and a caller retrying a whole gesture blind.
func TestARefusedEventNamesItsPositionInTheBatch(t *testing.T) {
	_, err := decodeInputEvents([]inputEvent{
		{Type: "touch", Action: "down", X: 1, Y: 1},
		{Type: "nonsense"},
	})
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "event 2 of 2") {
		t.Errorf("the error does not say which event was refused: %v", err)
	}
}

// TestAFingerComingUpHasNoPressure. Every browser reports the pressure of the
// pointer that just left, and a device handed a non-zero pressure on an UP
// resolves the contradiction by keeping the pointer down. Normalising is kinder
// than refusing: the caller is not wrong to pass on what the event said.
//
// Falsify: delete the `if action == scrcpy.TouchUp { p = 0 }` normalisation.
func TestAFingerComingUpHasNoPressure(t *testing.T) {
	events, err := decodeInputEvents([]inputEvent{
		{Type: "touch", Action: "up", X: 1, Y: 1, Pressure: 1.0},
	})
	if err != nil {
		t.Fatalf("decodeInputEvents: %v", err)
	}
	touch, ok := events[0].(screen.Touch)
	if !ok {
		t.Fatalf("decoded to %T, want screen.Touch", events[0])
	}
	if touch.Pressure != 0 {
		t.Errorf("an UP carries pressure %v; the device keeps the pointer down and the phone "+
			"behaves as though somebody is still holding the screen", touch.Pressure)
	}
}

// TestTheThreeEventTypesAllDecode is the breadth check, so that adding a fourth
// shape to the wire without adding it here is visible.
func TestTheThreeEventTypesAllDecode(t *testing.T) {
	events, err := decodeInputEvents([]inputEvent{
		{Type: "touch", Action: "down", X: 1, Y: 2, Pressure: 1},
		{Type: "key", Action: "up", Keycode: 4, MetaState: 0},
		{Type: "scroll", X: 1, Y: 2, V: -1},
	})
	if err != nil {
		t.Fatalf("decodeInputEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("decoded %d events, want 3", len(events))
	}
	if _, ok := events[0].(screen.Touch); !ok {
		t.Errorf("events[0] is %T, want screen.Touch", events[0])
	}
	if _, ok := events[1].(screen.Key); !ok {
		t.Errorf("events[1] is %T, want screen.Key", events[1])
	}
	if _, ok := events[2].(screen.Scroll); !ok {
		t.Errorf("events[2] is %T, want screen.Scroll", events[2])
	}
}

// ---------------------------------------------------------------------------
// Config helpers
// ---------------------------------------------------------------------------

// screenOnConfig is a farm with a jar pinned and no fence proxy: the shape the
// demo and every evaluation install run in.
func screenOnConfig() *config.Config {
	return &config.Config{
		Screen: config.Screen{
			ServerSHA:     strings.Repeat("ab", 32),
			ServerVersion: "3.1",
			MaxSize:       config.DefaultScreenMaxSize,
			SessionTTL:    config.DefaultScreenSessionTTL,
			MaxSessions:   config.DefaultScreenMaxSessions,
		},
	}
}

// fenceClientOn is a farm that enforces the fence at the device. Only the
// presence of a TLS config matters here — FenceClient.Enabled() is exactly
// TLS != nil — and building a real certificate chain would test internal/config
// rather than this package's branch on it.
func fenceClientOn() config.FenceClient {
	return config.FenceClient{TLS: &tls.Config{MinVersion: tls.VersionTLS13}}
}

// stubScreenArtifacts satisfies the narrow store interface and is never called
// by a test in this file: every case here is refused before a jar is needed.
type stubScreenArtifacts struct{}

func (stubScreenArtifacts) EnsureOnDevice(context.Context, string, string, artifacts.PushFunc) (artifacts.EnsureResult, error) {
	return artifacts.EnsureResult{}, nil
}

// TestAProcessWithNoArtifactStoreSaysWhichDirectoryToSet. The api role opens one
// for POST /api/v1/artifacts; a farm where that failed gets a warning at startup
// and, until now, no explanation at all on this route.
//
// Falsify: delete the screenStore == nil case from screenUnavailable.
func TestAProcessWithNoArtifactStoreSaysWhichDirectoryToSet(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/unreachable?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	s, err := New(screenOnConfig(), pool,
		WithAuthenticator(NewAllowAll(slog.New(slog.DiscardHandler), "tester")),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.screens.Close() })

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/devices/dev-1/screen", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503\n%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "FARM_ARTIFACT_DIR") {
		t.Errorf("the refusal does not name the directory to set:\n%s", rec.Body)
	}
}

// TestEveryMisconfigurationIsReportedAtOnce.
//
// internal/config states the rule and the reason: an operator with two problems
// who is told about one of them does two deploys to find both. A farm turning
// this feature on for the first time plausibly has all three wrong at once.
//
// Falsify: turn screenUnavailable back into a switch that returns on the first
// match.
func TestEveryMisconfigurationIsReportedAtOnce(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/unreachable?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	// Nothing pinned, no store, and a fence with no control certificate.
	cfg := &config.Config{FenceClient: fenceClientOn()}
	s, err := New(cfg, pool,
		WithAuthenticator(NewAllowAll(slog.New(slog.DiscardHandler), "tester")),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.screens.Close() })

	reason, remedy := s.screenUnavailable()
	for _, want := range []string{"no screen server pinned", "no artifact store", "control-class"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the reason does not mention %q, so this problem is invisible until the "+
				"others are fixed:\n%s", want, reason)
		}
	}
	for _, want := range []string{
		config.EnvScreenServerSHA, "FARM_ARTIFACT_DIR", config.EnvFenceControlCert,
	} {
		if !strings.Contains(remedy, want) {
			t.Errorf("the remedy does not name %s:\n%s", want, remedy)
		}
	}
}
