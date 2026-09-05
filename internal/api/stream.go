package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Server-Sent Events.
//
// The dashboard needs live fleet, lease and recovery state, and it needs it
// without every open tab running its own polling loop against Postgres. So one
// poller reads the database on a short interval, diffs the result against the
// previous read, and fans the CHANGES out to every connected client. Ten
// dashboards cost the same queries as one.
//
// Three properties matter more than the transport choice:
//
//   - When nobody is watching, nothing is queried. A wall-mounted dashboard
//     that nobody has open must not compete with the renewal path for
//     connections.
//   - A client that cannot keep up is disconnected, never waited on. Blocking
//     the poller on one slow reader would stall every other client and hold a
//     database connection while it did.
//   - Comment heartbeats keep proxies from closing an idle stream. A dashboard
//     that silently stops updating is worse than one that visibly reconnects.
const (
	// streamBuffer is how many events a client may fall behind before it is
	// dropped and told to reconnect.
	streamBuffer = 64

	// streamHeartbeat defeats idle timeouts in proxies and load balancers,
	// which are commonly 30 or 60 seconds.
	streamHeartbeat = 15 * time.Second

	// streamResync pushes a full snapshot to every client periodically, so a
	// dashboard that missed a delta — or that has been open long enough for a
	// countdown to drift — converges without a reconnect.
	streamResync = 30 * time.Second

	// streamWindow is how far back terminal rows stay in the poller's view, so
	// a lease that ended or a quarantine that closed is reported once before it
	// ages out.
	streamWindow = 5 * time.Minute
)

type sseEvent struct {
	name string
	data []byte
}

func newSSEEvent(name string, payload any) *sseEvent {
	b, err := json.Marshal(payload)
	if err != nil {
		b = []byte(`{"error":"event encoding failed"}`)
	}
	return &sseEvent{name: name, data: b}
}

type streamClient struct {
	ch   chan *sseEvent
	done chan struct{}
	// scope is the tenant this client is confined to, "" for an operator.
	// Every frame it receives is rendered in this scope: the same poll reads
	// differently to a tenant, which sees its own leases and a masked outline
	// of everyone else's, and to an operator, which sees the farm.
	scope string
	// needInit is true until the client has received a full snapshot. Guarded
	// by streamHub.mu.
	needInit bool
	closed   bool
}

type streamHub struct {
	mu      sync.Mutex
	clients map[*streamClient]struct{}
	closed  bool

	// kick asks the poller for an immediate read, so a client that has just
	// connected does not wait a whole interval for its first frame.
	kick chan struct{}

	log     *slog.Logger
	metrics *httpMetrics
}

func newStreamHub(log *slog.Logger, m *httpMetrics) *streamHub {
	return &streamHub{
		clients: make(map[*streamClient]struct{}),
		kick:    make(chan struct{}, 1),
		log:     log,
		metrics: m,
	}
}

// subscribe registers a client confined to scope ("" for an operator).
//
// Its first frame comes from the next poll, rendered in its own scope; the
// kick makes that poll happen now rather than at the next tick. There is no
// cached snapshot to hand it: a cache would have to be kept per scope, be
// invalidated for the scopes that have no client, and after an idle period —
// when the poller has deliberately stopped reading — the frame it served
// would predate it. Rendering only at publish time means every frame a client
// ever receives was rendered from the poll that just happened, for the price
// of one poll's latency on connect.
func (h *streamHub) subscribe(scope string) *streamClient {
	c := &streamClient{
		ch:       make(chan *sseEvent, streamBuffer),
		done:     make(chan struct{}),
		scope:    scope,
		needInit: true,
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		close(c.done)
		c.closed = true
		return c
	}
	h.clients[c] = struct{}{}
	n := len(h.clients)
	h.mu.Unlock()

	h.metrics.streamClients.Set(float64(n))

	select {
	case h.kick <- struct{}{}:
	default:
	}
	return c
}

