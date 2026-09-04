// Package artifacts is content-addressed storage for the bytes a job names.
//
// # Why content addressing
//
// A job spec that says "push /builds/nightly/app.apk" is a promise about a
// filesystem that the runner cannot keep: the path is mutable, the build that
// produced it is gone, and two devices in the same run can end up with
// different bytes. A job spec that says "push
// sha256:9f2b…" names the content itself. The farm can then answer the only
// question that matters at provisioning time — "are these exact bytes already
// on that device?" — without transferring anything.
//
// That question is what makes a 60-device fleet affordable. A 200 MB APK
// pushed to 60 devices is 12 GB over USB; pushed once and skipped 59 times it
// is 200 MB. farm.device_artifacts is the ledger that permits the skip, and
// EnsureOnDevice is the only thing that reads and writes it.
//
// # Relationship to the lease invariant
//
// A lease is ended by the job, by a deadline the user wrote down, or by a
// human. Nothing else.
//
// Nothing in this package ends a lease, and nothing here can. It imports
// neither internal/lease nor internal/adbwire: the push itself arrives as a
// PushFunc supplied by the caller, so this package holds no socket, no
// transport and no device handle. When a push fails, EnsureOnDevice records
// 'failed' on the row and returns the error to a caller that is still holding
// its lease and its device. That row is a note about the last attempt, not a
// verdict on the job — the caller retries inside the lease it already owns.
// A broken pipe halfway through a 200 MB push costs a retry, never a device.
//
// # Storage seam
//
// Backend is the boundary between "which bytes" and "where the bytes live".
// DirBackend, the only implementation here, is a local directory; an S3
// backend slots in behind the same three methods without any caller learning
// about it. The seam is deliberately narrow — Create, Open, URL — because
// every extra verb is one more thing an object store has to emulate.
package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors. Callers branch on these with errors.Is.
var (
	// ErrNotFound means farm.artifacts has no row for the sha. The bytes may
	// or may not be sitting in the backend; without a row the farm does not
	// know what they are, so it will not serve them.
	ErrNotFound = errors.New("artifacts: no such artifact; upload the content before naming its digest")

	// ErrBlobMissing means the row exists but the backend has no bytes under
	// that sha. Distinct from corruption on purpose: absent content is a
	// recoverable state (re-upload it), whereas content that hashes to
	// something else is a lie that must never be served.
	ErrBlobMissing = errors.New("artifacts: artifact bytes are absent from the backend; re-upload the content or restore the blob directory")

	// ErrUnknownDevice means the farm cannot address that device: either the
	// id has no row in farm.devices (SQLSTATE 23503 on
	// device_artifacts_device_id_fkey) or it is not a uuid at all (22P02).
	ErrUnknownDevice = errors.New("artifacts: device is not in farm.devices; enrol it before provisioning artifacts to it")

	// ErrTooLarge means the reader handed to Put exceeded the store's size
	// limit. The partially staged bytes are discarded.
	ErrTooLarge = errors.New("artifacts: content exceeds the configured size limit; raise it with WithMaxSize or upload smaller content")
)

// Postgres error codes, spelled out rather than pulled from a dependency,
// matching internal/lease.
const (
	sqlStateForeignKeyViolation = "23503"
	// sqlStateInvalidTextRepresentation is what a device id that will not cast
	// to uuid raises. Without translating it the caller gets the driver's
	// "invalid input syntax for type uuid", which names no table and no fix.
	sqlStateInvalidTextRepresentation = "22P02"
)

// Foreign key names on farm.device_artifacts, from migrations/00004_operate.sql.
//
// Both keys raise 23503, so the SQLSTATE alone cannot say which one failed.
// Reporting "unknown device" when it was the ARTIFACT row that vanished sends
// an operator to the wrong table at the wrong hour, so the constraint name is
// the discriminator.
const (
	fkDeviceArtifactsDevice = "device_artifacts_device_id_fkey"
	fkDeviceArtifactsSHA    = "device_artifacts_sha256_fkey"
)

// bookkeepingTimeout bounds the writes that record the outcome of a push on a
// context detached from the caller's. Long enough for a healthy database,
// short enough that a shutdown is not held up by an unreachable one.
const bookkeepingTimeout = 5 * time.Second

// DefaultMaxSize is the ceiling Put applies when the caller sets none. It is
// far above any real APK or OBB and exists only so an HTTP upload handler
// cannot fill the control plane's disk with a single request.
const DefaultMaxSize int64 = 8 << 30

// Kind mirrors, exactly and exhaustively, the CHECK constraint on
// farm.artifacts.kind in migrations/00004_operate.sql.
//
// Unlike lease.ReleaseReason — which is deliberately NOT validated in Go so
// that the database stays the enforcement point — Kind IS checked before Put
// does any work. The difference is cost, not principle: rejecting a bad
// release reason costs one round trip, whereas rejecting a bad kind after the
// fact would mean streaming and hashing 200 MB to disk first and failing on
// the INSERT afterwards.
type Kind string

const (
	KindAPK    Kind = "apk"
	KindFile   Kind = "file"
	KindScript Kind = "script"
	KindBundle Kind = "bundle"
)

// Valid reports whether k is one of the four kinds the schema permits.
func (k Kind) Valid() bool {
	switch k {
	case KindAPK, KindFile, KindScript, KindBundle:
		return true
	default:
		return false
	}
}

