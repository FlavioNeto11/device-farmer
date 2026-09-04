package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/flaviopadilha/device-farmer/internal/artifacts"
)

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

// CodeDigestMismatch accompanies 400 from POST /api/v1/artifacts when the
// bytes that arrived do not hash to the digest the client announced. It is its
// own code because it is the one upload failure a client can fix by itself:
// the transfer was corrupted or the wrong file was named, and retrying the
// same request unchanged will fail the same way.
const CodeDigestMismatch = "digest_mismatch"

// defaultMaxUpload bounds one upload at the HTTP edge.
//
// artifacts.Store has its own ceiling, and this is deliberately a second,
// lower one: the store's limit protects the disk, this one protects the
// listener. An unbounded upload holds a connection, a staging file and a
// request slot for as long as a client cares to keep writing, and the
// renewal path shares all three.
const defaultMaxUpload int64 = 2 << 30

// maxArtifactNameLen bounds the name an upload may declare.
//
// farm.artifacts.name is unconstrained text and the value arrives in a query
// string, so without a bound here one request writes as much as the request
// line permits into a column that every later list response renders — for
// every artifact, to every client, forever. Refused rather than truncated: a
// client that cannot find its upload again under the name it chose has been
// lied to about what was stored.
const maxArtifactNameLen = 512

// referenceScanTimeout bounds the scan that answers "what still names this
// digest".
//
// The job half of it is a sequential scan of farm.jobs against a jsonb path,
// and on DELETE it runs inside a transaction holding the artifact row locked.
// Unbounded, one operator's delete can pin a pool connection for as long as
// farm.jobs takes to walk — and that pool is the one a holder borrows from to
// renew. Nothing here may end a lease, but starving the renewal path of
// connections would end one by the back door, so the scan gets a deadline and
// fails CLOSED: a timeout refuses the delete, it never permits one.
const referenceScanTimeout = 15 * time.Second

// referenceListLimit bounds how many rows of each kind a refusal renders. The
// counts reported beside them are exact regardless — see references.
const referenceListLimit = 20

// deviceReferenceLimit bounds the ledger rows a refusal renders.
const deviceReferenceLimit = 500

// defaultDeviceArtifactLimit and maxDeviceArtifactLimit bound
// GET /devices/{id}/artifacts. A device's ledger keeps a row per digest it has
// ever held, including 'removed' ones, so the honest answer to "what is on
// this device" grows for the life of the phone and needs a page.
const (
	defaultDeviceArtifactLimit = 500
	maxDeviceArtifactLimit     = 5000
)

// ArtifactAPI serves the artifact half of the HTTP API.
//
// It is separate from Server because the blob backend is the parent's choice —
// a directory today, an object store later — and api has no business deciding
// where bytes live. It borrows the Server for everything else: the same error
// envelope, the same auth middleware, the same access log, the same pool.
type ArtifactAPI struct {
	srv       *Server
	store     *artifacts.Store
	maxUpload int64
}

// ArtifactOption configures an ArtifactAPI.
type ArtifactOption func(*ArtifactAPI)

// WithMaxUpload caps the request body one upload may carry. A non-positive
// value is ignored, because "no limit" is not a configuration this endpoint
// offers.
func WithMaxUpload(n int64) ArtifactOption {
	return func(a *ArtifactAPI) {
		if n > 0 {
			a.maxUpload = n
		}
	}
}

// NewArtifactAPI binds an artifact store to a Server.
func NewArtifactAPI(srv *Server, store *artifacts.Store, opts ...ArtifactOption) (*ArtifactAPI, error) {
	if srv == nil {
		return nil, errors.New("api: nil server")
	}
	if store == nil {
		return nil, errors.New("api: nil artifact store")
	}
	a := &ArtifactAPI{srv: srv, store: store, maxUpload: defaultMaxUpload}
	for _, o := range opts {
		o(a)
	}
	return a, nil
}

