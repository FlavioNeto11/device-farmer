package api

// Choosing the authenticator is a decision with exactly one dangerous outcome,
// and this file exists to make that outcome impossible to reach by accident.
//
// The dangerous outcome is an open control plane. Every route in router.go that
// can take a device away from a running job — revoke, drain, slot power, bulk
// exec, quarantine close — is gated by requireRole, and AllowAll answers that
// gate with the operator role for every caller, including one that presented no
// credential at all. A deployment that runs open is therefore a deployment where
// anyone who can reach the port can destroy somebody's six-hour run.
//
// So the rule here is: silence is never consent. A token list that parses is
// authentication. A token list that does not parse is a startup failure, not a
// downgrade to open. No token list at all is open in exactly two situations,
// and no others:
//
//   - the listener is a loopback address, which is not a policy but a fact
//     about the socket, or
//   - a human wrote FARM_API_ALLOW_ANONYMOUS=true (or FARM_API_AUTH=allow-all)
//     down on purpose.
//
// Everything else refuses to start and says which variable would have allowed
// it.
//
// Refusing to start is safe here, which is not obvious and is the reason it is
// the chosen failure. An api process that never comes up stops writing
// farm.component_beat, and farm.reaper_arm takes min(beat_at) across the
// components in FARM_REAPER_COMPONENTS: the stale api row becomes a
// control_plane_gap and every held and suspect lease has that gap added to its
// expires_at and reclaimable_at. A control plane that refuses to boot therefore
// costs its tenants no lease budget at all, while one that boots open costs
// them everything the first stranger who finds the port decides to revoke.
//
// Note what is NOT on that list: any operator-chosen label. An earlier revision
// of this file also ran open when FARM_COMPONENT was "demo", on the theory that
// the demo drives simulated hardware and has nothing to lose. FARM_COMPONENT is
// a free-text name an operator picks for a process, and the escape was real:
// `farmd all` with FARM_COMPONENT=demo passes config.Config.Validate in full —
// neither "all" nor "demo" is on the renewal path, so the BLOCKER 8 assertion
// never fires — and would have served an open API on 0.0.0.0 over real phones
// with no authentication variable set anywhere. On the api role the same rename
// is caught only by the reaper-components assertion, whose error text tells the
// operator to add the name to FARM_REAPER_COMPONENTS, after which that farm
// comes up open too. A control plane guarding six-hour runs cannot rest its
// authentication decision on what somebody called the process, so the two
// inputs that decide it here are a socket and a stated intent.

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

// EnvAllowAnonymous is the deliberate opt-in to serving this API with no
// authentication whatsoever.
//
// It exists because the two deployments that arrive here without a token list
// are otherwise indistinguishable from inside the process: an evaluation farm on
// somebody's laptop, and a production manifest whose FARM_API_TOKENS line was
// lost in a merge. Both look exactly like "unset". Requiring a second variable
// splits them, because nobody writes FARM_API_ALLOW_ANONYMOUS=true by accident.
//
// It is read here rather than in internal/config for the reason auth.go already
// gives for its own two names: authentication is this package's concern, and
// config.Config is shared by every role in the binary, most of which have no
// HTTP surface to protect.
const EnvAllowAnonymous = "FARM_API_ALLOW_ANONYMOUS"

// anonymousSubject is the actor written to farm.audit_log while authentication
// is off. It is deliberately neither a person nor a service name: a revoke row
// that reads "anonymous" is honest that nobody knows who ended that lease.
//
// It is also the exact subject cmd/farmd passes today, so turning this wiring on
// does not silently rewrite the audit history of an evaluation farm.
const anonymousSubject = "anonymous"

// ---------------------------------------------------------------------------
// The bearer half of the role matrix
// ---------------------------------------------------------------------------

// Grant is one configured credential's authority with the credential removed.
//
// This is the shape that may be logged. A Grant carries a role, an audit
// subject and a tenant, all three of which already appear in farm.audit_log; it
// never carries the token, and there is deliberately no field it could be put
// in.
type Grant struct {
	Role    Role
	Subject string
	Tenant  string
}

