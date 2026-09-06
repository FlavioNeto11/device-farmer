package api

// Stream tickets: how a browser opens an event stream it cannot put a header on.
//
// GET /api/v1/stream is tenant-gated like every other read, and the dashboard
// reads it with EventSource, which has no way to send one. The constructor
// takes a URL and a withCredentials flag, and that is the entire API. So on
// every farm that sets FARM_API_TOKENS — which is every farm that is not
// somebody's laptop — the browser's stream request arrived with no
// Authorization header, earned its 401, and the page fell back to refetching
// everything every five seconds. The Step column, which is fed by the stream's
// job frames and by nothing else, showed an em dash on every row for the whole
// of that fallback.
//
// There were four ways out, and this is the reasoning for the one taken.
//
//   - The operator's own token in the query string. Rejected. app.js states
//     the rule next to readToken: the token is attached as a header, "never as
//     a query parameter, where it would land in access logs, in the URL bar
//     and in anything the operator pastes into a ticket". leases.go spends a
//     page on the same idea from the other end — holder_instance is withheld
//     from every list because a credential that is merely VISIBLE is a
//     credential that ends up in a screenshot, a support dump or a log search.
//     A long-lived token that can revoke leases and cut power to a slot with a
//     live job on it does not go in a URL, and no dashboard feature is worth
//     making that false.
//   - A cookie. It works with EventSource and keeps the URL clean, but a
//     cookie is ambient: the browser attaches it to every request to this
//     origin whether the page meant it or not, which is the definition of the
//     CSRF surface this API does not have. POST /leases/{id}/revoke is not
//     reachable from a form on someone else's site today, and it becomes
//     reachable the moment a credential rides on ambient state. Buying a live
//     stream with that is a bad trade, and it would have to be paid back with
//     a token endpoint, a per-form nonce and a SameSite argument in every
//     future review.
//   - fetch() with a ReadableStream and a hand-written SSE parser. Correct —
//     headers work — and it was the runner-up. It costs frame assembly,
//     multi-line data:, comment lines and retry: parsing inside a page that
//     has no dependencies by policy, plus the reconnection EventSource does
//     for free, all of it in the path that carries alerts. That is a great
//     deal of new hand-written code to avoid one extra request.
//   - A ticket. This.
//
// A ticket is minted by POST /api/v1/stream/ticket — an ordinary authenticated
// request, with the token in the header where the rule says it belongs — and
// spent on GET /api/v1/stream?ticket=… . It IS in a URL, which is the shape
// the rule forbids for the token, so it is built to be worth nothing to
// whoever reads it back out of a proxy log:
//
//   - It expires in 30 seconds. The dashboard spends it in the same tick it
//     was minted; a log is read minutes or days later.
//   - It is single-use. The legitimate client burns it on connect, so by the
//     time the log line exists the value in it has already been spent.
//   - It opens exactly one route. redeem refuses anything that is not GET on
//     streamPath, so a stolen ticket cannot revoke a lease, read the audit log,
//     list the fleet — or mint itself a successor.
//   - It carries the identity it was minted for and nothing else: same
//     subject, same role, same tenant confinement, so the stream renders in
//     the scope that token had. A tenant's ticket does not become an
//     operator's.
//   - It is never shown to a human, so it cannot be pasted anywhere. The page
//     mints it, spends it and drops it; nothing renders it.
//
// What that leaves in the URL is a 30-second, single-use, read-only window on
// data the same caller could already read — a materially different object from
// the credential the rule was written to protect. The rule itself is untouched:
// the token still travels only in a header, including on the request that mints
// this.
//
// Tickets are held as digests and compared in constant time for the reason
// StaticBearer gives for doing the same with tokens: plaintext credentials in a
// map get printed by the first goroutine dump.
//
// # The store is per-process, and that is visible in a multi-replica farm
//
// There is no table behind this. A restart therefore invalidates every
// outstanding ticket, which costs nothing — the stream those tickets belonged
// to died with the process, and the page mints a new one when it reconnects.
//
// The case that is visible is more than one api replica, which the Helm chart
// defaults to. The mint and the redeem are separate connections and a
// per-connection load balancer may put them on different pods, so a redeem can
// arrive at a replica that never minted anything and answer 401. The page
// handles it by minting again — see connectStream in app.js, which retries
// promptly before falling back to polling — and with N replicas each attempt
// succeeds with probability 1/N, so it converges in well under a second rather
// than degrading. A deployment that wants it deterministic rather than
// probabilistic sets api.service.sessionAffinity in the chart, which says
// there why that is not the default.
//
// The alternative was a stateless signed ticket any replica could verify, and
// it was rejected twice over: it cannot be single-use, which is the property
// doing most of the work above, and the signing key would have to come out of
// the Authenticator — the interface auth.go keeps deliberately narrow so that a
// deployment can drop in OIDC by implementing one method. A JWKS-backed
// authenticator has no shared secret to offer.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"
)

