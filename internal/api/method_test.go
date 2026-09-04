package api

// Two HTTP-level properties, tested at the level a client experiences them.
//
// The first is that a wrong verb on a real path says so. The catch-all under
// /api/v1/ makes net/http's own 405 unreachable, so "the resource is fine, the
// method is not" has to be reconstructed from the routing table — and the test
// that matters is the negative one: reconstructing it must NOT turn a typo'd
// URL into a 405, because that would advertise routes that do not exist.
//
// The second is that the content route hands back the bytes it was asked for.
// The name of the resource is the sha256 of its body, so the test is not "did
// something come back" but "does what came back hash to the name it was
// fetched under". Anything less would pass for a store that served the wrong
// artifact.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/artifacts"
)

// ---------------------------------------------------------------------------
// 405 / 404
// ---------------------------------------------------------------------------

// routerShapedMux mirrors the shape of the real table: method-qualified routes,
// a wildcard route, a catch-all under /api/v1/ that answers JSON, and a
// catch-all at "/" for the dashboard. The shape is what the method check reads,
// so a fixture that omitted the catch-alls would test nothing.
func routerShapedMux() *http.ServeMux {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }

	mux.HandleFunc("GET /api/v1/fleet", ok)
	mux.HandleFunc("GET /api/v1/jobs", ok)
	mux.HandleFunc("POST /api/v1/jobs", ok)
	mux.HandleFunc("POST /api/v1/leases/acquire", ok)
	mux.HandleFunc("GET /api/v1/artifacts/{sha}", ok)
	mux.HandleFunc("DELETE /api/v1/artifacts/{sha}", ok)

	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound,
			"no such API route: "+r.Method+" "+r.URL.Path, nil)
	})
	mux.Handle("/api/v1/", methodFallback(mux, notFound))
	mux.HandleFunc("/", ok)
	return mux
}

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// TestKnownPathWrongVerbIs405WithAllow is the gap itself: before the fallback,
// DELETE /api/v1/fleet answered 404 and told a client the fleet endpoint did
// not exist on this deployment.
func TestKnownPathWrongVerbIs405WithAllow(t *testing.T) {
	mux := routerShapedMux()

	rec := do(t, mux, http.MethodDelete, "/api/v1/fleet")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/v1/fleet = %d, want 405 (404 would say the resource is gone when only the verb is wrong)", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD, OPTIONS" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD, OPTIONS")
	}
	// RFC 9110 makes Allow mandatory on 405; a 405 without it tells the client
	// no more than the 404 it replaced.
	if rec.Header().Get("Allow") == "" {
		t.Error("405 carries no Allow header")
	}
	if code := errorCode(t, rec); code != CodeMethodNotAllowed {
		t.Errorf("error code = %q, want %q", code, CodeMethodNotAllowed)
	}
}

// TestUnknownPathStays404 is the property that keeps the 405 honest.
func TestUnknownPathStays404(t *testing.T) {
	mux := routerShapedMux()

	for _, target := range []string{
		"/api/v1/nonexistent",
		"/api/v1/fleeet",
		"/api/v1/fleet/extra",
		"/api/v1/",
	} {
		for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodOptions} {
			rec := do(t, mux, method, target)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404; nothing in the table serves that path under any verb",
					method, target, rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow != "" {
				t.Errorf("%s %s advertised Allow=%q for a path that does not exist", method, target, allow)
			}
		}
	}
}