// Grants reports what the configured tokens may do, sorted most privileged
// first so a startup line reads the same way on every restart.
//
// It is the answer to the question an operator actually asks after a rollout —
// "what did I just grant?" — which cannot be answered by reading the token list,
// because the token list is a secret and is usually not in front of them.
func (b *StaticBearer) Grants() []Grant {
	out := make([]Grant, 0, len(b.tokens))
	for _, t := range b.tokens {
		out = append(out, Grant{
			Role:    t.identity.Role,
			Subject: t.identity.Subject,
			Tenant:  t.identity.Tenant,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].Role.rank(), out[j].Role.rank(); a != b {
			return a > b
		}
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return out[i].Tenant < out[j].Tenant
	})
	return out
}

// Reach reports whether any configured credential satisfies min.
//
// Role.AtLeast answers that question for one caller at request time; this
// answers it for the whole token list at startup. The difference matters: a
// token file with nothing but tenant credentials passes every check in
// NewStaticBearer and still leaves every operator route in router.go
// permanently unreachable, which is discovered at 3am by the person trying to
// revoke a lease from a wedged job.
func (b *StaticBearer) Reach(min Role) bool {
	for _, t := range b.tokens {
		if t.identity.Role.AtLeast(min) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

// AuthenticatorFor picks this deployment's authenticator from its configuration
// and refuses anything ambiguous.
//
// The outcomes, in the order they are decided:
//
//   - FARM_API_TOKENS lists usable credentials -> StaticBearer, and the roles it
//     granted are logged (never the tokens).
//   - FARM_API_TOKENS is set but yields no usable credential -> an error. A
//     deployment that tried to configure authentication and got it wrong must
//     not come up open; that is a silent downgrade from "protected" to "anyone
//     may revoke", and it happens exactly when somebody is already having a bad
//     day with a secret store.
//   - Nothing configured, and the listener is loopback or open was asked for by
//     name -> AllowAll, warned at WARN by NewAllowAll with the consequence
//     spelled out. An evaluation farm keeps working.
//   - Nothing configured anywhere else -> an error naming EnvAllowAnonymous,
//     because the manifest that forgot its tokens must fail to start rather than
//     serve an open control plane and look healthy doing it.
//
// Contradictions are errors rather than precedence rules. A manifest that both
// lists tokens and asks for anonymous access — or that says allow-all in one
// place and "no anonymous access" in another — has two authors who disagree,
// and picking a winner here means one of them is wrong without ever being told.
// Half the time the half that loses is the one asking for authentication.
func AuthenticatorFor(cfg *config.Config, log *slog.Logger) (Authenticator, error) {
	if log == nil {
		log = slog.Default()
	}

	// Both malformed values are reported together, for the reason
	// internal/config gives for its own report-everything rule: an operator
	// with two typos should fix a manifest in one edit, not learn about the
	// second one from the next crash loop.
	mode, modeErr := authMode()
	anon, anonErr := anonymousRequested()
	if err := errors.Join(modeErr, anonErr); err != nil {
		return nil, err
	}

	raw := strings.TrimSpace(os.Getenv(EnvAuthTokens))

	// Every case below is a manifest whose two halves disagree about whether
	// this API is authenticated. None of them is resolved by precedence: the
	// losing half would be silently ignored, and half the time the half that
	// loses is the one asking for authentication.
	//
	// The third case is the one worth naming. FARM_API_ALLOW_ANONYMOUS=false is
	// what an operator writes to close a farm down, and it is worth nothing if
	// a FARM_API_AUTH=allow-all left over in a base manifest quietly outranks
	// it. Explicitly false and unset are therefore different answers here.
	switch {
	case raw != "" && anon.allow:
		return nil, contradictoryOpenness(
			EnvAuthTokens, "lists credentials",
			EnvAllowAnonymous, "is true, which asks for an open API")
	case raw != "" && mode == "allow-all":
		return nil, contradictoryOpenness(
			EnvAuthTokens, "lists credentials",
			EnvAuthMode, "is allow-all, which disables authentication")
	case mode == "allow-all" && anon.set && !anon.allow:
		return nil, contradictoryOpenness(
			EnvAllowAnonymous, "is false, which demands credentials",
			EnvAuthMode, "is allow-all, which disables authentication")
	case mode == "bearer" && anon.allow:
		return nil, contradictoryOpenness(
			EnvAuthMode, "is bearer, which demands credentials",
			EnvAllowAnonymous, "is true, which asks for an open API")
	}

	if raw != "" {
		sb, err := NewStaticBearer(strings.Split(raw, ","))
		if err != nil {
			return nil, tokenParseRefusal(raw, err)
		}
		grants := sb.Grants()
		log.Info("API authentication is enabled",
			"authenticator", sb.Name(),
			"credentials", len(grants),
			"roles", roleSummary(grants),
			"subjects", subjectSummary(grants))
		if !sb.Reach(RoleOperator) {
			log.Warn("no configured credential holds the operator role",
				"consequence", "revoke, drain, slot power, bulk exec and quarantine close are "+
					"unreachable with this token list; a wedged job's device cannot be taken back by hand",
				"fix", fmt.Sprintf("add an entry to %s with role operator or admin", EnvAuthTokens))
		}
		return sb, nil
	}

	// FARM_API_AUTH=bearer with no tokens is a half-written configuration, and
	// the missing half is the credentials. Falling back to open here would give
	// the deployment the exact opposite of what it asked for.
	if mode == "bearer" {
		return nil, fmt.Errorf("%s=bearer but %s is empty; there is nothing to authenticate against. "+
			"Set %s to a comma-separated list of \"<token>:<role>[:<subject>[:<tenant>]]\", "+
			"or clear %s if this deployment is meant to run open",
			EnvAuthMode, EnvAuthTokens, EnvAuthTokens, EnvAuthMode)
	}

	why, ok := openModeAllowed(cfg, mode, anon)
	if !ok {
		return nil, openRefusal(cfg)
	}
	// NewAllowAll carries the WARN and the consequence text; it is not repeated
	// here, so there is one wording of "authentication is disabled" in this
	// package and it cannot drift from the one the startup banner prints.
	log.Info("running without authentication", "permitted_by", why)
	return NewAllowAll(log, anonymousSubject), nil
}

// AuthOption resolves the authenticator and wraps it for New.
//
// It exists so the parent's option list stays a list of options, and because
// WithAuthenticator ignores a nil authenticator: a caller that forgot to check
// the error would otherwise reach New with s.auth still nil and get "no
// authenticator configured", which is a true statement about the wrong problem.
// AuthOption never returns an Option carrying nil.
func AuthOption(cfg *config.Config, log *slog.Logger) (Option, error) {
	a, err := AuthenticatorFor(cfg, log)
	if err != nil {
		return nil, err
	}
	return WithAuthenticator(a), nil
}

// ---------------------------------------------------------------------------
// Deciding whether open is allowed
// ---------------------------------------------------------------------------

// authMode reads EnvAuthMode strictly.
//
// A typo must not fall through to a default, because one of the two defaults
// available is "no authentication".
func authMode() (string, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(EnvAuthMode)))
	switch mode {
	case "", "bearer", "allow-all":
		return mode, nil
	}
	return "", fmt.Errorf("%s=%q is not a mode; set it to \"bearer\" (with %s), "+
		"to \"allow-all\" to serve this API open on purpose, or unset it",
		EnvAuthMode, mode, EnvAuthTokens)
}

