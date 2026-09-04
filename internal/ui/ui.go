// Package ui serves the operator dashboard: one HTML page, one stylesheet,
// one script, embedded in the farmd binary.
//
// # Why embedded, and why no build step
//
// This is the surface a human stares at while a rack is on fire. Every
// dependency between "an operator opens a browser" and "the operator sees the
// fleet" is a way for that surface to be unavailable exactly when it matters:
// an npm registry outage, an unreachable CDN, a sidecar that did not schedule,
// a static bucket in another region. The dashboard is therefore plain
// HTML/CSS/vanilla JS compiled into the same static binary as the control
// plane. If farmd is running, the dashboard is running, at the same commit,
// with no network fetch beyond the page itself.
//
// # Caching
//
// Assets are served with a strong ETag and Cache-Control: no-cache, which
// means "you may keep it, but revalidate every time". A conditional GET costs
// one round trip and 304 bytes; a stale dashboard during an incident costs an
// operator acting on data that is not on the screen. That trade is not close,
// so no asset is ever served from cache without revalidation. Immutable
// content-hashed URLs are the usual alternative, but they require a build
// step, which is the thing this package exists to avoid.
//
// # Dev mode
//
// Handler(WithDevDir(dir)) — or the FARM_UI_DEV_DIR environment variable —
// reads the three files from disk on every request with Cache-Control:
// no-store, so an editor save plus a browser reload is the whole edit loop.
// Production is the embedded path and needs no flag.
package ui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
)

// EnvDevDir names the directory to serve the dashboard from instead of the
// embedded copy. Set it to internal/ui/assets during development.
const EnvDevDir = "FARM_UI_DEV_DIR"

// The embed patterns are explicit rather than a directory glob so that a stray
// editor swap file or a .DS_Store in assets/ cannot become a served asset.
//
//go:embed assets/index.html assets/app.js assets/docs.js assets/style.css
//go:embed assets/docs
var embedded embed.FS

// Option configures Handler.
type Option func(*settings)

type settings struct {
	devDir string
}

// WithDevDir serves the dashboard from dir on disk instead of the embedded
// copy, re-reading every file on every request. It overrides FARM_UI_DEV_DIR.
func WithDevDir(dir string) Option {
	return func(s *settings) { s.devDir = dir }
}

// Handler returns the dashboard handler. Mount it at "/" after the API routes;
// it claims only the asset paths it actually has and answers everything else
// with the API's JSON error envelope, so a mistyped /api/v2/fleet still reads
// as an API error rather than as an HTML page.
//
// It panics only on a defect in this package's own embedding, which is a build
// time property and cannot depend on runtime input.
func Handler(opts ...Option) http.Handler {
	s := settings{devDir: os.Getenv(EnvDevDir)}
	for _, o := range opts {
		o(&s)
	}
	if s.devDir != "" {
		return &diskHandler{root: os.DirFS(s.devDir), dir: s.devDir}
	}
	sub, err := fs.Sub(embedded, "assets")
	if err != nil {
		panic(fmt.Sprintf("ui: embedded assets unreadable: %v", err))
	}
	h, err := newEmbeddedHandler(sub)
	if err != nil {
		panic(fmt.Sprintf("ui: embedded assets unreadable: %v", err))
	}
	return h
}

// asset is one prepared representation set: the bytes, an optional gzip
// encoding of the same bytes, and a distinct ETag for each, because two
// encodings of one resource are two representations and sharing an ETag
// between them corrupts intermediary caches.
type asset struct {
	name    string
	ctype   string
	raw     []byte
	rawETag string
	gz      []byte
	gzETag  string
}

type embeddedHandler struct {
	assets map[string]*asset
}

