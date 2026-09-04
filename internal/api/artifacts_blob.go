package api

// The bytes half of the artifact API, and the sweep that reclaims them.
//
// # Why a content route exists at all
//
// ArtifactAPI mounts five routes and every one of them is metadata. A farm can
// therefore accept a 200 MB APK, hash it, file it, push it to sixty phones and
// never hand it back. That matters more here than in an ordinary object store
// because the blob directory is the ONLY copy: the upload was streamed
// straight through to the backend, nothing else retains it, and
// farm.artifacts.url is a file:// path on a machine the client cannot reach.
// Content-addressed storage that cannot be read from is a write-only disk.
//
// # Why a sweep exists at all
//
// DELETE removes the farm.artifacts row and says so plainly in its response —
// "blob_retained": true. That is the correct behaviour for the request (a
// later upload of identical content adopts the bytes again), but it means the
// backend only ever grows. Nothing in artifacts.Backend removes anything, and
// nothing should: reclaiming disk is a deliberate, auditable maintenance
// action over the whole store, not a side effect of one HTTP request that
// might be racing an upload of the same digest.

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/artifacts"
)

// CodeBlobMissing accompanies 410 Gone from the content route: farm.artifacts
// has the row, the backend does not have the bytes. It is not 404, because
// 404 would contradict the metadata route that is still answering 200 for the
// same digest, and it is not 500, because the server is working correctly and
// telling the truth about a store that has lost content. The fix is to
// re-upload the same bytes, which the digest names exactly.
const CodeBlobMissing = "blob_missing"

// CodeBlobCorrupt accompanies 500 when the stored length disagrees with
// farm.artifacts.size_bytes. Content addressed by sha256 that serves different
// bytes is worse than serving nothing: it launders corruption into a device,
// into a test result, and into the ledger that says the device already has
// this build. The store refuses to hand the reader over and so does this.
const CodeBlobCorrupt = "blob_corrupt"

// ---------------------------------------------------------------------------
// GET /api/v1/artifacts/{sha}/content
// ---------------------------------------------------------------------------

// blobSource resolves a digest to its bytes and its row. artifacts.Store.Get
// is the only implementation that ships; the parameter exists so the streaming
// half of this endpoint — ranges, the etag, the byte-for-byte fidelity that
// content addressing rests on — is exercisable without a database.
type blobSource func(ctx context.Context, sha string) (artifacts.Blob, artifacts.Artifact, error)

// failFunc is the error-mapping half, for the same reason.
type failFunc func(w http.ResponseWriter, r *http.Request, op string, err error)

// blobCacheControl is what a content-addressed URL may honestly promise: the
// bytes under a sha256 cannot change, so they never need revalidating.
//
// private, not public: the route requires a credential, and a shared proxy
// caching an authenticated response is how one tenant's build reaches another
// tenant's client.
const blobCacheControl = "private, max-age=31536000, immutable"

// contentHandler streams an artifact's bytes.
func contentHandler(open blobSource, fail failFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sha, ok := artifactSHA(w, r)
		if !ok {
			return
		}

		blob, art, err := open(r.Context(), sha)
		if err != nil {
			fail(w, r, "download artifact", err)
			return
		}
		defer blob.Close()

		serveBlob(w, r, art, blob)
	}
}

