package api

// A stream ticket is a credential that travels in a URL, which is the shape
// this codebase otherwise forbids. Everything below pins one of the properties
// that makes that acceptable: it opens one route, it is spent once, it expires,
// it confers no more than the identity it was minted for, and it cannot be
// laundered into a longer-lived one.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ticketOutcome reads one counter out of a registry by gathering it, which is
// what /metrics does. prometheus/testutil would be shorter and pulls a module
// this project does not otherwise depend on.
func ticketOutcome(t *testing.T, reg *prometheus.Registry, outcome string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "test_stream_tickets_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "outcome" && lp.GetValue() == outcome {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// ticketTestServer is the minimum a mint and a redeem touch: an authenticator,
// a logger and a store. No database is involved — authorisation is decided
// before any handler runs.
func ticketTestServer(a Authenticator) *Server {
	return &Server{
		auth:    a,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		tickets: newStreamTicketStore(nil),
	}
}

// mintVia runs POST /api/v1/stream/ticket through requireRole exactly as the
// router does, and returns the status and the decoded body.
func mintVia(t *testing.T, s *Server, authz string) (int, streamTicketResponse) {
	t.Helper()
	h := s.requireRole(RoleTenant, http.HandlerFunc(s.handleStreamTicket))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stream/ticket", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out streamTicketResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// redeemVia presents a ticket the way EventSource does: in the query string,
// with no Authorization header at all.
func redeemVia(s *Server, method, path string, ticket string) (Identity, int, bool) {
	var reached bool
	var seen Identity
	h := s.requireRole(RoleTenant, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		seen, _ = IdentityFrom(r.Context())
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	}))
	target := path
	if ticket != "" {
		target += "?" + streamTicketParam + "=" + url.QueryEscape(ticket)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return seen, rec.Code, reached
}

func TestStreamTicketOpensTheStreamWithNoHeader(t *testing.T) {
	s := ticketTestServer(bearerFor(t, "ci:tenant:ci-bot:acme"))

	code, minted := mintVia(t, s, "Bearer ci")
	if code != http.StatusOK {
		t.Fatalf("minting a ticket = %d, want 200", code)
	}
	if minted.Ticket == "" {
		t.Fatal("the mint returned no ticket")
	}
	// The client is told the parameter name so it never hard-codes one that
	// can drift from the server's.
	if minted.Param != streamTicketParam || !minted.SingleUse || minted.ExpiresInS <= 0 {
		t.Fatalf("mint response = %+v, want param %q, single use and a positive lifetime",
			minted, streamTicketParam)
	}

	// This is the request a browser actually makes: no Authorization header.
	id, code, reached := redeemVia(s, http.MethodGet, streamPath, minted.Ticket)
	if code != http.StatusOK || !reached {
		t.Fatalf("redeeming a ticket on the stream = %d (handler reached: %v), want 200", code, reached)
	}
	// The scope is the whole point: the stream renders per identity, so a
	// ticket that widened one would leak another tenant's leases.
	if id.Subject != "ci-bot" || id.Role != RoleTenant || id.Tenant != "acme" {
		t.Errorf("redeemed identity = %+v, want ci-bot/tenant/acme", id)
	}
	// The method records both that a real credential vouched for this caller
	// and that this request rode in on a ticket.
	if id.Method != "bearer+ticket" {
		t.Errorf("method = %q, want bearer+ticket", id.Method)
	}
}

func TestStreamTicketIsSpentOnce(t *testing.T) {
	s := ticketTestServer(bearerFor(t, "op:operator:alice"))
	_, minted := mintVia(t, s, "Bearer op")

	if _, code, _ := redeemVia(s, http.MethodGet, streamPath, minted.Ticket); code != http.StatusOK {
		t.Fatalf("first redemption = %d, want 200", code)
	}
	// Single use is what makes the copy of this value sitting in a proxy log
	// worthless: by the time anyone reads it, the dashboard has spent it.
	if _, code, reached := redeemVia(s, http.MethodGet, streamPath, minted.Ticket); code != http.StatusUnauthorized || reached {
		t.Fatalf("second redemption = %d (handler reached: %v), want 401", code, reached)
	}
}

func TestStreamTicketOpensNothingButTheStream(t *testing.T) {
	s := ticketTestServer(bearerFor(t, "op:operator:alice"))

	// Every one of these must fail, and must not consume the ticket either:
	// the last line proves the ticket was still whole after all of them.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/fleet"},
		{http.MethodGet, "/api/v1/events"},
		{http.MethodGet, "/api/v1/leases"},
		// A ticket that could mint its own successor would be a permanent
		// credential in a URL, which is the thing this design refuses.
		{http.MethodPost, "/api/v1/stream/ticket"},
		// Same path, wrong verb.
		{http.MethodPost, streamPath},
	} {
		_, minted := mintVia(t, s, "Bearer op")
		_, code, reached := redeemVia(s, tc.method, tc.path, minted.Ticket)
		if code != http.StatusUnauthorized || reached {
			t.Errorf("%s %s with a stream ticket = %d (handler reached: %v), want 401",
				tc.method, tc.path, code, reached)
		}
		// It was refused rather than quietly burned, so the legitimate stream
		// connect that follows still works.
		if _, code, _ := redeemVia(s, http.MethodGet, streamPath, minted.Ticket); code != http.StatusOK {
			t.Errorf("after %s %s the ticket no longer opens the stream (%d)", tc.method, tc.path, code)
		}
	}
}