const (
	// streamTicketTTL is how long a minted ticket stays redeemable. It is the
	// budget for one round trip from the dashboard, not for a session: the page
	// opens the EventSource in the continuation of the mint's own promise.
	streamTicketTTL = 30 * time.Second

	// streamTicketParam is the query parameter the ticket is presented in. It
	// is named in the mint response so the client never has to hard-code it.
	streamTicketParam = "ticket"

	// maxStreamTickets bounds the store. Minting is authenticated, so this is
	// not a defence against strangers; it is a ceiling on what a looping
	// dashboard — or a page left open through a network partition, minting
	// every couple of seconds and redeeming none of them — can accumulate in
	// the memory of the process that also serves the renewal path.
	maxStreamTickets = 4096

	// maxStreamTicketsPerSubject is why the ceiling above is not the only one.
	//
	// Minting is gated at RoleTenant, the lowest role there is, and a shared
	// ceiling with no per-caller share is a ceiling one caller can spend on
	// everybody's behalf: a tenant looping on the mint fills the map and every
	// OTHER dashboard on that replica — the operators' included, during
	// whatever incident had them open — starts getting 503 and falls back to
	// polling. This bounds that to the offender's own share.
	//
	// It is generous against the legitimate shape. One page holds at most one
	// unspent ticket, and the burst case is a farm flapping while several tabs
	// reconnect: a handful each, all of them expiring within 30 seconds.
	maxStreamTicketsPerSubject = 64

	// streamTicketBytes is the entropy in one ticket. 256 bits, because a
	// guessable ticket is a stream nobody authenticated.
	streamTicketBytes = 32
)

// errTooManyStreamTickets is returned when the store, or the caller's share of
// it, is full of unspent tickets. It is a 503 rather than a 500: the condition
// is transient by construction, since everything in the store expires within
// streamTicketTTL.
var errTooManyStreamTickets = errors.New("api: too many outstanding stream tickets")

// streamTicket is one unspent ticket.
type streamTicket struct {
	// digest is the ticket's own SHA-256, kept for the constant-time compare
	// that actually decides redemption. The map key is its hex, exactly as in
	// StaticBearer, so a map probe alone never decides the outcome.
	digest   [sha256.Size]byte
	identity Identity
	expires  time.Time
}

// streamTicketStore mints and redeems stream tickets.
//
// Its zero value is not usable and a nil store is: every method tolerates a nil
// receiver, because the request-path tests build a Server literal with only the
// fields authorisation touches, and a nil store must mean "no ticket is valid
// here" rather than a panic on a request that presented one.
type streamTicketStore struct {
	mu      sync.Mutex
	tickets map[string]streamTicket

	// now is the clock, replaced in tests. Expiry is the property that makes a
	// ticket in a log line worthless, so it is tested rather than assumed.
	now func() time.Time

	metrics *httpMetrics
}

func newStreamTicketStore(m *httpMetrics) *streamTicketStore {
	return &streamTicketStore{
		tickets: make(map[string]streamTicket),
		now:     time.Now,
		metrics: m,
	}
}

// countTicket records an outcome, tolerating the metric-less test server.
func (s *streamTicketStore) countTicket(outcome string) {
	if s == nil || s.metrics == nil || s.metrics.streamTickets == nil {
		return
	}
	s.metrics.streamTickets.WithLabelValues(outcome).Inc()
}

// mint issues a ticket for id and returns it with its lifetime.
//
// The identity is frozen at mint time, which is a 30-second window in which a
// token that has just been removed from FARM_API_TOKENS can still open a
// stream. That is the same window a request already in flight has, it is
// read-only, and closing it would mean re-authenticating a credential the
// redeeming request does not carry — which is the entire problem this exists
// to solve.
func (s *streamTicketStore) mint(id Identity) (string, time.Duration, error) {
	if s == nil {
		return "", 0, errors.New("api: no stream ticket store on this server")
	}
	var raw [streamTicketBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// Without entropy the ticket would be guessable, and a guessable
		// ticket is an unauthenticated stream. Fail the mint instead.
		return "", 0, err
	}
	ticket := base64.RawURLEncoding.EncodeToString(raw[:])
	digest := sha256.Sum256([]byte(ticket))

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	held := s.sweepLocked(now, id.Subject)
	if len(s.tickets) >= maxStreamTickets || held >= maxStreamTicketsPerSubject {
		s.countTicket("refused")
		return "", 0, errTooManyStreamTickets
	}
	s.tickets[hex.EncodeToString(digest[:])] = streamTicket{
		digest:   digest,
		identity: id,
		expires:  now.Add(streamTicketTTL),
	}
	s.countTicket("minted")
	return ticket, streamTicketTTL, nil
}