// ValidSHA256 reports whether s is a lowercase hex sha256 digest, which is
// what farm.artifacts.sha256's CHECK requires and what DirBackend needs before
// it will build a path out of it.
//
// Exported because a sha arriving from an API request is untrusted input and
// callers should be able to reject it at the edge. Hand-rolled rather than a
// regexp: this runs on every Get on the provisioning path.
func ValidSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// CorruptError reports that stored bytes do not match the name they are filed
// under. It is returned INSTEAD of the content, never alongside it.
type CorruptError struct {
	SHA      string // the name the content claims to have
	Got      string // what it actually hashes to, empty when only the size was checked
	WantSize int64  // size recorded in farm.artifacts
	GotSize  int64  // size the backend actually holds
}

func (e *CorruptError) Error() string {
	if e.Got != "" {
		return fmt.Sprintf("artifacts: %s is corrupt and was not served: content hashes to %s (%d bytes stored, %d expected); re-upload the content",
			e.SHA, e.Got, e.GotSize, e.WantSize)
	}
	return fmt.Sprintf("artifacts: %s is corrupt and was not served: %d bytes stored, %d expected; re-upload the content",
		e.SHA, e.GotSize, e.WantSize)
}

// ---------------------------------------------------------------------
// Storage seam
// ---------------------------------------------------------------------

// Blob is an open artifact.
//
// io.ReaderAt is part of the contract, not an accident: an APK is a zip and a
// zip's central directory lives at the END of the file, so a forward-only
// reader cannot parse one. Any backend that wants to hold APKs must therefore
// support random access — for an object store, ranged GETs.
type Blob interface {
	io.Reader
	io.ReaderAt
	io.Closer
}

// BlobWriter stages content whose name is not known until the last byte has
// been read, because the name IS the hash of the content.
//
// The two-phase shape is what makes a crash safe. Bytes accumulate somewhere
// unnamed; Commit publishes them atomically under the digest. There is no
// window in which a partially written file sits under a hash that claims to
// describe it.
type BlobWriter interface {
	io.Writer

	// Commit publishes the staged bytes under sha. It is idempotent with
	// respect to content: committing a digest the backend already holds
	// discards the staged copy and succeeds, because identical digests mean
	// identical bytes.
	Commit(sha string) error

	// Abort discards the staged bytes. It is a no-op after a successful
	// Commit, so callers can `defer w.Abort()` unconditionally.
	Abort() error
}

// Backend is where the bytes live. DirBackend below is the local-directory
// implementation; an S3 implementation satisfies the same three methods.
type Backend interface {
	// Create opens a staging area for content of unknown digest.
	Create(ctx context.Context) (BlobWriter, error)

	// Open returns the content stored under sha and its size in bytes. It
	// must report a missing blob as an error satisfying
	// errors.Is(err, fs.ErrNotExist).
	Open(ctx context.Context, sha string) (Blob, int64, error)

	// URL is the human-facing location recorded in farm.artifacts.url, so an
	// operator reading the table can find the bytes. Nothing in this package
	// resolves it back into a reader — Get always goes through Open.
	URL(sha string) string
}

// stageDir holds partially written blobs. It lives under the root, not in
// os.TempDir, because rename is only atomic within one filesystem and
// os.TempDir is routinely a different one. The leading underscore keeps it
// clear of the two-hex-character fan-out directories.
const stageDir = "_staging"

// DirBackend stores blobs in a local directory, fanned out one level by the
// first two hex characters of the digest so no directory holds more than a few
// hundred entries at fleet scale.
type DirBackend struct {
	root string
}

// NewDirBackend prepares root and its staging directory.
func NewDirBackend(root string) (*DirBackend, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("artifacts: blob directory must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("artifacts: resolve blob directory %q: %w", root, err)
	}
	if err := os.MkdirAll(filepath.Join(abs, stageDir), 0o755); err != nil {
		return nil, fmt.Errorf("artifacts: prepare blob directory %q: %w", abs, err)
	}
	return &DirBackend{root: abs}, nil
}

// Root is the absolute directory the backend writes under.
func (d *DirBackend) Root() string { return d.root }

// path is the final resting place of a digest. The ValidSHA256 gate is a
// security boundary, not a nicety: a sha reaching here from an API request
// without it could contain separators or "..", and Join would happily build a
// path outside the root.
func (d *DirBackend) path(sha string) (string, error) {
	if !ValidSHA256(sha) {
		return "", fmt.Errorf("artifacts: %q is not a sha256 digest (want 64 lowercase hex characters)", sha)
	}
	return filepath.Join(d.root, sha[:2], sha), nil
}

// URL renders the blob's location as a file URL. Built by hand rather than
// through net/url because a Windows absolute path ("C:\...") needs the leading
// slash that a bare path does not carry.
func (d *DirBackend) URL(sha string) string {
	p, err := d.path(sha)
	if err != nil {
		return ""
	}
	slashed := filepath.ToSlash(p)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return "file://" + slashed
}

// Create stages a new blob in the staging directory.
func (d *DirBackend) Create(ctx context.Context) (BlobWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Join(d.root, stageDir), "put-*")
	if err != nil {
		return nil, fmt.Errorf("artifacts: stage blob under %s: %w", d.root, err)
	}
	return &dirWriter{f: f, backend: d, staged: f.Name()}, nil
}