// TestAllowHeaderContents pins the exact header for each shape in the table.
func TestAllowHeaderContents(t *testing.T) {
	mux := routerShapedMux()

	cases := []struct {
		method, target, allow, route string
	}{
		// A read-only route. HEAD is included because net/http's mux serves it
		// from the GET pattern, so a client that sends HEAD will succeed.
		{http.MethodPost, "/api/v1/fleet", "GET, HEAD, OPTIONS", "/api/v1/fleet"},
		{http.MethodPut, "/api/v1/jobs", "GET, HEAD, OPTIONS, POST", "/api/v1/jobs"},
		// Write-only: no GET is registered, so no GET is advertised.
		{http.MethodGet, "/api/v1/leases/acquire", "OPTIONS, POST", "/api/v1/leases/acquire"},
		// A wildcard route resolves to the pattern, not the concrete path, so
		// the reported route is bounded by the routing table.
		{http.MethodPost, "/api/v1/artifacts/" + hexRun(64), "DELETE, GET, HEAD, OPTIONS", "/api/v1/artifacts/{sha}"},
		// A verb the server has never heard of is still answered with the set
		// that would have worked.
		{"FROB", "/api/v1/fleet", "GET, HEAD, OPTIONS", "/api/v1/fleet"},
	}

	for _, tc := range cases {
		rec := do(t, mux, tc.method, tc.target)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.target, rec.Code)
			continue
		}
		if got := rec.Header().Get("Allow"); got != tc.allow {
			t.Errorf("%s %s: Allow = %q, want %q", tc.method, tc.target, got, tc.allow)
		}
		if got := errorDetailString(t, rec, "path"); got != tc.route {
			t.Errorf("%s %s: detail.path = %q, want %q", tc.method, tc.target, got, tc.route)
		}
	}
}

// TestAllowNeverListsTheMethodThatJustFailed guards the one entry a client
// would act on by retrying the request it already sent.
//
// It calls allowedMethods directly, and it has to. Through the mux the guard
// is unobservable: a request only reaches the fallback because the mux found
// no method-qualified route for THAT verb, so re-probing the same verb returns
// the catch-all again and the entry could never have appeared. Asserting it
// end-to-end therefore passes with the guard deleted — it did — and the
// property is worth more than that, because the day this handler is mounted
// somewhere the mux has not already ruled that verb out, an Allow header
// naming the verb that just failed is a client retrying forever.
func TestAllowNeverListsTheMethodThatJustFailed(t *testing.T) {
	mux := routerShapedMux()

	// GET /api/v1/fleet is registered, so this is exactly the case the mux
	// would never deliver here.
	allow, route := allowedMethods(mux, httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil))
	if slices.Contains(allow, http.MethodGet) {
		t.Errorf("allowedMethods = %v for a GET, and lists GET: the client is told to retry what just failed", allow)
	}
	if !slices.Contains(allow, http.MethodHead) {
		t.Errorf("allowedMethods = %v, want HEAD: the mux serves HEAD from the GET pattern", allow)
	}
	if route != "/api/v1/fleet" {
		t.Errorf("route = %q, want /api/v1/fleet", route)
	}

	// And the same property as a client sees it.
	rec := do(t, mux, http.MethodPost, "/api/v1/fleet")
	for _, part := range strings.Split(rec.Header().Get("Allow"), ", ") {
		if part == http.MethodPost {
			t.Fatalf("Allow = %q lists POST, the method that was just refused", rec.Header().Get("Allow"))
		}
	}
}

// TestOptionsIsDiscoveryNotAnError: OPTIONS on a known path is a client asking
// what it may do, and answering 405 to that would put ordinary discovery into
// the 4xx rate an operator reads as clients getting things wrong.
func TestOptionsIsDiscoveryNotAnError(t *testing.T) {
	mux := routerShapedMux()

	rec := do(t, mux, http.MethodOptions, "/api/v1/jobs")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS /api/v1/jobs = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD, OPTIONS, POST" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD, OPTIONS, POST")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 carries a %d byte body", rec.Body.Len())
	}
}

// refusingAuth is an Authenticator that identifies nobody, which is what an
// anonymous caller looks like to the real ones.
type refusingAuth struct{}

func (refusingAuth) Authenticate(*http.Request) (Identity, error) {
	return Identity{}, ErrUnauthenticated
}
func (refusingAuth) Name() string { return "refusing" }

