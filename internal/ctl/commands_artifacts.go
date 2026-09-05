package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// `ctl artifacts gc` and `ctl artifacts delete` are the two verbs that make
// the artifact store smaller, and they shrink different things.
//
// delete removes the farm.artifacts ROW: the farm forgets the content exists,
// no spec can reference it and no push can skip it. The bytes stay. The server
// says so in its reply — "blob_retained": true — because content addressing
// means an unreferenced blob is inert, a later upload of identical bytes
// adopts it, and removing it is a sweep over the whole store rather than a
// side effect of one request that might be racing an upload of the same
// digest.
//
// gc is that sweep. Until it had a caller, disk under the blob root only ever
// grew: DELETE kept the bytes by design and nothing else touched them. It is
// dry by default and --apply is the opt-in, the same way the route treats
// ?apply, because the two invocations differ only in whether a disk gets
// smaller and the safer of the two is what a mistyped command should do.
//
// Neither verb goes anywhere near a lease. A blob is bytes on the API host's
// disk; no device holds one, no fence guards one, and nothing here can end a
// run.

// blobGCEntry is one blob a sweep considered collectable, as the API reports
// it.
type blobGCEntry struct {
	SHA256     string    `json:"sha256"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
	Deleted    bool      `json:"deleted"`
}

// blobGCReport is the shape of POST /api/v1/artifacts/gc.
//
// The counts are exact and the list is capped: a dry run that listed twelve
// blobs while the apply removed nine hundred would be a lie in the direction
// that costs disk nobody expected to lose, so the counts are what this
// command renders first and the list is what it renders last.
type blobGCReport struct {
	DryRun       bool      `json:"dry_run"`
	Root         string    `json:"root"`
	GraceSeconds int64     `json:"grace_seconds"`
	StartedAt    time.Time `json:"started_at"`
	DurationMS   int64     `json:"duration_ms"`

	Scanned      int   `json:"scanned"`
	ScannedBytes int64 `json:"scanned_bytes"`
	Unrecognised int   `json:"unrecognised"`
	Referenced   int   `json:"referenced"`
	WithinGrace  int   `json:"within_grace"`

	Collectable      int   `json:"collectable"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	Deleted          int   `json:"deleted"`
	FreedBytes       int64 `json:"freed_bytes"`
	Adopted          int   `json:"adopted"`

	Blobs     []blobGCEntry `json:"blobs"`
	Truncated bool          `json:"truncated"`
	Problems  []string      `json:"problems"`
}

// blobGCClientTimeout is the per-request deadline a sweep gets when the
// operator did not set one.
//
// The server bounds one sweep at five minutes; ctl's ordinary deadline is
// thirty seconds. A store with a few hundred thousand blobs can legitimately
// take longer than the latter to enumerate, and a client that hung up at
// thirty seconds would report a failure for a sweep that then finished — and,
// on --apply, would report a failure for bytes that were removed.
const blobGCClientTimeout = 6 * time.Minute

func cmdArtifactsGC(ctx context.Context, s *session, args []string) error {
	fs := newFlags("artifacts gc", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	apply := fs.Bool("apply", false, "remove the collectable blobs; without it the sweep only reports")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageErrf("usage: ctl artifacts gc [--apply --reason r]")
	}
	if !flagGiven(fs, "timeout") {
		g.timeout = blobGCClientTimeout
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}

	if !*apply {
		rep, raw, err := runBlobGC(ctx, e, false)
		if err != nil {
			return err
		}
		return e.renderBlobGC(rep, raw)
	}

	if err := e.requireReason("artifacts gc --apply"); err != nil {
		return err
	}

	// The preflight is the dry run. It is the same sweep with the same
	// answer, so the numbers the operator approves are the numbers the apply
	// will act on — minus whatever an upload adopts in between, which the
	// apply's second look catches and reports.
	preview, _, err := runBlobGC(ctx, e, false)
	if err != nil {
		return err
	}
	if preview.Collectable == 0 {
		e.out.Text("nothing is collectable under %s: %d blob(s) scanned, %d referenced, %d within the %s grace. "+
			"Nothing was removed.", preview.Root, preview.Scanned, preview.Referenced, preview.WithinGrace,
			duration(preview.GraceSeconds))
		return blobGCOutcome(preview)
	}

	f := &Fields{}
	f.Add("blob root", preview.Root)
	f.Addf("grace", "%s — a blob younger than this is never touched", duration(preview.GraceSeconds))
	f.Addf("scanned", "%d blob(s), %s", preview.Scanned, bytesCell(preview.ScannedBytes))
	f.Addf("referenced", "%d — a row names them, kept", preview.Referenced)
	f.Addf("within grace", "%d — too young to judge, kept", preview.WithinGrace)
	f.Addf("collectable", "%d blob(s), %s", preview.Collectable, bytesCell(preview.ReclaimableBytes))
	if preview.Truncated {
		f.Addf("listed", "%d of %d — the count above is exact, the list is capped",
			len(preview.Blobs), preview.Collectable)
	}
	headline := fmt.Sprintf("About to DELETE %d blob(s), %s, from the artifact store.\n"+
		"Nothing names them — no farm.artifacts row, no device ledger row — and each is older than the\n"+
		"grace period. A later upload of identical bytes stores them again; nothing else brings them back.\n"+
		"No device, lease or job is involved: these are bytes on the API host's disk.",
		preview.Collectable, bytesCell(preview.ReclaimableBytes))
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	rep, raw, err := runBlobGC(ctx, e, true)
	if err != nil {
		// A sweep that removed bytes and then lost its connection looks like
		// one that never ran. The dry run settles it: what it lists now is
		// what is still there.
		return e.unknownOutcome(err, "ctl artifacts gc")
	}
	return e.renderBlobGC(rep, raw)
}