// Open returns the stored file. *os.File satisfies Blob directly, which is
// what lets the APK parser seek to the zip central directory without the
// store buffering the whole artifact in memory.
func (d *DirBackend) Open(ctx context.Context, sha string) (Blob, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	p, err := d.path(sha)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		// Preserved unwrapped enough for errors.Is(err, fs.ErrNotExist), which
		// is how Store tells "absent" from "unreadable".
		return nil, 0, fmt.Errorf("artifacts: open %s: %w", sha, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("artifacts: stat %s: %w", sha, err)
	}
	return f, st.Size(), nil
}

type dirWriter struct {
	f       *os.File
	backend *DirBackend
	// staged is the path the bytes are accumulating under. It is cleared once
	// they are either published under their digest or removed — so it doubles
	// as "there is still a temporary file out there to clean up", which is what
	// lets a failed removal be retried by Abort instead of leaking silently.
	staged string
	closed bool
}

func (w *dirWriter) Write(p []byte) (int, error) { return w.f.Write(p) }

// close shuts the handle at most once, so Abort after Commit is harmless.
func (w *dirWriter) close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.f.Close()
}

// discard removes the staged file. On failure it leaves w.staged set so a
// later Abort tries again: a staging directory that grows a file per failed
// upload is a disk-full outage weeks later, with nothing in the logs to say
// where it came from.
func (w *dirWriter) discard() error {
	if w.staged == "" {
		return nil
	}
	err := os.Remove(w.staged)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		w.staged = ""
		return nil
	}
	return fmt.Errorf("artifacts: remove staged blob %s: %w", w.staged, err)
}

// Commit fsyncs, then renames. The order is the whole point: the rename is
// what makes the digest visible, so the bytes must be on the platter before
// the name that vouches for them exists. A crash before the rename leaves a
// nameless file in the staging directory — garbage, and harmless. A crash
// after it leaves complete content. The dangerous state, a truncated file
// under a digest that claims to describe it, is unreachable.
func (w *dirWriter) Commit(sha string) error {
	dst, err := w.backend.path(sha)
	if err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("artifacts: sync staged blob: %w", err)
	}
	if err := w.close(); err != nil {
		return fmt.Errorf("artifacts: close staged blob: %w", err)
	}

	// Content addressing makes this cheap: if the digest is already stored,
	// the bytes are by definition the ones we just staged. Checking first also
	// avoids replacing a file another process may have open, which fails on
	// Windows.
	if _, err := os.Stat(dst); err == nil {
		// A removal failure is deliberately not returned — the upload
		// succeeded, and failing it would throw away correct content over a
		// stray temp file. discard leaves w.staged set, so the caller's
		// deferred Abort retries and reports it.
		_ = w.discard()
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("artifacts: prepare blob directory for %s: %w", sha, err)
	}
	if err := os.Rename(w.staged, dst); err != nil {
		// A concurrent Put of the same content can win the race between the
		// Stat above and this rename. Identical digests are identical bytes,
		// so the loser has nothing to mourn.
		if _, statErr := os.Stat(dst); statErr == nil {
			_ = w.discard()
			return nil
		}
		return fmt.Errorf("artifacts: publish blob %s: %w", sha, err)
	}
	w.staged = "" // published; Abort must never remove it now

	// The rename is the thing that makes the digest visible, and Put writes the
	// farm.artifacts row immediately after. Without this the row can outlive
	// the directory entry across a power loss, producing exactly the state Put
	// promises never to produce: a row pointing at bytes that are not there.
	//
	// Best effort by necessity: a directory fsync is not portable (Windows
	// rejects it outright), and the file's own Sync above already has the
	// content on the platter, so a refusal here costs durability of the NAME
	// on platforms that never offered it.
	if dir, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = dir.Sync()
		dir.Close()
	}

	if err := os.Chmod(dst, 0o644); err != nil {
		// The bytes are committed and correct; a permissions surprise on the
		// staging temp file's mode is not worth failing an upload over.
		return nil
	}
	return nil
}

func (w *dirWriter) Abort() error {
	err := w.close()
	if rmErr := w.discard(); rmErr != nil && err == nil {
		err = rmErr
	}
	return err
}

// ---------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------

// Artifact is one row of farm.artifacts.
type Artifact struct {
	SHA256 string
	Kind   Kind
	Name   string
	Size   int64

	// Package and VersionCode are populated for APKs whose manifest could be
	// read. Zero values mean "unknown", never "absent from the APK".
	Package     string
	VersionCode int64

	// VersionName is parsed but NOT persisted: farm.artifacts has no column
	// for it. It is carried on the returned value so a caller can log or
	// display what it just stored.
	VersionName string

	URL        string
	UploadedBy string
	CreatedAt  time.Time
}

// PutResult reports what Put did.
type PutResult struct {
	Artifact

	// Inserted is false when the content was already in the store and this
	// Put only refreshed its metadata. Useful for logging a deduplicated
	// upload as such rather than as a new artifact.
	Inserted bool

	// ManifestErr is non-nil when kind was apk and the manifest could not be
	// read. The artifact IS stored and IS pushable; only package and
	// version_code are absent. An unparseable APK is still a file, and
	// refusing it outright would break every repackaged, obfuscated or
	// vendor-mangled build in the fleet.
	ManifestErr error
}

// EnsureResult reports what EnsureOnDevice did.
type EnsureResult struct {
	Artifact Artifact

	// Pushed is false when farm.device_artifacts already said 'present' and
	// no bytes crossed the wire. This is the 60-device economy, measured.
	Pushed bool

	// RemotePath is whatever the PushFunc reported, or what a previous push
	// recorded. Empty when the pusher named no path.
	RemotePath string

	InstalledAt time.Time
}