func (h *streamHub) unsubscribe(c *streamClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		if !c.closed {
			c.closed = true
			close(c.done)
		}
	}
	n := len(h.clients)
	h.mu.Unlock()
	h.metrics.streamClients.Set(float64(n))
}

func (h *streamHub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// publish hands one poll's result to the clients, each rendered in its own
// scope: a full snapshot to anyone who has not had one yet (or to everyone on
// a resync), the delta since prev to the rest.
//
// One poll, one state, rendered at most once per scope that has a client
// right now — ten operator dashboards share one render, and a tenant's
// dashboard costs one more. The render runs under the lock so that a client
// subscribing mid-publish is either served this poll or waits for the next;
// it is a JSON encode of the fleet, not a database read.
func (h *streamHub) publish(prev, cur streamState, havePrev, resync bool) {
	var dropped []*streamClient

	fulls := map[string][]*sseEvent{}
	deltas := map[string][]*sseEvent{}
	render := func(c *streamClient) []*sseEvent {
		if c.needInit || resync {
			events, ok := fulls[c.scope]
			if !ok {
				events = fullEvents(cur, c.scope)
				fulls[c.scope] = events
			}
			return events
		}
		if !havePrev {
			return nil
		}
		events, ok := deltas[c.scope]
		if !ok {
			events = deltaEvents(prev, cur, c.scope)
			deltas[c.scope] = events
		}
		return events
	}

	h.mu.Lock()
	for c := range h.clients {
		events := render(c)
		if len(events) == 0 {
			continue
		}
		ok := true
		for _, ev := range events {
			select {
			case c.ch <- ev:
			case <-c.done:
				ok = false
			default:
				// The client is more than streamBuffer events behind. Waiting
				// on it would stall the poller and every other dashboard, so it
				// is disconnected and will get a fresh snapshot when the
				// browser reconnects.
				ok = false
			}
			if !ok {
				break
			}
		}
		if !ok {
			dropped = append(dropped, c)
			continue
		}
		c.needInit = false
	}
	for _, c := range dropped {
		if _, ok := h.clients[c]; ok {
			delete(h.clients, c)
			if !c.closed {
				c.closed = true
				close(c.done)
			}
		}
	}
	n := len(h.clients)
	h.mu.Unlock()

	if len(dropped) > 0 {
		h.metrics.streamDropped.Add(float64(len(dropped)))
		h.log.Warn("dropped event-stream clients that fell behind", "count", len(dropped))
	}
	h.metrics.streamClients.Set(float64(n))
}

// closeAll ends every stream. Called at the start of shutdown, because an SSE
// response never completes on its own and http.Server.Shutdown would otherwise
// wait out the entire grace period on connections that are healthy by design.
func (h *streamHub) closeAll() {
	h.mu.Lock()
	h.closed = true
	for c := range h.clients {
		if !c.closed {
			c.closed = true
			close(c.done)
		}
	}
	h.clients = make(map[*streamClient]struct{})
	h.mu.Unlock()
	h.metrics.streamClients.Set(0)
}

// handleStream serves GET /api/v1/stream.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)

	// Headers first, and only then the first flush. Flushing before the header
	// is set commits a default 200 with a sniffed content type, and every
	// header set afterwards — including text/event-stream itself — is silently
	// discarded, which a browser's EventSource rejects.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Nginx buffers proxied responses by default, which turns a live stream
	// into a very slow file download.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// retry tells the browser's EventSource how soon to come back, so a client
	// dropped for falling behind returns promptly with a fresh snapshot.
	if _, err := fmt.Fprintf(w, ": connected\nretry: 3000\n\n"); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		// Without flushing there is no streaming: an intermediary that buffered
		// the whole response would deliver the dashboard's first update when
		// the stream ends, which is never. The status is already committed, so
		// the only thing left is to close rather than pretend.
		s.log.WarnContext(r.Context(), "connection cannot stream events", "err", err)
		return
	}

	client := s.stream.subscribe(tenantScope(r.Context()))
	defer s.stream.unsubscribe(client)

	beat := time.NewTicker(streamHeartbeat)
	defer beat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-client.done:
			return
		case ev := <-client.ch:
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case <-beat.C:
			// A comment frame: valid SSE, ignored by EventSource, and enough
			// traffic to keep an idle-timeout proxy from closing the stream.
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The poller
// ---------------------------------------------------------------------------

