package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
)

// Environment variables read by this package.
//
// They live here rather than in internal/config because authentication is the
// API's own concern and config.Config is shared by every role in the binary;
// when a deployment grows a real identity provider, MustAuthenticator is the
// single call site to change and these two names disappear with it.
const (
	// EnvAuthMode selects the shipped authenticator: "bearer" (default when
	// tokens are present) or "allow-all".
	EnvAuthMode = "FARM_API_AUTH"

	// EnvAuthTokens is a comma-separated list of token specifications, each
	// "<token>:<role>[:<subject>[:<tenant>]]". See NewStaticBearer.
	EnvAuthTokens = "FARM_API_TOKENS"
)

// Role is the coarse authorisation level of a caller. Three levels, ordered.
//
// The distinction that matters is tenant vs operator. A tenant may run its own
// jobs and renew its own leases; an operator may take a device away from
// someone. Every route that can destroy work in progress — revoke, drain, slot
// power, bulk exec, quarantine close — is operator-only, and that is enforced
// by middleware rather than by each handler remembering to check.
type Role string

const (
	// RoleTenant may submit jobs, acquire, renew, release and read the fleet.
	RoleTenant Role = "tenant"
	// RoleOperator may additionally take actions that disturb other people's
	// work. Every such action is written to farm.audit_log with the actor.
	RoleOperator Role = "operator"
	// RoleAdmin is an operator plus whatever future privilege needs a level
	// above one. It satisfies every operator check.
	RoleAdmin Role = "admin"
)