// serveBlob writes the response for one artifact.
//
// Range support is real, not aspirational, and the check is in the type system
// rather than in a runtime probe: artifacts.Blob embeds io.ReaderAt, so EVERY
// backend already provides random access — the APK parser depends on it,
// because a zip's central directory lives at the end of the file. An S3 backend
// satisfying the same interface serves ranges through ranged GETs. So the blob
// is wrapped in an io.SectionReader, which turns that guaranteed ReaderAt into
// the ReadSeeker http.ServeContent wants, and ServeContent then does the parts
// that are tedious to get right by hand: Range and multi-range parsing,
// If-Range, If-None-Match against the etag set below, 206 with Content-Range,
// 416 for an unsatisfiable range, Content-Length on every path, and a bodyless
// HEAD.
//
// Ranges matter here for the same reason the store is content-addressed: a
// resumable download of a 200 MB APK over a flaky link costs the remainder,
// not the whole thing again.
func serveBlob(w http.ResponseWriter, r *http.Request, art artifacts.Artifact, blob artifacts.Blob) {
	h := w.Header()

	// Set before ServeContent, which sniffs the body only when Content-Type is
	// absent. Sniffing uploaded content on the control plane's own origin —
	// the same origin the dashboard is served from — is how a stored HTML file
	// becomes stored XSS against an operator's session, which is also why the
	// disposition is attachment and nosniff is set unconditionally.
	h.Set("Content-Type", blobContentType(art.Kind))
	h.Set("X-Content-Type-Options", "nosniff")
	if cd := blobDisposition(art.Name); cd != "" {
		h.Set("Content-Disposition", cd)
	}

	// The digest IS the etag. This is the one endpoint in the API where a
	// strong validator is free and exact: two responses share an etag if and
	// only if they are the same bytes, by construction. It is quoted because
	// an unquoted etag is not an etag, and ServeContent will not match a
	// malformed one against If-None-Match or If-Range.
	h.Set("ETag", `"`+art.SHA256+`"`)
	h.Set("Cache-Control", blobCacheControl)
	if d := reprDigest(art.SHA256); d != "" {
		// RFC 9530: the same fact as the etag, in the form a client that wants
		// to verify the transfer can consume without parsing an opaque
		// validator. It describes the full representation, so it is correct on
		// a 206 too.
		h.Set("Repr-Digest", d)
	}

	// Empty name on purpose: the content type is already decided above, and
	// letting ServeContent re-derive one from a client-supplied filename would
	// undo that. art.CreatedAt gives If-Modified-Since something true to
	// compare against, though the etag is the validator that matters.
	http.ServeContent(w, r, "", art.CreatedAt, io.NewSectionReader(blob, 0, art.Size))
}

// blobContentType maps a stored kind onto a media type.
//
// Only the APK gets a specific type, because only the APK has one that a
// client acts on. Everything else is octet-stream rather than a guess derived
// from the uploaded name: the name is untrusted text chosen by whoever
// uploaded, and a media type derived from it is a media type the uploader
// chose for a response the control plane serves from its own origin.
func blobContentType(kind artifacts.Kind) string {
	if kind == artifacts.KindAPK {
		return "application/vnd.android.package-archive"
	}
	return "application/octet-stream"
}

// blobDisposition renders Content-Disposition for the stored name, which
// reached farm.artifacts.name from a query string and is therefore untrusted.
//
// mime.FormatMediaType does the quoting and the RFC 2231 encoding a non-ASCII
// filename needs, and it percent-encodes a control character rather than
// emitting it — so a CRLF in the name is not a response-splitting primitive
// even before this function touches it, and net/http would flatten one anyway
// on the way out. The two transformations below are the parts FormatMediaType
// does NOT do, and they are the parts a client acts on:
//
// Control characters are dropped so the name a browser offers is a name and
// not "%0D%0AX-Evil%3A%201" — and so a name made ONLY of them yields no
// filename at all rather than one composed of escaped NULs.
//
// Path separators become underscores, which is the load-bearing one:
// FormatMediaType is perfectly happy to render filename="../../etc/passwd",
// and there are clients that will happily write it.
func blobDisposition(name string) string {
	clean := strings.Map(func(rn rune) rune {
		switch {
		case rn < 0x20, rn == 0x7f:
			return -1
		case rn == '/', rn == '\\':
			// A path separator in a download filename is a directory traversal
			// waiting for a client that trusts it.
			return '_'
		default:
			return rn
		}
	}, strings.TrimSpace(name))

	if clean == "" {
		return "attachment"
	}
	if cd := mime.FormatMediaType("attachment", map[string]string{"filename": clean}); cd != "" {
		return cd
	}
	return "attachment"
}

// reprDigest renders a hex sha256 as an RFC 9530 Repr-Digest value. A digest
// that will not decode yields no header rather than a wrong one.
func reprDigest(sha string) string {
	raw, err := hex.DecodeString(sha)
	if err != nil {
		return ""
	}
	return "sha-256=:" + base64.StdEncoding.EncodeToString(raw) + ":"
}