// TestAPIFallbackAnswersAnonymousCallersNothing.
//
// An unauthenticated 404 map is a free inventory of the control plane. An
// unauthenticated 405 map is a strictly better one — it enumerates the paths
// AND the verbs each accepts — so the method check may only ever run for a
// caller the server already trusts. The credential check therefore lives
// inside apiFallback rather than in the line that mounts it, and this is the
// test that says so: not "there is a requireRole somewhere", but "an anonymous
// request learns nothing about the table".
func TestAPIFallbackAnswersAnonymousCallersNothing(t *testing.T) {
	srv := &Server{auth: refusingAuth{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/fleet", func(w http.ResponseWriter, r *http.Request) {})
	mux.Handle("/api/v1/", srv.apiFallback(mux))

	for _, tc := range [][2]string{
		{http.MethodDelete, "/api/v1/fleet"},    // would be 405 + Allow
		{http.MethodOptions, "/api/v1/fleet"},   // would be 204 + Allow
		{http.MethodGet, "/api/v1/nonexistent"}, // would be 404
	} {
		rec := do(t, mux, tc[0], tc[1])
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc[0], tc[1], rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "" {
			t.Errorf("%s %s handed an anonymous caller Allow=%q", tc[0], tc[1], allow)
		}
	}
}

// TestRegisteredMethodsAreUntouched: the fallback must be invisible to every
// request that already matched.
func TestRegisteredMethodsAreUntouched(t *testing.T) {
	mux := routerShapedMux()

	for _, tc := range [][2]string{
		{http.MethodGet, "/api/v1/fleet"},
		{http.MethodHead, "/api/v1/fleet"},
		{http.MethodPost, "/api/v1/jobs"},
		{http.MethodDelete, "/api/v1/artifacts/" + hexRun(64)},
	} {
		rec := do(t, mux, tc[0], tc[1])
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s = %d, want 200", tc[0], tc[1], rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "" {
			t.Errorf("%s %s: a matched route emitted Allow=%q", tc[0], tc[1], allow)
		}
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/artifacts/{sha}/content
// ---------------------------------------------------------------------------

// memBlob is an artifacts.Blob over a byte slice. bytes.Reader supplies both
// halves of the contract, ReaderAt included, which is what the range path
// needs.
type memBlob struct {
	*bytes.Reader
	closed bool
}

func (b *memBlob) Close() error {
	b.closed = true
	return nil
}

// contentFixture builds a mux serving one artifact's bytes under its digest.
func contentFixture(t *testing.T, payload []byte, kind artifacts.Kind, name string) (http.Handler, string, *memBlob) {
	t.Helper()
	sum := sha256.Sum256(payload)
	sha := hex.EncodeToString(sum[:])

	art := artifacts.Artifact{
		SHA256:    sha,
		Kind:      kind,
		Name:      name,
		Size:      int64(len(payload)),
		CreatedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
	}
	blob := &memBlob{Reader: bytes.NewReader(payload)}

	open := func(_ context.Context, got string) (artifacts.Blob, artifacts.Artifact, error) {
		if got != sha {
			return nil, artifacts.Artifact{}, artifacts.ErrNotFound
		}
		return blob, art, nil
	}
	fail := func(w http.ResponseWriter, _ *http.Request, op string, err error) {
		t.Helper()
		t.Errorf("content handler failed: op=%q err=%v", op, err)
		w.WriteHeader(http.StatusInternalServerError)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/artifacts/{sha}/content", contentHandler(open, fail))
	return mux, sha, blob
}

// TestContentServesBytesThatHashToTheirOwnName is the only assertion that
// actually proves the endpoint works. Content addressed by sha256 that serves
// different bytes is worse than serving nothing, so the body is re-hashed and
// compared against the name it was fetched under.
func TestContentServesBytesThatHashToTheirOwnName(t *testing.T) {
	// Deliberately not a round number and larger than one buffer, so a
	// truncating or short-reading implementation cannot pass by accident.
	payload := make([]byte, 1<<16+4097)
	for i := range payload {
		payload[i] = byte(i*31 + 7)
	}

	mux, sha, blob := contentFixture(t, payload, artifacts.KindAPK, "app-release.apk")
	rec := do(t, mux, http.MethodGet, "/api/v1/artifacts/"+sha+"/content")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	got := sha256.Sum256(rec.Body.Bytes())
	if hex.EncodeToString(got[:]) != sha {
		t.Fatalf("the content route served bytes hashing to %s under the name %s",
			hex.EncodeToString(got[:]), sha)
	}
	if n := rec.Body.Len(); n != len(payload) {
		t.Errorf("served %d bytes, want %d", n, len(payload))
	}

	h := rec.Header()
	if got, want := h.Get("Content-Length"), strconv.Itoa(len(payload)); got != want {
		t.Errorf("Content-Length = %q, want %q", got, want)
	}
	// The digest IS the etag: a strong validator, exact by construction.
	if got, want := h.Get("Etag"), `"`+sha+`"`; got != want {
		t.Errorf("ETag = %q, want %q", got, want)
	}
	if got, want := h.Get("Content-Type"), "application/vnd.android.package-archive"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got := h.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	// Uploaded content is served from the same origin as the dashboard, so it
	// must never be sniffed into an executable type.
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if !blob.closed {
		t.Error("the blob was not closed; a handler that leaks descriptors runs the control plane out of them")
	}
}

// TestContentRangeServesTheRequestedBytes proves the range support is real,
// which is what makes resuming a 200 MB APK cost the remainder instead of the
// whole thing.
func TestContentRangeServesTheRequestedBytes(t *testing.T) {
	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	mux, sha, _ := contentFixture(t, payload, artifacts.KindFile, "block.bin")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/"+sha+"/content", nil)
	req.Header.Set("Range", "bytes=10-19")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got, want := rec.Body.String(), string(payload[10:20]); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Content-Range"), fmt.Sprintf("bytes 10-19/%d", len(payload)); got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
}

// TestContentEtagRevalidates: an immutable body under a strong validator should
// cost nothing to re-check.
func TestContentEtagRevalidates(t *testing.T) {
	payload := []byte("the bytes a job names")
	mux, sha, _ := contentFixture(t, payload, artifacts.KindFile, "note.txt")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/"+sha+"/content", nil)
	req.Header.Set("If-None-Match", `"`+sha+`"`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304 for a matching etag", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 carried a %d byte body", rec.Body.Len())
	}
}

// TestContentRejectsANonDigestPath keeps the path variable from reaching the
// backend, where it becomes a filesystem path.
//
// The fixture's resolver fails the test if it is ever called, so "never
// reached" is asserted as well as "not 200": the guard has to be in front of
// the store, not inside it.
func TestContentRejectsANonDigestPath(t *testing.T) {
	mux, _, _ := contentFixture(t, []byte("x"), artifacts.KindFile, "x")

	for _, bad := range []string{"not-a-digest", hexRun(63), hexRun(64) + "0", "DEADBEEF"} {
		rec := do(t, mux, http.MethodGet, "/api/v1/artifacts/"+bad+"/content")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET .../%s/content = %d, want 400", bad, rec.Code)
		}
	}

	// A LITERAL traversal never reaches the handler: net/http cleans the path
	// first and answers with a redirect to the cleaned form. Asserted rather
	// than assumed, because "the mux handles it" is exactly the kind of belief
	// that stops being true.
	rec := do(t, mux, http.MethodGet, "/api/v1/artifacts/../../etc/passwd/content")
	if rec.Code == http.StatusOK {
		t.Error("a traversal path was served with 200")
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "..") {
		t.Errorf("traversal redirected to %q, which still carries a traversal", loc)
	}

	// A PERCENT-ENCODED traversal is the case that matters, and it is the
	// opposite of the one above: net/http cleans the escaped path, so "%2e%2e"
	// is not a dot-segment to the mux, the route matches, and the handler is
	// entered with PathValue("sha") decoded back into "../../etc/passwd". The
	// digest check is the only thing standing between that and a filepath.Join
	// inside the backend, so it is asserted here on the exact input that
	// reaches it rather than on the one the mux already ate.
	for _, bad := range []string{
		"%2e%2e",
		"%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"..%2fx",
	} {
		rec := do(t, mux, http.MethodGet, "/api/v1/artifacts/"+bad+"/content")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET .../%s/content = %d, want 400; an encoded traversal reaches the handler decoded",
				bad, rec.Code)
		}
	}
}