// PushFunc transfers an artifact's bytes to a device and reports where they
// landed. It is supplied by the caller so that this package never touches a
// transport: EnsureOnDevice's bookkeeping is testable without an ADB server,
// and a socket error has no path into artifact state beyond a 'failed' row.
//
// The returned path is recorded in farm.device_artifacts.detail so a later job
// — or an operator at 3am — can find the file without guessing.
type PushFunc func(ctx context.Context, a Artifact, blob Blob) (remotePath string, err error)

// ---------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------

// Store binds farm.artifacts and farm.device_artifacts to a Backend.
type Store struct {
	pool    *pgxpool.Pool
	blobs   Backend
	log     *slog.Logger
	maxSize int64
}

// Option configures a Store.
type Option func(*Store)

// WithLogger sets the logger. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) {
		if l != nil {
			s.log = l
		}
	}
}

// WithMaxSize caps the bytes Put will accept. A non-positive value restores
// DefaultMaxSize rather than meaning "unlimited", because an unlimited upload
// endpoint is a way to take the control plane down with one request.
func WithMaxSize(n int64) Option {
	return func(s *Store) {
		if n <= 0 {
			n = DefaultMaxSize
		}
		s.maxSize = n
	}
}

// NewStore wraps a pool and a backend.
func NewStore(pool *pgxpool.Pool, blobs Backend, opts ...Option) (*Store, error) {
	if pool == nil {
		return nil, errors.New("artifacts: nil pool")
	}
	if blobs == nil {
		return nil, errors.New("artifacts: nil backend")
	}
	s := &Store{pool: pool, blobs: blobs, log: slog.Default(), maxSize: DefaultMaxSize}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// PutOption carries the optional facts about one upload.
type PutOption func(*putConfig)

type putConfig struct {
	uploadedBy string
}

// WithUploadedBy records who supplied the bytes, for farm.artifacts.uploaded_by.
func WithUploadedBy(who string) PutOption {
	return func(c *putConfig) { c.uploadedBy = who }
}

// Put streams reader into the backend, hashing as it goes, and upserts the
// resulting digest into farm.artifacts.
//
// The digest is not known until the last byte has been read, so the content is
// staged under a temporary name and published by Commit. The bytes are
// committed BEFORE the row is written: an orphaned blob with no row is
// harmless garbage that a later Put of the same content simply adopts, whereas
// a row pointing at bytes that are not there is a promise the farm cannot
// keep.
//
// For KindAPK the committed blob is reopened and its manifest parsed. Reopening
// costs one extra read of a file that is already in the page cache and avoids
// holding 200 MB in memory purely to look at four attributes near the front of
// a zip. A manifest that will not parse is reported in PutResult.ManifestErr
// and logged; it never fails the Put.
func (s *Store) Put(ctx context.Context, r io.Reader, kind Kind, name string, opts ...PutOption) (PutResult, error) {
	if r == nil {
		return PutResult{}, errors.New("artifacts: nil reader")
	}
	if !kind.Valid() {
		return PutResult{}, fmt.Errorf("artifacts: %q is not a kind the schema permits (apk, file, script, bundle)", kind)
	}
	name = sanitizeText(name)
	if name == "" {
		return PutResult{}, errors.New("artifacts: artifact name must not be empty")
	}
	var cfg putConfig
	for _, o := range opts {
		o(&cfg)
	}
	cfg.uploadedBy = sanitizeText(cfg.uploadedBy)

	w, err := s.blobs.Create(ctx)
	if err != nil {
		return PutResult{}, err
	}
	// Safe unconditionally: Abort is a no-op once Commit has published the
	// bytes. When Put fails before that, this is the only thing standing
	// between a rejected upload and a permanent file in the staging directory,
	// so its error is logged rather than dropped.
	defer func() {
		if err := w.Abort(); err != nil {
			s.log.Warn("staged artifact bytes could not be discarded; sweep the staging directory",
				"name", name, "err", err)
		}
	}()

	h := sha256.New()
	// One extra byte past the limit is enough to tell "exactly at the cap"
	// from "over it", without reading the rest of a hostile stream. Guarded
	// against wrapping: a caller may pass MaxInt64 to mean "no practical
	// limit", and a negative limit makes io.LimitReader yield nothing — which
	// would store every upload as the empty blob and report success.
	probe := s.maxSize
	if probe < math.MaxInt64 {
		probe++
	}
	limited := io.LimitReader(r, probe)
	size, err := io.Copy(io.MultiWriter(w, h), limited)
	if err != nil {
		return PutResult{}, fmt.Errorf("artifacts: stream %q: %w", name, err)
	}
	if size > s.maxSize {
		return PutResult{}, fmt.Errorf("artifacts: %q: %w (limit %d bytes)", name, ErrTooLarge, s.maxSize)
	}
	sha := hex.EncodeToString(h.Sum(nil))

	if err := w.Commit(sha); err != nil {
		return PutResult{}, err
	}

	out := PutResult{Artifact: Artifact{
		SHA256:     sha,
		Kind:       kind,
		Name:       name,
		Size:       size,
		URL:        s.blobs.URL(sha),
		UploadedBy: cfg.uploadedBy,
	}}

	if kind == KindAPK {
		man, mErr := s.readManifest(ctx, sha)
		if mErr != nil {
			out.ManifestErr = mErr
			s.log.Warn("apk manifest unreadable; artifact stored without package metadata",
				"sha256", sha, "name", name, "err", mErr)
		} else {
			out.Package = man.Package
			out.VersionCode = man.VersionCode
			out.VersionName = man.VersionName
		}
	}

	if err := s.upsert(ctx, &out); err != nil {
		return PutResult{}, err
	}
	return out, nil
}

// readManifest reopens a just-committed blob and parses its AndroidManifest.
func (s *Store) readManifest(ctx context.Context, sha string) (Manifest, error) {
	blob, size, err := s.blobs.Open(ctx, sha)
	if err != nil {
		return Manifest{}, err
	}
	defer blob.Close()
	return ParseAPK(blob, size)
}

// upsert writes the row and fills in the server-assigned fields.
//
// Merge policy, which the COALESCEs encode:
//   - kind and name follow the newest declaration, since a caller may be
//     correcting a mislabelled upload.
//   - package, version_code, url and uploaded_by are only ever FILLED IN.
//     Re-uploading identical bytes as a plain 'file' must not erase the
//     package metadata an earlier APK upload managed to parse, and it must
//     not rewrite the record of who first introduced the content.
func (s *Store) upsert(ctx context.Context, out *PutResult) error {
	const q = `
INSERT INTO farm.artifacts (sha256, kind, name, size_bytes, package, version_code, url, uploaded_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (sha256) DO UPDATE
   SET kind         = EXCLUDED.kind,
       name         = EXCLUDED.name,
       size_bytes   = EXCLUDED.size_bytes,
       package      = COALESCE(EXCLUDED.package, farm.artifacts.package),
       version_code = COALESCE(EXCLUDED.version_code, farm.artifacts.version_code),
       url          = COALESCE(EXCLUDED.url, farm.artifacts.url),
       uploaded_by  = COALESCE(EXCLUDED.uploaded_by, farm.artifacts.uploaded_by)
RETURNING package, version_code, url, uploaded_by, created_at, (created_at = now()) AS inserted`

	// created_at = now() discriminates insert from update without the xmax
	// trick: now() is the transaction timestamp, a fresh row's created_at
	// DEFAULT is that same timestamp, and any pre-existing row was stamped by
	// an earlier transaction. Each call below is its own implicit transaction.
	var pkg, url, uploadedBy *string
	var versionCode *int64
	err := s.pool.QueryRow(ctx, q,
		out.SHA256, string(out.Kind), out.Name, out.Size,
		nullString(out.Package), nullInt64(out.VersionCode),
		nullString(out.URL), nullString(out.UploadedBy),
	).Scan(&pkg, &versionCode, &url, &uploadedBy, &out.CreatedAt, &out.Inserted)
	if err != nil {
		return fmt.Errorf("artifacts: record %s: %w", out.SHA256, err)
	}
	out.Package = derefString(pkg)
	out.VersionCode = derefInt64(versionCode)
	out.URL = derefString(url)
	out.UploadedBy = derefString(uploadedBy)
	return nil
}

// Lookup reads one row of farm.artifacts. Returns ErrNotFound when the farm
// has never been told about that digest.
func (s *Store) Lookup(ctx context.Context, sha string) (Artifact, error) {
	if !ValidSHA256(sha) {
		return Artifact{}, fmt.Errorf("artifacts: %q is not a sha256 digest (want 64 lowercase hex characters): %w", sha, ErrNotFound)
	}
	const q = `
SELECT a.kind, a.name, a.size_bytes, a.package, a.version_code, a.url, a.uploaded_by, a.created_at
  FROM farm.artifacts a WHERE a.sha256 = $1`

	a := Artifact{SHA256: sha}
	var kind string
	var pkg, url, uploadedBy *string
	var versionCode *int64
	err := s.pool.QueryRow(ctx, q, sha).
		Scan(&kind, &a.Name, &a.Size, &pkg, &versionCode, &url, &uploadedBy, &a.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Artifact{}, fmt.Errorf("artifacts: %s: %w", sha, ErrNotFound)
		}
		return Artifact{}, fmt.Errorf("artifacts: look up %s: %w", sha, err)
	}
	a.Kind = Kind(kind)
	a.Package = derefString(pkg)
	a.VersionCode = derefInt64(versionCode)
	a.URL = derefString(url)
	a.UploadedBy = derefString(uploadedBy)
	return a, nil
}