// sweepLocked drops expired tickets and reports how many live ones subject
// still holds.
//
// It runs on every mint, which is the only operation that grows the map, so an
// idle process holds nothing and a busy one never accumulates more than one
// TTL's worth of dead entries. Counting the caller's share in the same pass is
// why there is no second index to keep consistent: the walk is already paid
// for, and a per-subject counter that drifted from the map would enforce a
// quota against a number nobody wrote.
func (s *streamTicketStore) sweepLocked(now time.Time, subject string) int {
	held := 0
	for key, t := range s.tickets {
		if !now.Before(t.expires) {
			delete(s.tickets, key)
			continue
		}
		if t.identity.Subject == subject {
			held++
		}
	}
	return held
}

// redeem spends the ticket on r, if r is a request a ticket may open at all.
//
// The route check is the ticket's scope and is deliberately made here rather
// than at registration: this runs from the authentication step, which every
// route shares, and a ticket that authenticated anything else would be a
// bearer credential in a URL — the thing the file header rejects. Method and
// path are compared against the one route that cannot send a header.
func (s *streamTicketStore) redeem(r *http.Request) (Identity, bool) {
	if s == nil || r == nil || r.URL == nil {
		return Identity{}, false
	}
	raw := r.URL.Query().Get(streamTicketParam)
	if raw == "" {
		return Identity{}, false
	}
	// The presence check comes first so this one is countable. A ticket
	// offered against /fleet, /events or the mint route itself is the exact
	// shape a leaked ticket being probed would have, and it is the one thing
	// here that nobody legitimate ever does — a dashboard sends the parameter
	// only on the stream. Refused silently it would be undetectable, so it
	// gets its own series.
	if r.Method != http.MethodGet || r.URL.Path != streamPath {
		s.countTicket("misrouted")
		return Identity{}, false
	}
	digest := sha256.Sum256([]byte(raw))

	s.mu.Lock()
	defer s.mu.Unlock()
	key := hex.EncodeToString(digest[:])
	t, found := s.tickets[key]
	if !found {
		s.countTicket("unknown")
		return Identity{}, false
	}
	// Spent on sight, before the outcome is decided. A ticket that reached the
	// map is finished either way: single use is what makes the copy sitting in
	// a proxy log worthless, and it must not survive a redemption that failed
	// for some other reason.
	delete(s.tickets, key)

	if subtle.ConstantTimeCompare(t.digest[:], digest[:]) != 1 {
		s.countTicket("unknown")
		return Identity{}, false
	}
	if !s.now().Before(t.expires) {
		s.countTicket("expired")
		return Identity{}, false
	}
	// The redemption is recorded as its own authentication method, keeping the
	// authenticator that actually vouched for this caller: "bearer+ticket"
	// says both that a real credential was presented and that this particular
	// request rode in on a ticket.
	id := t.identity
	id.Method = id.Method + "+ticket"
	s.countTicket("redeemed")
	return id, true
}

// ---------------------------------------------------------------------------
// POST /api/v1/stream/ticket
// ---------------------------------------------------------------------------

// streamTicketResponse is what the dashboard receives.
//
// It names the parameter it must send the ticket in, so the client holds no
// copy of a constant that lives here. expires_in_s is advisory — the server
// enforces it — and is reported so a client can tell "my ticket went stale" from
// "the control plane rejected me", which are different bugs.
type streamTicketResponse struct {
	Ticket     string `json:"ticket"`
	Param      string `json:"param"`
	ExpiresInS int    `json:"expires_in_s"`
	SingleUse  bool   `json:"single_use"`
}

// handleStreamTicket serves POST /api/v1/stream/ticket.
//
// POST, not GET, for two reasons that both matter here: it mints state, and a
// GET that hands out a credential is the kind of URL that gets prefetched by a
// browser, retried by a proxy and pasted into a runbook.
func (s *Server) handleStreamTicket(w http.ResponseWriter, r *http.Request) {
	// requireRole has already run; a route reached without an identity would
	// be a routing bug, and minting for an unidentified caller would be the
	// hole this file exists to avoid.
	id, ok := IdentityFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthenticated,
			"present a valid credential for this API", nil)
		return
	}

	ticket, ttl, err := s.tickets.mint(id)
	if err != nil {
		if errors.Is(err, errTooManyStreamTickets) {
			writeError(w, http.StatusServiceUnavailable, CodeUnavailable,
				"too many stream tickets are outstanding on this instance; retry in a few seconds", nil)
			return
		}
		s.log.ErrorContext(r.Context(), "minting a stream ticket failed", "err", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal error", nil)
		return
	}

	// A ticket is a credential with a 30-second life; a cache that held one
	// would hand the next caller somebody else's stream scope.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, streamTicketResponse{
		Ticket:     ticket,
		Param:      streamTicketParam,
		ExpiresInS: int(ttl / time.Second),
		SingleUse:  true,
	})
}