// TestBlobDispositionRendersAnUntrustedNameExactly: the name arrived in a
// query string and reaches a response header.
//
// The renderings are pinned exactly rather than checked for the absence of
// control characters, and that is the whole point of this test. "No CR in the
// output" is satisfied by mime.FormatMediaType all by itself — it
// percent-encodes a control character instead of passing it through — so an
// assertion at that level passes with the sanitising in this file deleted, and
// says nothing at all about the transformation that actually matters. A
// separator is NOT something FormatMediaType touches: it will render
// filename="../../etc/passwd" without complaint, and some client will write it.
func TestBlobDispositionRendersAnUntrustedNameExactly(t *testing.T) {
	cases := []struct{ name, want string }{
		{"plain.apk", "attachment; filename=plain.apk"},
		// The CRLF is gone, not escaped: the filename stays readable.
		{"app.apk\r\nX-Evil: 1", `attachment; filename="app.apkX-Evil: 1"`},
		// The traversal is neutralised in the value itself.
		{"../../etc/passwd", "attachment; filename=.._.._etc_passwd"},
		{`C:\builds\app.apk`, `attachment; filename="C:_builds_app.apk"`},
		// A name that is nothing but control characters yields no filename,
		// rather than a filename made of escaped NULs.
		{"\x00\x01", "attachment"},
		{"", "attachment"},
		{"   ", "attachment"},
		// Non-ASCII survives, through the encoding RFC 2231 defines for it.
		{"relat\u00f3rio.apk", "attachment; filename*=utf-8''relat%C3%B3rio.apk"},
	}

	for _, tc := range cases {
		got := blobDisposition(tc.name)
		if got != tc.want {
			t.Errorf("blobDisposition(%q)\n got %q\nwant %q", tc.name, got, tc.want)
		}
		for _, bad := range []string{"\r", "\n", "\x00"} {
			if bytes.Contains([]byte(got), []byte(bad)) {
				t.Errorf("blobDisposition(%q) = %q, which carries a raw control character", tc.name, got)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Blob GC
// ---------------------------------------------------------------------------

// TestBlobGCEnumerateTouchesOnlyProperlyFiledDigests is the safety property of
// the sweep. Everything it will later consider deleting comes from here, so
// anything this refuses to enumerate can never be collected — including the
// staging directory an upload is writing into right now.
func TestBlobGCEnumerateTouchesOnlyProperlyFiledDigests(t *testing.T) {
	root := t.TempDir()
	sha := hexRun(62) + "ab" // a valid digest that fans out to "00"

	write := func(rel string, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The one thing that is a blob.
	write("00/"+sha, "payload")
	// The staging directory: nameless bytes belonging to an upload that may
	// still be running. Never this sweep's business.
	write("_staging/put-123456", "half an apk")
	// A directory that is not a fan-out.
	write("backups/whatever", "operator's copy")
	// Right directory, wrong name.
	write("00/README", "notes")
	// Right name, wrong directory: it did not get there through DirBackend.
	write("ff/"+sha, "misfiled")
	// A stray at the root.
	write("manifest.json", "{}")

	g := &BlobGC{root: root}
	files, unrecognised, problems, err := g.enumerate()
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	if len(problems) != 0 {
		t.Errorf("problems = %v, want none", problems)
	}
	if len(files) != 1 {
		t.Fatalf("enumerated %d blobs, want exactly 1: %+v", len(files), files)
	}
	if files[0].sha != sha {
		t.Errorf("enumerated %s, want %s", files[0].sha, sha)
	}
	if files[0].size != int64(len("payload")) {
		t.Errorf("size = %d, want %d", files[0].size, len("payload"))
	}
	if unrecognised != 5 {
		t.Errorf("unrecognised = %d, want 5", unrecognised)
	}
}

// TestBlobGCGraceKeepsAYoungBlobWithoutAskingTheDatabase is the fence that
// makes the whole sweep safe to run beside live uploads.
//
// artifacts.Store.Put commits bytes and THEN writes the row that names them.
// Between those two statements a perfectly live artifact is indistinguishable
// from garbage, and the only thing that tells them apart is age. The server has
// no database here on purpose: g.srv.pool is nil, so if the grace fence let a
// young blob through to the reference scan this test would not report a wrong
// count, it would panic — which is the strongest available statement that the
// fence is applied BEFORE anything else, and that a sweep over nothing but
// young blobs is not merely harmless but silent.
func TestBlobGCGraceKeepsAYoungBlobWithoutAskingTheDatabase(t *testing.T) {
	root := t.TempDir()
	sha := hexRun(62) + "cd"
	dir := filepath.Join(root, "00")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(dir, sha)
	if err := os.WriteFile(blob, []byte("bytes whose row is still being written"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := &BlobGC{srv: &Server{}, root: root, grace: time.Hour}
	rep, err := g.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if rep.WithinGrace != 1 {
		t.Errorf("within_grace = %d, want 1", rep.WithinGrace)
	}
	if rep.Deleted != 0 || rep.FreedBytes != 0 {
		t.Errorf("deleted %d blobs (%d bytes); a blob younger than the grace period must survive",
			rep.Deleted, rep.FreedBytes)
	}
	if rep.Collectable != 0 {
		t.Errorf("collectable = %d, want 0", rep.Collectable)
	}
	if _, err := os.Stat(blob); err != nil {
		t.Errorf("the blob is gone from disk: %v", err)
	}
}

// gcFixture builds a sweep over a root holding exactly one blob, old enough
// that the grace fence is not what is under test, with the reference oracle
// supplied by the caller.
//
// answer is given the call number, because the ORDER of the three questions
// this sweep asks — the scan, the second look, and the confirmation after the
// unlink — is the entire design. A fixture that could not answer them
// differently could not tell the design from a coin toss.
func gcFixture(t *testing.T, payload string,
	answer func(call int, shas []string) (map[string]bool, error)) (g *BlobGC, sha, path string, calls *int) {
	t.Helper()

	root := t.TempDir()
	sha = hexRun(62) + "ef"
	dir := filepath.Join(root, "00")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, sha)
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	n := 0
	g = &BlobGC{
		srv:   &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		root:  root,
		grace: time.Hour,
	}
	g.refs = func(_ context.Context, shas []string) (map[string]bool, error) {
		n++
		return answer(n, shas)
	}
	return g, sha, path, &n
}

// TestBlobGCKeepsTheBytesWhenTheSecondLookFails is the fail-closed property,
// and it is the one that decides whether this sweep is safe to run at all.
//
// A database that answers "I do not know" is not a database that answered "no
// row names this". Bytes reclaimed on a failed question are content the farm
// told sixty phones it still had; disk kept on a failed question is disk the
// next sweep reclaims. The blob must survive, and — just as important — it
// must not appear in the collectable figure an operator reads as "this much is
// safe to free".
func TestBlobGCKeepsTheBytesWhenTheSecondLookFails(t *testing.T) {
	g, sha, path, calls := gcFixture(t, "orphaned bytes",
		func(call int, _ []string) (map[string]bool, error) {
			if call == 1 {
				return nil, nil // the scan: nothing names it
			}
			return nil, errors.New("connection reset by peer")
		})

	rep, err := g.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the blob was reclaimed on an unanswered question: %v", statErr)
	}
	if rep.Deleted != 0 || rep.FreedBytes != 0 {
		t.Errorf("deleted=%d freed=%d, want 0/0", rep.Deleted, rep.FreedBytes)
	}
	if rep.Collectable != 0 || rep.ReclaimableBytes != 0 {
		t.Errorf("collectable=%d (%d bytes): a blob the sweep could not ask about is not known to be collectable",
			rep.Collectable, rep.ReclaimableBytes)
	}
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], sha) {
		t.Errorf("problems = %v, want one naming %s", rep.Problems, sha)
	}
	if *calls != 2 {
		t.Errorf("asked the database %d times, want 2 (the scan and the second look)", *calls)
	}
}

// TestBlobGCKeepsABlobAdoptedBetweenTheScanAndTheUnlink is the race the second
// look exists for.
//
// An upload of content that already sits on disk as an orphan does NOT refresh
// the file's mtime — DirBackend's Commit sees the digest present and discards
// the staged copy — so the grace fence cannot catch this one. Only asking again
// can.
func TestBlobGCKeepsABlobAdoptedBetweenTheScanAndTheUnlink(t *testing.T) {
	g, sha, path, _ := gcFixture(t, "content someone just re-uploaded",
		func(call int, _ []string) (map[string]bool, error) {
			if call == 1 {
				return nil, nil
			}
			return map[string]bool{hexRun(62) + "ef": true}, nil
		})

	rep, err := g.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("a blob that acquired a row mid-sweep was deleted anyway: %v", statErr)
	}
	if rep.Adopted != 1 {
		t.Errorf("adopted = %d, want 1 (%s)", rep.Adopted, sha)
	}
	if rep.Deleted != 0 || rep.Collectable != 0 {
		t.Errorf("deleted=%d collectable=%d, want 0/0", rep.Deleted, rep.Collectable)
	}
	if len(rep.Problems) != 0 {
		t.Errorf("problems = %v; losing a race to an upload is the mechanism working, not a fault", rep.Problems)
	}
}

// TestBlobGCNamesTheDigestItLostARaceFor: the window between the second look
// and the unlink is narrow, not closed. When something does land in it the
// sweep cannot undo the deletion, so the one thing it must not do is stay
// quiet — "re-upload exactly this content" is a repair an operator can carry
// out, and a hole discovered weeks later at provisioning time is not.
func TestBlobGCNamesTheDigestItLostARaceFor(t *testing.T) {
	g, sha, path, calls := gcFixture(t, "bytes lost to a race",
		func(call int, _ []string) (map[string]bool, error) {
			if call < 3 {
				return nil, nil // scan and second look: unnamed
			}
			return map[string]bool{hexRun(62) + "ef": true}, nil // the row appeared
		})

	rep, err := g.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("the blob is still on disk, so this test no longer exercises a lost race")
	}
	if rep.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", rep.Deleted)
	}
	if *calls != 3 {
		t.Fatalf("asked the database %d times, want 3; the confirmation after the unlink did not happen", *calls)
	}
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], sha) ||
		!strings.Contains(rep.Problems[0], "re-upload") {
		t.Errorf("problems = %v, want one naming %s and the repair", rep.Problems, sha)
	}
}