// Get opens the content stored under sha.
//
// It checks the stored length against farm.artifacts.size_bytes before handing
// the reader back. That is a cheap, O(1) truthfulness check that catches the
// realistic corruption — a truncated file from a full disk or a killed
// process — without reading the artifact twice. It is NOT a substitute for
// Verify, which re-hashes; use Verify when the question is integrity rather
// than plausibility.
//
// The caller closes the returned Blob.
func (s *Store) Get(ctx context.Context, sha string) (Blob, Artifact, error) {
	a, err := s.Lookup(ctx, sha)
	if err != nil {
		return nil, Artifact{}, err
	}
	blob, size, err := s.blobs.Open(ctx, sha)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, a, fmt.Errorf("artifacts: %s: %w", sha, ErrBlobMissing)
		}
		return nil, a, err
	}
	if size != a.Size {
		blob.Close()
		return nil, a, &CorruptError{SHA: sha, WantSize: a.Size, GotSize: size}
	}
	return blob, a, nil
}

// Verify re-reads the stored bytes and re-hashes them.
//
// It returns *CorruptError rather than the content, which is the entire point:
// a store that notices bad bytes and serves them anyway is worse than one that
// never checked, because it launders the corruption into a device and a test
// result. A digest is a claim about content, and this is the only thing in the
// package that audits the claim.
//
// Verify reads the whole artifact, so it belongs on a maintenance path, not on
// every push.
func (s *Store) Verify(ctx context.Context, sha string) error {
	a, err := s.Lookup(ctx, sha)
	if err != nil {
		return err
	}
	blob, size, err := s.blobs.Open(ctx, sha)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("artifacts: %s: %w", sha, ErrBlobMissing)
		}
		return err
	}
	defer blob.Close()

	h := sha256.New()
	read, err := io.Copy(h, blob)
	if err != nil {
		return fmt.Errorf("artifacts: read %s for verification: %w", sha, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != sha || read != a.Size || size != a.Size {
		e := &CorruptError{SHA: sha, WantSize: a.Size, GotSize: read}
		// Got stays empty when the content hashes correctly and only the
		// recorded length disagrees, so the message does not report a digest
		// mismatch that did not happen — that case is a wrong row, not wrong
		// bytes, and it is repaired somewhere else entirely.
		if got != sha {
			e.Got = got
		}
		return e
	}
	return nil
}

// ---------------------------------------------------------------------
// Per-device provisioning
// ---------------------------------------------------------------------

// EnsureOnDevice makes sha present on a device, pushing only if it has to.
//
// The ledger in farm.device_artifacts is what turns provisioning from an
// O(devices x bytes) problem into an O(new bytes) one: at 60 devices, a
// 200 MB APK that is already installed costs one indexed primary-key lookup
// instead of a four-minute USB transfer. That is the whole reason the table
// exists.
//
// The sequence, and why it is in this order:
//
//  1. Read the artifact and the device's row together. An unknown digest
//     fails here, before anything touches the device.
//  2. If the row says 'present', return with Pushed false. Nothing is
//     transferred and no row is written.
//  3. Open the blob and check its length. A local store that has lost the
//     bytes must fail before the ledger is told a push is under way.
//  4. Mark 'pending'. A crash mid-push then leaves a row that says so,
//     rather than no row at all.
//  5. Push, then record 'present' or 'failed'.
//
// A failed push is a transport failure, and a transport failure does not end
// anything. The error is returned to a caller that still holds its lease and
// its device, and 'failed' is a note about the last attempt that the next call
// will overwrite. Retry inside the lease you already have.
//
// The skip in step 2 trusts the ledger, so anything that wipes a device — a
// medium or hard reset, a factory wipe, a reflash — MUST call MarkRemoved or
// ForgetDevice. Otherwise this will happily skip a push for content the device
// no longer holds.
func (s *Store) EnsureOnDevice(ctx context.Context, deviceID, sha string, push PushFunc) (EnsureResult, error) {
	if push == nil {
		return EnsureResult{}, errors.New("artifacts: nil push function")
	}
	if !ValidSHA256(sha) {
		return EnsureResult{}, fmt.Errorf("artifacts: %q is not a sha256 digest (want 64 lowercase hex characters): %w", sha, ErrNotFound)
	}

	a, state, remotePath, installedAt, err := s.deviceRow(ctx, deviceID, sha)
	if err != nil {
		return EnsureResult{}, err
	}
	if state == "present" {
		return EnsureResult{Artifact: a, Pushed: false, RemotePath: remotePath, InstalledAt: installedAt}, nil
	}

	blob, size, err := s.blobs.Open(ctx, sha)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return EnsureResult{}, fmt.Errorf("artifacts: %s: %w", sha, ErrBlobMissing)
		}
		return EnsureResult{}, err
	}
	defer blob.Close()
	if size != a.Size {
		return EnsureResult{}, &CorruptError{SHA: sha, WantSize: a.Size, GotSize: size}
	}

	if err := s.markPending(ctx, deviceID, sha); err != nil {
		return EnsureResult{}, err
	}

	path, pushErr := push(ctx, a, blob)
	if pushErr != nil {
		s.recordFailure(ctx, deviceID, sha, pushErr)
		// Named as retryable on purpose: a transport failure here is a failed
		// transfer, not a failed job, and the caller still holds the lease and
		// the device it started with.
		return EnsureResult{Artifact: a}, fmt.Errorf(
			"artifacts: push %s to device %s failed and was recorded; retry inside the lease you already hold: %w",
			sha, deviceID, pushErr)
	}

	at, err := s.markPresent(ctx, deviceID, sha, sanitizeText(path))
	if err != nil {
		// The bytes ARE on the device; only the ledger write failed. Saying so
		// is better than reporting a push failure that did not happen — the
		// worst outcome of a lost 'present' is one redundant push next time.
		return EnsureResult{Artifact: a, Pushed: true, RemotePath: path},
			fmt.Errorf("artifacts: %s pushed to device %s but the ledger write failed: %w", sha, deviceID, err)
	}
	return EnsureResult{Artifact: a, Pushed: true, RemotePath: path, InstalledAt: at}, nil
}

