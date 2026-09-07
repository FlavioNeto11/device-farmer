package api

// POST /api/v1/devices/{id}/exec on a farm that enforces the fence at the ADB
// socket.
//
// The route was dead and nobody could tell. internal/fenceproxy holds a
// connection carrying no lease fence to an exact-match whitelist of ADB service
// strings; this route's string is "shell,v2,raw:" followed by whatever an
// operator typed, so the proxy refused every exec on every fenced farm. The
// refusal arrived as a 502 Bad Gateway saying the command "did not complete
// against this device", which reads like a wedged handset — so the symptom sent
// operators to look at phones that were fine.
//
// These tests pin the decision and, as much as the message, its WORDING: the
// operator has to learn that the farm will not run this, not that the device is
// broken, and a lease holder must never be able to read it as a fence verdict.

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

func fencedConfig() *config.Config {
	return &config.Config{
		FenceClient: config.FenceClient{TLS: &tls.Config{MinVersion: tls.VersionTLS13}},
	}
}

// TestExecIsNotRefusedOnAFarmWithNoFenceProxy is the half that must not change.
// The whole point of the knob being a knob is that a farm without it behaves
// exactly as it did before the proxy existed.
func TestExecIsNotRefusedOnAFarmWithNoFenceProxy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{
		{"no config at all", nil},
		{"config with no fence client certificate", &config.Config{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Server{cfg: tc.cfg}
			w := httptest.NewRecorder()
			if s.refuseExecBehindTheFence(nil, w, "logcat -d") {
				t.Fatal("exec was refused on a farm that does not run the fence proxy; this route " +
					"is the operator's only way to ask a device a question and it must keep working")
			}
			if w.Code != http.StatusOK || w.Body.Len() != 0 {
				t.Errorf("nothing should have been written: status=%d body=%q", w.Code, w.Body.String())
			}
		})
	}
}

// TestExecOnAFencedFarmIsRefusedWithAMessageAboutThePolicy is the other half.
func TestExecOnAFencedFarmIsRefusedWithAMessageAboutThePolicy(t *testing.T) {
	t.Parallel()

	// A real request and a real metric registry, so the counting and logging the
	// refusal does are executed rather than skipped past on a nil.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{
		cfg:  fencedConfig(),
		log:  log,
		reg:  prometheus.NewRegistry(),
		auth: NewAllowAll(log, "test"),
	}
	if err := s.registerMetrics(true); err != nil {
		t.Fatalf("building the metrics: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/devices/d1/exec", nil)
	r.SetPathValue("id", "d1")

	w := httptest.NewRecorder()
	if !s.refuseExecBehindTheFence(r, w, "rm -rf /sdcard") {
		t.Fatal("exec was admitted on a farm whose hosts run the fence proxy. The proxy refuses " +
			"an arbitrary shell, so admitting it here means the operator gets a 502 that looks " +
			"like broken hardware instead of an answer about the policy.")
	}

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d. A 502 says the gateway failed and a 409 says the device "+
			"is busy; neither is true. The farm is configured not to run this.",
			w.Code, http.StatusNotImplemented)
	}

	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not the one error shape this package emits: %v (%q)", err, w.Body.String())
	}
	if body.Error.Code != CodeExecNotAdmitted {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeExecNotAdmitted)
	}

	// THE ONE CODE IT MAY NEVER BE. CodeFenced is the terminal verdict a lease
	// holder aborts a six-hour job on. A route switched off by a deployment's
	// admission policy sharing that code would destroy work.
	if body.Error.Code == CodeFenced {
		t.Error("the refusal carries CodeFenced, which a holder reads as 'the lease is gone, abort'. " +
			"No lease is involved in this refusal.")
	}

	msg := body.Error.Message
	for _, want := range []string{
		// That the command did not run. An operator who thinks it might have is
		// an operator who will not re-run it somewhere it works.
		"was NOT sent to the device",
		// That no lease moved, in the words this project uses everywhere else.
		"no lease was affected",
		// What to do instead.
		"job step under a lease",
		// Which knob did this, by name, so the farm's operator can find it.
		config.EnvFenceClientCert,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q. It is the only thing the operator will read:\n%s",
				want, msg)
		}
	}

	// Counted. An attempt to run an arbitrary shell on a handset is the most
	// security-relevant thing this route sees, and a refusal nobody can measure
	// leaves nobody able to say how often it is tried — or to notice a client
	// retrying it forever against a farm that will never run it.
	if got := execOutcome(t, s.reg, "not_admitted"); got != 1 {
		t.Errorf("farm_api_device_execs_total{outcome=\"not_admitted\"} = %v, want 1", got)
	}
}

// execOutcome reads one counter out of a registry by gathering it, which is what
// /metrics does. prometheus/testutil would be shorter and pulls in a module this
// project does not otherwise depend on; stream_ticket_test.go says the same.
func execOutcome(t *testing.T, reg *prometheus.Registry, outcome string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "farm_api_device_execs_total" {
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
