package api

// The interactive-control routes: a live screen, and a human's finger on it.
//
// # Why these are operator-only, and why that is not timidity
//
// Every tenant-readable route in this package narrows what it returns by
// tenant_id — see tenantScope and the masking in fleet.go, which nils named
// fields a tenant may not see. A framebuffer has no named fields. There is
// nothing to mask: the picture is whatever is on the phone, which may be another
// tenant's login screen, another tenant's test data, or another tenant's
// customer's name. The same argument already made bulk shell output
// operator-only (TestBulkReadsAreOperatorOnly), and it is stronger here.
//
// So there is no tenant path to a screen at all, and TestScreenRoutesAreOperatorOnly
// reads router.go as text to keep it that way. The mutation it exists to catch
// is not malice; it is somebody helpfully "letting tenants see their own
// devices" on a Friday.
//
// # Why the api does not decode the video
//
// It splices it. The body of GET .../screen is the scrcpy stream verbatim: four
// bytes of codec id, a twelve-byte session header, then length-prefixed Annex-B
// packets. internal/screen reads the first sixteen fixed-width bytes — the input
// path needs the frame's dimensions to place a touch — and this handler copies
// the rest with a buffer of its own. No length a handset chose ever reaches an
// allocator in the process that answers POST /leases/{id}/renew.
//
// The browser decodes it with WebCodecs, which takes Annex-B H.264 directly, so
// nothing in the path transcodes.
//
// # Why there is no stream ticket
//
// The existing ticket exists because EventSource cannot send a header
// (stream_ticket.go). fetch can, and the dashboard's screen panel is a fetch
// feeding a VideoDecoder. Widening the ticket to a second route would cost the
// "misrouted" counter its meaning — that counter is how a leaked ticket being
// probed is noticed — so the ticket stays scoped to exactly one path and this
// route is authenticated like every other one.
//
// # What a dead stream means
//
// Nothing. A severed socket, a closed tab, a replaced pod, an encoder dying on
// the handset: all of them end BYTES. The device is exactly as leased afterwards
// as it was before, because farm.leases.release_reason is CHECK-constrained to
// seven values and none of them is about a connection. Every refusal below that
// could be read as a verdict on a lease says so in words, for the same reason
// handleDeviceExec's do.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/internal/artifacts"
	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/scrcpy"
	"github.com/flaviopadilha/device-farmer/internal/screen"
)

// admissionClassControl is the credential class the fence proxy admits a screen
// under. It is a fourth class rather than a reuse of "lease" for a reason worth
// keeping next to the string: the lease class consults no service whitelist at
// all and gets no audit line in the proxy, so dialling as lease from here would
// widen what the api can open on a phone AND delete the record of the widening
// in the same edit. Control is bounded by both a whitelist and a fence.
//
// The word is advisory on the wire — the proxy takes the authoritative class
// from the client certificate — which is why this path needs its own
// certificate (FARM_FENCE_CONTROL_CERT) and not just its own string.
const admissionClassControl = "control"

// screenCopyBuffer is the splice buffer.
//
// THIS NUMBER IS THE WHOLE ALLOCATION POLICY OF THIS HANDLER. It is a constant,
// chosen here, and it is the only buffer in the video path: nothing in this
// process ever allocates a length that came off the wire. 64 KiB is four times
// the fence proxy's own splice buffer and comfortably larger than a typical
// inter-frame packet, so a frame usually crosses in one or two reads.
const screenCopyBuffer = 64 << 10

// ScreenContentType is the media type of the stream body.
//
// A vendor type rather than video/h264, deliberately. The body is NOT an H.264
// elementary stream: it is scrcpy's framing around one. A client that trusted
// video/h264 and handed the body to a decoder would feed it twelve-byte headers
// as though they were NAL units.
const ScreenContentType = "application/vnd.device-farmer.screen"

// Headers the stream carries. The session id travels in a header rather than in
// the body because the body is binary and starts with the codec id — a JSON
// preamble in front of it would mean every client needs a parser before it can
// find the video.
const (
	HeaderScreenSession = "X-Screen-Session"
	HeaderScreenDevice  = "X-Screen-Device"
	HeaderScreenFrame   = "X-Screen-Frame"
)