// Register mounts the artifact routes on mux.
//
// The roles are chosen here, at registration, exactly as they are for every
// other route in this package. Reads and uploads are tenant work; DELETE is
// not, because farm.device_artifacts.sha256 cascades — removing an artifact
// row silently removes every device ledger row that named it, which is how a
// fleet quietly starts re-pushing 200 MB to sixty phones.
func (a *ArtifactAPI) Register(mux *http.ServeMux) {
	s := a.srv
	mux.Handle("POST /api/v1/artifacts", s.requireRole(RoleTenant, http.HandlerFunc(a.handleUpload)))
	mux.Handle("GET /api/v1/artifacts", s.requireRole(RoleTenant, http.HandlerFunc(a.handleList)))
	mux.Handle("GET /api/v1/artifacts/{sha}", s.requireRole(RoleTenant, http.HandlerFunc(a.handleGet)))
	mux.Handle("DELETE /api/v1/artifacts/{sha}", s.requireRole(RoleOperator, http.HandlerFunc(a.handleDelete)))
	mux.Handle("GET /api/v1/devices/{id}/artifacts", s.requireRole(RoleTenant, http.HandlerFunc(a.handleDeviceArtifacts)))
}

// rowQuerier is the slice of pgxpool.Pool and pgx.Tx the reference scan needs,
// so the same code answers "what still references this" both for the read-only
// GET and inside the transaction that DELETE holds the artifact row locked in.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// artifactView is one row of farm.artifacts.
//
// version_name is present on an upload response and absent everywhere else,
// and that is not an oversight: farm.artifacts has no column for it, so it
// exists only in the reply to the request that parsed the APK.
type artifactView struct {
	SHA256      string    `json:"sha256"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	SizeBytes   int64     `json:"size_bytes"`
	Package     string    `json:"package,omitempty"`
	VersionCode int64     `json:"version_code,omitempty"`
	VersionName string    `json:"version_name,omitempty"`
	URL         string    `json:"url,omitempty"`
	UploadedBy  string    `json:"uploaded_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`

	// DevicesPresent counts the devices whose ledger says these exact bytes
	// are already on them. It is the number that decides whether a job's push
	// step costs 200 MB over USB or nothing at all.
	DevicesPresent *int64 `json:"devices_present,omitempty"`
}

func artifactViewOf(a artifacts.Artifact) artifactView {
	return artifactView{
		SHA256:      a.SHA256,
		Kind:        string(a.Kind),
		Name:        a.Name,
		SizeBytes:   a.Size,
		Package:     a.Package,
		VersionCode: a.VersionCode,
		VersionName: a.VersionName,
		URL:         a.URL,
		UploadedBy:  a.UploadedBy,
		CreatedAt:   a.CreatedAt,
	}
}

// artifactColumns is farm.artifacts' projection, in the order scanArtifact
// reads it.
const artifactColumns = `
  a.sha256, a.kind, a.name, a.size_bytes, a.package, a.version_code,
  a.url, a.uploaded_by, a.created_at`

// scanArtifact reads artifactColumns. The nullable columns are scanned through
// pointers and flattened, so a client never has to distinguish JSON null from
// an absent key for a value that means "not an APK" either way.
func scanArtifact(sc scanner, extra ...any) (artifactView, error) {
	var (
		v                    artifactView
		pkg, url, uploadedBy *string
		versionCode          *int64
	)
	dest := make([]any, 0, 9+len(extra))
	dest = append(dest, &v.SHA256, &v.Kind, &v.Name, &v.SizeBytes, &pkg, &versionCode,
		&url, &uploadedBy, &v.CreatedAt)
	dest = append(dest, extra...)
	if err := sc.Scan(dest...); err != nil {
		return artifactView{}, err
	}
	v.Package = derefString(pkg)
	v.VersionCode = derefInt64(versionCode)
	v.URL = derefString(url)
	v.UploadedBy = derefString(uploadedBy)
	return v, nil
}

// artifactDeviceRow is one row of farm.device_artifacts as seen from an
// artifact: which device holds it, and what the last attempt did.
type artifactDeviceRow struct {
	DeviceID      string     `json:"device_id"`
	FarmUID       string     `json:"farm_uid"`
	State         string     `json:"state"`
	InstalledAt   *time.Time `json:"installed_at,omitempty"`
	RemotePath    string     `json:"remote_path,omitempty"`
	Error         string     `json:"error,omitempty"`
	RemovedReason string     `json:"removed_reason,omitempty"`
}