func newEmbeddedHandler(fsys fs.FS) (*embeddedHandler, error) {
	h := &embeddedHandler{assets: map[string]*asset{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		h.assets[p] = prepare(p, b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The page is useless without the script and the stylesheet it names, and a
	// binary that serves a blank dashboard during an incident is worse than one
	// that refuses to start: the first is discovered by an operator at 3am, the
	// second at build time. Handler turns this into a panic for that reason.
	for _, required := range [...]string{"index.html", "app.js", "docs.js", "style.css", "docs/index.json"} {
		if _, ok := h.assets[required]; !ok {
			return nil, fmt.Errorf("%s missing from the embedded assets", required)
		}
	}
	return h, nil
}

func prepare(name string, b []byte) *asset {
	a := &asset{
		name:    name,
		ctype:   contentType(name),
		raw:     b,
		rawETag: etag(b, "i"),
	}
	// Below ~1 KiB the gzip header and the extra branch cost more than the
	// saving; above it the dashboard's JS and CSS compress by roughly 4x on a
	// link an operator may be reaching over a VPN.
	if len(b) >= 1024 && compressible(a.ctype) {
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err == nil {
			if _, err := zw.Write(b); err == nil && zw.Close() == nil && buf.Len() < len(b) {
				a.gz = buf.Bytes()
				a.gzETag = etag(a.gz, "g")
			}
		}
	}
	return a
}

func (h *embeddedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := resolve(w, r)
	if !ok {
		return
	}
	a, ok := h.assets[name]
	if !ok {
		notFound(w, r)
		return
	}
	// no-cache, not max-age: keep the copy, but never paint it without asking
	// the server first. See the package comment.
	serve(w, r, a, "no-cache")
}

// diskHandler is the dev-mode twin. It re-reads on every request so that the
// edit loop is "save, reload", and it never lets a browser keep a copy.
type diskHandler struct {
	root fs.FS
	dir  string
}

func (h *diskHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := resolve(w, r)
	if !ok {
		return
	}
	f, err := h.root.Open(name) // os.DirFS rejects "..", so name cannot escape dir
	if err != nil {
		// Name the directory. The overwhelmingly common cause of a 404 in dev
		// mode is FARM_UI_DEV_DIR pointing somewhere that is not internal/ui/
		// assets, and "no such path: /app.js" sends the reader looking for a
		// bug in the router instead of at their own environment.
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("%s is not in the dev asset directory %s (%s=%s serves the dashboard from disk)",
				name, h.dir, EnvDevDir, h.dir))
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.IsDir() {
		// Reading a directory would fail further down with a message about a
		// bad file descriptor, which describes nothing.
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("%s is a directory in %s, not a dashboard asset", name, h.dir))
		return
	}
	b, err := io.ReadAll(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read_failed",
			fmt.Sprintf("reading %s from %s: %v", name, h.dir, err))
		return
	}
	a := &asset{name: name, ctype: contentType(name), raw: b, rawETag: etag(b, "d")}
	serve(w, r, a, "no-store")
}

// resolve maps a request path to an asset name, and answers the request itself
// when the method is wrong. "/" is the dashboard; every other path is a
// literal file name, since the app routes inside the page with the URL hash
// and therefore needs no history-API fallback.
func resolve(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			r.Method+" is not allowed on the dashboard")
		return "", false
	}
	p := path.Clean("/" + r.URL.Path)
	if p == "/" {
		return "index.html", true
	}
	return strings.TrimPrefix(p, "/"), true
}

func serve(w http.ResponseWriter, r *http.Request, a *asset, cache string) {
	body, tag, enc := a.raw, a.rawETag, ""
	if a.gz != nil && acceptsGzip(r) {
		body, tag, enc = a.gz, a.gzETag, "gzip"
	}

	head := w.Header()
	head.Set("Content-Type", a.ctype)
	head.Set("Cache-Control", cache)
	head.Set("ETag", tag)
	head.Set("Vary", "Accept-Encoding")
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Referrer-Policy", "same-origin")
	if enc != "" {
		head.Set("Content-Encoding", enc)
	}
	if a.ctype == typeHTML {
		// The dashboard loads nothing from anywhere but its own origin: no
		// CDN, no fonts, no analytics. Saying so in a policy turns any future
		// accidental external reference into a console error instead of a
		// silent dependency.
		//
		// Neither script nor style gets 'unsafe-inline'. The page carries no
		// <style> block and no style attribute; the one thing it sizes at
		// runtime, the battery meter, is written through the CSSOM
		// (element.style.width), which CSP does not govern and 'unsafe-inline'
		// would not be needed for. Granting it anyway would buy nothing and
		// would disarm the protection for the injected <style> that a future
		// mistake could introduce.
		head.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; "+
				"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; "+
				"base-uri 'none'; object-src 'none'; form-action 'none'")
	}

	if noneMatch(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	head.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

const typeHTML = "text/html; charset=utf-8"

func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return typeHTML
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json; charset=utf-8"
	case ".ico":
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

func compressible(ctype string) bool {
	return strings.HasPrefix(ctype, "text/") ||
		strings.HasPrefix(ctype, "application/json") ||
		strings.HasPrefix(ctype, "image/svg")
}

// etag derives a strong validator from the bytes. The kind byte distinguishes
// representations (identity, gzip, disk) so two encodings never collide.
func etag(b []byte, kind string) string {
	sum := sha256.Sum256(b)
	return `"` + kind + hex.EncodeToString(sum[:12]) + `"`
}

func noneMatch(header, tag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		// A weak comparison is the correct one for If-None-Match, so W/"x"
		// and "x" match; our own tags are always strong.
		if strings.TrimPrefix(candidate, "W/") == strings.TrimPrefix(tag, "W/") {
			return true
		}
	}
	return false
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		field := strings.TrimSpace(part)
		name, params, _ := strings.Cut(field, ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		// "gzip;q=0" is an explicit refusal, not an offer.
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if q, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && q == 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "no such path: "+r.URL.Path)
}

// writeError emits the same envelope as the rest of the API — a JSON error
// from /api/v1/fleet and a JSON error from a mistyped asset path should be
// parseable by the same client code.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}