// TestBlobGCCollectsAnOrphanAndSaysExactlyWhatItFreed is the ordinary case,
// and the dry run beside it is the promise that a dry run is a report and not
// a rehearsal with side effects.
func TestBlobGCCollectsAnOrphanAndSaysExactlyWhatItFreed(t *testing.T) {
	const payload = "nothing in the farm names these bytes"
	unnamed := func(int, []string) (map[string]bool, error) { return nil, nil }

	g, sha, path, calls := gcFixture(t, payload, unnamed)

	dry, err := g.DryRun(context.Background())
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if !dry.DryRun || dry.Deleted != 0 {
		t.Errorf("dry run reported deleted=%d", dry.Deleted)
	}
	if dry.Collectable != 1 || dry.ReclaimableBytes != int64(len(payload)) {
		t.Errorf("dry run: collectable=%d bytes=%d, want 1/%d", dry.Collectable, dry.ReclaimableBytes, len(payload))
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("a dry run removed the blob: %v", statErr)
	}
	if *calls != 1 {
		t.Errorf("dry run asked the database %d times, want 1: there is no unlink to take a second look before",
			*calls)
	}

	rep, err := g.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if rep.Deleted != 1 || rep.FreedBytes != int64(len(payload)) {
		t.Errorf("deleted=%d freed=%d, want 1/%d", rep.Deleted, rep.FreedBytes, len(payload))
	}
	if len(rep.Problems) != 0 {
		t.Errorf("problems = %v, want none", rep.Problems)
	}
	if len(rep.Blobs) != 1 || rep.Blobs[0].SHA256 != sha || !rep.Blobs[0].Deleted {
		t.Errorf("blobs = %+v, want one entry for %s marked deleted", rep.Blobs, sha)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("the blob is still on disk after a sweep that reported freeing it")
	}
}