// artifactJobRef is one job whose spec names an artifact by digest.
type artifactJobRef struct {
	JobID    string `json:"job_id"`
	State    string `json:"state"`
	TenantID string `json:"tenant_id"`
}

// deviceArtifactView is one row of farm.device_artifacts as seen from a
// device: the artifact, plus what this device did with it.
type deviceArtifactView struct {
	artifactView
	State         string     `json:"state"`
	InstalledAt   *time.Time `json:"installed_at,omitempty"`
	RemotePath    string     `json:"remote_path,omitempty"`
	Error         string     `json:"error,omitempty"`
	RemovedReason string     `json:"removed_reason,omitempty"`
}

// ---------------------------------------------------------------------------
// POST /api/v1/artifacts
// ---------------------------------------------------------------------------

// handleUpload streams one upload into the store.
//
// The body is the content and nothing else — no multipart, no base64, no JSON
// envelope — because every one of those alternatives puts a framing layer
// between the socket and the disk, and this endpoint's entire job is to move
// several hundred megabytes without ever holding them. The metadata rides in
// the query string:
//
//	kind    one of apk, file, script, bundle (farm.artifacts.kind's CHECK)
//	name    the human-facing filename
//	sha256  optional, the digest the client believes it is sending
//
// The digest is computed here regardless — content addressing is the point of
// the store — so sha256 is not how the artifact is named, it is how the client
// proves the transfer was faithful.
func (a *ArtifactAPI) handleUpload(w http.ResponseWriter, r *http.Request) {
	kind := artifacts.Kind(strings.ToLower(queryString(r, "kind")))
	if !kind.Valid() {
		badRequest(w, "kind must be one of apk, file, script, bundle",
			map[string]any{"permitted_kinds": []string{
				string(artifacts.KindAPK), string(artifacts.KindFile),
				string(artifacts.KindScript), string(artifacts.KindBundle)}})
		return
	}
	name := queryString(r, "name")
	if name == "" {
		badRequest(w, "name is required: an artifact with no name cannot be found again by a human", nil)
		return
	}
	if len(name) > maxArtifactNameLen {
		badRequest(w, fmt.Sprintf("name must be at most %d bytes, got %d", maxArtifactNameLen, len(name)),
			map[string]any{"max_name_bytes": maxArtifactNameLen, "got_bytes": len(name)})
		return
	}

	declared := strings.ToLower(queryString(r, "sha256"))
	if declared != "" && !artifacts.ValidSHA256(declared) {
		badRequest(w, "sha256 must be 64 lowercase hex characters", nil)
		return
	}

	// Rejected on the announced length before a single byte is read, so an
	// oversized upload costs one round trip instead of however long it takes
	// to stream a refusal's worth of content to disk. A client using
	// Expect: 100-continue never sends the body at all.
	if r.ContentLength > a.maxUpload {
		a.tooLarge(w, r.ContentLength)
		return
	}

	var src io.Reader = http.MaxBytesReader(w, r.Body, a.maxUpload)
	if declared != "" {
		src = newDeclaredDigest(src, declared)
	}

	res, err := a.store.Put(r.Context(), src, kind, name,
		artifacts.WithUploadedBy(actor(r.Context())))
	if err != nil {
		a.fail(w, r, "upload artifact", err)
		return
	}

	view := artifactViewOf(res.Artifact)
	body := map[string]any{
		"artifact": view,
		// The content was already in the store and this upload only refreshed
		// its metadata. Worth saying: a client that uploads the same build
		// from sixty CI shards should be able to see that fifty-nine of them
		// were free.
		"deduplicated": !res.Inserted,
	}
	if res.ManifestErr != nil {
		// The artifact IS stored and IS pushable; only package and
		// version_code are missing. Reporting this as a failure would refuse
		// every repackaged or obfuscated build in the fleet.
		body["manifest_error"] = res.ManifestErr.Error()
	}

	w.Header().Set("Location", "/api/v1/artifacts/"+res.SHA256)
	writeJSON(w, http.StatusCreated, body)
}

