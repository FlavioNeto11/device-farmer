package e2e

// The dashboard's live stream, on a farm that requires credentials.
//
// This scenario exists because the defect it guards was invisible to every
// other test in the suite: they all speak HTTP with an Authorization header,
// and the browser cannot. EventSource takes a URL and nothing else, so on any
// farm with FARM_API_TOKENS set the dashboard's stream request arrived
// anonymous, was refused exactly as it should have been, and the page fell
// back to polling every five seconds — with the Step column, which is fed by
// the stream and by nothing else, showing an em dash on every row.
//
// So the request shapes below are deliberately the browser's, not a client's:
// GET /api/v1/stream with no Authorization header at all. What makes it work
// is a ticket minted over a request that can carry the header. See
// internal/api/stream_ticket.go for why that is allowed to live in a URL when
// the token is not.

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDashboardStreamsOnAnAuthenticatedFarm(t *testing.T) {
	// The api alone. This scenario is about who may open the stream, not about
	// what flows through it, and a scheduler or a ladder here would only add
	// farm-wide sweeps beside the assertions.
	f := newFarm(t, farmOpts{Roles: []string{"api"}})

	// -----------------------------------------------------------------
	// The defect, still refused.
	// -----------------------------------------------------------------
	//
	// This is byte for byte what `new EventSource('/api/v1/stream')` sends.
	// It must keep failing: the fix is not that the stream became reachable,
	// it is that the browser gained a way to authenticate.
	f.get(t, "", "/api/v1/stream").mustStatus(t, http.StatusUnauthorized)
	f.post(t, "", "/api/v1/stream/ticket", nil).mustStatus(t, http.StatusUnauthorized)

	// -----------------------------------------------------------------
	// Minting: an ordinary authenticated request, header and all.
	// -----------------------------------------------------------------

	minted := f.post(t, "operator", "/api/v1/stream/ticket", nil).mustStatus(t, http.StatusOK)
	ticket := minted.str(t, "ticket")
	if param := minted.str(t, "param"); param != "ticket" {
		t.Fatalf("the mint named its query parameter %q; this test and the dashboard both send \"ticket\"", param)
	}
	// The ticket must not be the token. A mint that echoed the credential back
	// would put the credential in the URL, which is the whole thing being
	// avoided.
	if strings.Contains(ticket, f.tokens["operator"]) || ticket == f.tokens["operator"] {
		t.Fatal("the minted ticket contains the bearer token")
	}

	// -----------------------------------------------------------------
	// Redeeming: the browser's request, now answered.
	// -----------------------------------------------------------------

	frame := browserStream(t, f, ticket)
	if frame == "" {
		t.Fatal("the stream opened but sent no event before the deadline")
	}

	// -----------------------------------------------------------------
	// What a copy of the ticket in a proxy log is worth.
	// -----------------------------------------------------------------

	// Spent. The legitimate connect above burned it, so the value sitting in
	// any access log has already been used by the time anyone can read it.
	if code := browserStreamStatus(t, f, ticket); code != http.StatusUnauthorized {
		t.Errorf("replaying a spent ticket = %d, want 401: a stream ticket is single-use", code)
	}

	// And a fresh one opens the stream and nothing else — not the fleet, not
	// the audit log, and not a successor ticket, which would make it permanent.
	fresh := f.post(t, "operator", "/api/v1/stream/ticket", nil).mustStatus(t, http.StatusOK).str(t, "ticket")
	for _, path := range []string{"/api/v1/fleet", "/api/v1/events", "/api/v1/leases"} {
		res := f.get(t, "", path+"?ticket="+url.QueryEscape(fresh))
		if res.Status != http.StatusUnauthorized {
			t.Errorf("GET %s with a stream ticket = %d, want 401", path, res.Status)
		}
	}
	if res := f.post(t, "", "/api/v1/stream/ticket?ticket="+url.QueryEscape(fresh), nil); res.Status != http.StatusUnauthorized {
		t.Errorf("minting with a stream ticket = %d, want 401: a ticket must not renew itself", res.Status)
	}
	// None of those refusals consumed it, so the dashboard's own connect still
	// works after a scanner has probed the farm with it.
	if code := browserStreamStatus(t, f, fresh); code != http.StatusOK {
		t.Errorf("the stream after those refusals = %d, want 200: they must refuse, not burn", code)
	}

	// -----------------------------------------------------------------
	// A tenant's ticket is a tenant's ticket.
	// -----------------------------------------------------------------
	//
	// The stream renders per identity, so a ticket that widened the scope it
	// was minted with would hand one tenant another's leases. The scope itself
	// is asserted in tenantscope_test.go; what matters here is that the
	// redeemed role is the minting role and not more.
	tenantTicket := f.post(t, "tenant", "/api/v1/stream/ticket", nil).mustStatus(t, http.StatusOK).str(t, "ticket")
	if code := browserStreamStatus(t, f, tenantTicket); code != http.StatusOK {
		t.Errorf("a tenant's ticket on the stream = %d, want 200", code)
	}
	// A tenant may not read a bulk run, with or without a ticket.
	bulk := f.post(t, "tenant", "/api/v1/stream/ticket", nil).mustStatus(t, http.StatusOK).str(t, "ticket")
	if res := f.get(t, "", "/api/v1/bulk?ticket="+url.QueryEscape(bulk)); res.Status != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/bulk with a tenant's stream ticket = %d, want 401", res.Status)
	}
}

// browserStream opens the event stream the way the dashboard does — no
// Authorization header, the ticket in the query string — and returns the name
// of the first event frame it receives.
func browserStream(t *testing.T, f *farm, ticket string) string {
	t.Helper()

	// Generous, because the first frame waits on one poll of the database the
	// control plane is also using. Subscribing kicks the poller, so the normal
	// path returns in well under a second; this is a ceiling on a failure.
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		f.API(t)+"/api/v1/stream?ticket="+url.QueryEscape(ticket), nil)
	if err != nil {
		t.Fatalf("building the stream request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening the event stream with a ticket: %v", err)
	}
	// Cancelling the context is what ends the stream; closing the body alone
	// would leave the server writing heartbeats into a reader that is gone.
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/stream?ticket=… = %d, want 200: the browser still cannot stream", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream: EventSource rejects anything else", ct)
	}

	var event string
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event != "":
			var payload map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				t.Fatalf("the %q frame is not JSON: %v", event, err)
			}
			// The first frame of a fresh subscription is the snapshot, which
			// is what fills the dashboard on connect — including the Step
			// column, whose only source is the "job" frame's step digest.
			if payload["snapshot"] != true {
				t.Fatalf("the first %q frame is a delta, not a snapshot: %v", event, payload)
			}
			return event
		}
	}
	return ""
}

// browserStreamStatus opens the same request and reports only the status,
// for the cases where the point is the refusal.
func browserStreamStatus(t *testing.T, f *farm, ticket string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		f.API(t)+"/api/v1/stream?ticket="+url.QueryEscape(ticket), nil)
	if err != nil {
		t.Fatalf("building the stream request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening the event stream: %v", err)
	}
	// A 200 here is a live stream this function does not read: cancel first so
	// the server stops writing into it, then close.
	cancel()
	_ = res.Body.Close()
	return res.StatusCode
}
