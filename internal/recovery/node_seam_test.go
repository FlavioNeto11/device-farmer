package recovery_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/node"
	"github.com/flaviopadilha/device-farmer/internal/recovery"
)

// This file sits on the seam between the two packages, which is why it is an
// external test package: recovery cannot import node (node imports recovery),
// and the defect lived precisely in the fact that the two never met in a test.
//
// The ladder classifies a HostRunner's error with errors.Is against its own
// sentinels and takes anything else as a failed rung — the answer it escalates
// on. In production the HostRunner is *node.Client, whose ErrRefused and
// ErrUnreachable were plain errors.New and answered to neither, so every
// refusal an agent made and every host the client could not reach reached the
// ladder as broken hardware. The unit tests on each side passed throughout:
// recovery's used fakes that wrapped recovery's sentinels, and node's asked
// only node's questions.
//
// Nothing here can touch a lease: the actuator under test is the shipped one,
// and TestNothingHereCanEndALease guards its vocabulary.

// stubAgent answers the node API's two hardware routes with one prepared
// status and body, so the client sees exactly what a farmd-node would send.
type stubAgent struct {
	hostID string
	status int
	body   node.OpResponse
}

func (s *stubAgent) handler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		var req node.OpRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.HostID != s.hostID {
			http.Error(w, "not this host", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_ = json.NewEncoder(w).Encode(s.body)
	}
	mux.HandleFunc("POST "+node.PathUSBReset, reply)
	mux.HandleFunc("POST "+node.PathPortPower, reply)
	return mux
}

// deadEndpoint is an address that was listening a moment ago and is not any
// more: the shape of an agent that has gone away, which the client must report
// as unreachable after its dial retries and not as a rung that ran.
func deadEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

func realClient(t *testing.T, hostID, endpoint string) *node.Client {
	t.Helper()
	c, err := node.NewClient(node.ClientConfig{
		Resolver:     node.StaticResolver{hostID: endpoint},
		Token:        "seam-test-token",
		DialRetries:  2,
		RetryBackoff: time.Millisecond,
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func rungOn(hostID string, tier int) recovery.Action {
	names := map[int]string{3: "usb_reset", 4: "port_power"}
	return recovery.Action{
		Tier: tier, TierName: names[tier],
		DeviceID: "22222222-2222-2222-2222-222222222222",
		SlotID:   7, Devpath: "usb:3-1.4", RackSlot: "r1u3s04",
		HostID: hostID,
	}
}

// TestRealNodeClientErrorsReachTheLadderClassified drives the shipped
// ADBActuator with the shipped *node.Client against an agent that refuses,
// an agent that cannot, an agent that has gone away, and — as the control —
// an agent whose rung genuinely failed. The first three must not escalate; the
// last must.
//
// Falsify, one at a time: make node.ErrRefused a plain errors.New (the two
// refusals below classify as failed); make node.ErrUnreachable a plain
// errors.New (the dead agent classifies as failed); drop the ReasonGanged arm
// from Client.statusError (the ganged case loses its kind).
func TestRealNodeClientErrorsReachTheLadderClassified(t *testing.T) {
	t.Parallel()
	const hostID = "rack1-host-a"

	cases := []struct {
		name     string
		tier     int
		agent    *stubAgent // nil means the agent is not there
		want     recovery.Disposition
		wantKind string
		// wantWords must survive from the agent's own sentence into the
		// refusal an operator reads.
		wantWords string
	}{
		{
			name: "a ganged domain is refused, and refused for that reason",
			tier: 4,
			agent: &stubAgent{hostID: hostID, status: http.StatusConflict, body: node.OpResponse{
				Error: "cycling port 4 of hub 3-1 shares one power domain with 2 device(s) " +
					"nobody authorised — usb:3-1.2, usb:3-1.3",
				Refused: true, Reason: node.ReasonGanged}},
			want: recovery.DispositionRefused, wantKind: recovery.RefusalKindGanged,
			wantWords: "shares one power domain",
		},
		{
			name: "the agent's own policy is a refusal with no kind",
			tier: 3,
			agent: &stubAgent{hostID: hostID, status: http.StatusConflict, body: node.OpResponse{
				Error:   "this request names host rack1-host-a and usb:3-1.4 is a port on rack9",
				Refused: true, Reason: node.ReasonPolicy}},
			want: recovery.DispositionRefused, wantWords: "is a port on rack9",
		},
		{
			name: "a build that cannot perform the rung is a refusal",
			tier: 3,
			agent: &stubAgent{hostID: hostID, status: http.StatusNotImplemented, body: node.OpResponse{
				Error: "USBDEVFS_RESET: not supported on this host", Refused: true,
				Reason: node.ReasonUnsupported}},
			want: recovery.DispositionRefused, wantWords: "not supported on this host",
		},
		{
			name: "an agent that has gone away is unreachable",
			tier: 4,
			want: recovery.DispositionUnreachable,
		},
		{
			name: "a rung the agent ran and lost is failed, and escalates",
			tier: 3,
			agent: &stubAgent{hostID: hostID, status: http.StatusInternalServerError,
				body: node.OpResponse{Error: "the device did not re-enumerate within 20s"}},
			want: recovery.DispositionFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			endpoint := deadEndpoint(t)
			if tc.agent != nil {
				srv := httptest.NewServer(tc.agent.handler())
				t.Cleanup(srv.Close)
				endpoint = srv.URL
			}
			actuator := recovery.NewADBActuator(slog.New(slog.DiscardHandler),
				realClient(t, hostID, endpoint))

			res, err := actuator.Recover(context.Background(), rungOn(hostID, tc.tier))
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			got := recovery.DispositionOf(res)
			if got != tc.want {
				t.Fatalf("disposition = %q, want %q; the ladder %s on this verdict, and the "+
					"row says: %s", got, tc.want,
					map[bool]string{true: "ESCALATES", false: "holds"}[got.Escalate()],
					recovery.RefusalOf(res))
			}
			if got.Escalate() != (tc.want == recovery.DispositionFailed) {
				t.Fatalf("Escalate() = %v for %q", got.Escalate(), got)
			}
			if kind := recovery.RefusalKindOf(res); kind != tc.wantKind {
				t.Errorf("refusal_kind = %q, want %q", kind, tc.wantKind)
			}
			if tc.wantWords != "" && !strings.Contains(recovery.RefusalOf(res), tc.wantWords) {
				t.Errorf("the agent's words %q did not reach the refusal: %q",
					tc.wantWords, recovery.RefusalOf(res))
			}
		})
	}
}