// screenArtifacts is the narrow view of the artifact store this path needs.
//
// Narrow so that the screen path cannot put an artifact into the store, remove
// one, or sweep a blob — it can only ask for a known digest to be present on a
// device. A handler that could Put would be a handler through which an operator
// shell could become a permanent artifact.
type screenArtifacts interface {
	EnsureOnDevice(ctx context.Context, deviceID, sha string, push artifacts.PushFunc) (artifacts.EnsureResult, error)
}

// screenDeviceFactory builds the bound device a session runs over.
//
// It takes the fence because the preamble is a property of the PLACEMENT, not of
// the endpoint — the same shape jobrunner.dial uses — so a client is built per
// session rather than cached per host.
type screenDeviceFactory func(endpoint, devpath string, fence int64) screen.Device

func defaultScreenDeviceFactory(cfg *config.Config) screenDeviceFactory {
	return func(endpoint, devpath string, fence int64) screen.Device {
		cli := adbwire.New(endpoint,
			adbwire.WithTLS(cfg.FenceControl.TLS),
			adbwire.WithAdmissionPreamble(func() (string, string, int64, bool) {
				return admissionClassControl, devpath, fence, true
			}))
		return screen.Bind(cli, devpath)
	}
}

// screenUnavailable reports why this farm cannot serve a screen, or "" if it
// can.
//
// It returns the remedy as well as the reason, following the precedent in
// ops.go's host-agent refusal: a 503 that names the variable to set is a deploy,
// and a 503 that does not is a support ticket.
// It reports EVERY problem, not the first one. internal/config states the rule
// this follows and the reason for it: an operator with two problems who is told
// about one of them does two deploys to find both. A farm turning this feature on
// for the first time plausibly has all three of these wrong at once — no jar
// pinned, no artifact directory, no control certificate — and a switch that
// returned early would reveal them one deploy at a time.
func (s *Server) screenUnavailable() (reason, remedy string) {
	var reasons, remedies []string

	if !s.cfg.Screen.Enabled() {
		reasons = append(reasons,
			"this farm has no screen server pinned, so there is nothing to push to the handset")
		remedies = append(remedies, fmt.Sprintf(
			"upload the server jar with POST /api/v1/artifacts?kind=file and set %s to the digest "+
				"it was stored under, together with %s",
			config.EnvScreenServerSHA, config.EnvScreenServerVersion))
	}

	if s.screenStore == nil {
		reasons = append(reasons,
			"this process has no artifact store, so the screen server cannot be put on a device")
		remedies = append(remedies,
			"start the api role with a writable artifact directory; FARM_ARTIFACT_DIR names it")
	}

	// The asymmetry that has to be stated, because it is the difference between
	// the demo working and the demo being unreachable: on a farm with no fence
	// proxy the admission preamble is inert and a plain TCP dial is what every
	// other ADB client in this process already does, so no certificate is needed
	// or wanted. On a FENCED farm the proxy requires mTLS, and a screen with no
	// control certificate fails the handshake — which surfaces as a dial error
	// and sends an operator to look at the network, the one place the problem is
	// not.
	if s.cfg.FenceClient.Enabled() && !s.cfg.FenceControl.Enabled() {
		reasons = append(reasons,
			"this farm enforces the fence at the device and this process has no control-class "+
				"certificate, so a screen cannot be admitted by the proxy")
		remedies = append(remedies, fmt.Sprintf(
			"set %s and %s on the api role; the CA is shared with %s",
			config.EnvFenceControlCert, config.EnvFenceControlKey, config.EnvFenceClientCA))
	}

	if len(reasons) == 0 {
		return "", ""
	}
	return strings.Join(reasons, "; and "), strings.Join(remedies, "; then ")
}