// failBlob maps the two failures the content route can produce that
// ArtifactAPI.fail does not name, and hands everything else to it.
//
// Both are logged: a missing blob and a corrupt blob are facts about this
// deployment's disk, and the client's copy of the message is not where an
// operator will find them.
func (a *ArtifactAPI) failBlob(w http.ResponseWriter, r *http.Request, op string, err error) {
	var corrupt *artifacts.CorruptError
	switch {
	case errors.Is(err, artifacts.ErrBlobMissing):
		a.srv.log.ErrorContext(r.Context(), "artifact bytes are absent from the blob backend",
			"op", op, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusGone, CodeBlobMissing,
			"the farm has a record of this artifact but the backend no longer holds its bytes; "+
				"re-upload the same content, or restore the blob directory from backup",
			// Normalised exactly as artifactSHA normalised it before the
			// lookup, so the digest named here is the digest that was actually
			// looked up. An operator re-uploading against a value the server
			// echoed back in a different case would file the bytes under a
			// name the row does not use.
			map[string]string{"sha256": strings.ToLower(strings.TrimSpace(r.PathValue("sha")))})

	case errors.As(err, &corrupt):
		a.srv.log.ErrorContext(r.Context(), "artifact bytes do not match the digest they are filed under",
			"op", op, "sha256", corrupt.SHA, "want_size", corrupt.WantSize, "got_size", corrupt.GotSize,
			"got_sha256", corrupt.Got)
		writeError(w, http.StatusInternalServerError, CodeBlobCorrupt, corrupt.Error(),
			map[string]any{
				"sha256":        corrupt.SHA,
				"expected_size": corrupt.WantSize,
				"stored_size":   corrupt.GotSize,
			})

	default:
		a.fail(w, r, op, err)
	}
}

// RegisterBlobRoutes mounts the content route.
//
// Separate from Register because it is a separate decision: a deployment that
// wants metadata-only artifact endpoints keeps the five routes and skips this
// one. Tenant, not operator — reading content the caller could already push to
// a device it holds is not a privileged act.
func (a *ArtifactAPI) RegisterBlobRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/artifacts/{sha}/content",
		a.srv.requireRole(RoleTenant, contentHandler(a.store.Get, a.failBlob)))
}

// ---------------------------------------------------------------------------
// Blob garbage collection
// ---------------------------------------------------------------------------

// DefaultBlobGCGrace is how old a blob must be before a sweep will consider it.
//
// The window this closes is inside artifacts.Store.Put, which commits the
// bytes and THEN writes the farm.artifacts row — deliberately, because an
// orphaned blob is harmless garbage whereas a row pointing at absent bytes is
// a promise the farm cannot keep. Between those two statements the blob is on
// disk with nothing naming it, which is exactly the shape this sweep hunts.
// For an APK that window also contains a full re-read of the file to parse its
// manifest. An hour is far past any of that and costs only a delayed
// reclamation.
const DefaultBlobGCGrace = time.Hour

// MinBlobGCGrace is the floor a caller may configure. A sweep with a grace of
// seconds is a race against every in-flight upload, so it is refused at
// construction rather than discovered as a missing artifact.
const MinBlobGCGrace = time.Minute

// blobGCListLimit bounds how many blobs a report enumerates. The counts beside
// the list are exact regardless — a dry run that said "12 blobs" while the
// apply removed nine hundred would be a lie in the direction that costs disk
// nobody expected to lose.
const blobGCListLimit = 500

// blobGCBatch is how many digests one reference query carries. Bounded so a
// store with a million blobs does not build a single array parameter that
// large.
const blobGCBatch = 1000

// blobRoot is the capability a sweep needs and artifacts.Backend does not
// offer. The Backend seam is Create/Open/URL on purpose — every extra verb is
// something an object store has to emulate — so enumeration is asked for
// separately, and a backend that cannot enumerate is refused with an
// explanation rather than swept incorrectly.
type blobRoot interface {
	Root() string
}

// BlobGC reclaims blobs that nothing names.
//
// It is not wired into any request path by default. Deleting bytes is a
// maintenance action with a blast radius the size of the disk, and the safe
// default for such a thing is that somebody asks for it.
type BlobGC struct {
	srv   *Server
	root  string
	grace time.Duration

	// refs answers "does anything still name these digests". It is
	// namedDigests, reading Postgres, in every deployment.
	//
	// The seam exists for the same reason blobSource does above: the parts of
	// this sweep that decide whether bytes live — the second look, its failure,
	// and the race it is there to catch — are the parts nobody can afford to
	// have wrong, and none of them can be provoked on demand against a real
	// database. A nil value means "read Postgres", so a zero BlobGC still does
	// the real thing.
	refs digestRefs

	// running serialises sweeps. Two concurrent sweeps would each see the
	// other's candidates, race on the same unlink, and report freed bytes
	// twice.
	running sync.Mutex
}