// deviceRow reads the artifact and its per-device state in one round trip.
func (s *Store) deviceRow(ctx context.Context, deviceID, sha string) (Artifact, string, string, time.Time, error) {
	const q = `
SELECT a.kind, a.name, a.size_bytes, a.package, a.version_code, a.url, a.uploaded_by, a.created_at,
       da.state, da.installed_at, da.detail->>'remote_path'
  FROM farm.artifacts a
  LEFT JOIN farm.device_artifacts da
    ON da.sha256 = a.sha256 AND da.device_id = $2::uuid
 WHERE a.sha256 = $1`

	a := Artifact{SHA256: sha}
	var kind string
	var pkg, url, uploadedBy, state, remotePath *string
	var versionCode *int64
	var installedAt *time.Time

	err := s.pool.QueryRow(ctx, q, sha, deviceID).Scan(
		&kind, &a.Name, &a.Size, &pkg, &versionCode, &url, &uploadedBy, &a.CreatedAt,
		&state, &installedAt, &remotePath)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Artifact{}, "", "", time.Time{}, fmt.Errorf("artifacts: %s: %w", sha, ErrNotFound)
		}
		// A malformed device id is caught HERE, before the blob is opened and
		// long before anything is pushed, which is the point of reading the
		// device's row first.
		if e := deviceRefError(err, deviceID, sha); e != nil {
			return Artifact{}, "", "", time.Time{}, e
		}
		return Artifact{}, "", "", time.Time{}, fmt.Errorf("artifacts: read device %s state for %s: %w", deviceID, sha, err)
	}
	a.Kind = Kind(kind)
	a.Package = derefString(pkg)
	a.VersionCode = derefInt64(versionCode)
	a.URL = derefString(url)
	a.UploadedBy = derefString(uploadedBy)

	var at time.Time
	if installedAt != nil {
		at = *installedAt
	}
	return a, derefString(state), derefString(remotePath), at, nil
}