// refuseScreenUnavailable writes the 503 and audits it.
//
// A configuration refusal on an operator route still writes a row, because
// "nobody could open a screen all week" is a thing someone will later need to
// find, and capabilities.go sets the same precedent.
func (s *Server) refuseScreenUnavailable(w http.ResponseWriter, r *http.Request, reason, remedy string) {
	detail := map[string]any{"fault": faultConfiguration, "remedy": remedy}
	ctx, cancel := detachedCtx(r.Context())
	defer cancel()
	s.auditAction(ctx, actor(r.Context()), "screen.unavailable",
		"route:"+r.URL.Path, "", detail)
	writeError(w, http.StatusServiceUnavailable, CodeUnavailable, reason, detail)
}

// ---------------------------------------------------------------------------
// GET /api/v1/devices/{id}/screen
// ---------------------------------------------------------------------------

// handleDeviceScreen opens a session and splices its video to the client.
//
// The response is long-lived: minutes, and legitimately hours on a wall display.
// That has three consequences this handler has to honour and which are easy to
// miss:
//
//   - It is excluded from the in-flight gauge and demoted in the access log, by
//     isLongLived in router.go. A screen on a wall would otherwise read as
//     permanent saturation on the gauge an operator consults to decide whether
//     the api is overloaded.
//   - Its audit row is written when the session OPENS, not when the handler
//     returns, because the handler may not return today. A second row records
//     the close, with the duration and the input count.
//   - The session is registered with a manager the shutdown path closes.
//     Without that, http.Server.Shutdown waits out the whole drain grace period
//     on every deploy.
func (s *Server) handleDeviceScreen(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		badRequest(w, "device id is required", nil)
		return
	}
	if reason, remedy := s.screenUnavailable(); reason != "" {
		s.refuseScreenUnavailable(w, r, reason, remedy)
		return
	}

	maxSize := s.cfg.Screen.MaxSize
	if q := strings.TrimSpace(r.URL.Query().Get("max_size")); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < config.MinScreenMaxSize || n > s.cfg.Screen.MaxSize {
			badRequest(w, fmt.Sprintf("max_size must be between %d and %d, which is this farm's "+
				"configured ceiling; a larger frame is refused here rather than asked of the "+
				"handset", config.MinScreenMaxSize, s.cfg.Screen.MaxSize),
				map[string]any{"requested": q, "ceiling": s.cfg.Screen.MaxSize})
			return
		}
		maxSize = n
	}
	force := r.URL.Query().Get("force") == "true"
	why := strings.TrimSpace(r.URL.Query().Get("reason"))

	d, err := s.lookupDevice(r.Context(), id)
	if err != nil {
		s.fail(w, r, "screen: resolve device", err)
		return
	}
	if d.ADBDevpath == nil || *d.ADBDevpath == "" {
		writeError(w, http.StatusConflict, CodeConflict,
			"this device is not in a slot, so it has no physical address to stream from",
			map[string]string{"device_id": d.DeviceID, "farm_uid": d.FarmUID})
		return
	}
	if d.ADBEndpoint == nil || *d.ADBEndpoint == "" {
		writeError(w, http.StatusConflict, CodeConflict,
			"this device's host has no ADB endpoint recorded, so there is nowhere to dial",
			map[string]string{"device_id": d.DeviceID, "host_id": derefString(d.HostID)})
		return
	}

	// The lease gate, copied from handleDeviceExec rather than reinvented: a
	// screen on a device in the middle of somebody's six-hour run can wreck
	// that run, and the holder gets no signal that it happened.
	if d.Lease != nil && !force {
		writeError(w, http.StatusConflict, CodeConflict,
			"this device holds a live lease; driving its screen can wreck that job's run. "+
				"Retry with force=true and a reason if you mean to do it anyway. Opening a "+
				"screen does not end the lease either way.",
			map[string]any{
				"lease_id":  derefString(d.Lease.ID),
				"job_id":    derefString(d.Lease.JobID),
				"tenant_id": derefString(d.Lease.TenantID),
				"holder":    derefString(d.Lease.Holder),
				"protected": d.Lease.Protected,
			})
		return
	}
	if d.Lease != nil && why == "" {
		badRequest(w, "a reason is required to open a screen on a leased device; it lands in "+
			"farm.audit_log next to your name and is the only record of why", nil)
		return
	}

	// The fence. On a fenced farm the control class must present a devpath AND a
	// fence or the proxy calls the connection malformed — so a free device has
	// nothing to present and cannot be screened. That is the mechanism talking,
	// not a policy invented here, and saying so is better than letting an
	// operator discover it as OutcomeRefuseMalformed in a proxy log.
	var fence int64
	if d.Lease != nil && d.Lease.Fence != nil {
		fence = *d.Lease.Fence
	}
	if s.cfg.FenceControl.Enabled() && fence == 0 {
		writeError(w, http.StatusConflict, CodeConflict,
			"this farm enforces the fence at the device, and a screen is admitted by presenting "+
				"the lease's fence. This device holds no lease, so there is no fence to present "+
				"and the proxy would refuse the connection as malformed. Create a lease on the "+
				"device first.",
			map[string]any{"device_id": d.DeviceID, "fence_floor": d.FenceFloor})
		return
	}

	jarID, err := screen.JarIDFromSHA(s.cfg.Screen.ServerSHA)
	if err != nil {
		// Unreachable in practice: config validation refused a malformed digest
		// at boot. Kept because "unreachable" and "unchecked" must not be the
		// same line of code.
		s.refuseScreenUnavailable(w, r, "the pinned screen server digest is not a sha256",
			"correct "+config.EnvScreenServerSHA)
		return
	}

	dev := s.newScreenDevice(*d.ADBEndpoint, *d.ADBDevpath, fence)
	sess, err := s.screens.Open(r.Context(), dev, screen.Options{
		DeviceID:  d.DeviceID,
		JarID:     jarID,
		Version:   s.cfg.Screen.ServerVersion,
		MaxSize:   maxSize,
		EnsureJar: s.ensureScreenJar(d.DeviceID, dev),
	})
	if err != nil {
		s.refuseScreenOpen(w, r, d, err)
		return
	}
	defer sess.Close()

	opened := time.Now()
	width, height := sess.Frame()
	auditDetail := map[string]any{
		"session":  sess.ID,
		"devpath":  *d.ADBDevpath,
		"host_id":  derefString(d.HostID),
		"frame":    fmt.Sprintf("%dx%d", width, height),
		"forced":   force,
		"max_size": maxSize,
	}
	if d.Lease != nil {
		auditDetail["lease_id"] = derefString(d.Lease.ID)
		auditDetail["job_id"] = derefString(d.Lease.JobID)
	}

	// Audited on OPEN, because a session may outlive any reasonable idea of
	// "afterwards". The close row below carries what only the end knows.
	bookCtx, cancelBook := detachedCtx(r.Context())
	defer cancelBook()
	s.auditAction(bookCtx, actor(r.Context()), "screen.open", "device:"+d.DeviceID, why, auditDetail)
	s.recordEvent(bookCtx, s.screenEvent("screen_open", d, auditDetail, r))
	s.metrics.screenSessions.Inc()

	defer func() {
		// A second detached context: the request's is cancelled the moment the
		// viewer goes away, which is precisely when this row has to be written.
		closeCtx, cancelClose := detachedCtx(r.Context())
		defer cancelClose()
		detail := map[string]any{}
		for k, v := range auditDetail {
			detail[k] = v
		}
		detail["duration_ms"] = time.Since(opened).Milliseconds()
		detail["inputs"] = sess.Inputs()
		// Said in the row itself, because an auditor reading "screen.close" on a
		// device under lease will otherwise wonder whether this is what ended
		// the lease.
		detail["lease_effect"] = "none: a screen session ends bytes and never a lease"
		if log := sess.ServerLog(); log != "" {
			detail["server_log"] = log
		}
		s.auditAction(closeCtx, actor(r.Context()), "screen.close", "device:"+d.DeviceID, why, detail)
		s.recordEvent(closeCtx, s.screenEvent("screen_close", d, detail, r))
		s.metrics.screenSessions.Dec()
	}()

	s.spliceScreen(w, r, sess)
}