// anonymousSetting is FARM_API_ALLOW_ANONYMOUS as three states rather than two.
//
// Unset and explicitly false both mean "do not run open", but only one of them
// is a statement. An operator who writes false is saying this farm must have
// credentials, and that statement has to be able to contradict a
// FARM_API_AUTH=allow-all inherited from somewhere else — otherwise the act of
// locking a farm down accomplishes nothing and reports nothing.
type anonymousSetting struct {
	set   bool
	allow bool
}

// anonymousRequested reads the opt-in strictly.
//
// A value that is not a boolean is an error rather than a false: somebody who
// wrote FARM_API_ALLOW_ANONYMOUS=yes meant yes, and answering that with a
// refusal that does not mention the variable they just set sends them looking
// in the wrong file.
func anonymousRequested() (anonymousSetting, error) {
	raw := strings.TrimSpace(os.Getenv(EnvAllowAnonymous))
	if raw == "" {
		return anonymousSetting{}, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return anonymousSetting{}, fmt.Errorf("%s=%q is not a boolean; set it to true to serve "+
			"this API open on purpose, or to false (or unset) to require credentials",
			EnvAllowAnonymous, raw)
	}
	return anonymousSetting{set: true, allow: v}, nil
}

// contradictoryOpenness refuses a manifest whose two halves disagree about
// whether this API is authenticated, and names both so the operator can see
// which one they did not know was there.
func contradictoryOpenness(authName, authMeaning, openName, openMeaning string) error {
	return fmt.Errorf("%s %s while %s %s. These cannot both be honoured, and guessing "+
		"would silently ignore whichever one somebody meant — in a control plane where "+
		"the wrong guess lets any caller revoke a lease and end somebody's job. "+
		"Remove %s to serve this API open on purpose, or remove %s to require credentials",
		authName, authMeaning, openName, openMeaning, authName, openName)
}