// digestRefs reports which of shas something in the farm still names.
type digestRefs func(ctx context.Context, shas []string) (map[string]bool, error)

// BlobGCOption configures a BlobGC.
type BlobGCOption func(*BlobGC)

// WithBlobGCGrace sets how old a blob must be to be collectable.
func WithBlobGCGrace(d time.Duration) BlobGCOption {
	return func(g *BlobGC) { g.grace = d }
}

// NewBlobGC binds a sweep to a server and a blob backend.
func NewBlobGC(srv *Server, backend artifacts.Backend, opts ...BlobGCOption) (*BlobGC, error) {
	if srv == nil {
		return nil, errors.New("api: nil server")
	}
	if backend == nil {
		return nil, errors.New("api: nil blob backend")
	}
	rooted, ok := backend.(blobRoot)
	if !ok {
		return nil, fmt.Errorf("api: %T cannot be swept: artifacts.Backend is Create/Open/URL only, "+
			"so reclaiming disk needs a backend that can also enumerate what it holds "+
			"(artifacts.DirBackend does, through Root)", backend)
	}
	root := strings.TrimSpace(rooted.Root())
	if root == "" {
		return nil, errors.New("api: blob backend reports an empty root directory")
	}

	g := &BlobGC{srv: srv, root: root, grace: DefaultBlobGCGrace}
	for _, o := range opts {
		o(g)
	}
	if g.grace < MinBlobGCGrace {
		return nil, fmt.Errorf("api: a blob GC grace period of %s races artifacts.Store.Put, "+
			"which commits bytes before it writes the row that names them; the minimum is %s",
			g.grace, MinBlobGCGrace)
	}
	return g, nil
}

// BlobGCEntry is one blob a sweep considered collectable.
type BlobGCEntry struct {
	SHA256     string    `json:"sha256"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
	Deleted    bool      `json:"deleted"`
}

// BlobGCReport is what a sweep did, or — on a dry run — what it would have
// done.
type BlobGCReport struct {
	DryRun       bool      `json:"dry_run"`
	Root         string    `json:"root"`
	GraceSeconds int64     `json:"grace_seconds"`
	StartedAt    time.Time `json:"started_at"`
	DurationMS   int64     `json:"duration_ms"`

	// Scanned counts the files in the backend that are named like a digest.
	Scanned      int   `json:"scanned"`
	ScannedBytes int64 `json:"scanned_bytes"`
	// Unrecognised counts entries the sweep is not willing to reason about —
	// the staging directory, a stray file, anything not filed as
	// <root>/<xx>/<sha256>. They are never touched.
	Unrecognised int `json:"unrecognised"`
	// Referenced counts blobs a farm.artifacts row or a farm.device_artifacts
	// row still names.
	Referenced int `json:"referenced"`
	// WithinGrace counts blobs young enough that a row may still be on its way.
	WithinGrace int `json:"within_grace"`

	Collectable      int   `json:"collectable"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	Deleted          int   `json:"deleted"`
	FreedBytes       int64 `json:"freed_bytes"`
	// Adopted counts blobs an upload claimed between the scan and the unlink.
	// Not an error: the second look is what caught it, and the bytes stayed.
	Adopted int `json:"adopted"`

	Blobs     []BlobGCEntry `json:"blobs"`
	Truncated bool          `json:"truncated"`
	// Problems are per-entry failures that did not stop the sweep. A sweep
	// that cannot read one directory still reclaims the rest.
	Problems []string `json:"problems,omitempty"`
}

// DryRun reports what a sweep would free without removing anything.
func (g *BlobGC) DryRun(ctx context.Context) (BlobGCReport, error) {
	return g.sweep(ctx, false)
}

// Collect removes every blob no row names and reports what it freed.
func (g *BlobGC) Collect(ctx context.Context) (BlobGCReport, error) {
	return g.sweep(ctx, true)
}

// ErrBlobGCBusy is returned when a sweep is already running. Sweeps are
// idempotent, so the caller loses nothing by waiting for the one in progress.
var ErrBlobGCBusy = errors.New("api: a blob sweep is already running")

// blobFile is one candidate on disk.
type blobFile struct {
	sha  string
	path string
	size int64
	mod  time.Time
}