// ensureScreenJar adapts the artifact store to internal/screen's callback.
//
// The path comes from internal/screen, which derives it from the jar's content
// id, and the digest comes from the configuration. Those two must agree or the
// CLASSPATH points at a file that is not there — which presents as a server that
// exits silently and a socket that never appears, the least diagnosable failure
// in this path. JarIDFromSHA is the single place the truncation happens.
func (s *Server) ensureScreenJar(deviceID string, dev screen.Device) screen.EnsureJar {
	return func(ctx context.Context, remote string) error {
		_, err := s.screenStore.EnsureOnDevice(ctx, deviceID, s.cfg.Screen.ServerSHA,
			func(ctx context.Context, a artifacts.Artifact, blob artifacts.Blob) (string, error) {
				// 0644 because app_process READS the jar; it does not execute
				// it, and 0755 here would be cargo cult.
				if err := dev.Push(ctx, blob, remote, 0o644); err != nil {
					return "", err
				}
				return remote, nil
			})
		return err
	}
}

// refuseScreenOpen turns internal/screen's failures into the right status.
//
// The distinctions are the point. "Already open" must not be retried against the
// same device; "at the cap" should be retried later; a socket that never appeared
// is a 502 carrying the handset's own words, because "the server never started"
// and "the server started and published nothing" send an operator to different
// places and the server's stderr is the only evidence which it was.
func (s *Server) refuseScreenOpen(w http.ResponseWriter, r *http.Request, d fleetDevice, err error) {
	base := map[string]any{"device_id": d.DeviceID, "devpath": derefString(d.ADBDevpath)}

	switch {
	case errors.Is(err, screen.ErrSessionExists):
		writeError(w, http.StatusConflict, CodeConflict,
			"this device already has a live screen session. scrcpy is one encoder per session, "+
				"so a second one is refused rather than queued: two would be two people "+
				"driving one phone with neither able to tell.",
			mergeDetail(base, map[string]any{"reason": "session_already_open"}))
		return

	case errors.Is(err, screen.ErrTooManySessions):
		writeError(w, http.StatusConflict, CodeConflict,
			fmt.Sprintf("this api process is at its limit of %d concurrent screen sessions. "+
				"The limit exists so that a wall of open tabs refuses the next one instead of "+
				"starving the request that renews every lease in the farm.", s.cfg.Screen.MaxSessions),
			mergeDetail(base, map[string]any{
				"reason":       "session_cap",
				"max_sessions": s.cfg.Screen.MaxSessions,
				"remedy":       config.EnvScreenMaxSessions,
			}))
		return

	case errors.Is(err, screen.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable,
			"this api process is shutting down and is not opening new screen sessions. "+
				"No lease is affected by a restart.",
			mergeDetail(base, map[string]any{"reason": "shutting_down"}))
		return
	}

	detail := mergeDetail(base, nil)
	var se *screen.SocketError
	if errors.As(err, &se) {
		detail["socket"] = se.Socket
		detail["waited_ms"] = se.Waited.Milliseconds()
		if se.ServerLog != "" {
			detail["server_log"] = se.ServerLog
		}
	}
	if te, ok := adbwire.AsTransport(err); ok {
		detail["transport_kind"] = te.Kind.String()
	}
	s.log.WarnContext(r.Context(), "screen session could not be opened",
		"device_id", d.DeviceID, "devpath", derefString(d.ADBDevpath), "err", err)
	writeError(w, http.StatusBadGateway, CodeADBError,
		"the screen server did not start on this device: "+err.Error()+". No lease was affected.",
		detail)
}