// markPending records the intent to push.
//
// Everything the previous attempt left behind is cleared: installed_at, the
// path it landed on, why it was retracted, why it failed. A row must describe
// the attempt that is happening now, or an operator reading it at 3am is
// reading a sentence assembled from three different pushes.
func (s *Store) markPending(ctx context.Context, deviceID, sha string) error {
	const q = `
INSERT INTO farm.device_artifacts (device_id, sha256, state)
VALUES ($1::uuid, $2, 'pending')
ON CONFLICT (device_id, sha256) DO UPDATE
   SET state = 'pending', installed_at = NULL,
       detail = farm.device_artifacts.detail - 'error' - 'remote_path' - 'removed_reason'`

	if _, err := s.pool.Exec(ctx, q, deviceID, sha); err != nil {
		if e := deviceRefError(err, deviceID, sha); e != nil {
			return e
		}
		return fmt.Errorf("artifacts: mark %s pending on device %s: %w", sha, deviceID, err)
	}
	return nil
}

// markPresent closes out a successful push. The stale 'error' key is dropped
// rather than overwritten with null, so an operator reading detail sees the
// record of the last attempt and not a tombstone from a previous one.
func (s *Store) markPresent(ctx context.Context, deviceID, sha, remotePath string) (time.Time, error) {
	const q = `
UPDATE farm.device_artifacts
   SET state = 'present', installed_at = now(), detail = (detail - 'error') || $3::jsonb
 WHERE device_id = $1::uuid AND sha256 = $2
RETURNING installed_at`

	detail := map[string]any{}
	if remotePath != "" {
		detail["remote_path"] = remotePath
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return time.Time{}, fmt.Errorf("artifacts: encode push detail: %w", err)
	}
	var at time.Time
	if err := s.pool.QueryRow(ctx, q, deviceID, sha, string(raw)).Scan(&at); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// markPending wrote this row moments ago, so its absence means the
			// device or the artifact was deleted mid-push.
			return time.Time{}, fmt.Errorf("artifacts: ledger row for %s on device %s disappeared during the push: %w", sha, deviceID, err)
		}
		return time.Time{}, fmt.Errorf("artifacts: mark %s present on device %s: %w", sha, deviceID, err)
	}
	return at, nil
}