// sweep is the whole collector.
//
// The order is load-bearing:
//
//  1. Enumerate the backend. Anything not filed as <root>/<xx>/<sha256> is
//     counted and left completely alone — the staging directory included,
//     because a nameless staged file belongs to an upload that is still
//     running or to a crash, and neither is this sweep's business.
//  2. Drop everything younger than the grace period. This is the fence around
//     Put's commit-then-insert window.
//  3. Ask the database, in batches, which digests are still named — both the
//     artifact table and the per-device ledger, for the reason namedDigests
//     gives.
//  4. Take a SECOND look at each candidate immediately before unlinking it.
//     Step 3's answer ages, and it ages dangerously in one specific case: a
//     re-upload of content that already exists as an orphan does NOT refresh
//     the file's mtime — artifacts.DirBackend's Commit sees the digest already
//     present and discards the staged copy — so an old blob can acquire a row
//     while this sweep is walking. The re-check narrows that window to the gap
//     between one query and one unlink.
//  5. After the unlinks, re-ask about everything that was removed. If a row
//     appeared anyway, the sweep says so loudly and names the digest, because
//     "re-upload this exact content" is an instruction an operator can act on
//     and a silent hole is not.
func (g *BlobGC) sweep(ctx context.Context, apply bool) (BlobGCReport, error) {
	if !g.running.TryLock() {
		return BlobGCReport{}, ErrBlobGCBusy
	}
	defer g.running.Unlock()

	started := time.Now()
	rep := BlobGCReport{
		DryRun:       !apply,
		Root:         g.root,
		GraceSeconds: int64(g.grace / time.Second),
		StartedAt:    started,
	}

	files, unrecognised, problems, err := g.enumerate()
	if err != nil {
		// The root itself could not be read, so this sweep saw NOTHING. That
		// is not a per-entry problem to note beside a report; a report saying
		// "scanned 0, deleted 0" with a 200 beside it is indistinguishable
		// from a clean store, and the operator who runs this from cron would
		// read months of successful sweeps off a backend that had moved.
		return rep, err
	}
	rep.Unrecognised = unrecognised
	rep.Problems = problems
	rep.Scanned = len(files)

	// Server clock against filesystem timestamps written by this same host.
	// No client-supplied time reaches any decision here, and none is sent to
	// the database.
	cutoff := time.Now().Add(-g.grace)
	candidates := make([]blobFile, 0, len(files))
	for _, f := range files {
		rep.ScannedBytes += f.size
		if f.mod.After(cutoff) {
			rep.WithinGrace++
			continue
		}
		candidates = append(candidates, f)
	}

	// Resolved once so a sweep cannot change its mind about where the answer
	// comes from halfway through.
	refs := g.refs
	if refs == nil {
		refs = g.namedDigests
	}

	shas := make([]string, 0, len(candidates))
	for _, f := range candidates {
		shas = append(shas, f.sha)
	}
	named, err := refs(ctx, shas)
	if err != nil {
		return rep, err
	}

	var removed []string
	for _, f := range candidates {
		if named[f.sha] {
			rep.Referenced++
			continue
		}
		if err := ctx.Err(); err != nil {
			// An operator who hung up gets a partial sweep, which is exactly
			// as safe as a whole one: nothing here is transactional and the
			// next run picks up where this stopped.
			rep.Problems = append(rep.Problems, "sweep stopped early: "+err.Error())
			break
		}

		if apply {
			// The second look, taken before anything is counted rather than
			// after, and failing CLOSED in both directions: an error here keeps
			// the bytes AND withholds the collectable count, because a sweep
			// that could not ask the question does not know the answer — and
			// "collectable" is a number an operator reads as "this much is safe
			// to free". The cost of keeping a dead blob is disk the next sweep
			// reclaims; the cost of removing a live one is content the farm
			// promised it still had.
			again, err := refs(ctx, []string{f.sha})
			if err != nil {
				rep.Problems = append(rep.Problems, "recheck "+f.sha+": "+err.Error())
				continue
			}
			if again[f.sha] {
				rep.Adopted++
				continue
			}
		}

		rep.Collectable++
		rep.ReclaimableBytes += f.size
		entry := BlobGCEntry{SHA256: f.sha, SizeBytes: f.size, ModifiedAt: f.mod}

		if apply {
			if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				// Collectable but not collected: nothing names it and it is
				// past grace, so the count stands and the entry stays in the
				// list with Deleted false. That is what lets "collectable 9,
				// deleted 8" reconcile against the problem printed beside it,
				// instead of the blob vanishing from a report that still
				// counted it.
				rep.Problems = append(rep.Problems, "remove "+f.sha+": "+err.Error())
			} else {
				rep.Deleted++
				rep.FreedBytes += f.size
				removed = append(removed, f.sha)
				entry.Deleted = true
			}
		}

		if len(rep.Blobs) < blobGCListLimit {
			rep.Blobs = append(rep.Blobs, entry)
		} else {
			rep.Truncated = true
		}
	}

	if len(removed) > 0 {
		g.reportLostRaces(ctx, refs, removed, &rep)
	}

	rep.DurationMS = time.Since(started).Milliseconds()
	return rep, nil
}