// spliceScreen writes the session's bytes to the client and nothing else.
//
// It allocates one buffer, of a size chosen in this file, and never consults a
// length from the wire. Read the package comment at the top before changing it.
func (s *Server) spliceScreen(w http.ResponseWriter, r *http.Request, sess *screen.Session) {
	rc := http.NewResponseController(w)
	width, height := sess.Frame()

	h := w.Header()
	h.Set("Content-Type", ScreenContentType)
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set(HeaderScreenSession, sess.ID)
	h.Set(HeaderScreenDevice, sess.DeviceID)
	h.Set(HeaderScreenFrame, fmt.Sprintf("%dx%d", width, height))
	// Nginx buffers proxied responses by default, which for a video stream
	// means the viewer sees nothing and then several seconds at once.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(sess.Preamble()); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		// A connection that cannot stream is not a connection that can show a
		// screen. Say so once, at warn, the way handleStream does.
		s.log.WarnContext(r.Context(), "connection cannot stream a screen", "err", err)
		return
	}

	// THE WRITE DEADLINE IS THE SESSION BOUND, and it is the only one.
	//
	// A viewer that closed its tab fails the next write immediately. The case
	// this handles is the other one: a viewer that holds the socket open and
	// reads nothing — a suspended laptop, a wedged browser — where the kernel
	// buffer fills and a write would otherwise block forever, holding three ADB
	// transports and an encoder on a phone for as long as the process lives.
	//
	// A deadline reaches that; closing the session does not, because closing the
	// video socket unblocks a READ and this is blocked on a WRITE.
	//
	// IT ENDS BYTES. When it fires, this handler returns, the session closes,
	// and the lease is exactly as valid as it was a moment earlier. There is no
	// path from here to farm.leases and adding one would be the idle timeout
	// LEASE-01 forbids.
	deadline := s.cfg.Screen.SessionTTL
	if deadline <= 0 {
		deadline = config.DefaultScreenSessionTTL
	}

	buf := make([]byte, screenCopyBuffer)
	src := sess.Video()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sess.Done():
			return
		default:
		}

		n, readErr := src.Read(buf)
		if n > 0 {
			if err := rc.SetWriteDeadline(time.Now().Add(deadline)); err != nil {
				// A ResponseWriter with no deadline support: accept it rather
				// than refuse the feature, and say so once. The other bounds
				// (the session cap, the viewer closing its tab) still apply.
				s.log.DebugContext(r.Context(), "screen stream has no write deadline", "err", err)
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
		if readErr != nil {
			// EOF here means the server on the handset stopped, which is an
			// ordinary end of a screen. It is NOT an ordinary end of a lease,
			// and this function has no way to confuse the two because it cannot
			// reach one.
			if !errors.Is(readErr, io.EOF) {
				s.log.DebugContext(r.Context(), "screen stream ended",
					"session", sess.ID, "err", readErr)
			}
			return
		}
	}
}

