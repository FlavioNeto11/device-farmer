package api

// These tests are about one property: it must be impossible to end up serving
// an open control plane by accident. Everything below is either "the deployment
// said what it wanted and got it" or "the deployment was ambiguous and got a
// refusal instead of a guess".
//
// The second half of the file is about the other direction — what happens to a
// request once authentication IS in force — and in particular about the renewal
// path, where a wrong answer costs a device.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

// clearAuthEnv removes every variable this wiring reads, so a test describes
// its own environment completely and cannot be changed by the shell that
// happened to run `go test`.
func clearAuthEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvAuthTokens, "")
	t.Setenv(EnvAuthMode, "")
	t.Setenv(EnvAllowAnonymous, "")
}

// captureLog returns a logger and the buffer it writes to. The startup line is
// part of the contract here: "which roles did I just grant" is only answerable
// from the log, because the token list is a secret.
func captureLog() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func exposedCfg() *config.Config {
	return &config.Config{APIAddr: "0.0.0.0:8080", Component: "api"}
}

func loopbackCfg() *config.Config {
	return &config.Config{APIAddr: "127.0.0.1:8420", Component: "api"}
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

func TestAuthenticatorForTokensAreUsedAndLoggedWithoutSecrets(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvAuthTokens, "s3cret-op:operator:alice,s3cret-ci:tenant:ci-bot:acme")

	log, out := captureLog()
	// Exposed on purpose: a token list is authentication, so the listener
	// address must not enter into it.
	a, err := AuthenticatorFor(exposedCfg(), log)
	if err != nil {
		t.Fatalf("AuthenticatorFor: %v", err)
	}
	if a.Name() != "bearer" {
		t.Fatalf("authenticator = %q, want bearer", a.Name())
	}

	sb, ok := a.(*StaticBearer)
	if !ok {
		t.Fatalf("authenticator is %T, want *StaticBearer", a)
	}
	grants := sb.Grants()
	if len(grants) != 2 {
		t.Fatalf("grants = %d, want 2: %+v", len(grants), grants)
	}
	// Sorted most privileged first, so the startup line is stable across
	// restarts even though the token map is not.
	if grants[0].Role != RoleOperator || grants[0].Subject != "alice" {
		t.Errorf("grants[0] = %+v, want operator/alice", grants[0])
	}
	if grants[1].Role != RoleTenant || grants[1].Tenant != "acme" {
		t.Errorf("grants[1] = %+v, want tenant confined to acme", grants[1])
	}
	if !sb.Reach(RoleOperator) {
		t.Error("Reach(operator) = false, but a credential holds the operator role")
	}

	logged := out.String()
	for _, want := range []string{"operator=1 tenant=1", "alice", "ci-bot@acme"} {
		if !strings.Contains(logged, want) {
			t.Errorf("startup log does not mention %q:\n%s", want, logged)
		}
	}
	// The whole point of hashing the tokens is undone if the wiring prints them.
	for _, secret := range []string{"s3cret-op", "s3cret-ci"} {
		if strings.Contains(logged, secret) {
			t.Errorf("startup log leaked the token %q:\n%s", secret, logged)
		}
	}
}

func TestAuthenticatorForWarnsWhenNoCredentialCanRevoke(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvAuthTokens, "t0ken:tenant:ci-bot")

	log, out := captureLog()
	a, err := AuthenticatorFor(exposedCfg(), log)
	if err != nil {
		t.Fatalf("AuthenticatorFor: %v", err)
	}
	if sb := a.(*StaticBearer); sb.Reach(RoleOperator) {
		t.Fatal("Reach(operator) = true for a tenant-only token list")
	}
	// A tenant-only token list is legal but leaves every operator route
	// unreachable; that has to be said at rollout, not discovered during an
	// incident.
	if !strings.Contains(out.String(), "no configured credential holds the operator role") {
		t.Errorf("tenant-only token list did not warn:\n%s", out.String())
	}
}