// openModeAllowed reports whether this deployment may serve the API with no
// authentication, and why.
//
// Only two things can say yes, and neither can be produced by a manifest that
// merely forgot its tokens:
//
//   - A stated intent. FARM_API_ALLOW_ANONYMOUS=true and FARM_API_AUTH=allow-all
//     have no meaning other than "serve this open", so nobody sets one by
//     accident and nobody sets one thinking it does something else.
//   - A loopback listener. That is not a policy, it is a fact about the socket:
//     no other machine can open it, so the blast radius of "everyone is an
//     operator" is the person sitting at the keyboard. A production manifest
//     configures the opposite — an exposed bind — which is why forgetting the
//     tokens there is refused rather than tolerated.
//
// Nothing about the component name, the role, or any other operator-chosen
// label participates. See the note at the top of this file for the escape that
// cost.
func openModeAllowed(cfg *config.Config, mode string, anon anonymousSetting) (string, bool) {
	switch {
	case anon.allow:
		return EnvAllowAnonymous + "=true", true
	case mode == "allow-all":
		return EnvAuthMode + "=allow-all", true
	}
	if cfg == nil {
		// No configuration means no evidence about exposure, and the safe
		// direction for missing evidence is to demand the opt-in.
		return "", false
	}
	if loopbackListener(cfg.APIAddr) {
		return "loopback listener " + cfg.APIAddr, true
	}
	return "", false
}

// openRefusal is the error a deployment gets when it configured no
// authentication and is not somewhere open is safe. It names the variable that
// would have allowed it deliberately, because the operator's next action is
// either to write that variable or to write a token list, and the message has
// to be enough to choose.
func openRefusal(cfg *config.Config) error {
	// The unknown-address wording is not decoration: AuthenticatorFor is
	// reachable with a nil config from a parent that has not loaded one yet,
	// and telling that operator the API "would listen on an unknown address,
	// which is reachable from the network" would be a guess stated as a fact.
	exposure := "The API would listen on an address this process could not " +
		"determine, so there is no evidence it is unreachable from the network, and"
	if cfg != nil && strings.TrimSpace(cfg.APIAddr) != "" {
		exposure = fmt.Sprintf("The API would listen on %s, which is reachable from the network, and",
			cfg.APIAddr)
	}
	return fmt.Errorf(`%s is empty and this deployment is not one where running open is safe.

%s with no
authenticator every caller is granted the operator role: anyone who can reach
that address could revoke leases, drain hosts and power-cycle slots, ending work
that is hours old.

Fix by setting %s to a comma-separated list of
"<token>:<role>[:<subject>[:<tenant>]]", role one of tenant, operator, admin.

To serve an OPEN control plane on purpose — an evaluation farm, the packaged
demo, a laptop — set %s=true and this refusal becomes a warning. Binding the
listener to loopback does the same, because then no other machine can reach it.`,
		EnvAuthTokens, exposure, EnvAuthTokens, EnvAllowAnonymous)
}