// screenEvent builds the farm.events row.
//
// It fills LeaseID and JobID when there is a lease, which device_exec's own
// event row does NOT do today — so the tenant whose job was touched cannot see
// it in their own timeline. Not repeating that here is the whole reason this
// helper exists rather than an inline struct literal.
func (s *Server) screenEvent(kind string, d fleetDevice, detail map[string]any, r *http.Request) eventRow {
	e := eventRow{
		Kind:     kind,
		DeviceID: &d.DeviceID,
		SlotID:   d.SlotID,
		Actor:    actor(r.Context()),
		Detail:   detail,
	}
	if d.Lease != nil {
		// Pointers, and only when non-empty: the columns are cast ::uuid, so an
		// empty string is an error rather than a null.
		if id := derefString(d.Lease.ID); id != "" {
			e.LeaseID = &id
		}
		if id := derefString(d.Lease.JobID); id != "" {
			e.JobID = &id
		}
	}
	return e
}

// ---------------------------------------------------------------------------
// POST /api/v1/devices/{id}/input
// ---------------------------------------------------------------------------

type inputRequest struct {
	// Session is the id from the X-Screen-Session header of the stream this
	// input belongs to. It is required: input is a property of a session, and
	// a request that merely named a device would let a caller touch a phone
	// whose screen they are not watching.
	Session string `json:"session"`

	Events []inputEvent `json:"events"`
}