func TestAuthenticatorForOpenOnLoopback(t *testing.T) {
	clearAuthEnv(t)

	log, out := captureLog()
	a, err := AuthenticatorFor(loopbackCfg(), log)
	if err != nil {
		t.Fatalf("an evaluation farm on loopback must still start: %v", err)
	}
	if a.Name() != "allow-all" {
		t.Fatalf("authenticator = %q, want allow-all", a.Name())
	}
	// "Open" has to actually mean open, and the subject has to be the one
	// farm.audit_log will carry for every revoke on this farm: an audit row
	// that names a person nobody authenticated would be worse than one that
	// admits nobody knows.
	id, err := a.Authenticate(httptest.NewRequest("GET", "/api/v1/fleet", nil))
	if err != nil {
		t.Fatalf("allow-all refused a request: %v", err)
	}
	if id.Role != RoleOperator || id.Subject != anonymousSubject || id.Method != "allow-all" {
		t.Errorf("identity = %+v, want operator/%s via allow-all", id, anonymousSubject)
	}

	logged := out.String()
	if !strings.Contains(logged, "AUTHENTICATION IS DISABLED") {
		t.Errorf("open mode was not warned about:\n%s", logged)
	}
	if !strings.Contains(logged, "revoke leases") {
		t.Errorf("open-mode warning does not spell out the consequence:\n%s", logged)
	}
}

func TestAuthenticatorForRefusesOpenOnAnExposedListener(t *testing.T) {
	clearAuthEnv(t)

	log, _ := captureLog()
	a, err := AuthenticatorFor(exposedCfg(), log)
	if err == nil {
		t.Fatalf("a manifest that forgot its tokens came up with %q instead of failing", a.Name())
	}
	if a != nil {
		t.Errorf("authenticator = %v, want nil alongside the error", a)
	}
	msg := err.Error()
	// The operator's next action is either "write a token list" or "say you
	// meant it"; the refusal has to name both variables or it cannot be acted
	// on from the crash log alone.
	for _, want := range []string{EnvAuthTokens, EnvAllowAnonymous, "0.0.0.0:8080"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, msg)
		}
	}
}

// TestAuthenticatorForRefusesOpenWithNoConfigAtAll pins the direction a missing
// config fails in. A caller that has not loaded one yet supplies no evidence
// about exposure, and the refusal must not invent any: claiming the listener is
// "reachable from the network" when the address is unknown is a guess printed
// as a fact, and an operator who checks it and finds it wrong stops believing
// the rest of the message.
func TestAuthenticatorForRefusesOpenWithNoConfigAtAll(t *testing.T) {
	clearAuthEnv(t)

	log, _ := captureLog()
	a, err := AuthenticatorFor(nil, log)
	if err == nil {
		t.Fatalf("a nil config produced %q instead of a refusal", a.Name())
	}
	if strings.Contains(err.Error(), "which is reachable from the network") {
		t.Errorf("refusal asserts an exposure it cannot know about:\n%s", err)
	}
	if !strings.Contains(err.Error(), EnvAllowAnonymous) {
		t.Errorf("refusal does not name %s:\n%s", EnvAllowAnonymous, err)
	}
}

func TestAuthenticatorForAnonymousOptInAllowsOpen(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvAllowAnonymous, "true")

	// This is the path the packaged demo takes. docker-compose.yml binds the
	// demo to 0.0.0.0 inside its container so the published port works, so the
	// demo cannot lean on the loopback allowance and says out loud, in the
	// manifest an operator reads, that this API is open: the demo service
	// carries FARM_API_ALLOW_ANONYMOUS: "true" and a comment saying why.
	//
	// This comment described that manifest for a while before the manifest
	// said it, and nothing caught the drift — no test in this repository reads
	// docker-compose.yml. `docker compose up -d`, the first command in the
	// README, brought up a demo container that exited 1 here on every start
	// and was restarted forever. If this line is ever deleted from the compose
	// file, that is what comes back.
	log, out := captureLog()
	a, err := AuthenticatorFor(exposedCfg(), log)
	if err != nil {
		t.Fatalf("the deliberate opt-in was refused: %v", err)
	}
	if a.Name() != "allow-all" {
		t.Fatalf("authenticator = %q, want allow-all", a.Name())
	}
	if !strings.Contains(out.String(), "AUTHENTICATION IS DISABLED") {
		t.Errorf("the opt-in silenced the warning:\n%s", out.String())
	}
}