// loopbackListener reports whether addr binds only to this machine.
//
// A bare port (":8080") and the unspecified address ("0.0.0.0:8080") are NOT
// loopback: they accept from every interface, which is exactly the manifest
// shape this guard exists to catch. A hostname other than "localhost" is
// treated as exposed rather than resolved, because a DNS lookup on the startup
// path can hang, and "it might resolve to loopback" is not evidence worth
// blocking a boot for.
func loopbackListener(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Not host:port at all — a Unix path or a typo. Either way there is no
		// evidence of a loopback bind.
		return false
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// Reporting a broken token list without printing it
// ---------------------------------------------------------------------------

// redactedField is what a withheld piece of FARM_API_TOKENS renders as.
const redactedField = `"[redacted]"`

// tokenParseRefusal builds the error for a FARM_API_TOKENS list that could not
// be parsed.
//
// The reason from NewStaticBearer is quoted rather than wrapped with %w, and it
// is passed through redactTokenSpecs first. Both of those are load bearing.
// NewStaticBearer names the offending entry by position, which is the only way
// to fix a list nobody is allowed to print — but it also renders the role field
// with %q, and the most common way to get this list wrong is to write the
// fields in the wrong order:
//
//	FARM_API_TOKENS=alice:s3cr3t          # subject:token, not token:role
//
// which puts a live bearer token in the role position. Wrapping that error
// verbatim would print the token into the startup log, the crash loop, and
// whatever ships those to log storage — for a deployment that is already
// failing to boot and whose operator is about to paste the output into a
// ticket.
func tokenParseRefusal(raw string, err error) error {
	reason, safe := redactTokenSpecs(err.Error(), raw)
	if !safe {
		// A field survived redaction, so the reason cannot be shown at all.
		// Losing the entry number is a bad morning; printing a live credential
		// is a rotation.
		reason = fmt.Sprintf("the reason is withheld because it could not be rendered "+
			"without echoing part of %s", EnvAuthTokens)
	}
	// The boilerplate below deliberately contains no example values, only
	// placeholders. An illustrative "<subject>:<token>" spelled out with real
	// looking words would be indistinguishable, to a reader and to a test, from
	// a field that leaked out of the operator's own list.
	return fmt.Errorf("%s is set but yields no usable credential: %s\n\n"+
		"Each entry is \"<token>:<role>[:<subject>[:<tenant>]]\", comma separated, "+
		"with role one of tenant, operator, admin. Check the field ORDER before "+
		"suspecting the token: the secret comes first and the role second, so a list "+
		"written the other way round is read as a token whose role is unknown. "+
		"The API is NOT started open because of this: a deployment that tried to "+
		"configure authentication and failed is not a deployment that wants no "+
		"authentication",
		EnvAuthTokens, reason)
}

// redactTokenSpecs removes every operator-supplied field of raw from msg, and
// reports whether it succeeded.
//
// The rule it enforces is absolute and easy to state: no field of
// FARM_API_TOKENS reaches a log through this path unless that field is one of
// the three role names, which are a closed set and carry no secret. Fields are
// matched in their %q form because that is how NewStaticBearer renders the one
// field it echoes; the second pass then checks for any bare survivor, so a
// future message that interpolates a field with %s is caught here rather than
// discovered in a log.
//
// A field short enough to occur inside the boilerplate ("a", "is") makes this
// report false and costs the caller its detail. That is the right direction to
// fail: the operator loses an entry number, not a credential.
func redactTokenSpecs(msg, raw string) (string, bool) {
	fields := make([]string, 0, 8)
	for _, spec := range strings.Split(raw, ",") {
		for _, field := range strings.Split(spec, ":") {
			trimmed := strings.TrimSpace(field)
			if trimmed == "" {
				continue
			}
			if _, isRole := ParseRole(trimmed); isRole {
				continue
			}
			fields = append(fields, field)
			if field != trimmed {
				fields = append(fields, trimmed)
			}
		}
	}
	for _, f := range fields {
		msg = strings.ReplaceAll(msg, strconv.Quote(f), redactedField)
	}
	for _, f := range fields {
		if strings.Contains(msg, f) {
			return "", false
		}
	}
	return msg, true
}

// ---------------------------------------------------------------------------
// Log rendering
// ---------------------------------------------------------------------------

// roleSummary renders the granted roles as "admin=1 operator=2 tenant=5".
//
// Counts rather than a list of subjects per role: the number is what an
// operator compares against the manifest they just rolled out, and it fits on
// one line of a startup log.
func roleSummary(grants []Grant) string {
	counts := map[Role]int{}
	for _, g := range grants {
		counts[g.Role]++
	}
	var b strings.Builder
	for _, r := range []Role{RoleAdmin, RoleOperator, RoleTenant} {
		n, ok := counts[r]
		if !ok {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%d", r, n)
	}
	if b.Len() == 0 {
		return "none"
	}
	return b.String()
}

// maxLoggedSubjects bounds the startup line for a farm with a large token file.
// The count in roleSummary is already exact; this list is orientation, not an
// inventory.
const maxLoggedSubjects = 12

// subjectSummary renders the audit subjects the tokens will write to
// farm.audit_log, with the tenant where one confines the credential.
//
// These are safe to print: every one of them is already written to the audit
// log on each operator action, and a subject that was not named in the spec is
// derived by auth.go from the token's DIGEST rather than from the token.
func subjectSummary(grants []Grant) string {
	var b strings.Builder
	for i, g := range grants {
		if i == maxLoggedSubjects {
			fmt.Fprintf(&b, " (+%d more)", len(grants)-maxLoggedSubjects)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(g.Subject)
		if g.Tenant != "" {
			b.WriteString("@" + g.Tenant)
		}
	}
	if b.Len() == 0 {
		return "none"
	}
	return b.String()
}