// runBlobGC issues one sweep. apply is carried in the query string because
// that is where the route reads it, and the reason rides beside it for the
// same reason: the request has no body to put either in.
func runBlobGC(ctx context.Context, e *env, apply bool) (blobGCReport, json.RawMessage, error) {
	q := url.Values{}
	if apply {
		q.Set("apply", "true")
		setIf(q, "reason", e.reason)
	}
	var rep blobGCReport
	raw, err := e.client.PostQuery(ctx, apiPrefix+"/artifacts/gc", q, nil)
	if err != nil {
		return rep, nil, err
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return rep, raw, fmt.Errorf("POST %s/artifacts/gc: the response did not decode: %w", apiPrefix, err)
	}
	return rep, raw, nil
}

// renderBlobGC prints a sweep's report and then honours its outcome, in that
// order and in every format: a sweep that could not read one fan-out
// directory still reclaimed the rest, and the operator has to see both.
func (e *env) renderBlobGC(rep blobGCReport, raw json.RawMessage) error {
	if e.format == FormatJSON {
		if err := e.out.RawJSON(raw); err != nil {
			return err
		}
		return blobGCOutcome(rep)
	}

	f := &Fields{}
	if rep.DryRun {
		f.Add("mode", "DRY RUN — nothing was removed")
	} else {
		f.Add("mode", "APPLIED")
	}
	f.Add("blob root", rep.Root)
	f.Addf("grace", "%s — a blob younger than this is never touched", duration(rep.GraceSeconds))
	f.Addf("took", "%s", millis(rep.DurationMS))
	f.Gap()
	f.Addf("scanned", "%d blob(s), %s", rep.Scanned, bytesCell(rep.ScannedBytes))
	f.Addf("unrecognised", "%s not filed as a digest — never touched",
		plural(rep.Unrecognised, "entry", "entries"))
	f.Addf("referenced", "%d — a row names them, kept", rep.Referenced)
	f.Addf("within grace", "%d — too young to judge, kept", rep.WithinGrace)
	f.Addf("collectable", "%d blob(s), %s", rep.Collectable, bytesCell(rep.ReclaimableBytes))
	if !rep.DryRun {
		f.Addf("deleted", "%d blob(s), %s freed", rep.Deleted, bytesCell(rep.FreedBytes))
		if rep.Adopted > 0 {
			// Not a failure: the second look before each unlink is what
			// caught it, and the bytes stayed.
			f.Addf("adopted", "%d — claimed by an upload between the scan and the unlink, kept", rep.Adopted)
		}
	}
	if err := e.out.Fields(f); err != nil {
		return err
	}

	if len(rep.Blobs) > 0 {
		e.out.Blank()
		// The digest is the blob's identity and is never abbreviated, for the
		// same reason the listing never abbreviates it: a truncated hash
		// resolves to nothing.
		var t *Table
		if rep.DryRun {
			t = NewTable("SHA256", "SIZE", "MODIFIED")
		} else {
			t = NewTable("SHA256", "SIZE", "MODIFIED", "DELETED")
		}
		t.MaxCell(72)
		for _, b := range rep.Blobs {
			mod := b.ModifiedAt
			if rep.DryRun {
				t.Row(b.SHA256, bytesCell(b.SizeBytes), stamp(&mod))
			} else {
				t.Row(b.SHA256, bytesCell(b.SizeBytes), stamp(&mod), yesNo(b.Deleted))
			}
		}
		if err := e.out.Table(t); err != nil {
			return err
		}
	}
	if rep.Truncated {
		e.warnf("the list is capped at %d entries; the counts above are exact", len(rep.Blobs))
	}
	for _, p := range rep.Problems {
		e.warnf("problem: %s", p)
	}
	if rep.DryRun && rep.Collectable > 0 {
		e.out.Blank()
		e.out.Text("re-run with --apply --reason ... to remove them. The apply takes a second look at every " +
			"blob before unlinking it, so one adopted by an upload in the meantime is kept.")
	}
	return blobGCOutcome(rep)
}