// TestBlobGCUnreadableRootIsAnErrorNotAnEmptySweep guards the silent failure
// this sweep is most likely to have: a backend whose directory moved, was
// unmounted, or was never created. Reporting "scanned 0, deleted 0" and 200 for
// that is indistinguishable from a clean store, and an operator running this
// from cron would read months of successful sweeps off a disk nobody looked at.
func TestBlobGCUnreadableRootIsAnErrorNotAnEmptySweep(t *testing.T) {
	g := &BlobGC{srv: &Server{}, root: filepath.Join(t.TempDir(), "not-created"), grace: time.Hour}

	rep, err := g.DryRun(context.Background())
	if err == nil {
		t.Fatalf("a sweep over an unreadable root reported success: %+v", rep)
	}
	if !strings.Contains(err.Error(), "blob root") {
		t.Errorf("error = %v, which does not say which directory could not be read", err)
	}
}

// TestQueryBoolRefusesEveryShapeThatIsNotAnAnswer: this parameter decides
// whether a disk gets smaller, so the only input that may be read as "no" is
// the absence of the parameter.
func TestQueryBoolRefusesEveryShapeThatIsNotAnAnswer(t *testing.T) {
	get := func(target string) (bool, error) {
		return queryBool(httptest.NewRequest(http.MethodPost, target, nil), "apply")
	}

	if v, err := get("/api/v1/artifacts/gc"); err != nil || v {
		t.Errorf("absent apply = (%v, %v), want (false, nil)", v, err)
	}
	if v, err := get("/api/v1/artifacts/gc?apply=true"); err != nil || !v {
		t.Errorf("apply=true = (%v, %v), want (true, nil)", v, err)
	}
	if v, err := get("/api/v1/artifacts/gc?apply=false"); err != nil || v {
		t.Errorf("apply=false = (%v, %v), want (false, nil)", v, err)
	}

	for _, target := range []string{
		// A bare flag is what an operator types when they mean it. Reading it
		// as "dry run" sweeps nothing and reports success.
		"/api/v1/artifacts/gc?apply",
		"/api/v1/artifacts/gc?apply=",
		"/api/v1/artifacts/gc?apply=yes%20please",
		// Two answers is not an answer, and picking one of them silently picks
		// it for a disk.
		"/api/v1/artifacts/gc?apply=true&apply=false",
	} {
		if v, err := get(target); err == nil {
			t.Errorf("%s = (%v, nil), want an error", target, v)
		}
	}
}