func (a *ArtifactAPI) tooLarge(w http.ResponseWriter, got int64) {
	detail := map[string]any{"limit_bytes": a.maxUpload}
	if got > 0 {
		detail["declared_bytes"] = got
	}
	writeError(w, http.StatusRequestEntityTooLarge, CodeBadRequest,
		fmt.Sprintf("an upload may be at most %d bytes on this instance", a.maxUpload), detail)
}

// ---------------------------------------------------------------------------
// Declared-digest verification
// ---------------------------------------------------------------------------

// digestMismatchError reports an upload that did not match its own promise.
type digestMismatchError struct {
	Declared string
	Received string
	Bytes    int64
}

func (e *digestMismatchError) Error() string {
	return fmt.Sprintf("the upload does not match the digest it declared: %s was announced, "+
		"but %d bytes hashing to %s arrived; nothing was stored",
		e.Declared, e.Bytes, e.Received)
}

// declaredDigest verifies an upload against the digest the client announced,
// and does it IN the stream rather than after it.
//
// After is too late. artifacts.Store.Put publishes the bytes and writes
// farm.artifacts before it returns, so a check on the digest it hands back
// would have to delete a row and orphan a blob it had just created — and a
// crash between the two leaves a row nobody asked for. Failing the read
// instead makes Put's own error path do the work: the staged bytes are
// aborted, no row is written, and a corrupted transfer leaves the farm exactly
// as it was.
type declaredDigest struct {
	src      io.Reader
	sum      hash.Hash
	declared string
	read     int64
}

func newDeclaredDigest(src io.Reader, declared string) *declaredDigest {
	return &declaredDigest{src: src, sum: sha256.New(), declared: declared}
}

// Read hashes what passes through and substitutes a mismatch error for the
// stream's EOF.
//
// Only a clean EOF triggers the comparison. A truncated upload surfaces as its
// own transport error, and reporting that as a digest mismatch would tell a
// client its file was wrong when its network was.
func (d *declaredDigest) Read(p []byte) (int, error) {
	n, err := d.src.Read(p)
	if n > 0 {
		// hash.Hash.Write is documented never to return an error.
		d.sum.Write(p[:n])
		d.read += int64(n)
	}
	if errors.Is(err, io.EOF) {
		if got := hex.EncodeToString(d.sum.Sum(nil)); got != d.declared {
			return n, &digestMismatchError{Declared: d.declared, Received: got, Bytes: d.read}
		}
	}
	return n, err
}

// ---------------------------------------------------------------------------
// GET /api/v1/artifacts
// ---------------------------------------------------------------------------

