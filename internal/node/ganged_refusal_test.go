package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flaviopadilha/device-farmer/internal/recovery"
)

// A ganged refusal is the one answer whose remedy is a purchase order. It has
// to keep its name from uhubctl's output, through the agent's JSON, through the
// client, into the recovery ladder's classification — because at the far end
// the ladder counts it under a metric label operators are told to act on.
// These tests walk that path and the two ways it used to be lost: prose behind
// a generic 409, and a node vocabulary the ladder did not recognise at all.

// TestGangedRefusalIsTypedAcrossTheWire drives the real Agent.Handler with a
// platform that refuses a cycle for a ganged domain, reads the wire, then reads
// the same answer through Client and asks the questions the ladder asks.
//
// Falsify, one at a time: wrap ErrRefused instead of ErrGangedDomain in
// uhubctl.go's refusal (the fake here mirrors it); drop "reason" from
// opHandler's JSON; drop the ReasonGanged arm from statusError.
func TestGangedRefusalIsTypedAcrossTheWire(t *testing.T) {
	prev := platform
	t.Cleanup(func() { platform = prev })
	// The same shape uhubctl.go returns, with the same sentinel.
	platform = fakePlatform{portPowerErr: fmt.Errorf(
		"node: %w: cycling port 2 of hub 3-1.4 shares one power domain with 1 device(s) "+
			"nobody authorised — usb:3-1.4.3 (on hub 3-1.4)", ErrGangedDomain)}

	h, err := testAgent(t, testHost).Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// The wire: a 409 whose body carries the reason as a word.
	payload, _ := json.Marshal(OpRequest{HostID: testHost, Devpath: testPath})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+PathPortPower, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", PathPortPower, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, body)
	}
	var out OpResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the agent's answer is not this API's JSON: %s", body)
	}
	if !out.Refused || out.Reason != ReasonGanged {
		t.Fatalf("OpResponse = %+v, want refused with reason %q; a client must not have to "+
			"find the word \"ganged\" in a sentence", out, ReasonGanged)
	}

	// The client: the same word becomes the sentinel the ladder classifies by.
	err = newClient(t, srv.URL).PortPower(context.Background(), testHost, testPath)
	if err == nil {
		t.Fatal("a ganged refusal came back as a completed rung")
	}
	if !errors.Is(err, ErrGangedDomain) {
		t.Errorf("errors.Is(err, ErrGangedDomain) = false: %v", err)
	}
	if !IsRefused(err) || IsUnreachable(err) {
		t.Errorf("a ganged refusal must be a refusal and not an outage: %v", err)
	}
	if !errors.Is(err, recovery.ErrRungRefused) {
		t.Errorf("the ladder cannot classify this error at all; it does not answer to "+
			"recovery.ErrRungRefused: %v", err)
	}
	if !errors.Is(err, recovery.ErrRungRefusedGanged) {
		t.Errorf("the ladder cannot tell this refusal from any other; it does not answer to "+
			"recovery.ErrRungRefusedGanged: %v", err)
	}
	if !strings.Contains(err.Error(), "shares one power domain") {
		t.Errorf("the agent's own words were dropped on the way: %v", err)
	}
}