// TestAuthenticatorForIgnoresTheComponentName is the regression test for a real
// escape, and it is the reason this package no longer trusts any
// operator-chosen label.
//
// An earlier revision ran open whenever FARM_COMPONENT was "demo", reasoning
// that the demo drives simulated hardware. FARM_COMPONENT is free text an
// operator picks, and internal/config does not defend the name: `farmd all`
// with FARM_COMPONENT=demo passes configuration preflight in full, because
// neither "all" nor "demo" is on the renewal path and the BLOCKER 8 assertion
// never fires. That deployment would have served an open API on 0.0.0.0 over
// real phones with no authentication variable set anywhere. On the api role the
// rename is caught only by the reaper-components assertion — whose error text
// tells the operator to add the name to FARM_REAPER_COMPONENTS, after which
// that farm comes up open too.
//
// A component named "demo", "test", "staging" or anything else is now exactly
// as protected as one named "api".
func TestAuthenticatorForIgnoresTheComponentName(t *testing.T) {
	for _, name := range []string{"demo", "test", "staging", "api"} {
		t.Run(name, func(t *testing.T) {
			clearAuthEnv(t)

			log, _ := captureLog()
			cfg := &config.Config{APIAddr: "0.0.0.0:8080", Component: name}
			a, err := AuthenticatorFor(cfg, log)
			if err == nil {
				t.Fatalf("FARM_COMPONENT=%s bought an open control plane on 0.0.0.0:8080 (%q)",
					name, a.Name())
			}
			if !strings.Contains(err.Error(), EnvAllowAnonymous) {
				t.Errorf("refusal does not name the variable that would have allowed it:\n%s", err)
			}
		})
	}
}

func TestAuthenticatorForMalformedTokensNeverDowngradeToOpen(t *testing.T) {
	// Every case below runs on a LOOPBACK config, where open mode would
	// otherwise be permitted. A token list that fails to parse must fail the
	// boot even there: the deployment asked for authentication.
	cases := map[string]string{
		"no role":        "justatoken",
		"empty token":    ":operator",
		"unknown role":   "t0ken:wizard",
		"only separator": " , ",
		"duplicate":      "same:operator:alice,same:tenant:bob",
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv(EnvAuthTokens, spec)

			log, _ := captureLog()
			a, err := AuthenticatorFor(loopbackCfg(), log)
			if err == nil {
				t.Fatalf("%q produced %q instead of an error", spec, a.Name())
			}
			if a != nil {
				t.Fatalf("%q produced authenticator %q alongside an error", spec, a.Name())
			}
			if !strings.Contains(err.Error(), EnvAuthTokens) {
				t.Errorf("error does not name %s:\n%s", EnvAuthTokens, err)
			}
			if strings.Contains(err.Error(), spec) {
				t.Errorf("error echoed the token list back:\n%s", err)
			}
		})
	}
}

// TestAuthenticatorForNeverPrintsAFieldOfTheTokenList is the leak test.
//
// The likeliest way to get this list wrong is to write the fields in the wrong
// order — "alice:s3cr3t" instead of "s3cr3t:operator:alice" — which puts a live
// bearer token in the role position. NewStaticBearer renders that field with %q
// to say which value it rejected, so the refusal this package builds around it
// would otherwise print the token into the startup log and into every restart
// of the resulting crash loop, for an operator who is about to paste the output
// into a ticket.
func TestAuthenticatorForNeverPrintsAFieldOfTheTokenList(t *testing.T) {
	const secret = "s3cr3t-bearer-token"

	cases := map[string]struct {
		spec   string
		leaked []string
	}{
		"fields in the wrong order": {
			spec:   "alice:" + secret,
			leaked: []string{secret, "alice"},
		},
		"token in the subject position": {
			spec:   "alice:notarole:" + secret,
			leaked: []string{secret, "alice", "notarole"},
		},
		"whitespace around the bad field": {
			spec:   "alice: " + secret + " ",
			leaked: []string{secret, "alice"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv(EnvAuthTokens, c.spec)

			log, out := captureLog()
			a, err := AuthenticatorFor(loopbackCfg(), log)
			if err == nil {
				t.Fatalf("%q produced %q instead of an error", c.spec, a.Name())
			}
			// The error is what the process prints on the way out, so it is
			// the log line as far as the operator is concerned.
			printed := err.Error() + out.String()
			for _, leak := range c.leaked {
				if strings.Contains(printed, leak) {
					t.Errorf("refusal printed the token-list field %q:\n%s", leak, printed)
				}
			}
			// Redacting must not turn the refusal into a shrug: it still has
			// to name the variable and say what the entries look like.
			for _, want := range []string{EnvAuthTokens, "<token>:<role>"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal no longer mentions %q:\n%s", want, err)
				}
			}
		})
	}
}