// rank orders the roles. An unknown role ranks zero and therefore satisfies
// nothing, which is the safe direction for a typo in a token specification.
func (r Role) rank() int {
	switch r {
	case RoleTenant:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}

// Valid reports whether r is one of the three defined roles.
func (r Role) Valid() bool { return r.rank() > 0 }

// AtLeast reports whether r satisfies a requirement of min.
func (r Role) AtLeast(min Role) bool { return r.rank() >= min.rank() }

// String makes Role printable in logs and audit rows.
func (r Role) String() string { return string(r) }

// ParseRole converts untrusted text into a Role, rejecting anything else.
func ParseRole(s string) (Role, bool) {
	r := Role(strings.TrimSpace(s))
	if !r.Valid() {
		return "", false
	}
	return r, true
}

// Identity is who the request is from. Subject is what lands in
// farm.audit_log.actor, so it must identify a human or a named service, never
// a shared word like "api".
type Identity struct {
	// Subject names the caller. It is written to farm.audit_log for every
	// operator action, which is the whole point of having it.
	Subject string
	// Role is the caller's authorisation level.
	Role Role
	// Tenant, when non-empty, confines the caller to one tenant's jobs and
	// leases. An operator or admin normally leaves it empty and sees the farm.
	Tenant string
	// Method records how the caller was authenticated ("bearer", "allow-all"),
	// so an audit row shows whether authentication was actually in force.
	Method string
}

// ErrUnauthenticated is returned by an Authenticator that could not identify
// the caller. It is the only error an authenticator should return for an
// ordinary missing or wrong credential; anything else is treated as a server
// fault and logged.
var ErrUnauthenticated = errors.New("api: unauthenticated")

// Authenticator turns a request into an Identity.
//
// THIS IS THE OIDC SEAM. A deployment that wants real identity implements this
// one method — verify the bearer JWT against a JWKS, map a claim onto Role,
// map another onto Tenant — and passes it to WithAuthenticator. Nothing else in
// this package needs to change, and no OIDC implementation is invented here:
// a hand-rolled token verifier is a security hole with good intentions, and the
// verifier belongs to whichever library the deployment already trusts.
//
// Implementations must not block for long: they run on the request path,
// including the renewal path, where a stall costs a device.
type Authenticator interface {
	// Authenticate returns the caller's identity, or ErrUnauthenticated.
	Authenticate(r *http.Request) (Identity, error)
	// Name identifies the scheme in logs and in the startup banner.
	Name() string
}

// ---------------------------------------------------------------------------
// AllowAll
// ---------------------------------------------------------------------------

// AllowAll authenticates every request as one operator identity.
//
// It exists for the demo and for local development, and it is not a default
// anybody should reach by accident: anyone who can reach the port can revoke a
// lease and take a device away from a running job. NewAllowAll logs that at
// WARN on construction and Server logs it again in its startup banner, because
// a security posture nobody was told about is the one that ships to production.
type AllowAll struct {
	subject string
}

// allowAllName is the scheme name AllowAll reports. Two other places
// branch on it — the startup banner and the farm_api_auth_open gauge that
// a page reads — and a literal in each of them is a string that can drift
// away from the method that produces it without any test noticing.
const allowAllName = "allow-all"

// NewAllowAll builds the demo authenticator and says so, loudly, once.
func NewAllowAll(log *slog.Logger, subject string) *AllowAll {
	if log == nil {
		log = slog.Default()
	}
	if subject == "" {
		subject = "anonymous"
	}
	log.Warn("AUTHENTICATION IS DISABLED: every request is granted the operator role",
		"authenticator", "allow-all",
		"subject", subject,
		"consequence", "anyone who can reach this port can revoke leases, drain hosts and power-cycle slots",
		"fix", fmt.Sprintf("set %s to a token list, or supply an Authenticator", EnvAuthTokens))
	return &AllowAll{subject: subject}
}

// Authenticate grants the operator role to everyone.
func (a *AllowAll) Authenticate(*http.Request) (Identity, error) {
	return Identity{Subject: a.subject, Role: RoleOperator, Method: "allow-all"}, nil
}

// Name identifies the scheme.
func (a *AllowAll) Name() string { return allowAllName }

// ---------------------------------------------------------------------------
// StaticBearer
// ---------------------------------------------------------------------------

// StaticBearer authenticates "Authorization: Bearer <token>" against a fixed
// token list, normally supplied by the deployment's secret store through
// EnvAuthTokens.
//
// Tokens are stored as SHA-256 digests and compared in constant time. Keeping
// plaintext in a map and looking it up would leak token bytes through timing,
// and would also print live credentials into any goroutine dump.
type StaticBearer struct {
	// keyed by hex(sha256(token)); the digest is also compared byte-for-byte
	// in constant time so a map probe alone never decides the outcome.
	tokens map[string]staticToken
}

type staticToken struct {
	digest   [sha256.Size]byte
	identity Identity
}

// NewStaticBearer builds an authenticator from token specifications.
//
// Each spec is "<token>:<role>[:<subject>[:<tenant>]]", for example:
//
//	"s3cret:operator:alice"                 alice, operator, whole farm
//	"t0ken:tenant:ci-bot:acme"              ci-bot, tenant, confined to acme
//
// A spec with an unknown role, an empty token, or fewer than two fields is an
// error rather than a skipped line: a token list that silently loses an entry
// produces a 401 storm at 3am with no explanation.
func NewStaticBearer(specs []string) (*StaticBearer, error) {
	sb := &StaticBearer{tokens: make(map[string]staticToken, len(specs))}
	for i, spec := range specs {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		parts := strings.Split(spec, ":")
		if len(parts) < 2 {
			return nil, fmt.Errorf("api: token spec %d is not \"<token>:<role>[:<subject>[:<tenant>]]\"", i+1)
		}
		token := strings.TrimSpace(parts[0])
		if token == "" {
			return nil, fmt.Errorf("api: token spec %d has an empty token", i+1)
		}
		role, ok := ParseRole(parts[1])
		if !ok {
			return nil, fmt.Errorf("api: token spec %d has role %q; want tenant, operator or admin", i+1, parts[1])
		}
		digest := sha256.Sum256([]byte(token))
		key := hex.EncodeToString(digest[:])

		// A spec that named no subject still needs one, because Subject is
		// written to farm.audit_log for every operator action. It is derived
		// from the DIGEST, never from the token itself: the audit log is read
		// by humans, copied into tickets and shipped to log storage, and a
		// prefix of a live bearer token has no business in any of those.
		id := Identity{Subject: "token:" + key[:8], Role: role, Method: "bearer"}
		if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
			id.Subject = strings.TrimSpace(parts[2])
		}
		if len(parts) > 3 {
			id.Tenant = strings.TrimSpace(parts[3])
		}

		if _, dup := sb.tokens[key]; dup {
			return nil, fmt.Errorf("api: token spec %d repeats a token already defined", i+1)
		}
		sb.tokens[key] = staticToken{digest: digest, identity: id}
	}
	if len(sb.tokens) == 0 {
		return nil, errors.New("api: no usable token specifications")
	}
	return sb, nil
}

// Authenticate verifies the bearer token.
func (b *StaticBearer) Authenticate(r *http.Request) (Identity, error) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return Identity{}, ErrUnauthenticated
	}
	scheme, token, ok := strings.Cut(raw, " ")
	if !ok || !strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
		return Identity{}, ErrUnauthenticated
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return Identity{}, ErrUnauthenticated
	}

	digest := sha256.Sum256([]byte(token))
	entry, found := b.tokens[hex.EncodeToString(digest[:])]
	if !found {
		return Identity{}, ErrUnauthenticated
	}
	// The map already matched; this is the comparison that actually decides,
	// and it runs in constant time regardless of how close the tokens were.
	if subtle.ConstantTimeCompare(entry.digest[:], digest[:]) != 1 {
		return Identity{}, ErrUnauthenticated
	}
	return entry.identity, nil
}