// TestClientReadsTheRefusalReasonAsAWordNotAsProse: the classification is
// OpResponse.Reason, honoured on a 409 and nowhere else. An agent older than
// the field answers in prose only and is read as a plain refusal; a 5xx that
// happens to carry the word is still a failed rung, because the status is what
// says whether the port was touched.
//
// Falsify: match "power domain" in the body text instead of comparing Reason.
func TestClientReadsTheRefusalReasonAsAWordNotAsProse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		status  int
		body    string
		refused bool
		ganged  bool
	}{
		{"an older agent, prose only", http.StatusConflict,
			`{"error":"cycling port 4 of hub 3-1 shares one power domain with 2 device(s) nobody authorised","refused":true}`,
			true, false},
		{"reason ganged", http.StatusConflict,
			`{"error":"declined","refused":true,"reason":"ganged"}`, true, true},
		{"reason policy", http.StatusConflict,
			`{"error":"this request names host rack9 and usb:3-1.4.2 is a port on rack1-host-a","refused":true,"reason":"policy"}`,
			true, false},
		{"the word on a 500 is not a refusal", http.StatusInternalServerError,
			`{"error":"the device did not re-enumerate","reason":"ganged"}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := &nodeAPI{hostID: testHost, token: testToken, status: tc.status, body: tc.body}
			err := newClient(t, serve(t, api)).PortPower(context.Background(), testHost, testPath)
			if err == nil {
				t.Fatalf("HTTP %d returned nil", tc.status)
			}
			if got := IsRefused(err); got != tc.refused {
				t.Errorf("IsRefused = %v, want %v: %v", got, tc.refused, err)
			}
			if got := errors.Is(err, ErrGangedDomain); got != tc.ganged {
				t.Errorf("errors.Is(ErrGangedDomain) = %v, want %v: %v", got, tc.ganged, err)
			}
		})
	}
}

// TestNodeVocabularyAnswersToTheLadders is the other half of the seam, and the
// older defect. The recovery ladder classifies a HostRunner's error with
// errors.Is against its own two sentinels and takes anything else as a failed
// rung — the answer it escalates on. This package's words matched neither, so
// every refusal the agent made and every host the client could not reach
// arrived at the ladder as broken hardware.
//
// Falsify: turn ErrRefused back into errors.New("refused by the host agent").
func TestNodeVocabularyAnswersToTheLadders(t *testing.T) {
	t.Parallel()

	yes := []struct {
		name   string
		err    error
		target error
	}{
		{"ErrRefused is a refused rung", ErrRefused, recovery.ErrRungRefused},
		{"ErrNotSupported is a refused rung", ErrNotSupported, recovery.ErrRungRefused},
		{"ErrUnreachable is an unreachable host", ErrUnreachable, recovery.ErrHostUnreachable},
		{"ErrGangedDomain is a refusal", ErrGangedDomain, ErrRefused},
		{"ErrGangedDomain is a refused rung", ErrGangedDomain, recovery.ErrRungRefused},
		{"ErrGangedDomain is the ganged refusal", ErrGangedDomain, recovery.ErrRungRefusedGanged},
		// As the client actually wraps them.
		{"a wrapped unreachable", fmt.Errorf("node: %w: never reached the agent", ErrUnreachable),
			recovery.ErrHostUnreachable},
		{"a wrapped 501", fmt.Errorf("node: %w: %w: cannot on this platform", ErrRefused, ErrNotSupported),
			recovery.ErrRungRefused},
	}
	for _, tc := range yes {
		if !errors.Is(tc.err, tc.target) {
			t.Errorf("%s: errors.Is(%v, %v) = false", tc.name, tc.err, tc.target)
		}
	}

	// The distinctions the contract keeps must survive: a refusal is not an
	// outage, an unsupported platform is not a refusal by policy (501 is not
	// 409), and a plain refusal is not a ganged one.
	no := []struct {
		name   string
		err    error
		target error
	}{
		{"a refusal is not an outage", ErrRefused, recovery.ErrHostUnreachable},
		{"an outage is not a refusal", ErrUnreachable, recovery.ErrRungRefused},
		{"unsupported is not ErrRefused", ErrNotSupported, ErrRefused},
		{"a plain refusal is not ganged", ErrRefused, ErrGangedDomain},
		{"a plain refusal is not the ganged rung refusal", ErrRefused, recovery.ErrRungRefusedGanged},
	}
	for _, tc := range no {
		if errors.Is(tc.err, tc.target) {
			t.Errorf("%s: errors.Is(%v, %v) = true", tc.name, tc.err, tc.target)
		}
	}

	// And the words themselves did not change: operators grep for them.
	for _, tc := range []struct{ err, want string }{
		{ErrRefused.Error(), "refused by the host agent"},
		{ErrNotSupported.Error(), "not supported on this host"},
		{ErrUnreachable.Error(), "the host agent could not be reached"},
	} {
		if tc.err != tc.want {
			t.Errorf("message %q, want %q", tc.err, tc.want)
		}
	}

	// StatusFor still maps the ganged refusal to the 409 the contract promises.
	if got := StatusFor(fmt.Errorf("node: %w: cycling", ErrGangedDomain)); got != http.StatusConflict {
		t.Errorf("StatusFor(ganged) = %d, want 409", got)
	}
	if got := ReasonFor(fmt.Errorf("node: %w: cycling", ErrGangedDomain)); got != ReasonGanged {
		t.Errorf("ReasonFor(ganged) = %q, want %q", got, ReasonGanged)
	}
	if got := ReasonFor(fmt.Errorf("node: %w: other host", ErrRefused)); got != ReasonPolicy {
		t.Errorf("ReasonFor(refused) = %q, want %q", got, ReasonPolicy)
	}
	if got := ReasonFor(fmt.Errorf("node: %w: windows", ErrNotSupported)); got != ReasonUnsupported {
		t.Errorf("ReasonFor(unsupported) = %q, want %q", got, ReasonUnsupported)
	}
	if got := ReasonFor(errors.New("ioctl: input/output error")); got != "" {
		t.Errorf("ReasonFor(a failed rung) = %q, want none", got)
	}
}
