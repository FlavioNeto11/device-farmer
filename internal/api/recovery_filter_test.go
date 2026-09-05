package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GET /api/v1/recovery exists so an operator can find things at 3am. "Every
// refusal at tier 4 in the last hour" is the question; these tests are that
// question asked through the query string, and the four ways of asking it
// wrong that must come back as a 400 naming the parameter rather than as an
// empty list that looks like an answer.

// TestRecoveryFilterRejectsGarbage covers the parsing that needs no database.
//
// Falsify: accept any outcome string, or parse since with time.ParseDuration
// alone (the RFC3339 spelling then 400s).
func TestRecoveryFilterRejectsGarbage(t *testing.T) {
	t.Parallel()

	bad := []struct{ query, wantMsg string }{
		{"outcome=bogus", "outcome must be one of"},
		// The CHECK values are lowercase; a filter that silently matched
		// nothing would hide a typo behind an empty list.
		{"outcome=REFUSED", "outcome must be one of"},
		{"since=yesterday", "since must be an RFC3339 timestamp or a duration"},
		{"since=-2h", "positive duration"},
		{"since=0s", "positive duration"},
		{"since=2026-09-05", "since must be an RFC3339 timestamp or a duration"},
	}
	for _, tc := range bad {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/recovery?"+tc.query, nil)
		if _, msg := parseRecoveryFilter(r); !strings.Contains(msg, tc.wantMsg) {
			t.Errorf("%s: message %q does not say %q", tc.query, msg, tc.wantMsg)
		}
	}

	good := []struct {
		query string
		check func(f recoveryFilter) string
	}{
		{"outcome=refused", func(f recoveryFilter) string {
			if f.outcome != "refused" {
				return fmt.Sprintf("outcome = %q", f.outcome)
			}
			return ""
		}},
		{"since=2h", func(f recoveryFilter) string {
			if f.sinceAt != nil || f.sinceFor == nil || *f.sinceFor != "7200000000 microseconds" {
				return fmt.Sprintf("sinceAt = %v, sinceFor = %v", f.sinceAt, f.sinceFor)
			}
			return ""
		}},
		{"since=90m", func(f recoveryFilter) string {
			if f.sinceFor == nil || *f.sinceFor != "5400000000 microseconds" {
				return fmt.Sprintf("sinceFor = %v", f.sinceFor)
			}
			return ""
		}},
		{"since=2026-09-05T10:00:00Z", func(f recoveryFilter) string {
			if f.sinceFor != nil || f.sinceAt == nil || !f.sinceAt.Equal(time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)) {
				return fmt.Sprintf("sinceAt = %v, sinceFor = %v", f.sinceAt, f.sinceFor)
			}
			return ""
		}},
		{"since=2026-09-05T10:00:00-03:00", func(f recoveryFilter) string {
			if f.sinceAt == nil || !f.sinceAt.Equal(time.Date(2026, 9, 5, 13, 0, 0, 0, time.UTC)) {
				return fmt.Sprintf("sinceAt = %v", f.sinceAt)
			}
			return ""
		}},
		{"limit=5000", func(f recoveryFilter) string {
			if f.limit != 1000 {
				return fmt.Sprintf("limit = %d, want the clamp", f.limit)
			}
			return ""
		}},
		{"hub=3-1&host=h01&device=abc", func(f recoveryFilter) string {
			if f.hub != "3-1" || f.hostID != "h01" || f.deviceID != "abc" {
				return fmt.Sprintf("hub=%q host=%q device=%q", f.hub, f.hostID, f.deviceID)
			}
			return ""
		}},
		{"", func(f recoveryFilter) string {
			if f.outcome != "" || f.tier != nil || f.hub != "" || f.sinceAt != nil || f.sinceFor != nil || f.limit != 100 {
				return fmt.Sprintf("the empty query is not \"everything\": %+v", f)
			}
			return ""
		}},
	}
	for _, tc := range good {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/recovery?"+tc.query, nil)
		f, msg := parseRecoveryFilter(r)
		if msg != "" {
			t.Errorf("%q was refused: %s", tc.query, msg)
			continue
		}
		if problem := tc.check(f); problem != "" {
			t.Errorf("%q: %s", tc.query, problem)
		}
	}
}