// Name identifies the scheme.
func (b *StaticBearer) Name() string { return "bearer" }

// AuthenticatorFromEnv builds the authenticator described by the environment.
//
// With FARM_API_TOKENS set it returns a StaticBearer. With FARM_API_AUTH set to
// "allow-all" — and only then — it returns AllowAll and logs that
// authentication is off. With neither set it returns an error, because a
// control plane that can revoke leases must not fall back to "no auth" merely
// because a variable was forgotten.
func AuthenticatorFromEnv(log *slog.Logger) (Authenticator, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(EnvAuthMode)))
	raw := strings.TrimSpace(os.Getenv(EnvAuthTokens))

	switch {
	case mode == "allow-all":
		return NewAllowAll(log, "demo-operator"), nil
	case raw != "":
		return NewStaticBearer(strings.Split(raw, ","))
	case mode == "bearer":
		return nil, fmt.Errorf("api: %s=bearer but %s is empty", EnvAuthMode, EnvAuthTokens)
	default:
		return nil, fmt.Errorf("api: no authenticator configured: set %s to a token list, "+
			"or %s=allow-all to disable authentication deliberately", EnvAuthTokens, EnvAuthMode)
	}
}

// ---------------------------------------------------------------------------
// Request plumbing
// ---------------------------------------------------------------------------

type identityKey struct{}

// withIdentity stores the caller on the request context.
func withIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// identitySlot carries the authenticated caller back OUT to the access log.
//
// requireRole runs inside the mux and hands the identity to the next handler on
// a derived request, so the request instrument wrapped — the one it logs from —
// never sees that context value. Without this slot every access-log line is
// anonymous, and "who ran that" has to be reconstructed from farm.audit_log,
// which only covers the routes that write to it.
type identitySlot struct {
	mu sync.Mutex
	id Identity
	ok bool
}

func (s *identitySlot) set(id Identity) {
	s.mu.Lock()
	s.id, s.ok = id, true
	s.mu.Unlock()
}

func (s *identitySlot) get() (Identity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id, s.ok
}

type identitySlotKey struct{}

func withIdentitySlot(ctx context.Context, slot *identitySlot) context.Context {
	return context.WithValue(ctx, identitySlotKey{}, slot)
}

func identitySlotFrom(ctx context.Context) (*identitySlot, bool) {
	slot, ok := ctx.Value(identitySlotKey{}).(*identitySlot)
	return slot, ok && slot != nil
}

// IdentityFrom returns the authenticated caller carried by ctx. ok is false on
// a request that never passed through the auth middleware — the health,
// metrics and dashboard routes.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// actor returns the audit actor for this request. Every operator route is
// behind requireRole, so an authenticated identity is always present there;
// the fallback exists so an audit row can never be written with an empty actor
// column, which farm.audit_log forbids anyway.
func actor(ctx context.Context) string {
	if id, ok := IdentityFrom(ctx); ok && id.Subject != "" {
		return id.Subject
	}
	return "unidentified"
}

// requireRole authenticates the request and refuses it unless the caller's
// role is at least min.
//
// 401 and 403 are kept apart deliberately: 401 means "I do not know who you
// are, present a credential", 403 means "I know exactly who you are and you may
// not do this". A client that cannot tell them apart retries a permission
// failure forever.
func (s *Server) requireRole(min Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.auth.Authenticate(r)
		if err != nil {
			if !errors.Is(err, ErrUnauthenticated) {
				s.log.ErrorContext(r.Context(), "authenticator failed",
					"authenticator", s.auth.Name(), "path", r.URL.Path, "err", err)
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="device-farmer"`)
			writeError(w, http.StatusUnauthorized, CodeUnauthenticated,
				"present a valid credential for this API", nil)
			return
		}
		// Published before the role check so a 403 names the caller too: "who
		// tried to revoke that" is worth as much as "who did".
		if slot, ok := identitySlotFrom(r.Context()); ok {
			slot.set(id)
		}
		if !id.Role.AtLeast(min) {
			writeError(w, http.StatusForbidden, CodeForbidden,
				fmt.Sprintf("this route requires the %s role; you have %s", min, id.Role),
				map[string]string{"required_role": string(min), "your_role": string(id.Role)})
			return
		}
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
	})
}