// stubBackend satisfies artifacts.Backend and nothing more, which is the point:
// the three-verb seam cannot be enumerated, so a sweep over it must be refused
// rather than attempted.
type stubBackend struct{}

func (stubBackend) Create(context.Context) (artifacts.BlobWriter, error) { return nil, nil }
func (stubBackend) Open(context.Context, string) (artifacts.Blob, int64, error) {
	return nil, 0, nil
}
func (stubBackend) URL(string) string { return "" }

func TestNewBlobGCRefusesWhatItCannotSweep(t *testing.T) {
	if _, err := NewBlobGC(&Server{}, stubBackend{}); err == nil {
		t.Fatal("NewBlobGC accepted a backend it cannot enumerate")
	}

	dir, err := artifacts.NewDirBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewDirBackend: %v", err)
	}
	if _, err := NewBlobGC(&Server{}, dir); err != nil {
		t.Fatalf("NewBlobGC(DirBackend): %v", err)
	}
}

// TestNewBlobGCRefusesAGraceThatRacesUploads: Store.Put commits the bytes and
// then writes the row. A sweep whose grace period is shorter than that gap
// deletes content whose row is being written right now.
func TestNewBlobGCRefusesAGraceThatRacesUploads(t *testing.T) {
	dir, err := artifacts.NewDirBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewDirBackend: %v", err)
	}

	if _, err := NewBlobGC(&Server{}, dir, WithBlobGCGrace(time.Second)); err == nil {
		t.Fatal("NewBlobGC accepted a one-second grace period")
	}
	g, err := NewBlobGC(&Server{}, dir, WithBlobGCGrace(2*time.Hour))
	if err != nil {
		t.Fatalf("NewBlobGC: %v", err)
	}
	if g.grace != 2*time.Hour {
		t.Errorf("grace = %s, want 2h", g.grace)
	}
	if def, err := NewBlobGC(&Server{}, dir); err != nil || def.grace != DefaultBlobGCGrace {
		t.Errorf("default grace = %v (err %v), want %s", def, err, DefaultBlobGCGrace)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// hexRun is n zero hex digits: a syntactically valid digest prefix, so a test
// exercises the routing and not artifactSHA's rejection of it.
func hexRun(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v (body %q)", err, rec.Body.String())
	}
	return body.Error.Code
}

func errorDetailString(t *testing.T, rec *httptest.ResponseRecorder, key string) string {
	t.Helper()
	var body struct {
		Error struct {
			Detail map[string]any `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v (body %q)", err, rec.Body.String())
	}
	s, _ := body.Error.Detail[key].(string)
	return s
}