// enumerate walks the two-level fan-out artifacts.DirBackend writes.
//
// It reads exactly <root>/<xx>/<sha256> and nothing else. A directory that is
// not two lowercase hex characters is not descended into at all, which is what
// keeps the staging directory — and any operator's scratch folder that ends up
// beside it — outside the blast radius without this file having to know its
// name.
//
// A failure to read the root is returned as an error and not as a problem: it
// means the sweep enumerated nothing, and every count downstream of it would
// describe a store nobody looked at.
func (g *BlobGC) enumerate() (files []blobFile, unrecognised int, problems []string, err error) {
	tops, err := os.ReadDir(g.root)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("api: blob sweep could not read the blob root %s: %w", g.root, err)
	}

	for _, top := range tops {
		name := top.Name()
		if !top.IsDir() || !isFanoutDir(name) {
			unrecognised++
			continue
		}
		dir := filepath.Join(g.root, name)
		entries, err := os.ReadDir(dir)
		if err != nil {
			problems = append(problems, "read "+dir+": "+err.Error())
			continue
		}
		for _, e := range entries {
			sha := e.Name()
			// The file must be named for its own digest AND live in the
			// directory that digest fans out to. A file that satisfies one but
			// not the other is not something this sweep put there.
			if e.IsDir() || !artifacts.ValidSHA256(sha) || sha[:2] != name {
				unrecognised++
				continue
			}
			info, err := e.Info()
			if err != nil {
				problems = append(problems, "stat "+sha+": "+err.Error())
				continue
			}
			if !info.Mode().IsRegular() {
				unrecognised++
				continue
			}
			files = append(files, blobFile{
				sha:  sha,
				path: filepath.Join(dir, sha),
				size: info.Size(),
				mod:  info.ModTime(),
			})
		}
	}
	return files, unrecognised, problems, nil
}

// isFanoutDir reports whether name is one of the 256 two-hex-character
// directories artifacts.DirBackend fans blobs out into.
func isFanoutDir(name string) bool {
	if len(name) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// namedDigests returns the subset of these digests that something still names.
//
// Both tables are consulted in one statement. farm.device_artifacts.sha256 has
// an ON DELETE CASCADE foreign key onto farm.artifacts, so a ledger row cannot
// outlive its artifact today — the second half of the UNION is there because
// that constraint is the only thing making it true, and a schema change that
// relaxed it would otherwise turn this sweep into the thing that erases the
// record of what sixty phones are carrying.
func (g *BlobGC) namedDigests(ctx context.Context, shas []string) (map[string]bool, error) {
	const q = `
SELECT sha256 FROM farm.artifacts        WHERE sha256 = ANY($1::text[])
UNION
SELECT sha256 FROM farm.device_artifacts WHERE sha256 = ANY($1::text[])`

	named := make(map[string]bool, len(shas))
	for start := 0; start < len(shas); start += blobGCBatch {
		batch := shas[start:min(start+blobGCBatch, len(shas))]

		rows, err := g.srv.pool.Query(ctx, q, batch)
		if err != nil {
			return nil, fmt.Errorf("api: blob sweep reference scan: %w", err)
		}
		for rows.Next() {
			var sha string
			if err := rows.Scan(&sha); err != nil {
				rows.Close()
				return nil, fmt.Errorf("api: blob sweep reference scan: %w", err)
			}
			named[sha] = true
		}
		// rows.Err reports a failure that arrived mid-stream, which is the one
		// a Next loop cannot see. Without it a connection dropping halfway
		// through the scan reads as "the rest of these digests are unreferenced".
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("api: blob sweep reference scan: %w", err)
		}
	}
	return named, nil
}