// inputEvent is one message, in the wire shape the dashboard sends.
//
// X and Y are in VIDEO space — the frame the X-Screen-Frame header named, after
// max_size scaling and after whatever rotation the device was in when the stream
// started. They are NOT device pixels. A 1080x2400 panel streamed at max_size
// 1024 is 460x1024, so a caller that sends device pixels misses by more than
// half the screen. The frame travels with the stream for exactly this reason,
// and a coordinate outside it is refused rather than clamped.
type inputEvent struct {
	Type   string `json:"type"`   // touch | key | scroll
	Action string `json:"action"` // down | up | move
	X      int32  `json:"x,omitempty"`
	Y      int32  `json:"y,omitempty"`

	Pressure  float64 `json:"pressure,omitempty"`
	PointerID uint64  `json:"pointer_id,omitempty"`

	Keycode   int32 `json:"keycode,omitempty"`
	MetaState int32 `json:"meta_state,omitempty"`
	Repeat    int32 `json:"repeat,omitempty"`

	H float64 `json:"h,omitempty"`
	V float64 `json:"v,omitempty"`
}

type inputResponse struct {
	Accepted int   `json:"accepted"`
	Total    int64 `json:"total"`
}

// maxInputBatch bounds one request.
//
// A batch exists so a drag is one round trip rather than sixty; it is not a
// channel for bulk work. Sixty-four is about a second of continuous dragging at
// a realistic sampling rate, which is the longest gesture worth coalescing
// before the operator would see lag anyway.
const maxInputBatch = 64

// handleDeviceInput delivers input to a live session.
func (s *Server) handleDeviceInput(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		badRequest(w, "device id is required", nil)
		return
	}
	if reason, remedy := s.screenUnavailable(); reason != "" {
		s.refuseScreenUnavailable(w, r, reason, remedy)
		return
	}

	var req inputRequest
	if err := decodeJSON(w, r, &req); err != nil {
		badJSON(w, err)
		return
	}
	if strings.TrimSpace(req.Session) == "" {
		badRequest(w, "session is required; it is the X-Screen-Session header of the stream this "+
			"input belongs to", nil)
		return
	}
	if len(req.Events) == 0 {
		badRequest(w, "events is empty", nil)
		return
	}
	if len(req.Events) > maxInputBatch {
		badRequest(w, fmt.Sprintf("at most %d events per request; a batch coalesces one gesture, "+
			"not a workload", maxInputBatch), map[string]any{"sent": len(req.Events)})
		return
	}

	sess, err := s.screens.Lookup(req.Session)
	if err != nil {
		// NOT a 404. With more than one api replica the session lives in exactly
		// one of them, and a 404 would read as "that session is over" when it is
		// running perfectly well next door. The counter behind this is how an
		// operator discovers they need sessionAffinity, which the chart
		// documents; stream_ticket.go makes the same argument for mint/redeem.
		s.metrics.screenMisrouted.Inc()
		writeError(w, http.StatusConflict, CodeConflict,
			"this api process does not hold that screen session. With more than one api replica "+
				"the session lives in exactly one of them, and input has to reach the same one. "+
				"Enable api.service.sessionAffinity, or send input on the connection that opened "+
				"the stream.",
			map[string]any{"reason": "session_not_here", "session": req.Session})
		return
	}

	// The session is keyed by its own id, so a caller holding a session on
	// device A cannot aim a batch at device B by changing the path. Checked
	// rather than assumed, because the path is where an id is easiest to edit.
	if !sameDevice(sess.DeviceID, id) {
		d, lookupErr := s.lookupDevice(r.Context(), id)
		if lookupErr != nil || d.DeviceID != sess.DeviceID {
			writeError(w, http.StatusConflict, CodeConflict,
				"that session belongs to a different device than the one in this path. Input goes "+
					"to the device whose screen the session is showing, and nothing else.",
				map[string]any{"session_device": sess.DeviceID, "path_device": id})
			return
		}
	}

	events, err := decodeInputEvents(req.Events)
	if err != nil {
		badRequest(w, err.Error(), nil)
		return
	}

	n, err := sess.Send(events...)
	if err != nil {
		if errors.Is(err, screen.ErrNoSession) {
			writeError(w, http.StatusConflict, CodeConflict,
				"that screen session has ended, so there is nothing to send input to. The device's "+
					"lease, if it has one, is unaffected.",
				map[string]any{"reason": "session_ended", "session": req.Session})
			return
		}
		// An out-of-frame coordinate is the caller's mistake and is worth
		// distinguishing from a dead socket, because the caller can fix one.
		var oof *scrcpy.OutOfFrameError
		if errors.As(err, &oof) {
			badRequest(w, err.Error(), map[string]any{
				"frame": func() string { w, h := sess.Frame(); return fmt.Sprintf("%dx%d", w, h) }(),
				"note": "coordinates are in video space — the frame in X-Screen-Frame — not " +
					"device pixels",
			})
			return
		}
		s.metrics.screenInputs.WithLabelValues("error").Add(float64(len(events)))
		writeError(w, http.StatusBadGateway, CodeADBError,
			"the input did not reach the device: "+err.Error()+". No lease was affected.",
			map[string]any{"session": req.Session})
		return
	}
	s.metrics.screenInputs.WithLabelValues("ok").Add(float64(n))

	// DELIBERATELY NOT AUDITED PER EVENT. A row per touch is thousands of rows a
	// minute, so this feature cannot answer "what did this person do with the
	// phone" and the audit trail does not pretend otherwise: screen.close
	// carries a count, and "who held this device, from when to when, with input
	// enabled, and how many messages they sent" is the honest ceiling.
	writeJSON(w, http.StatusOK, inputResponse{Accepted: n, Total: sess.Inputs()})
}

