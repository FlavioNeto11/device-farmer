package config

// The control-class certificate is a SECOND client certificate in one process,
// and these tests exist because that is the unusual part.
//
// The fence proxy reads a connection's class from the client certificate, never
// from the preamble, so a process presents exactly one class per certificate.
// The api needs two: maintenance for POST /devices/{id}/exec, which runs on a
// device that may hold no lease and therefore has no fence to present, and
// control for a live screen, which must present one or be refused as malformed.
// Promoting the one certificate would have broken exec on every free device.
//
// What is worth guarding is not that two work. It is that the SECOND one does
// not damage the first: a farm that configured only the maintenance client must
// not start failing because a field it never set borrowed a file it did.

import (
	"strings"
	"testing"

	"github.com/flaviopadilha/device-farmer/test/testpki"
)

// TestControlCertificateIsOptionalAndDoesNotDisturbTheClient is the regression
// this test file was written for. The control client shares the maintenance
// client's CA, and an early draft borrowed it unconditionally — which gave the
// control instance one of its three files on every farm that configured the
// maintenance client alone, and the all-or-none rule then refused a
// configuration nobody had written.
//
// Falsify: in controlFenceClient, read the CA unconditionally instead of only
// when a control certificate was asked for.
func TestControlCertificateIsOptionalAndDoesNotDisturbTheClient(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, caFile := testpki.New(t).WriteClientPEM(dir, "maintenance", "api")

	env(t, withDSN(map[string]string{
		EnvFenceClientCert: certFile,
		EnvFenceClientKey:  keyFile,
		EnvFenceClientCA:   caFile,
	}))
	cfg, err := Load("api")
	if err != nil {
		t.Fatalf("a farm with the maintenance client alone was refused: %v", err)
	}
	if !cfg.FenceClient.Enabled() {
		t.Error("the maintenance client is off despite all three files being set")
	}
	if cfg.FenceControl.Enabled() {
		t.Error("the control client is on without a control certificate; a screen would dial " +
			"with the wrong class rather than reporting itself unavailable")
	}
}

// TestControlCertificateNamesItsOwnVariables. Two instances of one type share
// every message it prints, and a refusal that named FARM_FENCE_CLIENT_CERT for
// a mistake in FARM_FENCE_CONTROL_CERT sends an operator to edit the file that
// is not the one they got wrong.
//
// Falsify: delete the certVar/keyVar assignment in controlFenceClient, so vars()
// falls back to the maintenance names.
func TestControlCertificateNamesItsOwnVariables(t *testing.T) {
	dir := t.TempDir()
	pki := testpki.New(t)
	certFile, keyFile, caFile := pki.WriteClientPEM(dir, "maintenance", "api")
	ctlCert, _, _ := pki.WriteClientPEM(t.TempDir(), "control", "api")

	// A control certificate with no key: the all-or-none rule fires, and it
	// must fire naming the CONTROL variables.
	env(t, withDSN(map[string]string{
		EnvFenceClientCert:  certFile,
		EnvFenceClientKey:   keyFile,
		EnvFenceClientCA:    caFile,
		EnvFenceControlCert: ctlCert,
	}))
	_, err := Load("api")
	if err == nil {
		t.Fatal("Load accepted a control certificate with no key")
	}
	if !strings.Contains(err.Error(), EnvFenceControlKey) {
		t.Errorf("the refusal does not name %s, which is the variable that is missing:\n%s",
			EnvFenceControlKey, err)
	}
}

// TestControlCertificateNeedsTheSharedCA. The control instance is verified
// against the maintenance client's CA, so asking for one without the other is a
// configuration that cannot dial — and would not say so until the first screen
// was opened, which is the wrong moment to find out.
//
// WHAT ENFORCES IT is FenceClient's own all-or-none rule, reached because
// controlFenceClient leaves CAFile empty when FARM_FENCE_CLIENT_CA is unset:
// two files of three, refused, naming the third. An explicit second check was
// written here first and deleted, because falsifying it proved it never fired —
// the all-or-none rule got there first with a better message. This test is kept
// because the PROPERTY is worth pinning even though the mechanism is inherited.
//
// Falsify: make controlFenceClient default CAFile to something non-empty.
func TestControlCertificateNeedsTheSharedCA(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := testpki.New(t).WriteClientPEM(dir, "control", "api")

	env(t, withDSN(map[string]string{
		EnvFenceControlCert: certFile,
		EnvFenceControlKey:  keyFile,
	}))
	_, err := Load("api")
	if err == nil {
		t.Fatal("Load accepted a control certificate with no CA to verify the proxy against")
	}
	if !strings.Contains(err.Error(), EnvFenceClientCA) {
		t.Errorf("the refusal does not name %s, so an operator is not told which variable "+
			"completes the pair:\n%s", EnvFenceClientCA, err)
	}
}

// TestBothCertificatesLoadTogether is the shape a farm running interactive
// control actually deploys: two certificates, two classes, one process.
func TestBothCertificatesLoadTogether(t *testing.T) {
	dir := t.TempDir()
	pki := testpki.New(t)
	certFile, keyFile, caFile := pki.WriteClientPEM(dir, "maintenance", "api")
	ctlCert, ctlKey, _ := pki.WriteClientPEM(t.TempDir(), "control", "api")

	env(t, withDSN(map[string]string{
		EnvFenceClientCert:  certFile,
		EnvFenceClientKey:   keyFile,
		EnvFenceClientCA:    caFile,
		EnvFenceControlCert: ctlCert,
		EnvFenceControlKey:  ctlKey,
	}))
	cfg, err := Load("api")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.FenceClient.Enabled() || !cfg.FenceControl.Enabled() {
		t.Fatalf("maintenance enabled = %v, control enabled = %v; want both",
			cfg.FenceClient.Enabled(), cfg.FenceControl.Enabled())
	}
	if cfg.FenceClient.TLS == cfg.FenceControl.TLS {
		t.Error("both classes share one TLS configuration; they would present the same " +
			"certificate and therefore the same class")
	}

	// The certificates must actually differ in the field the proxy reads. This
	// is the assertion that catches a copy-paste in the deployment rather than
	// in this package: two files, same SAN, and every screen dials maintenance.
	san := func(f FenceClient) string {
		if f.Leaf == nil || len(f.Leaf.URIs) == 0 {
			return ""
		}
		return f.Leaf.URIs[0].String()
	}
	if a, b := san(cfg.FenceClient), san(cfg.FenceControl); a == b {
		t.Errorf("both certificates carry the SAN %q; the proxy reads the class from there, "+
			"so this process would present one class under two variables", a)
	}
}