// Digest types are deliberately comparable structs: the diff is a map lookup
// and a == , with no reflection and no per-field bookkeeping. They also
// deliberately EXCLUDE deadlines, because expires_at moves on every heartbeat
// and a dashboard does not need an event per renewal — the periodic resync
// carries fresh deadlines instead.
type fleetDigest struct {
	DeviceID    string `json:"device_id"`
	FarmUID     string `json:"farm_uid"`
	RackSlot    string `json:"rack_slot"`
	HostID      string `json:"host_id"`
	HubPath     string `json:"hub_path"`
	Health      string `json:"health"`
	ADBState    string `json:"adb_state"`
	AdminState  string `json:"admin_state"`
	SlotState   string `json:"slot_state"`
	LeaseID     string `json:"lease_id"`
	LeaseState  string `json:"lease_state"`
	JobID       string `json:"job_id"`
	Holder      string `json:"holder"`
	TenantID    string `json:"tenant_id"`
	Quarantined bool   `json:"quarantined"`
}

// forTenant is the digest as a caller confined to scope may see it: the
// lease's identity withheld unless the caller owns it, its state kept. An
// empty string is the digest's own spelling of "none", so a masked row reads
// as "held by somebody" rather than as free.
func (d fleetDigest) forTenant(scope string) fleetDigest {
	if d.LeaseID == "" || leaseVisible(scope, d.TenantID) {
		return d
	}
	d.LeaseID, d.JobID, d.Holder, d.TenantID = "", "", "", ""
	return d
}

type leaseDigest struct {
	LeaseID       string `json:"lease_id"`
	State         string `json:"state"`
	Fence         int64  `json:"fence"`
	DeviceID      string `json:"device_id"`
	RackSlot      string `json:"rack_slot"`
	JobID         string `json:"job_id"`
	TenantID      string `json:"tenant_id"`
	Holder        string `json:"holder"`
	Protected     bool   `json:"protected"`
	ReleaseReason string `json:"release_reason,omitempty"`
	ExpiresUnix   int64  `json:"expires_at_unix"`
}

// digestKey is what the diff compares. It omits ExpiresUnix so a routine
// heartbeat is not an event, while the emitted payload still carries the
// current deadline.
func (d leaseDigest) digestKey() leaseDigest {
	d.ExpiresUnix = 0
	return d
}

type jobDigest struct {
	JobID    string `json:"job_id"`
	State    string `json:"state"`
	TenantID string `json:"tenant_id"`
	QueueID  string `json:"queue_id"`
	PoolID   string `json:"pool_id"`
}