// TestRedactTokenSpecsWithholdsWhatItCannotClean covers the second pass. A
// message that still carries a field after quoted-form substitution — because
// something printed it bare — must be reported as unclean so the caller drops
// it, rather than shipped because the common case was handled.
func TestRedactTokenSpecsWithholdsWhatItCannotClean(t *testing.T) {
	const raw = "s3cr3t:operator:alice"

	clean, ok := redactTokenSpecs(`token spec 1 has role "s3cr3t"`, raw)
	if !ok {
		t.Fatalf("a quoted field could not be redacted: %q", clean)
	}
	if strings.Contains(clean, "s3cr3t") {
		t.Errorf("redaction left the field in: %q", clean)
	}

	if _, ok := redactTokenSpecs("token spec 1 has role s3cr3t", raw); ok {
		t.Error("a bare field was reported as safe to print")
	}
	// Role names are the one class of field that may be printed: they are a
	// closed set of three words and carry nothing.
	if _, ok := redactTokenSpecs("want tenant, operator or admin", raw); !ok {
		t.Error("a message containing only role names was withheld")
	}
}

// TestAuthenticatorForRefusesContradictions covers every manifest whose two
// halves disagree about whether this API is authenticated.
//
// The third case is the one that matters most and the one a precedence rule
// would get wrong. FARM_API_ALLOW_ANONYMOUS=false is what somebody writes to
// close a farm down; if a FARM_API_AUTH=allow-all inherited from a base
// manifest quietly outranked it, the act of locking the farm down would change
// nothing and say nothing. Both variables must be named, because the operator
// already knows about the one they just wrote.
func TestAuthenticatorForRefusesContradictions(t *testing.T) {
	cases := map[string]struct {
		tokens, mode, anon string
		wantNamed          []string
	}{
		"tokens and anonymous opt-in": {
			tokens: "t:operator:alice", anon: "true",
			wantNamed: []string{EnvAuthTokens, EnvAllowAnonymous},
		},
		"tokens and allow-all mode": {
			tokens: "t:operator:alice", mode: "allow-all",
			wantNamed: []string{EnvAuthTokens, EnvAuthMode},
		},
		"allow-all outranking an explicit refusal to run open": {
			mode: "allow-all", anon: "false",
			wantNamed: []string{EnvAuthMode, EnvAllowAnonymous},
		},
		"bearer and the anonymous opt-in": {
			mode: "bearer", anon: "true",
			wantNamed: []string{EnvAuthMode, EnvAllowAnonymous},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv(EnvAuthTokens, c.tokens)
			t.Setenv(EnvAuthMode, c.mode)
			t.Setenv(EnvAllowAnonymous, c.anon)

			log, _ := captureLog()
			a, err := AuthenticatorFor(loopbackCfg(), log)
			if err == nil {
				t.Fatalf("contradiction resolved silently to %q", a.Name())
			}
			for _, want := range c.wantNamed {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not name %s:\n%s", want, err)
				}
			}
		})
	}
}

// TestAuthenticatorForUnsetIsNotTheSameAsFalse pins the distinction the
// contradiction above rests on. An unset FARM_API_ALLOW_ANONYMOUS is silence,
// and silence must not be read as an objection: a loopback evaluation farm that
// never mentions the variable still starts.
func TestAuthenticatorForUnsetIsNotTheSameAsFalse(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvAuthMode, "allow-all")

	log, _ := captureLog()
	a, err := AuthenticatorFor(loopbackCfg(), log)
	if err != nil {
		t.Fatalf("an unset opt-in was treated as an objection: %v", err)
	}
	if a.Name() != "allow-all" {
		t.Fatalf("authenticator = %q, want allow-all", a.Name())
	}
}

func TestAuthenticatorForRejectsHalfWrittenConfiguration(t *testing.T) {
	cases := map[string]struct{ mode, anon, wantNamed string }{
		"bearer without tokens": {mode: "bearer", wantNamed: EnvAuthTokens},
		"unknown mode":          {mode: "allowall", wantNamed: EnvAuthMode},
		"opt-in is not a bool":  {anon: "yes please", wantNamed: EnvAllowAnonymous},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			clearAuthEnv(t)
			t.Setenv(EnvAuthMode, c.mode)
			t.Setenv(EnvAllowAnonymous, c.anon)

			log, _ := captureLog()
			a, err := AuthenticatorFor(loopbackCfg(), log)
			if err == nil {
				t.Fatalf("accepted a half-written configuration, giving %q", a.Name())
			}
			if !strings.Contains(err.Error(), c.wantNamed) {
				t.Errorf("error does not name %s:\n%s", c.wantNamed, err)
			}
		})
	}
}