// handleList serves GET /api/v1/artifacts?kind=&package=&limit=.
func (a *ArtifactAPI) handleList(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 200, 1, 2000)

	var (
		conds []string
		args  []any
	)
	if kind := strings.ToLower(queryString(r, "kind")); kind != "" {
		if !artifacts.Kind(kind).Valid() {
			badRequest(w, "kind must be one of apk, file, script, bundle", nil)
			return
		}
		args = append(args, kind)
		conds = append(conds, fmt.Sprintf("a.kind = $%d", len(args)))
	}
	if pkg := queryString(r, "package"); pkg != "" {
		args = append(args, pkg)
		conds = append(conds, fmt.Sprintf("a.package = $%d", len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)

	// The page is chosen first and the ledger is counted once against it.
	//
	// The obvious spelling — a correlated subquery beside the projection —
	// re-scans farm.device_artifacts once per artifact returned, because the
	// ledger's primary key is (device_id, sha256) and there is no index on
	// sha256 alone. Measured against 3 000 artifacts and 36 000 ledger rows
	// that is 3.7 SECONDS for limit=2000, against 10 ms for this form. This is
	// a tenant-role route, so the difference is one client's dashboard refresh
	// holding a pool connection for seconds at a time — and that pool is the
	// one a holder borrows from to renew. A lease is never ended by a slow
	// query, but a renewal that cannot get a connection is how one would be.
	query := fmt.Sprintf(`
WITH page AS (
  SELECT %s
    FROM farm.artifacts a
    %s
   ORDER BY a.created_at DESC
   LIMIT $%d)
SELECT p.sha256, p.kind, p.name, p.size_bytes, p.package, p.version_code,
       p.url, p.uploaded_by, p.created_at,
       COALESCE(n.devices_present, 0)
  FROM page p
  LEFT JOIN (SELECT d.sha256, count(*) AS devices_present
               FROM farm.device_artifacts d
              WHERE d.state = 'present'
                AND d.sha256 IN (SELECT sha256 FROM page)
              GROUP BY d.sha256) n ON n.sha256 = p.sha256
 ORDER BY p.created_at DESC`, artifactColumns, where, len(args))

	rows, err := a.srv.pool.Query(r.Context(), query, args...)
	if err != nil {
		a.fail(w, r, "list artifacts", err)
		return
	}
	defer rows.Close()

	out := make([]artifactView, 0, 64)
	counts := map[string]int{}
	var totalBytes int64
	for rows.Next() {
		var present int64
		v, err := scanArtifact(rows, &present)
		if err != nil {
			a.fail(w, r, "scan artifact", err)
			return
		}
		v.DevicesPresent = &present
		counts[v.Kind]++
		totalBytes += v.SizeBytes
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "read artifacts", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"artifacts":   out,
		"counts":      counts,
		"total_bytes": totalBytes,
		"truncated":   len(out) == limit,
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/artifacts/{sha}
// ---------------------------------------------------------------------------

// handleGet serves the metadata for one digest, together with everything that
// references it.
//
// The references are here and not only behind DELETE because the dashboard has
// to be able to explain a disabled button before somebody presses it. deletable
// is that answer in one field.
func (a *ArtifactAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	sha, ok := artifactSHA(w, r)
	if !ok {
		return
	}

	art, err := a.store.Lookup(r.Context(), sha)
	if err != nil {
		a.fail(w, r, "get artifact", err)
		return
	}

	// A tenant-scoped caller is shown only its own jobs. Whether another
	// tenant's job installs this build is not this caller's business, even
	// though the content itself is shared.
	scanCtx, cancelScan := context.WithTimeout(r.Context(), referenceScanTimeout)
	devices, deviceCount, jobs, jobCount, err := a.references(
		scanCtx, a.srv.pool, sha, tenantScope(r.Context()))
	cancelScan()
	if err != nil {
		a.fail(w, r, "get artifact references", err)
		return
	}

	states := map[string]int{}
	for _, d := range devices {
		states[d.State]++
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"artifact": artifactViewOf(art),
		"devices":  devices,
		// The counts are the totals, not len() of the lists beside them: both
		// lists are capped, and a page that said "2 devices" while DELETE
		// refuses over 900 would teach an operator to distrust the button.
		"device_count":  deviceCount,
		"device_states": states,
		"jobs":          jobs,
		"job_count":     jobCount,
		"deletable":     deviceCount == 0 && jobCount == 0,
	})
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/artifacts/{sha}
// ---------------------------------------------------------------------------

// handleDelete removes the farm's knowledge of one digest.
//
// It refuses whenever anything still names the content, and the refusal is the
// point of the endpoint. farm.device_artifacts.sha256 is ON DELETE CASCADE, so
// an unguarded delete would take the ledger rows with it — the fleet would
// forget that sixty phones already hold this build and re-push it to all of
// them — and a job whose spec names the digest would fail at provisioning with
// a foreign key error instead of an explanation. A terminal job's reference
// blocks too: deleting content a finished run installed destroys the only
// record of what it actually ran.
//
// The bytes themselves stay in the backend. artifacts.Backend has no verb that
// removes them, and it does not want one: content addressing means the blob is
// inert once nothing names it, a later upload of identical content adopts it,
// and reclaiming disk is a sweep over the backend rather than a side effect of
// an HTTP request.
//
// An optional ?reason= is recorded in farm.audit_log beside the caller's name.
func (a *ArtifactAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	sha, ok := artifactSHA(w, r)
	if !ok {
		return
	}
	who := actor(r.Context())

	tx, err := a.srv.pool.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "delete artifact: begin", err)
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(r.Context())) }()

	// FOR UPDATE conflicts with the FOR KEY SHARE that inserting a
	// farm.device_artifacts row takes on its parent, so no device can start
	// holding this artifact between the check below and the delete. A job
	// created in that same instant is not preventable — farm.jobs.spec is
	// jsonb with no key to lock — and fails loudly at provisioning, which is
	// the right end of that race to lose.
	var kind, name string
	err = tx.QueryRow(r.Context(),
		`SELECT kind, name FROM farm.artifacts WHERE sha256 = $1 FOR UPDATE`, sha).Scan(&kind, &name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such artifact",
				map[string]string{"sha256": sha})
			return
		}
		a.fail(w, r, "delete artifact: lock row", err)
		return
	}

	scanCtx, cancelScan := context.WithTimeout(r.Context(), referenceScanTimeout)
	devices, deviceCount, jobs, jobCount, err := a.references(scanCtx, tx, sha, "")
	cancelScan()
	if err != nil {
		// Includes the scan's own deadline. The delete does not happen, which
		// is the only safe direction for a guard that cannot see its evidence.
		a.fail(w, r, "delete artifact: references", err)
		return
	}
	if deviceCount > 0 || jobCount > 0 {
		a.srv.metrics.operatorActions.WithLabelValues("artifact.delete", "refused").Inc()
		writeError(w, http.StatusConflict, CodeConflict,
			fmt.Sprintf("this artifact is still referenced by %d device ledger row(s) and %d job spec(s); "+
				"retract it from those devices and let those jobs age out before deleting the content they name",
				deviceCount, jobCount),
			map[string]any{
				"sha256":       sha,
				"devices":      devices,
				"device_count": deviceCount,
				"jobs":         jobs,
				"job_count":    jobCount,
			})
		return
	}

	tag, err := tx.Exec(r.Context(), `DELETE FROM farm.artifacts WHERE sha256 = $1`, sha)
	if err != nil {
		a.fail(w, r, "delete artifact", err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such artifact",
			map[string]string{"sha256": sha})
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.fail(w, r, "delete artifact: commit", err)
		return
	}

	// The row is gone, so the audit entry naming who removed it must not be
	// lost because the caller hung up in the gap.
	auditCtx, cancel := detachedCtx(r.Context())
	a.srv.auditAction(auditCtx, who, "artifact.delete", "artifact:"+sha, queryString(r, "reason"),
		map[string]any{"sha256": sha, "kind": kind, "name": name})
	cancel()
	a.srv.metrics.operatorActions.WithLabelValues("artifact.delete", "ok").Inc()

	writeJSON(w, http.StatusOK, map[string]any{
		"sha256":  sha,
		"deleted": true,
		// Said plainly rather than implied, so nobody deletes a hundred
		// artifacts expecting the disk to empty.
		"blob_retained": true,
		"note": "the row was removed; the content-addressed bytes remain in the blob backend " +
			"and are adopted again by any later upload of identical content",
	})
}