func TestStreamTicketExpires(t *testing.T) {
	s := ticketTestServer(bearerFor(t, "op:operator:alice"))
	now := time.Now()
	s.tickets.now = func() time.Time { return now }

	_, minted := mintVia(t, s, "Bearer op")

	// One second before the deadline it still works; one second after it does
	// not. The expiry is the property that makes an old log line harmless.
	now = now.Add(streamTicketTTL - time.Second)
	if _, code, _ := redeemVia(s, http.MethodGet, streamPath, minted.Ticket); code != http.StatusOK {
		t.Fatalf("redemption inside the lifetime = %d, want 200", code)
	}

	_, minted = mintVia(t, s, "Bearer op")
	now = now.Add(streamTicketTTL + time.Second)
	if _, code, reached := redeemVia(s, http.MethodGet, streamPath, minted.Ticket); code != http.StatusUnauthorized || reached {
		t.Fatalf("redemption after the lifetime = %d (handler reached: %v), want 401", code, reached)
	}
}

func TestStreamTicketRejectsGarbageAndAbsence(t *testing.T) {
	s := ticketTestServer(bearerFor(t, "op:operator:alice"))

	for name, ticket := range map[string]string{
		"no ticket at all":     "",
		"a value never minted": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"an empty parameter":   " ",
	} {
		t.Run(name, func(t *testing.T) {
			if _, code, reached := redeemVia(s, http.MethodGet, streamPath, ticket); code != http.StatusUnauthorized || reached {
				t.Fatalf("status = %d (handler reached: %v), want 401", code, reached)
			}
		})
	}
}

func TestStreamTicketNeedsACredentialToMint(t *testing.T) {
	s := ticketTestServer(bearerFor(t, "op:operator:alice"))

	// The mint is an ordinary authenticated route. If it were not, the ticket
	// would be a way to reach the stream without a credential at all, which is
	// precisely the hole it exists to close.
	for name, authz := range map[string]string{
		"no header":   "",
		"wrong token": "Bearer nope",
	} {
		t.Run(name, func(t *testing.T) {
			if code, out := mintVia(t, s, authz); code != http.StatusUnauthorized || out.Ticket != "" {
				t.Fatalf("mint = %d with ticket %q, want 401 and nothing", code, out.Ticket)
			}
		})
	}
}

// A header that authenticates must be answered by the authenticator, never by
// the store: the ticket path exists for the one client that has no header, and
// must not become a second way in for the clients that do.
func TestStreamTicketNeverPreemptsTheAuthenticator(t *testing.T) {
	s := ticketTestServer(bearerFor(t, "op:operator:alice", "ci:tenant:ci-bot:acme"))
	_, minted := mintVia(t, s, "Bearer ci")

	h := s.requireRole(RoleTenant, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := IdentityFrom(r.Context())
		writeJSON(w, http.StatusOK, map[string]string{"subject": id.Subject, "method": id.Method})
	}))
	req := httptest.NewRequest(http.MethodGet, streamPath+"?"+streamTicketParam+"="+minted.Ticket, nil)
	req.Header.Set("Authorization", "Bearer op")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["subject"] != "alice" || got["method"] != "bearer" {
		t.Fatalf("identity = %v, want alice authenticated by bearer: the header must win", got)
	}
	// And the unused ticket was left alone, not silently consumed by a request
	// that never needed it.
	if id, code, _ := redeemVia(s, http.MethodGet, streamPath, minted.Ticket); code != http.StatusOK || id.Subject != "ci-bot" {
		t.Fatalf("the untouched ticket = %d for %q, want 200 for ci-bot", code, id.Subject)
	}
}

// An open farm mints a ticket it never needs, and that is the intended shape:
// the dashboard runs one code path in both postures rather than branching on a
// posture it cannot observe, and the ticket is inert wherever the
// authenticator already says yes.
func TestStreamTicketIsInertOnAnOpenFarm(t *testing.T) {
	s := ticketTestServer(NewAllowAll(slog.New(slog.NewTextHandler(io.Discard, nil)), "demo-operator"))

	code, minted := mintVia(t, s, "")
	if code != http.StatusOK || minted.Ticket == "" {
		t.Fatalf("mint on an open farm = %d, ticket %q", code, minted.Ticket)
	}
	id, code, reached := redeemVia(s, http.MethodGet, streamPath, minted.Ticket)
	if code != http.StatusOK || !reached {
		t.Fatalf("the stream on an open farm = %d (handler reached: %v)", code, reached)
	}
	// AllowAll answered first, so the ticket was never spent and the audit
	// method still reads "allow-all": nothing about this mechanism can make an
	// open farm look authenticated.
	if id.Method != "allow-all" {
		t.Errorf("method = %q, want allow-all — the authenticator answers before any ticket", id.Method)
	}
	if n := len(s.tickets.tickets); n != 1 {
		t.Errorf("the store holds %d tickets, want the one that was minted and not needed", n)
	}
}