type recoveryDigest struct {
	ID       int64  `json:"id"`
	Tier     int    `json:"tier"`
	TierName string `json:"tier_name"`
	DeviceID string `json:"device_id,omitempty"`
	SlotID   int64  `json:"slot_id,omitempty"`
	HostID   string `json:"host_id,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	Refusal  string `json:"refusal,omitempty"`
	Finished bool   `json:"finished"`
}

type quarantineDigest struct {
	ID       int64  `json:"id"`
	Scope    string `json:"scope"`
	DeviceID string `json:"device_id,omitempty"`
	SlotID   int64  `json:"slot_id,omitempty"`
	HostID   string `json:"host_id,omitempty"`
	Reason   string `json:"reason"`
	Closed   bool   `json:"closed"`
}

type hubDigest struct {
	HubID     int64  `json:"hub_id"`
	HostID    string `json:"host_id"`
	USBPath   string `json:"usb_path"`
	Devices   int    `json:"devices"`
	Healthy   int    `json:"healthy"`
	Unhealthy int    `json:"unhealthy"`
}

type streamState struct {
	fleet       map[string]fleetDigest
	leases      map[string]leaseDigest
	jobs        map[string]jobDigest
	recovery    map[int64]recoveryDigest
	quarantines map[int64]quarantineDigest
	hubs        map[int64]hubDigest
	lastGapID   int64
	gapDetail   string
}

// runStream is the single poller. One goroutine for the whole process,
// regardless of how many dashboards are open.
func (s *Server) runStream(ctx context.Context) {
	tick := time.NewTicker(s.streamInterval)
	defer tick.Stop()

	var (
		prev       streamState
		havePrev   bool
		lastResync time.Time
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.stream.kick:
		}

		if s.stream.clientCount() == 0 {
			// Nobody is watching: query nothing, and forget what we knew so the
			// next client is given a full, current snapshot rather than a diff
			// against stale state.
			havePrev = false
			continue
		}

		cur, err := s.pollStreamState(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.metrics.streamPollErrors.Inc()
			s.log.Warn("event stream poll failed", "err", err)
			continue
		}

		resync := time.Since(lastResync) >= streamResync
		if resync {
			lastResync = time.Now()
		}
		// Published on every poll even when nothing changed: clients that
		// have not had a snapshot yet are waiting for one.
		s.stream.publish(prev, cur, havePrev, resync)

		prev, havePrev = cur, true
	}
}

// pollStreamState reads every section in one pass.
func (s *Server) pollStreamState(ctx context.Context) (streamState, error) {
	// Bounded independently of the poll interval: a query that outlives its
	// tick must not pile up behind the next one.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	st := streamState{
		fleet:       map[string]fleetDigest{},
		leases:      map[string]leaseDigest{},
		jobs:        map[string]jobDigest{},
		recovery:    map[int64]recoveryDigest{},
		quarantines: map[int64]quarantineDigest{},
		hubs:        map[int64]hubDigest{},
	}

	const fleetQuery = `
SELECT f.device_id::text, f.farm_uid, coalesce(f.rack_slot,''), coalesce(f.host_id,''),
       coalesce(f.hub_path,''), coalesce(f.health,'unknown'), coalesce(f.adb_state,'unknown'),
       f.admin_state, coalesce(f.slot_state,''), coalesce(f.lease_id::text,''),
       coalesce(f.lease_state,''), coalesce(f.job_id::text,''), coalesce(f.holder,''),
       coalesce(f.tenant_id,''), (f.quarantine_id IS NOT NULL)
  FROM farm.v_fleet f`

	rows, err := s.pool.Query(ctx, fleetQuery)
	if err != nil {
		return streamState{}, fmt.Errorf("poll fleet: %w", err)
	}
	for rows.Next() {
		var d fleetDigest
		if err := rows.Scan(&d.DeviceID, &d.FarmUID, &d.RackSlot, &d.HostID, &d.HubPath,
			&d.Health, &d.ADBState, &d.AdminState, &d.SlotState, &d.LeaseID, &d.LeaseState,
			&d.JobID, &d.Holder, &d.TenantID, &d.Quarantined); err != nil {
			rows.Close()
			return streamState{}, fmt.Errorf("poll fleet: %w", err)
		}
		st.fleet[d.DeviceID] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return streamState{}, fmt.Errorf("poll fleet: %w", err)
	}

	const leaseQuery = `
SELECT l.id::text, l.state, l.fence, l.device_id::text, coalesce(s.rack_slot,''),
       l.job_id::text, l.tenant_id, l.holder, l.protected, coalesce(l.release_reason,''),
       EXTRACT(EPOCH FROM l.expires_at)::bigint
  FROM farm.leases l
  LEFT JOIN farm.slots s ON s.id = l.slot_id
 WHERE l.state IN ('held','suspect')
    OR l.released_at > now() - $1::interval`

	rows, err = s.pool.Query(ctx, leaseQuery, intervalSeconds(streamWindow))
	if err != nil {
		return streamState{}, fmt.Errorf("poll leases: %w", err)
	}
	for rows.Next() {
		var d leaseDigest
		if err := rows.Scan(&d.LeaseID, &d.State, &d.Fence, &d.DeviceID, &d.RackSlot,
			&d.JobID, &d.TenantID, &d.Holder, &d.Protected, &d.ReleaseReason,
			&d.ExpiresUnix); err != nil {
			rows.Close()
			return streamState{}, fmt.Errorf("poll leases: %w", err)
		}
		st.leases[d.LeaseID] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return streamState{}, fmt.Errorf("poll leases: %w", err)
	}

	const jobQuery = `
SELECT j.id::text, j.state, j.tenant_id, j.queue_id, j.pool_id
  FROM farm.jobs j
 WHERE j.state IN ('queued','allocating','running')
    OR j.finished_at > now() - $1::interval`

	rows, err = s.pool.Query(ctx, jobQuery, intervalSeconds(streamWindow))
	if err != nil {
		return streamState{}, fmt.Errorf("poll jobs: %w", err)
	}
	for rows.Next() {
		var d jobDigest
		if err := rows.Scan(&d.JobID, &d.State, &d.TenantID, &d.QueueID, &d.PoolID); err != nil {
			rows.Close()
			return streamState{}, fmt.Errorf("poll jobs: %w", err)
		}
		st.jobs[d.JobID] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return streamState{}, fmt.Errorf("poll jobs: %w", err)
	}

	const recoveryQuery = `
SELECT a.id, a.tier, t.name, coalesce(a.device_id::text,''), coalesce(a.slot_id,0),
       coalesce(a.host_id,''), coalesce(a.outcome,''), coalesce(a.refusal,''),
       (a.finished_at IS NOT NULL)
  FROM farm.recovery_attempts a
  JOIN farm.recovery_tiers t ON t.tier = a.tier
 WHERE a.started_at > now() - interval '30 minutes' OR a.finished_at IS NULL
 ORDER BY a.id DESC
 LIMIT 200`

	rows, err = s.pool.Query(ctx, recoveryQuery)
	if err != nil {
		return streamState{}, fmt.Errorf("poll recovery: %w", err)
	}
	for rows.Next() {
		var d recoveryDigest
		if err := rows.Scan(&d.ID, &d.Tier, &d.TierName, &d.DeviceID, &d.SlotID, &d.HostID,
			&d.Outcome, &d.Refusal, &d.Finished); err != nil {
			rows.Close()
			return streamState{}, fmt.Errorf("poll recovery: %w", err)
		}
		st.recovery[d.ID] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return streamState{}, fmt.Errorf("poll recovery: %w", err)
	}

	const quarantineQuery = `
SELECT q.id, q.scope, coalesce(q.device_id::text,''), coalesce(q.slot_id,0),
       coalesce(q.host_id,''), q.reason, (q.closed_at IS NOT NULL)
  FROM farm.quarantines q
 WHERE q.closed_at IS NULL OR q.closed_at > now() - $1::interval`

	rows, err = s.pool.Query(ctx, quarantineQuery, intervalSeconds(streamWindow))
	if err != nil {
		return streamState{}, fmt.Errorf("poll quarantines: %w", err)
	}
	for rows.Next() {
		var d quarantineDigest
		if err := rows.Scan(&d.ID, &d.Scope, &d.DeviceID, &d.SlotID, &d.HostID,
			&d.Reason, &d.Closed); err != nil {
			rows.Close()
			return streamState{}, fmt.Errorf("poll quarantines: %w", err)
		}
		st.quarantines[d.ID] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return streamState{}, fmt.Errorf("poll quarantines: %w", err)
	}

	const hubQuery = `
SELECT v.hub_id, v.host_id, v.usb_path, v.devices, v.healthy, v.unhealthy
  FROM farm.v_hub_health v`

	rows, err = s.pool.Query(ctx, hubQuery)
	if err != nil {
		return streamState{}, fmt.Errorf("poll hub health: %w", err)
	}
	for rows.Next() {
		var d hubDigest
		if err := rows.Scan(&d.HubID, &d.HostID, &d.USBPath, &d.Devices, &d.Healthy,
			&d.Unhealthy); err != nil {
			rows.Close()
			return streamState{}, fmt.Errorf("poll hub health: %w", err)
		}
		st.hubs[d.HubID] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return streamState{}, fmt.Errorf("poll hub health: %w", err)
	}

	// The most recent control-plane gap. A new row here means every live lease
	// just had its deadlines pushed out by the length of our outage, which the
	// dashboard should say out loud rather than leave as a mysterious jump in
	// every countdown.
	var (
		gapID     *int64
		component *string
		gapSecs   *int64
	)
	err = s.pool.QueryRow(ctx, `
SELECT g.id, g.component, EXTRACT(EPOCH FROM (g.ended_at - g.started_at))::bigint
  FROM farm.control_plane_gap g ORDER BY g.id DESC LIMIT 1`).
		Scan(&gapID, &component, &gapSecs)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// No gaps at all is the healthy case, not a failure.
		return streamState{}, fmt.Errorf("poll control-plane gap: %w", err)
	}
	if gapID != nil {
		st.lastGapID = *gapID
		st.gapDetail = fmt.Sprintf("%s was silent for %ds; every live lease was extended by that much",
			derefString(component), derefInt64(gapSecs))
	}

	return st, nil
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// fullEvents renders a complete snapshot in the caller's scope, sent to a
// client on connect and to everyone on the periodic resync.
//
// The cut is the one every read route makes. Fleet rows are all there, with
// another tenant's lease reduced to "held"; leases and jobs are only the
// caller's own; recovery attempts, quarantines and hub health pass whole,
// because they describe the hardware and name nobody's work. The counts are
// taken before the mask: how many devices are busy is a fact about the farm.
func fullEvents(st streamState, scope string) []*sseEvent {
	fleet := make([]fleetDigest, 0, len(st.fleet))
	health := map[string]int{}
	leasedCount := 0
	for _, d := range st.fleet {
		fleet = append(fleet, d.forTenant(scope))
		health[d.Health]++
		if d.LeaseID != "" {
			leasedCount++
		}
	}
	leases := make([]leaseDigest, 0, len(st.leases))
	for _, d := range st.leases {
		if leaseVisible(scope, d.TenantID) {
			leases = append(leases, d)
		}
	}
	jobs := make([]jobDigest, 0, len(st.jobs))
	for _, d := range st.jobs {
		if leaseVisible(scope, d.TenantID) {
			jobs = append(jobs, d)
		}
	}
	attempts := make([]recoveryDigest, 0, len(st.recovery))
	for _, d := range st.recovery {
		attempts = append(attempts, d)
	}
	quarantines := make([]quarantineDigest, 0, len(st.quarantines))
	for _, d := range st.quarantines {
		quarantines = append(quarantines, d)
	}

	events := []*sseEvent{
		newSSEEvent("fleet", map[string]any{
			"snapshot": true,
			"devices":  fleet,
			"counts": map[string]any{
				"total":  len(fleet),
				"health": health,
				"leased": leasedCount,
				"free":   len(fleet) - leasedCount,
			},
		}),
		newSSEEvent("lease", map[string]any{"snapshot": true, "leases": leases}),
		newSSEEvent("job", map[string]any{"snapshot": true, "jobs": jobs}),
		newSSEEvent("recovery", map[string]any{
			"snapshot": true, "attempts": attempts, "quarantines": quarantines,
		}),
	}

	if alerts := snapshotAlerts(st, scope); len(alerts) > 0 {
		events = append(events, newSSEEvent("alert", map[string]any{
			"snapshot": true, "alerts": alerts,
		}))
	}
	return events
}

// snapshotAlerts derives the standing alerts from current state: hubs where
// more than one device is unhealthy (a hub, cable or power-domain fault rather
// than N phone faults), and protected leases sitting in suspect, which the
// reaper will never take back on its own — they are waiting for a human. The
// hub alerts go to everyone; a lease alert names a lease, and goes only to
// the scope that owns it.
func snapshotAlerts(st streamState, scope string) []map[string]any {
	var alerts []map[string]any
	for _, h := range st.hubs {
		if h.Unhealthy > 1 {
			alerts = append(alerts, map[string]any{
				"kind":      "hub_correlation",
				"hub_id":    h.HubID,
				"host_id":   h.HostID,
				"usb_path":  h.USBPath,
				"devices":   h.Devices,
				"unhealthy": h.Unhealthy,
				"message": fmt.Sprintf("%d of %d devices on hub %s are unhealthy; suspect the hub, "+
					"its cable or its power domain before the phones", h.Unhealthy, h.Devices, h.USBPath),
			})
		}
	}
	for _, l := range st.leases {
		if l.State == "suspect" && l.Protected && leaseVisible(scope, l.TenantID) {
			alerts = append(alerts, map[string]any{
				"kind":      "protected_lease_suspect",
				"lease_id":  l.LeaseID,
				"job_id":    l.JobID,
				"tenant_id": l.TenantID,
				"rack_slot": l.RackSlot,
				"message": "a protected lease has gone suspect: it will NOT be reclaimed " +
					"automatically and is waiting for a human",
			})
		}
	}
	return alerts
}

// deltaEvents renders only what changed since the previous poll, in the
// caller's scope: the same cut as fullEvents, applied to the changed rows.
//
// The diff itself is taken on the unmasked state, so a change that the mask
// then hides — another tenant's holder renamed — still arrives as a changed
// fleet row. That is harmless: a dashboard treats a changed row as "refresh
// this", and the refresh is served through the same mask.
func deltaEvents(prev, cur streamState, scope string) []*sseEvent {
	var events []*sseEvent

	if changed, removed := diffFleet(prev.fleet, cur.fleet); len(changed) > 0 || len(removed) > 0 {
		payload := map[string]any{}
		if len(changed) > 0 {
			for i := range changed {
				changed[i] = changed[i].forTenant(scope)
			}
			payload["changed"] = changed
		}
		if len(removed) > 0 {
			payload["removed"] = removed
		}
		events = append(events, newSSEEvent("fleet", payload))
	}

	var (
		leaseChanges []leaseDigest
		alerts       []map[string]any
	)
	for id, cd := range cur.leases {
		if !leaseVisible(scope, cd.TenantID) {
			continue
		}
		pd, existed := prev.leases[id]
		if existed && pd.digestKey() == cd.digestKey() {
			continue
		}
		leaseChanges = append(leaseChanges, cd)

		switch {
		case existed && pd.State == "held" && cd.State == "suspect":
			// Suspect is an ALERT and nothing else: no reset, no reboot, no
			// reallocation. The device stays with its holder, and a heartbeat
			// anywhere in the grace band self-heals it at the same fence.
			alerts = append(alerts, map[string]any{
				"kind":      "lease_suspect",
				"lease_id":  cd.LeaseID,
				"job_id":    cd.JobID,
				"tenant_id": cd.TenantID,
				"rack_slot": cd.RackSlot,
				"protected": cd.Protected,
				"message": "lease heartbeats stopped and it is now suspect. NOTHING has been " +
					"released: the device is still held at the same fence, and a heartbeat " +
					"inside the grace band restores it with no work lost.",
			})
		case existed && pd.State == "suspect" && cd.State == "held":
			alerts = append(alerts, map[string]any{
				"kind":     "lease_self_healed",
				"lease_id": cd.LeaseID,
				"job_id":   cd.JobID,
				"message":  "a suspect lease self-healed on a heartbeat at the same fence; no work was lost",
			})
		case cd.ReleaseReason == "holder_expired":
			// The one release reason that represents work destroyed by us
			// rather than work that ended.
			alerts = append(alerts, map[string]any{
				"kind":      "lease_reclaimed",
				"lease_id":  cd.LeaseID,
				"job_id":    cd.JobID,
				"tenant_id": cd.TenantID,
				"rack_slot": cd.RackSlot,
				"message": "a lease was RECLAIMED after its holder went silent for ttl+grace across " +
					"no control-plane gap. Whatever that job was doing is gone.",
			})
		case cd.ReleaseReason == "operator_revoked":
			alerts = append(alerts, map[string]any{
				"kind":     "lease_revoked",
				"lease_id": cd.LeaseID,
				"job_id":   cd.JobID,
				"message":  "an operator revoked this lease; the previous holder is fenced",
			})
		}
	}
	if len(leaseChanges) > 0 {
		events = append(events, newSSEEvent("lease", map[string]any{"changed": leaseChanges}))
	}

	var jobChanges []jobDigest
	for id, cd := range cur.jobs {
		if !leaseVisible(scope, cd.TenantID) {
			continue
		}
		if pd, ok := prev.jobs[id]; !ok || pd != cd {
			jobChanges = append(jobChanges, cd)
		}
	}
	if len(jobChanges) > 0 {
		events = append(events, newSSEEvent("job", map[string]any{"changed": jobChanges}))
	}

	var (
		recoveryChanges   []recoveryDigest
		quarantineChanges []quarantineDigest
	)
	for id, cd := range cur.recovery {
		if pd, ok := prev.recovery[id]; !ok || pd != cd {
			recoveryChanges = append(recoveryChanges, cd)
			if cd.Outcome == "refused" && cd.Refusal != "" {
				alerts = append(alerts, map[string]any{
					"kind":      "recovery_refused",
					"tier":      cd.Tier,
					"tier_name": cd.TierName,
					"device_id": cd.DeviceID,
					"slot_id":   cd.SlotID,
					"message":   cd.Refusal,
				})
			}
		}
	}
	for id, cd := range cur.quarantines {
		if pd, ok := prev.quarantines[id]; !ok || pd != cd {
			quarantineChanges = append(quarantineChanges, cd)
		}
	}
	if len(recoveryChanges) > 0 || len(quarantineChanges) > 0 {
		payload := map[string]any{}
		if len(recoveryChanges) > 0 {
			payload["attempts"] = recoveryChanges
		}
		if len(quarantineChanges) > 0 {
			payload["quarantines"] = quarantineChanges
		}
		events = append(events, newSSEEvent("recovery", payload))
	}

	for id, ch := range cur.hubs {
		ph, existed := prev.hubs[id]
		if existed && (ph.Unhealthy > 1) == (ch.Unhealthy > 1) {
			continue
		}
		if ch.Unhealthy > 1 {
			alerts = append(alerts, map[string]any{
				"kind":      "hub_correlation",
				"hub_id":    ch.HubID,
				"host_id":   ch.HostID,
				"usb_path":  ch.USBPath,
				"devices":   ch.Devices,
				"unhealthy": ch.Unhealthy,
				"message": fmt.Sprintf("%d of %d devices on hub %s went unhealthy together; suspect "+
					"the hub, its cable or its power domain", ch.Unhealthy, ch.Devices, ch.USBPath),
			})
		} else if existed {
			alerts = append(alerts, map[string]any{
				"kind":     "hub_recovered",
				"hub_id":   ch.HubID,
				"host_id":  ch.HostID,
				"usb_path": ch.USBPath,
				"message":  "hub " + ch.USBPath + " is no longer showing correlated failures",
			})
		}
	}

	if cur.lastGapID != prev.lastGapID && cur.lastGapID != 0 {
		alerts = append(alerts, map[string]any{
			"kind":    "control_plane_gap",
			"gap_id":  cur.lastGapID,
			"message": "a control-plane outage was recorded and refunded: " + cur.gapDetail,
		})
	}

	if len(alerts) > 0 {
		events = append(events, newSSEEvent("alert", map[string]any{"alerts": alerts}))
	}
	return events
}

func diffFleet(prev, cur map[string]fleetDigest) (changed []fleetDigest, removed []string) {
	for id, cd := range cur {
		if pd, ok := prev[id]; !ok || pd != cd {
			changed = append(changed, cd)
		}
	}
	for id := range prev {
		if _, ok := cur[id]; !ok {
			removed = append(removed, id)
		}
	}
	return changed, removed
}