// references collects everything that still names sha, and how much of it
// there is.
//
// Both lists are capped and both counts are exact. The window count is the
// same on every row and is the number of rows that MATCHED, not the number
// returned: a refusal that says "3 jobs" while listing 20, or "500 devices"
// when there are 900, would be a lie in the direction that gets an artifact
// deleted. Counts, never len(), decide the refusal in handleDelete.
//
// The job scan is a sequential scan of farm.jobs against a jsonb path. It runs
// only on an operator-initiated delete and on a single artifact's detail page,
// and both callers give it referenceScanTimeout so it cannot hold a pool
// connection indefinitely. The path matches any payload member carrying a
// sha256 rather than push and install by name, so a step kind added to the
// vocabulary later cannot make this check quietly stop protecting the content
// it references — verified against push, install, nested payloads, and every
// malformed spec shape farm.jobs can hold, none of which make it error.
func (a *ArtifactAPI) references(ctx context.Context, q rowQuerier, sha, tenant string) (
	[]artifactDeviceRow, int, []artifactJobRef, int, error) {

	const deviceQuery = `
SELECT d.device_id::text, dev.farm_uid, d.state, d.installed_at,
       d.detail->>'remote_path', d.detail->>'error', d.detail->>'removed_reason',
       count(*) OVER ()
  FROM farm.device_artifacts d
  JOIN farm.devices dev ON dev.id = d.device_id
 WHERE d.sha256 = $1
 ORDER BY d.installed_at DESC NULLS LAST, dev.farm_uid
 LIMIT $2`

	rows, err := q.Query(ctx, deviceQuery, sha, deviceReferenceLimit)
	if err != nil {
		return nil, 0, nil, 0, err
	}
	defer rows.Close()

	devices := make([]artifactDeviceRow, 0, 8)
	deviceTotal := 0
	for rows.Next() {
		var (
			d                       artifactDeviceRow
			remote, failure, reason *string
			count                   int64
		)
		if err := rows.Scan(&d.DeviceID, &d.FarmUID, &d.State, &d.InstalledAt,
			&remote, &failure, &reason, &count); err != nil {
			return nil, 0, nil, 0, err
		}
		d.RemotePath = derefString(remote)
		d.Error = derefString(failure)
		d.RemovedReason = derefString(reason)
		deviceTotal = int(count)
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, nil, 0, err
	}
	// Closed before the next query rather than only by the defer: when q is a
	// pgx.Tx both queries run on one connection, and a second query with rows
	// still open on it fails.
	rows.Close()

	jobQuery := `
SELECT j.id::text, j.state, j.tenant_id, count(*) OVER ()
  FROM farm.jobs j
 WHERE jsonb_path_exists(j.spec, '$.steps[*].*.sha256 ? (@ == $sha)',
                         jsonb_build_object('sha', $1::text))`
	args := []any{sha}
	if tenant != "" {
		args = append(args, tenant)
		jobQuery += fmt.Sprintf(" AND j.tenant_id = $%d", len(args))
	}
	args = append(args, referenceListLimit)
	jobQuery += fmt.Sprintf(" ORDER BY j.created_at DESC LIMIT $%d", len(args))

	jobRows, err := q.Query(ctx, jobQuery, args...)
	if err != nil {
		return nil, 0, nil, 0, err
	}
	defer jobRows.Close()

	jobs := make([]artifactJobRef, 0, 8)
	jobTotal := 0
	for jobRows.Next() {
		var (
			j     artifactJobRef
			count int64
		)
		if err := jobRows.Scan(&j.JobID, &j.State, &j.TenantID, &count); err != nil {
			return nil, 0, nil, 0, err
		}
		jobTotal = int(count)
		jobs = append(jobs, j)
	}
	if err := jobRows.Err(); err != nil {
		return nil, 0, nil, 0, err
	}
	return devices, deviceTotal, jobs, jobTotal, nil
}