// TestAuthenticatorForReportsBothMalformedValues follows internal/config's
// rule: an operator with two typos fixes the manifest in one edit rather than
// learning about the second one from the next crash loop.
func TestAuthenticatorForReportsBothMalformedValues(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv(EnvAuthMode, "allowall")
	t.Setenv(EnvAllowAnonymous, "yes please")

	log, _ := captureLog()
	_, err := AuthenticatorFor(loopbackCfg(), log)
	if err == nil {
		t.Fatal("two malformed values were accepted")
	}
	for _, want := range []string{EnvAuthMode, EnvAllowAnonymous} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("only one problem was reported; %s is missing:\n%s", want, err)
		}
	}
}

func TestLoopbackListener(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8420": true,
		"localhost:8420": true,
		"[::1]:8420":     true,
		"127.9.9.9:80":   true,
		// A bare port and the unspecified address accept from every interface,
		// which is the manifest shape the guard exists to catch.
		":8080":              false,
		"0.0.0.0:8080":       false,
		"[::]:8080":          false,
		"10.0.0.4:8080":      false,
		"farm.internal:8080": false,
		"":                   false,
		"8080":               false,
	}
	for addr, want := range cases {
		if got := loopbackListener(addr); got != want {
			t.Errorf("loopbackListener(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestAuthOptionRefusesRatherThanReturningNil(t *testing.T) {
	clearAuthEnv(t)
	log, _ := captureLog()

	if _, err := AuthOption(exposedCfg(), log); err == nil {
		t.Fatal("AuthOption returned an option for a deployment that must not start")
	}

	t.Setenv(EnvAuthTokens, "t:operator:alice")
	opt, err := AuthOption(exposedCfg(), log)
	if err != nil {
		t.Fatalf("AuthOption: %v", err)
	}
	// The option must actually install the authenticator, or New's "no
	// authenticator" error would describe the wrong problem.
	var s Server
	opt(&s)
	if s.auth == nil || s.auth.Name() != "bearer" {
		t.Fatalf("option did not install the bearer authenticator: %+v", s.auth)
	}
}

// TestSummariesForALargeTokenList checks the two strings an operator reads at
// rollout. The count is exact; the subject list is truncated, and the "+N more"
// has to agree with it or the line quietly misrepresents what was granted.
func TestSummariesForALargeTokenList(t *testing.T) {
	if got := roleSummary(nil); got != "none" {
		t.Errorf("roleSummary(nil) = %q, want none", got)
	}
	if got := subjectSummary(nil); got != "none" {
		t.Errorf("subjectSummary(nil) = %q, want none", got)
	}

	var grants []Grant
	for i := 0; i < maxLoggedSubjects+3; i++ {
		grants = append(grants, Grant{Role: RoleTenant, Subject: fmt.Sprintf("bot-%02d", i)})
	}
	if got, want := roleSummary(grants), fmt.Sprintf("tenant=%d", len(grants)); got != want {
		t.Errorf("roleSummary = %q, want %q", got, want)
	}
	subjects := subjectSummary(grants)
	if !strings.Contains(subjects, "(+3 more)") {
		t.Errorf("subject summary does not account for the truncated entries: %q", subjects)
	}
	if strings.Contains(subjects, "bot-12") {
		t.Errorf("subject summary printed past the cap: %q", subjects)
	}
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// authTestServer is the minimum Server requireRole touches: an authenticator
// and a logger. No database is involved, because authorisation is decided
// before any handler runs.
func authTestServer(a Authenticator) *Server {
	return &Server{auth: a, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func bearerFor(t *testing.T, specs ...string) *StaticBearer {
	t.Helper()
	sb, err := NewStaticBearer(specs)
	if err != nil {
		t.Fatalf("NewStaticBearer(%v): %v", specs, err)
	}
	return sb
}

// call runs one request through requireRole and reports the status, the parsed
// error envelope, and whether the protected handler ran at all.
func call(s *Server, min Role, method, path, authz string) (*httptest.ResponseRecorder, APIError, bool) {
	var reached bool
	var seen Identity
	h := s.requireRole(min, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		seen, _ = IdentityFrom(r.Context())
		writeJSON(w, http.StatusOK, map[string]string{"subject": seen.Subject, "method": seen.Method})
	}))

	req := httptest.NewRequest(method, path, nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body errorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body.Error, reached
}

func TestRequireRoleRejectsMissingOrWrongCredentials(t *testing.T) {
	s := authTestServer(bearerFor(t, "s3cret:operator:alice"))

	cases := map[string]string{
		"no Authorization header": "",
		"wrong token":             "Bearer wrong",
		"wrong scheme":            "Basic YWxpY2U6czNjcmV0",
		"scheme with no token":    "Bearer ",
		"bare token":              "s3cret",
	}
	for name, authz := range cases {
		t.Run(name, func(t *testing.T) {
			rec, apiErr, reached := call(s, RoleTenant, "GET", "/api/v1/fleet", authz)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if reached {
				t.Fatal("the protected handler ran for an unauthenticated request")
			}
			if apiErr.Code != CodeUnauthenticated {
				t.Errorf("code = %q, want %q", apiErr.Code, CodeUnauthenticated)
			}
			// Without this header a client cannot tell it is meant to present a
			// bearer token rather than retry.
			if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
			}
		})
	}
}

func TestRequireRoleTenantCannotReachAnOperatorRoute(t *testing.T) {
	s := authTestServer(bearerFor(t, "op:operator:alice", "ci:tenant:ci-bot:acme"))

	// The route that takes a device away from a running job.
	rec, apiErr, reached := call(s, RoleOperator, "POST", "/api/v1/leases/L1/revoke", "Bearer ci")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if reached {
		t.Fatal("a tenant credential reached an operator-only handler")
	}
	if apiErr.Code != CodeForbidden {
		t.Errorf("code = %q, want %q", apiErr.Code, CodeForbidden)
	}
	// 403 and 401 are kept apart so a client does not retry a permission
	// failure forever; the detail says which role was missing.
	detail, _ := apiErr.Detail.(map[string]any)
	if detail["required_role"] != "operator" || detail["your_role"] != "tenant" {
		t.Errorf("detail = %v, want required_role=operator your_role=tenant", apiErr.Detail)
	}

	// The same route, with the credential that does hold the role.
	rec, _, reached = call(s, RoleOperator, "POST", "/api/v1/leases/L1/revoke", "Bearer op")
	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("operator credential got status %d (handler reached: %v)", rec.Code, reached)
	}
}

func TestRequireRoleCarriesTheIdentityToTheHandler(t *testing.T) {
	s := authTestServer(bearerFor(t, "ci:tenant:ci-bot:acme"))

	rec, _, reached := call(s, RoleTenant, "GET", "/api/v1/fleet", "Bearer ci")
	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("status = %d, handler reached: %v", rec.Code, reached)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// Subject is what lands in farm.audit_log, and Method records that
	// authentication was actually in force for this request.
	if got["subject"] != "ci-bot" || got["method"] != "bearer" {
		t.Errorf("identity = %v, want subject ci-bot authenticated by bearer", got)
	}
}

func TestRequireRolePublishesIdentityToTheAccessLog(t *testing.T) {
	s := authTestServer(bearerFor(t, "op:operator:alice"))

	// instrument reads the caller back out of this slot; requireRole runs on a
	// derived request whose context instrument cannot see, so a regression here
	// makes every access-log line anonymous.
	slot := &identitySlot{}
	req := httptest.NewRequest("GET", "/api/v1/fleet", nil).
		WithContext(withIdentitySlot(httptest.NewRequest("GET", "/", nil).Context(), slot))
	req.Header.Set("Authorization", "Bearer op")

	h := s.requireRole(RoleTenant, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	id, ok := slot.get()
	if !ok || id.Subject != "alice" {
		t.Fatalf("access-log identity = %+v (set: %v), want alice", id, ok)
	}
}

// ---------------------------------------------------------------------------
// The renewal path
// ---------------------------------------------------------------------------

// TestRequireRoleOnRenewIsNeverAFencingSignal is the invariant test for this
// file.
//
// POST /api/v1/leases/{id}/renew sits behind requireRole, so an authentication
// problem — a rotated secret, a token list that lost an entry, a client sending
// the wrong header — is answered here rather than by the handler. It must be
// answered as 401 "unauthenticated", which says nothing whatsoever about any
// lease, and never as 410 "fenced", which is terminal and tells the holder to
// abort the job and close every socket. Conflating the two turns a credential
// mistake into DeviceFarmer/STF #663: hours of work destroyed by something that
// was not the lease at all.
//
// The other half is that the handler must not run. requireRole refusing a
// renewal must leave farm.lease_renew uncalled, so the deadline does not move
// in either direction and the lease is exactly as it was.
func TestRequireRoleOnRenewIsNeverAFencingSignal(t *testing.T) {
	s := authTestServer(bearerFor(t, "ci:tenant:ci-bot:acme"))

	for name, authz := range map[string]string{
		"no credential":  "",
		"rotated secret": "Bearer the-old-token",
	} {
		t.Run(name, func(t *testing.T) {
			rec, apiErr, reached := call(s, RoleTenant,
				"POST", "/api/v1/leases/11111111-1111-1111-1111-111111111111/renew", authz)

			if reached {
				t.Fatal("the renew handler ran for a request that failed authentication")
			}
			if rec.Code == http.StatusGone {
				t.Fatalf("an authentication failure answered 410 Gone: the holder is being "+
					"told its lease is over because its credential is wrong (body: %s)",
					rec.Body.String())
			}
			if apiErr.Code == CodeFenced {
				t.Fatalf("an authentication failure answered %q: that is terminal and aborts "+
					"the job", CodeFenced)
			}
			if rec.Code != http.StatusUnauthorized || apiErr.Code != CodeUnauthenticated {
				t.Errorf("status/code = %d/%q, want 401/%s", rec.Code, apiErr.Code, CodeUnauthenticated)
			}
		})
	}
}

// TestRequireRoleTenantOnAnExpiredCredentialIsNotForbiddenEither guards the
// third confusion. A tenant renewing its OWN lease must not be answered 403:
// renew is a tenant route, and a 403 there would also read as terminal to a
// client that treats permission failures as unrecoverable.
func TestRequireRoleTenantCanRenew(t *testing.T) {
	s := authTestServer(bearerFor(t, "ci:tenant:ci-bot:acme"))

	rec, _, reached := call(s, RoleTenant,
		"POST", "/api/v1/leases/11111111-1111-1111-1111-111111111111/renew", "Bearer ci")
	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("a tenant could not reach renew: status %d, handler reached %v", rec.Code, reached)
	}
}

// faultyAuthenticator stands in for the OIDC seam auth.go documents: an
// authenticator whose backing store — a JWKS endpoint, a directory — is
// unreachable. That is a transport failure, and it returns something other
// than ErrUnauthenticated to say so.
type faultyAuthenticator struct{ err error }

func (f faultyAuthenticator) Authenticate(*http.Request) (Identity, error) {
	return Identity{}, f.err
}
func (f faultyAuthenticator) Name() string { return "faulty" }

// TestRequireRoleAuthenticatorFaultIsLoudAndNotTerminal covers a renewal that
// arrives while the authenticator's own dependency is down.
//
// Three things must hold, and they are the three that keep a transport failure
// from becoming a fencing event:
//
//   - the handler does not run, so nothing touches the lease;
//   - the response is not 410 and not "fenced", so the holder does not abort;
//   - the fault is logged at ERROR with the authenticator named, because a
//     control plane that answers every renewal 401 while its identity provider
//     is down must not do so silently.
//
// Note what this test does NOT assert: that 401 is the right status for a
// server-side fault. It is arguably wrong — 503 with CodeTransient is what the
// rest of this package returns for "we could not decide, retry" — but that call
// belongs to requireRole in auth.go, not here.
func TestRequireRoleAuthenticatorFaultIsLoudAndNotTerminal(t *testing.T) {
	buf := &bytes.Buffer{}
	s := &Server{
		auth: faultyAuthenticator{err: errors.New("jwks: dial tcp 10.0.0.9:443: i/o timeout")},
		log:  slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	rec, apiErr, reached := call(s, RoleTenant,
		"POST", "/api/v1/leases/11111111-1111-1111-1111-111111111111/renew", "Bearer whatever")

	if reached {
		t.Fatal("the renew handler ran after the authenticator failed")
	}
	if rec.Code == http.StatusGone || apiErr.Code == CodeFenced {
		t.Fatalf("an authenticator outage was reported as a fencing event: %d/%q",
			rec.Code, apiErr.Code)
	}
	logged := buf.String()
	if !strings.Contains(logged, "authenticator failed") || !strings.Contains(logged, "faulty") {
		t.Errorf("the authenticator's own failure was not logged:\n%s", logged)
	}
}