// blobGCOutcome is partial, not failed, when a sweep hit problems: the parts
// it could reach were swept — or, on a dry run, counted — and the report says
// which parts it could not. Exit 1 would send a script to retry a transport
// that worked.
func blobGCOutcome(rep blobGCReport) error {
	if len(rep.Problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the sweep hit %s and finished around them; see the warnings above",
		ErrPartial, plural(len(rep.Problems), "problem", "problems"))
}

// artifactDetailResponse is the shape of GET /api/v1/artifacts/{sha}. Only
// the parts the delete preflight prints are decoded.
type artifactDetailResponse struct {
	Artifact    artifact `json:"artifact"`
	DeviceCount int      `json:"device_count"`
	JobCount    int      `json:"job_count"`
	Deletable   bool     `json:"deletable"`
}

func cmdArtifactsDelete(ctx context.Context, s *session, args []string) error {
	fs := newFlags("artifacts delete", s.err)
	var g globals
	g.bind(fs)
	g.bindDestructive(fs)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrf("usage: ctl artifacts delete <sha256> --reason r")
	}
	sha := strings.ToLower(strings.TrimSpace(rest[0]))
	if !isSHA256(sha) {
		// The server would refuse a prefix too, but with a 400 that reads as
		// ctl's failure. The hash is the identity, and a spec references it
		// whole; nothing in this tool resolves a shorter one.
		return usageErrf("artifacts delete takes the full 64-character sha256 from `ctl artifacts`, not %q", rest[0])
	}
	e, err := s.open(&g)
	if err != nil {
		return err
	}
	if err := e.requireReason("artifacts delete"); err != nil {
		return err
	}
	path := apiPrefix + "/artifacts/" + url.PathEscape(sha)

	// The preflight reads what the server knows about the digest, including
	// the two counts that decide whether it will refuse. An operator who is
	// about to be told "no" should see the reason before typing yes.
	f := &Fields{}
	f.Add("sha256", sha)
	detail, _, readErr := fetch[artifactDetailResponse](ctx, e.client, path, nil)
	var remote *RemoteError
	switch {
	case errors.As(readErr, &remote) && remote.Status == http.StatusNotFound:
		// Nothing to delete, and nothing to confirm.
		return readErr
	case readErr != nil:
		e.warnf("could not read the artifact for the preflight (%v); the server remains the authority", readErr)
	default:
		a := detail.Artifact
		f.Add("kind", a.Kind)
		f.Add("name", a.Name)
		f.Add("size", bytesCell(a.Size))
		if a.Package != nil {
			f.Addf("package", "%s%s", *a.Package, versionSuffix(a.VersionCode))
		}
		f.Add("uploaded by", dash(a.UploadedBy))
		f.Add("created", stamp(&a.CreatedAt))
		f.Addf("devices holding it", "%d", detail.DeviceCount)
		f.Addf("jobs naming it", "%d", detail.JobCount)
		if !detail.Deletable {
			f.Add("deletable", "NO — the server will refuse this (exit 3) while anything still names it")
		}
	}

	headline := "About to DELETE this artifact's row. The farm forgets the content exists: no spec can\n" +
		"reference it and no push step can skip it. The bytes stay in the blob store until\n" +
		"`ctl artifacts gc --apply` sweeps them, and an identical upload adopts them again."
	if err := e.confirm(headline, f); err != nil {
		return err
	}

	raw, err := e.client.Delete(ctx, path, url.Values{"reason": {e.reason}})
	if err != nil {
		return e.unknownOutcome(err, "ctl artifacts")
	}
	if e.format == FormatJSON {
		return e.out.RawJSON(raw)
	}

	var res struct {
		SHA256       string `json:"sha256"`
		Deleted      bool   `json:"deleted"`
		BlobRetained bool   `json:"blob_retained"`
		Note         string `json:"note"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return e.out.RawJSON(raw)
	}
	out := &Fields{}
	out.Add("sha256", firstNonEmpty(res.SHA256, sha))
	out.Add("row deleted", yesNo(res.Deleted))
	out.Add("blob retained", yesNo(res.BlobRetained))
	if res.Note != "" {
		out.Add("note", res.Note)
	}
	return e.out.Fields(out)
}

// isSHA256 reports whether s is a full lowercase hex digest.
func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