// TestRecoveryFiltersNarrowTheAttempts runs the endpoint against a real
// database: four attempts on one hub of a throwaway host, then every filter,
// then every table-backed 400. The host id is unique per run and everything
// is removed on cleanup.
//
// Falsify: drop the `a.started_at >= now() - $7::interval` arm from the query
// (since=2h then returns the three-hour-old refusal), or return 200 with an
// empty list from resolveRecoveryFilter for an unknown tier.
func TestRecoveryFiltersNarrowTheAttempts(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		t.Skip("no DATABASE_URL; this case needs a real, migrated database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	// Registered before the row cleanup below, so it runs after it.
	t.Cleanup(pool.Close)

	host := fmt.Sprintf("u3api%x", time.Now().UnixNano()&0xffffffff)
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM farm.recovery_attempts WHERE host_id = $1`,
			`DELETE FROM farm.hosts WHERE id = $1`,
		} {
			if _, err := pool.Exec(ctx, q, host); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO farm.hosts (id, adb_endpoint) VALUES ($1, '127.0.0.1:1')`, host); err != nil {
		t.Fatalf("fixture host: %v", err)
	}
	var hubID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO farm.hubs (host_id, usb_path, port_count) VALUES ($1, '9-1', 4) RETURNING id`,
		host).Scan(&hubID); err != nil {
		t.Fatalf("fixture hub: %v", err)
	}

	// Ages are server-side arithmetic on now(): no client clock reaches the
	// database.
	seed := []struct {
		tier    int
		outcome string
		age     string
	}{
		{4, "refused", "10 minutes"},
		{4, "refused", "3 hours"},
		{1, "failed", "5 minutes"},
		{4, "recovered", "1 minute"},
	}
	ids := make([]int64, len(seed))
	for i, s := range seed {
		if err := pool.QueryRow(ctx, `
INSERT INTO farm.recovery_attempts (hub_id, host_id, tier, started_at, finished_at, outcome)
VALUES ($1, $2, $3, now() - $4::interval, now() - $4::interval, $5)
RETURNING id`, hubID, host, s.tier, s.age, s.outcome).Scan(&ids[i]); err != nil {
			t.Fatalf("fixture attempt %d: %v", i, err)
		}
	}

	s := &Server{
		pool:      pool,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		auth:      NewAllowAll(slog.New(slog.NewTextHandler(io.Discard, nil)), "test"),
		startedAt: time.Now(),
	}
	get := func(t *testing.T, query string) (int, map[string]any) {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handleRecovery(rr, httptest.NewRequest(http.MethodGet, "/api/v1/recovery?"+query, nil))
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: not JSON (%d): %s", query, rr.Code, rr.Body.String())
		}
		return rr.Code, body
	}
	// The error envelope is {"error": {"code", "message", ...}}.
	message := func(body map[string]any) string {
		env, _ := body["error"].(map[string]any)
		msg, _ := env["message"].(string)
		return msg
	}
	attemptIDs := func(body map[string]any) []int64 {
		var out []int64
		list, _ := body["attempts"].([]any)
		for _, a := range list {
			m, _ := a.(map[string]any)
			if id, ok := m["id"].(float64); ok {
				out = append(out, int64(id))
			}
		}
		return out
	}
	same := func(got []int64, want ...int64) bool {
		if len(got) != len(want) {
			return false
		}
		seen := map[int64]bool{}
		for _, g := range got {
			seen[g] = true
		}
		for _, w := range want {
			if !seen[w] {
				return false
			}
		}
		return true
	}
	mine := "host=" + host

	narrow := []struct {
		name  string
		query string
		want  []int64
	}{
		{"everything on the host", mine, ids},
		{"by outcome", mine + "&outcome=refused", []int64{ids[0], ids[1]}},
		{"outcome within a duration", mine + "&outcome=refused&since=2h", []int64{ids[0]}},
		{"by tier name", mine + "&tier=port_power", []int64{ids[0], ids[1], ids[3]}},
		{"by tier number", mine + "&tier=4", []int64{ids[0], ids[1], ids[3]}},
		{"by hub usb_path", mine + "&hub=9-1", ids},
		{"by hub id", fmt.Sprintf("%s&hub=%d", mine, hubID), ids},
		{"since an instant", mine + "&since=" + time.Now().Add(-30*time.Minute).UTC().Format(time.RFC3339),
			[]int64{ids[0], ids[2], ids[3]}},
		{"tier, outcome and window together", mine + "&tier=4&outcome=recovered&since=1h", []int64{ids[3]}},
		{"a window nothing falls in", mine + "&since=10s", nil},
	}
	for _, tc := range narrow {
		t.Run(tc.name, func(t *testing.T) {
			code, body := get(t, tc.query)
			if code != http.StatusOK {
				t.Fatalf("%s: HTTP %d: %s", tc.query, code, message(body))
			}
			for _, key := range []string{"attempts", "quarantines", "tiers"} {
				if _, ok := body[key]; !ok {
					t.Errorf("the response shape lost %q", key)
				}
			}
			if got := attemptIDs(body); !same(got, tc.want...) {
				t.Fatalf("%s returned attempts %v, want %v", tc.query, got, tc.want)
			}
		})
	}

	garbage := []struct{ query, wantMsg string }{
		{mine + "&tier=99", "not a tier number or name"},
		{mine + "&tier=nope", "not a tier number or name"},
		{mine + "&hub=nope", "neither a hub id nor a usb_path"},
		{mine + "&outcome=bogus", "outcome must be one of"},
		{mine + "&since=garbage", "since must be"},
	}
	for _, tc := range garbage {
		t.Run(tc.query, func(t *testing.T) {
			code, body := get(t, tc.query)
			if code != http.StatusBadRequest {
				t.Fatalf("HTTP %d, want 400: an unanswerable question must not look like an "+
					"empty answer", code)
			}
			if msg := message(body); !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("400 message %q does not say %q", msg, tc.wantMsg)
			}
		})
	}
}