// sameDevice compares a session's device id against whatever form the path used.
// The path accepts a uuid or a branded farm_uid, so a mismatch here is a reason
// to look the device up rather than a reason to refuse.
func sameDevice(sessionDeviceID, pathID string) bool {
	return strings.EqualFold(sessionDeviceID, pathID)
}

// decodeInputEvents turns the wire shape into internal/screen's closed set.
//
// Every unknown value is refused by name. A silently-dropped event in a
// down-move-up gesture is a finger that never lifted, and the caller has no way
// to tell: "accepted: 3" for a batch where one was ignored is a lie that shows
// up as a phone behaving as though somebody is holding the screen.
func decodeInputEvents(in []inputEvent) ([]screen.Event, error) {
	out := make([]screen.Event, 0, len(in))
	for i, e := range in {
		where := fmt.Sprintf("event %d of %d", i+1, len(in))
		switch e.Type {
		case "touch":
			action, err := touchAction(e.Action)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
			p := e.Pressure
			if action == scrcpy.TouchUp {
				// A finger coming up with pressure is a contradiction the
				// device resolves by keeping the pointer down. Normalised here
				// rather than refused, because every browser reports the
				// pressure of the pointer that just left and the caller is not
				// wrong to pass it on.
				p = 0
			}
			out = append(out, screen.Touch{
				Action: action, X: e.X, Y: e.Y, Pressure: p, PointerID: e.PointerID,
			})
		case "key":
			action, err := keyAction(e.Action)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
			out = append(out, screen.Key{
				Action: action, Keycode: e.Keycode, MetaState: e.MetaState, Repeat: e.Repeat,
			})
		case "scroll":
			out = append(out, screen.Scroll{X: e.X, Y: e.Y, Horizontal: e.H, Vertical: e.V})
		case "":
			return nil, fmt.Errorf("%s: type is required; it is one of touch, key or scroll", where)
		default:
			return nil, fmt.Errorf("%s: type %q is not one of touch, key or scroll", where, e.Type)
		}
	}
	return out, nil
}

func touchAction(s string) (scrcpy.TouchAction, error) {
	switch s {
	case "down":
		return scrcpy.TouchDown, nil
	case "up":
		return scrcpy.TouchUp, nil
	case "move":
		return scrcpy.TouchMove, nil
	case "":
		return 0, errors.New("action is required for a touch; it is one of down, up or move")
	default:
		return 0, fmt.Errorf("touch action %q is not one of down, up or move", s)
	}
}

func keyAction(s string) (scrcpy.KeyAction, error) {
	switch s {
	case "down":
		return scrcpy.KeyDown, nil
	case "up":
		return scrcpy.KeyUp, nil
	case "":
		return 0, errors.New("action is required for a key; it is down or up")
	default:
		return 0, fmt.Errorf("key action %q is not down or up", s)
	}
}