// ---------------------------------------------------------------------------
// GET /api/v1/devices/{id}/artifacts
// ---------------------------------------------------------------------------

// handleDeviceArtifacts answers "what is on this device", from the ledger.
//
// The ledger, not the device: this is what farm.device_artifacts believes, and
// it is what EnsureOnDevice consults before deciding to skip a push. A row
// saying 'present' that the phone has since had wiped is exactly the drift an
// operator opens this page to find, so the answer is never softened by probing
// the device — that would put a socket between an operator and a table.
func (a *ArtifactAPI) handleDeviceArtifacts(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		badRequest(w, "device id is required", nil)
		return
	}

	// Resolved through the fleet view so a device may be addressed by uuid or
	// by farm_uid, exactly as GET /api/v1/devices/{id} allows. Never by ADB
	// serial: duplicate OEM serials are real, and an artifact list for the
	// wrong phone is an operator pushing a build onto somebody's live run.
	dev, err := a.srv.lookupDevice(r.Context(), id)
	if err != nil {
		a.fail(w, r, "get device artifacts", err)
		return
	}

	limit := queryInt(r, "limit", defaultDeviceArtifactLimit, 1, maxDeviceArtifactLimit)

	args := []any{dev.DeviceID}
	stateFilter := ""
	switch state := strings.ToLower(queryString(r, "state")); state {
	case "", "all":
	case "pending", "present", "failed", "removed":
		args = append(args, state)
		stateFilter = fmt.Sprintf(" AND d.state = $%d", len(args))
	default:
		badRequest(w, "state must be one of all, pending, present, failed, removed", nil)
		return
	}
	args = append(args, limit)

	// A ledger row is never deleted when an artifact is retracted — it moves
	// to 'removed' — so this table grows for the life of the phone and the
	// answer needs a page even though no client chose its size.
	query := fmt.Sprintf(`
SELECT %s, d.state, d.installed_at,
       d.detail->>'remote_path', d.detail->>'error', d.detail->>'removed_reason'
  FROM farm.device_artifacts d
  JOIN farm.artifacts a ON a.sha256 = d.sha256
 WHERE d.device_id = $1::uuid%s
 ORDER BY d.installed_at DESC NULLS LAST, a.name
 LIMIT $%d`, artifactColumns, stateFilter, len(args))

	rows, err := a.srv.pool.Query(r.Context(), query, args...)
	if err != nil {
		a.fail(w, r, "list device artifacts", err)
		return
	}
	defer rows.Close()

	out := make([]deviceArtifactView, 0, 16)
	counts := map[string]int{}
	var bytesPresent int64
	for rows.Next() {
		var (
			v                       deviceArtifactView
			remote, failure, reason *string
		)
		art, err := scanArtifact(rows, &v.State, &v.InstalledAt, &remote, &failure, &reason)
		if err != nil {
			a.fail(w, r, "scan device artifact", err)
			return
		}
		v.artifactView = art
		v.RemotePath = derefString(remote)
		v.Error = derefString(failure)
		v.RemovedReason = derefString(reason)
		counts[v.State]++
		if v.State == "present" {
			bytesPresent += v.SizeBytes
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		a.fail(w, r, "read device artifacts", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": dev.DeviceID,
		"farm_uid":  dev.FarmUID,
		"artifacts": out,
		"counts":    counts,
		// Of the rows returned, not of the device: with truncated true the
		// figure is a floor, and saying so beats implying a total.
		"bytes_present": bytesPresent,
		"truncated":     len(out) == limit,
	})
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// artifactSHA reads and checks the {sha} path variable.
//
// The check is a boundary, not a nicety: the digest becomes a filesystem path
// in artifacts.DirBackend, and one that had not been through ValidSHA256 could
// carry separators or "..".
func artifactSHA(w http.ResponseWriter, r *http.Request) (string, bool) {
	sha := strings.ToLower(strings.TrimSpace(r.PathValue("sha")))
	if !artifacts.ValidSHA256(sha) {
		badRequest(w, "the artifact id must be a sha256 digest: 64 lowercase hex characters", nil)
		return "", false
	}
	return sha, true
}

// fail maps the errors internal/artifacts defines onto the envelope and hands
// everything else to Server.fail, so an artifact route answers with the same
// shape as every other route in this package.
func (a *ArtifactAPI) fail(w http.ResponseWriter, r *http.Request, op string, err error) {
	var (
		mismatch *digestMismatchError
		maxBytes *http.MaxBytesError
	)
	switch {
	case errors.As(err, &mismatch):
		a.srv.log.InfoContext(r.Context(), "upload rejected: declared digest did not match",
			"op", op, "declared", mismatch.Declared, "received", mismatch.Received,
			"bytes", mismatch.Bytes)
		writeError(w, http.StatusBadRequest, CodeDigestMismatch, mismatch.Error(),
			map[string]any{
				"declared_sha256": mismatch.Declared,
				"received_sha256": mismatch.Received,
				"received_bytes":  mismatch.Bytes,
			})

	case errors.As(err, &maxBytes), errors.Is(err, artifacts.ErrTooLarge):
		a.srv.log.InfoContext(r.Context(), "upload rejected: too large", "op", op, "err", err)
		a.tooLarge(w, r.ContentLength)

	case errors.Is(err, artifacts.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound,
			"no such artifact; upload the content before naming its digest", nil)

	case errors.Is(err, artifacts.ErrUnknownDevice):
		writeError(w, http.StatusNotFound, CodeNotFound,
			"no such device; enrol it before provisioning artifacts to it", nil)

	case r.Context().Err() != nil:
		// The caller hung up mid-upload. The staged bytes were discarded and
		// no row was written, so this is not a server failure and must not be
		// counted as one — a CI runner cancelling a build would otherwise show
		// up in the 5xx rate that pages a human.
		a.srv.fail(w, r, op, r.Context().Err())

	default:
		a.srv.fail(w, r, op, err)
	}
}