// recordFailure notes why the last push did not land.
//
// It runs on a context detached from the caller's, because the case that most
// needs an accurate row is exactly the one where the caller's context is
// already dead: a cancelled or timed-out push. Reusing that context would
// leave the row saying 'pending' forever and give the next operator no idea
// what happened. A failure to record is logged, never returned — the push
// error is the one the caller must see.
func (s *Store) recordFailure(ctx context.Context, deviceID, sha string, cause error) {
	const q = `
UPDATE farm.device_artifacts
   SET state = 'failed', installed_at = NULL, detail = detail || $3::jsonb
 WHERE device_id = $1::uuid AND sha256 = $2`

	raw, err := json.Marshal(map[string]any{"error": truncate(sanitizeText(cause.Error()), 2000)})
	if err != nil {
		s.log.Warn("could not encode artifact push failure", "sha256", sha, "device_id", deviceID, "err", err)
		return
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()
	tag, err := s.pool.Exec(bg, q, deviceID, sha, string(raw))
	if err != nil {
		s.log.Warn("could not record artifact push failure; the ledger still says pending for this pair",
			"sha256", sha, "device_id", deviceID, "cause", cause, "err", err)
		return
	}
	if tag.RowsAffected() == 0 {
		// The 'pending' row this call exists to close out is gone: the device
		// or the artifact was deleted mid-push. Nothing here can repair that,
		// but an operator hunting the failure will not find it in the ledger,
		// so the log is the only place it survives.
		s.log.Warn("artifact push failed but its ledger row had already been deleted",
			"sha256", sha, "device_id", deviceID, "cause", cause)
	}
}

// MarkRemoved retracts one artifact from a device's ledger, so the next
// EnsureOnDevice pushes instead of skipping. Call it after anything that
// removes content from a device: an uninstall step, or a reset tier that wipes
// user storage.
//
// Returns false when the device had no such row, or it already said 'removed'.
func (s *Store) MarkRemoved(ctx context.Context, deviceID, sha, reason string) (bool, error) {
	// Every key the previous attempt left is dropped, for the same reason
	// markPending drops them: a row that carries the path of a push, the error
	// of a later failure and the reason for this retraction is three sentences
	// from three different events, and an operator reads it as one.
	const q = `
UPDATE farm.device_artifacts
   SET state = 'removed', installed_at = NULL,
       detail = (detail - 'remote_path' - 'error' - 'removed_reason') || $3::jsonb
 WHERE device_id = $1::uuid AND sha256 = $2 AND state <> 'removed'`

	if !ValidSHA256(sha) {
		return false, fmt.Errorf("artifacts: %q is not a sha256 digest (want 64 lowercase hex characters)", sha)
	}
	raw, err := json.Marshal(removalDetail(reason))
	if err != nil {
		return false, fmt.Errorf("artifacts: encode removal detail: %w", err)
	}
	tag, err := s.pool.Exec(ctx, q, deviceID, sha, string(raw))
	if err != nil {
		if e := deviceRefError(err, deviceID, sha); e != nil {
			return false, e
		}
		return false, fmt.Errorf("artifacts: mark %s removed on device %s: %w", sha, deviceID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ForgetDevice retracts EVERY artifact from a device's ledger.
//
// This is what a factory reset, a reflash or a hard recovery tier owes the
// store. Without it the ledger keeps claiming content that the wipe destroyed,
// and every subsequent job silently skips a push it needed — a class of bug
// that presents as "the test ran against the wrong build", which is far harder
// to diagnose than one redundant transfer.
//
// Returns the number of rows retracted.
func (s *Store) ForgetDevice(ctx context.Context, deviceID, reason string) (int64, error) {
	const q = `
UPDATE farm.device_artifacts
   SET state = 'removed', installed_at = NULL,
       detail = (detail - 'remote_path' - 'error' - 'removed_reason') || $2::jsonb
 WHERE device_id = $1::uuid AND state <> 'removed'`

	raw, err := json.Marshal(removalDetail(reason))
	if err != nil {
		return 0, fmt.Errorf("artifacts: encode removal detail: %w", err)
	}
	tag, err := s.pool.Exec(ctx, q, deviceID, string(raw))
	if err != nil {
		if e := deviceRefError(err, deviceID, ""); e != nil {
			return 0, e
		}
		return 0, fmt.Errorf("artifacts: forget artifacts on device %s: %w", deviceID, err)
	}
	return tag.RowsAffected(), nil
}

// removalDetail builds the jsonb a retraction merges in. An empty reason
// contributes no key at all: an absent removed_reason reads as "nobody said",
// whereas an empty string reads as a bug in whatever wrote the row.
func removalDetail(reason string) map[string]any {
	d := map[string]any{}
	if r := truncate(sanitizeText(reason), 500); r != "" {
		d["removed_reason"] = r
	}
	return d
}

// ---------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------

// sanitizeText makes a string safe to store in a Postgres text column.
//
// Both replacements are load-bearing when the source is an APK: a manifest
// string pool is attacker-controlled, and Postgres rejects both invalid UTF-8
// and embedded NUL bytes outright. Without this, a malformed APK would not
// merely lose its metadata — it would fail the INSERT and lose the artifact.
func sanitizeText(s string) string {
	if strings.ContainsRune(s, 0) {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return strings.TrimSpace(strings.ToValidUTF8(s, "\uFFFD"))
}

// truncate caps a string at n bytes, stepping back to a rune boundary. Cutting
// mid-rune would put a partial code point into jsonb detail — encoding/json
// rewrites it to U+FFFD, so the row is never invalid, but the operator reading
// the last line of an adb error deserves the last line, not a mojibake tail.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// nullString and nullInt64 send SQL NULL rather than ” or 0, so that the
// COALESCE merge in upsert can tell "not known" from "known to be empty".
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullInt64(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// deviceRefError translates the ways Postgres can reject a (device, artifact)
// pair into errors that name what to fix, and returns nil for anything else so
// the caller keeps its own wrapping.
//
// Three distinct operator actions hide behind two opaque driver errors here —
// fix the id you passed, enrol the device, re-upload the artifact — and only
// the SQLSTATE plus the constraint name can tell them apart.
func deviceRefError(err error, deviceID, sha string) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}
	switch {
	case pgErr.Code == sqlStateInvalidTextRepresentation:
		return fmt.Errorf("artifacts: %q is not a device uuid: %w", deviceID, ErrUnknownDevice)
	case pgErr.Code != sqlStateForeignKeyViolation:
		return nil
	case pgErr.ConstraintName == fkDeviceArtifactsSHA:
		return fmt.Errorf("artifacts: %s: %w", sha, ErrNotFound)
	case pgErr.ConstraintName == fkDeviceArtifactsDevice:
		return fmt.Errorf("artifacts: device %s: %w", deviceID, ErrUnknownDevice)
	}
	return nil
}