// A Server built by a test that never wired a store — authTestServer does
// exactly this — must refuse a ticket rather than panic on one.
func TestStreamTicketStoreTolerAtesBeingAbsent(t *testing.T) {
	s := authTestServer(bearerFor(t, "op:operator:alice"))
	if _, code, reached := redeemVia(s, http.MethodGet, streamPath, "anything"); code != http.StatusUnauthorized || reached {
		t.Fatalf("status = %d (handler reached: %v), want 401", code, reached)
	}
	if _, _, err := s.tickets.mint(Identity{Subject: "alice"}); err == nil {
		t.Fatal("a server with no ticket store minted a ticket")
	}
}

// One caller must not be able to spend the whole store on everybody's behalf.
// Minting is gated at the lowest role there is, so without a per-caller share
// a tenant looping on the mint would answer every operator's dashboard with a
// 503 during whatever incident had them watching.
// A ticket offered against a route it does not open is the one thing here no
// legitimate client ever does — the dashboard sends the parameter only on the
// stream — so it is the shape a leaked ticket being probed has. Refused
// silently it would be invisible; it gets its own series instead.
func TestStreamTicketMisroutingIsCounted(t *testing.T) {
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_stream_tickets_total", Help: "test"}, []string{"outcome"})
	reg := prometheus.NewRegistry()
	if err := reg.Register(counter); err != nil {
		t.Fatalf("registering the test counter: %v", err)
	}
	s := &Server{
		auth:    bearerFor(t, "op:operator:alice"),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		tickets: newStreamTicketStore(&httpMetrics{streamTickets: counter}),
	}

	_, minted := mintVia(t, s, "Bearer op")
	for _, path := range []string{"/api/v1/fleet", "/api/v1/events"} {
		redeemVia(s, http.MethodGet, path, minted.Ticket)
	}
	if got := ticketOutcome(t, reg, "misrouted"); got != 2 {
		t.Errorf("misrouted = %v, want 2", got)
	}

	// An ordinary anonymous request carries no ticket at all and must not be
	// counted as an attempt to use one: that would bury the signal under every
	// 401 the farm already serves.
	redeemVia(s, http.MethodGet, "/api/v1/fleet", "")
	if got := ticketOutcome(t, reg, "misrouted"); got != 2 {
		t.Errorf("misrouted = %v after a request with no ticket, want it unchanged at 2", got)
	}

	if _, code, _ := redeemVia(s, http.MethodGet, streamPath, minted.Ticket); code != http.StatusOK {
		t.Fatal("the probed ticket was consumed rather than refused")
	}
	if got := ticketOutcome(t, reg, "redeemed"); got != 1 {
		t.Errorf("redeemed = %v, want 1", got)
	}
}

func TestStreamTicketStoreIsBoundedPerCaller(t *testing.T) {
	store := newStreamTicketStore(nil)
	now := time.Now()
	store.now = func() time.Time { return now }

	noisy := Identity{Subject: "ci-bot", Role: RoleTenant, Tenant: "acme", Method: "bearer"}
	for i := 0; i < maxStreamTicketsPerSubject; i++ {
		if _, _, err := store.mint(noisy); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	if _, _, err := store.mint(noisy); err != errTooManyStreamTickets {
		t.Fatalf("mint past the caller's share = %v, want errTooManyStreamTickets", err)
	}

	// Everyone else is unaffected, which is the whole point of the second
	// ceiling.
	if _, _, err := store.mint(Identity{Subject: "alice", Role: RoleOperator, Method: "bearer"}); err != nil {
		t.Fatalf("an operator was refused a ticket because a tenant was looping: %v", err)
	}

	// The share is not a permanent state either: everything expires, and the
	// sweep on the next mint reclaims it. This is why the handler answers 503
	// rather than 500.
	now = now.Add(streamTicketTTL + time.Second)
	if _, _, err := store.mint(noisy); err != nil {
		t.Fatalf("mint after everything expired: %v", err)
	}
	if n := len(store.tickets); n != 1 {
		t.Errorf("store holds %d tickets after the sweep, want 1", n)
	}
}

func TestStreamTicketStoreIsBoundedOverall(t *testing.T) {
	store := newStreamTicketStore(nil)
	store.now = time.Now

	// Distinct subjects, so it is the global ceiling being reached and not any
	// one caller's share.
	for i := 0; i < maxStreamTickets; i++ {
		id := Identity{Subject: "sub-" + strconv.Itoa(i/8), Role: RoleTenant, Method: "bearer"}
		if _, _, err := store.mint(id); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	if _, _, err := store.mint(Identity{Subject: "one-more", Method: "bearer"}); err != errTooManyStreamTickets {
		t.Fatalf("mint past the global ceiling = %v, want errTooManyStreamTickets", err)
	}
}