// reportLostRaces re-asks about everything that was removed.
//
// A digest that has a row now had one written while the sweep was unlinking
// its bytes. It cannot be undone, so it is named — in the log and in the
// report — because "re-upload exactly this content" is a repair, and a silent
// hole discovered weeks later at provisioning time is not.
func (g *BlobGC) reportLostRaces(ctx context.Context, refs digestRefs, removed []string, rep *BlobGCReport) {
	// Detached: the whole point of this check is that it survives an operator
	// closing their laptop halfway through the sweep they started.
	checkCtx, cancel := detachedCtx(ctx)
	defer cancel()

	named, err := refs(checkCtx, removed)
	if err != nil {
		rep.Problems = append(rep.Problems,
			"could not confirm that the reclaimed digests are still unreferenced: "+err.Error())
		return
	}
	for _, sha := range removed {
		if !named[sha] {
			continue
		}
		g.srv.log.ErrorContext(checkCtx,
			"a farm.artifacts row was written for a digest this sweep had just reclaimed; "+
				"re-upload that exact content",
			"sha256", sha, "root", g.root)
		rep.Problems = append(rep.Problems,
			"raced an upload of "+sha+": the row exists but the bytes were reclaimed; re-upload that content")
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/artifacts/gc
// ---------------------------------------------------------------------------

// blobGCTimeout bounds one sweep. Enumeration is 257 directory reads and the
// reference scan is indexed, so a store that cannot finish in this has
// something else wrong with it.
const blobGCTimeout = 5 * time.Minute

// Register mounts the sweep as an operator route.
//
// Dry run is the default and ?apply=true is the opt-in, because the two
// requests differ only in whether a disk gets smaller and the safer of the two
// is the one a mistyped command should perform.
func (g *BlobGC) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/artifacts/gc",
		g.srv.requireRole(RoleOperator, http.HandlerFunc(g.handleSweep)))
}

func (g *BlobGC) handleSweep(w http.ResponseWriter, r *http.Request) {
	apply, err := queryBool(r, "apply")
	if err != nil {
		badRequest(w, err.Error(), map[string]string{"parameter": "apply"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), blobGCTimeout)
	defer cancel()

	rep, err := g.sweep(ctx, apply)
	if err != nil {
		if errors.Is(err, ErrBlobGCBusy) {
			writeError(w, http.StatusConflict, CodeConflict,
				"a blob sweep is already running; sweeps are idempotent, so wait for it and read its report", nil)
			return
		}
		g.srv.fail(w, r, "sweep artifact blobs", err)
		return
	}

	if apply {
		// The bytes are gone before this line, so the row naming who removed
		// them must not be lost because the caller hung up in the gap.
		auditCtx, auditCancel := detachedCtx(r.Context())
		g.srv.auditAction(auditCtx, actor(r.Context()), "artifact.gc", "blobs:"+g.root,
			queryString(r, "reason"),
			map[string]any{
				"deleted":       rep.Deleted,
				"freed_bytes":   rep.FreedBytes,
				"scanned":       rep.Scanned,
				"adopted":       rep.Adopted,
				"grace_seconds": rep.GraceSeconds,
			})
		auditCancel()

		// A sweep that hit problems removed real bytes and is not a failure,
		// but it is not "ok" either: something it wanted to do it could not
		// do, and the alert that fires on a non-ok operator action is how that
		// reaches a human who is not reading the response body.
		outcome := "ok"
		if len(rep.Problems) > 0 {
			outcome = "partial"
		}
		g.srv.metrics.operatorActions.WithLabelValues("artifact.gc", outcome).Inc()
	}

	writeJSON(w, http.StatusOK, rep)
}

// queryBool reads a boolean query parameter.
//
// An ABSENT parameter is false. A parameter that is present in any other shape
// — "apply=yes please", a bare "?apply" with no value, or the same key twice
// with different answers — is an error, because every one of those silently
// meaning "dry run" is the kind of help nobody wants from a garbage collector.
// A bare "?apply" is the dangerous one: it is what an operator types when they
// mean it, and Query().Get would have read it as the empty string and swept
// nothing while reporting success.
func queryBool(r *http.Request, key string) (bool, error) {
	vals, ok := r.URL.Query()[key]
	if !ok {
		return false, nil
	}
	if len(vals) > 1 {
		return false, fmt.Errorf("%s was given %d times; give it once", key, len(vals))
	}
	raw := strings.TrimSpace(vals[0])
	if raw == "" {
		return false, fmt.Errorf("%s was given with no value; write %s=true or %s=false", key, key, key)
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, not %q", key, raw)
	}
	return v, nil
}
