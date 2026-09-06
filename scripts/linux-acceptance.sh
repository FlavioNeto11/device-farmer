#!/usr/bin/env bash
# linux-acceptance.sh — run the whole control plane on the platform it ships for.
#
# WHY THIS EXISTS, and why it is not a Go test.
#
# Three things about this project cannot be checked from a `go test` on a
# developer's machine, and all three had been taken on trust:
#
#   1. The binary starts. Every role builds a metrics registry at startup, and
#      for a while that registry registered two collectors twice and panicked.
#      Build, vet, gofmt and the entire test suite stayed green, because nothing
#      CALLED the function. Worse, the collision only existed on Linux:
#      prometheus's process collector describes nothing on other platforms. A
#      decision that behaves differently per GOOS has to be exercised on the
#      GOOS that runs it.
#
#   2. `topo.Sysfs` reads the USB tree. Every test in internal/topo that reads a
#      tree hands `FromFS` an `fstest.MapFS` — the one that names a real
#      directory does it to prove `Sysfs` REFUSES a path that is not a bus.
#      The shipped binary calls `Sysfs`, which refuses on any GOOS but Linux
#      and then reads through `os.DirFS`. The
#      difference is not cosmetic: whether a hub's ports can have their VBUS
#      switched is read from the file MODE of each port's `disable`, and a MapFS
#      can only assert a mode into being. Here the kernel's own stat answers.
#
#   3. The schema runs on the server you deploy. A schema is a contract with a
#      specific PostgreSQL: planner behaviour, aggregate availability and
#      defaults move between majors, and farm.resolve_device's fingerprint rung
#      already once needed an aggregate a major did not have. Running every
#      assertion suite against a second major is the cheapest way to find out.
#      The loop below globs test/assertions*.sql, so a suite added tomorrow is
#      run here without anyone remembering to come back and say so.
#
# WHAT IT DOES NOT PROVE. There is no handset here. The ADB servers are the
# demo's in-process fakes and the USB tree is written by this script, not
# enumerated by a kernel from real hardware. USBDEVFS_RESET and uhubctl are not
# exercised. Those are REC-03 and HW-05 in REQUIREMENTS.md and they stay open.
#
# USAGE
#   scripts/linux-acceptance.sh            # needs a local PostgreSQL and Go
#   From Windows, through WSL:
#     wsl -d Ubuntu -- bash -c 'cd /mnt/c/git/device-farmer && scripts/linux-acceptance.sh'
#
# Exits non-zero on the first failed check. Every check prints what it saw.

set -uo pipefail

# Never prompt, and never wait forever. A PGURL whose role does not exist used
# to hang this script on psql's password prompt — in CI that is worse than a
# wrong answer, because there is nothing to read and nothing to act on.
export PGCONNECT_TIMEOUT="${PGCONNECT_TIMEOUT:-10}"
PSQL="psql -w"

PGURL="${PGURL:-postgres://farm@127.0.0.1:5432/postgres?sslmode=disable}"
DBNAME="${DBNAME:-farm_acceptance}"
API_ADDR="${API_ADDR:-127.0.0.1:18080}"
METRICS_ADDR="${METRICS_ADDR:-127.0.0.1:19090}"
NODE_ADDR="${NODE_ADDR:-127.0.0.1:18082}"
NODE_METRICS_ADDR="${NODE_METRICS_ADDR:-127.0.0.1:19091}"
WORK="${WORK:-$(mktemp -d)}"
SYSFS="$WORK/sysfs"
BIN="$WORK/farmd"
DB="${PGURL%/*}/$DBNAME?sslmode=disable"

fails=0
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; fails=$((fails + 1)); }
have() { command -v "$1" >/dev/null 2>&1; }

cleanup() {
  pkill -f "$BIN" 2>/dev/null
  sleep 1
  $PSQL "$PGURL" -c "DROP DATABASE IF EXISTS $DBNAME WITH (FORCE)" >/dev/null 2>&1
  [ -n "${KEEP_WORK:-}" ] || rm -rf "$WORK"
}
trap 'rc=$?; cleanup; exit $rc' EXIT

q() { $PSQL "$DB" -tAqc "$1" 2>&1; }

# --------------------------------------------------------------------------
step "platform"
[ "$(uname -s)" = "Linux" ] || { echo "  this script must run on Linux; it is here to test the paths that only exist there"; exit 2; }
echo "  kernel    $(uname -r)"
echo "  workdir   $WORK"
for t in psql curl; do have "$t" || { echo "  missing $t"; exit 2; }; done
echo "  postgres  $($PSQL "$PGURL" -tAqc 'SHOW server_version' 2>&1 | head -1)"

# --------------------------------------------------------------------------
step "the binary under test"
# FARMD lets a Linux host run this against a binary it was GIVEN — which is how
# the container image arrives, and how a farm host that has no Go toolchain can
# still be checked. Without it, build from this tree.
if [ -n "${FARMD:-}" ]; then
  install -m 0755 "$FARMD" "$BIN" || { bad "cannot use FARMD=$FARMD"; exit 1; }
  ok "using $FARMD"
elif have go; then
  if go build -o "$BIN" ./cmd/farmd; then ok "built from this tree"; else bad "go build"; exit 1; fi
else
  echo "  no Go toolchain and no FARMD=<path>; nothing to test"; exit 2
fi
"$BIN" version >/dev/null 2>&1 || true

# --------------------------------------------------------------------------
step "migrate an empty database"
$PSQL "$PGURL" -c "DROP DATABASE IF EXISTS $DBNAME WITH (FORCE)" >/dev/null 2>&1
$PSQL "$PGURL" -c "CREATE DATABASE $DBNAME" >/dev/null 2>&1 || { bad "CREATE DATABASE (the role needs CREATEDB)"; exit 1; }
out=$(DATABASE_URL="$DB" "$BIN" migrate up 2>&1)
ver=$(printf '%s' "$out" | grep -oE 'schema version: [0-9]+' | grep -oE '[0-9]+$')
count=$(ls migrations/*.sql | wc -l)
if [ "$ver" = "$count" ]; then ok "schema v$ver from empty, $count migrations"; else bad "schema v$ver but $count migration files"; fi

# --------------------------------------------------------------------------
step "every SQL assertion suite, on this server"
for f in test/assertions*.sql; do
  a=$($PSQL "$DB" -v ON_ERROR_STOP=1 -f "$f" 2>&1)
  if printf '%s' "$a" | grep -q 'ASSERTIONS PASSED'; then ok "$(basename "$f")"
  else bad "$(basename "$f")"; printf '%s\n' "$a" | grep -Ei '^psql.*(ERROR|FATAL)' | head -2 | sed 's/^/        /'; fi
done

# --------------------------------------------------------------------------
step "the control plane starts and serves"
DATABASE_URL="$DB" FARM_API_ADDR="$API_ADDR" FARM_METRICS_ADDR="$METRICS_ADDR" \
FARM_API_TOKENS="op:operator:acceptance,tn:tenant:acme" FARM_LOG_LEVEL=info \
  "$BIN" demo -hosts 2 -devices 14 > "$WORK/farm.log" 2>&1 &
for i in $(seq 1 40); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://$API_ADDR/healthz" 2>/dev/null)
  [ "$code" = "200" ] && break
  sleep 1
done
if [ "$code" = "200" ]; then ok "healthz 200 after ${i}s"
else bad "the control plane never answered healthz"; tail -20 "$WORK/farm.log" | sed 's/^/        /'; exit 1; fi

code=$(curl -s -o /dev/null -w '%{http_code}' "http://$API_ADDR/api/v1/fleet")
[ "$code" = "401" ] && ok "no credential -> 401" || bad "no credential -> $code, want 401"
for r in capabilities fleet topology hosts slots recovery jobs leases; do
  o=$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer op' "http://$API_ADDR/api/v1/$r")
  [ "$o" = "200" ] && ok "operator /$r 200" || bad "operator /$r -> $o"
done
for r in reaper bulk; do
  t=$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer tn' "http://$API_ADDR/api/v1/$r")
  [ "$t" = "403" ] && ok "tenant /$r 403" || bad "tenant /$r -> $t, want 403"
done
for a in index lease jobs devices recovery operate surface requirements; do
  d=$(curl -s -o /dev/null -w '%{http_code}' "http://$API_ADDR/docs/$a.json")
  [ "$d" = "200" ] || bad "docs/$a.json -> $d"
done
ok "all eight docs areas served"

# --------------------------------------------------------------------------
step "metrics, including the two the binary owns"
m=$(curl -s "http://$METRICS_ADDR/metrics")
fam=$(printf '%s' "$m" | grep -c '^# TYPE farm_')
[ "$fam" -ge 100 ] && ok "$fam farm_* metric families" || bad "only $fam farm_* families"
printf '%s' "$m" | grep -q '^go_goroutines ' && ok "go_goroutines present" || bad "go_goroutines missing"
# process_* exists only on Linux, which is the point of running here.
printf '%s' "$m" | grep -q '^process_resident_memory_bytes ' \
  && ok "process_resident_memory_bytes present (Linux-only collector)" \
  || bad "process_resident_memory_bytes missing on Linux"

# --------------------------------------------------------------------------
step "let the farm run, then check THE INVARIANT"
# Wait for the EVENT, not for a clock. The demo drops a device offline while a
# job holds it on purpose — that is DeviceFarmer/STF #663's scenario — and it is
# the only thing here that actually exercises the claim. A fixed sleep makes the
# headline check conditional on how fast the box is: on a slow one nothing
# drops, the script prints a note and passes, and the run that was supposed to
# prove the invariant proved nothing.
DROP_WAIT="${DROP_WAIT:-240}"
for i in $(seq 1 "$DROP_WAIT"); do
  grep -q 'DEVICE DROPPING OFFLINE MID-LEASE' "$WORK/farm.log" 2>/dev/null && break
  sleep 1
done
if grep -q 'DEVICE DROPPING OFFLINE MID-LEASE' "$WORK/farm.log" 2>/dev/null; then
  ok "a device was dropped offline mid-lease after ${i}s; the scenario is live"
else
  bad "no device was dropped offline mid-lease within ${DROP_WAIT}s, so the invariant \
was never exercised. Everything below still checks the leases that DID end, but this run \
is not evidence about #663. Raise DROP_WAIT, or look at $WORK/farm.log."
fi
# Let a few more leases turn over now that the interesting one has happened.
sleep 20
reasons=$(q "SELECT coalesce(string_agg(release_reason || ' x' || n, ', ' ORDER BY release_reason), '(none yet)')
               FROM (SELECT release_reason, count(*) n FROM farm.leases
                      WHERE release_reason IS NOT NULL GROUP BY 1) t")
echo "  endings: $reasons"
# The CHECK has no connectivity term, so a connectivity ending is not merely
# absent — it is unrepresentable. Assert both: the constraint, and the data.
ck=$(q "SELECT pg_get_constraintdef(oid) FROM pg_constraint
         WHERE conrelid='farm.leases'::regclass AND conname LIKE '%release_reason%'")
for word in offline unreachable disconnect transport timeout adb; do
  printf '%s' "$ck" | grep -qi "$word" && bad "the release_reason CHECK admits '$word'"
done
ok "release_reason CHECK has no connectivity value: $(printf '%s' "$ck" | tr -d '\n' | head -c 160)"

bad_end=$(q "SELECT count(*) FROM farm.leases
              WHERE release_reason IS NOT NULL
                AND release_reason NOT IN ('completed','failed','job_cancelled','max_runtime',
                                           'operator_revoked','holder_expired','device_retired')")
[ "$bad_end" = "0" ] && ok "no lease ended outside the seven legitimate reasons" \
                     || bad "$bad_end lease(s) ended for something else"

# The demo drops devices offline WHILE a job holds them, on purpose. Those are
# the #663 event; every one of them must have ended for a reason the job or a
# deadline gave.
dropped=$(grep -c 'DEVICE DROPPING OFFLINE MID-LEASE' "$WORK/farm.log" 2>/dev/null || echo 0)
if [ "$dropped" -gt 0 ]; then
  echo "  $dropped device(s) were dropped offline mid-lease; here is what happened to every"
  echo "  lease this run named — not only those, because a lease that ended wrongly for"
  echo "  some other reason is equally a violation:"
  for lid in $(grep -o 'lease_id=[0-9a-f]\{8\}' "$WORK/farm.log" | cut -d= -f2 | sort -u); do
    row=$(q "SELECT state || ' / ' || coalesce(release_reason,'still held')
               FROM farm.leases WHERE id::text LIKE '$lid%'")
    case "$row" in
      *"still held"*|*completed*|*failed*|*max_runtime*|*operator_revoked*|*job_cancelled*)
        ok "  lease $lid: $row" ;;
      "") ;;
      *) bad "  lease $lid ended as: $row" ;;
    esac
  done
else
  echo "  (no mid-lease drop happened in this window; the invariant is still checked above)"
fi
# Re-read: the snapshot above was taken before the farm was let run, so it
# could only ever have said zero.
m2=$(curl -s "http://$METRICS_ADDR/metrics")
blips=$(printf '%s' "$m2" | grep -c 'kind="transport_gone".*} [1-9]')
total=$(printf '%s' "$m2" | grep 'kind="transport_gone"' | grep -oE '} [0-9]+$' | tr -d '} '         | awk '{n += $1} END {print n + 0}')
if [ "$blips" -gt 0 ]; then
  ok "$total transport failure(s) survived across $blips position(s), and no lease moved"
else
  echo "  no transport failure was recorded in this window; the invariant is still checked above"
fi

# --------------------------------------------------------------------------
step "farmd node: the USB tree, read off a real filesystem"
"$(dirname "$0")/mksysfs.sh" "$SYSFS" >/dev/null || { bad "could not build the sysfs fixture"; exit 1; }
mode=$(stat -c '%a' "$SYSFS/3-1/3-1:1.0/3-1-port1/disable")
[ "$mode" = "644" ] && ok "port disable mode 0644 (the kernel's signal for switchable VBUS)" \
                    || bad "port disable mode is $mode"

ADB=$(q "SELECT adb_endpoint FROM farm.hosts ORDER BY id LIMIT 1")
q "INSERT INTO farm.hosts (id, rack_id, rack_unit, adb_endpoint, admin_state)
   VALUES ('h-acceptance', (SELECT id FROM farm.racks LIMIT 1), 40, '$ADB', 'enabled')
   ON CONFLICT (id) DO UPDATE SET adb_endpoint = EXCLUDED.adb_endpoint" >/dev/null

DATABASE_URL="$DB" FARM_HOST_ID=h-acceptance FARM_SYSFS_ROOT="$SYSFS" \
FARM_ADB_ENDPOINT="$ADB" FARM_NODE_TOKEN=acceptance FARM_NODE_ADDR="$NODE_ADDR" \
FARM_METRICS_ADDR="$NODE_METRICS_ADDR" FARM_TOPO_MIN_PORTS=4 \
FARM_TOPO_INTERVAL=20s FARM_TOPO_CALL_TIMEOUT=5s FARM_LOG_LEVEL=info \
  "$BIN" node > "$WORK/node.log" 2>&1 &
sleep 20

grep -q 'usb discovery pass' "$WORK/node.log" \
  && ok "discovery ran: $(grep -m1 -o 'hubs=[0-9]* slots=[0-9]* written=[0-9]* skipped=[0-9]*' "$WORK/node.log")" \
  || { bad "discovery never ran"; tail -12 "$WORK/node.log" | sed 's/^/        /'; }

hubs=$(q "SELECT count(*) FROM farm.hubs WHERE host_id='h-acceptance'")
slots=$(q "SELECT count(*) FROM farm.slots WHERE host_id='h-acceptance'")
[ "$hubs" = "1" ] && ok "one hub registered (the four-port root hub was skipped, as the filter says)" \
                  || bad "$hubs hubs registered, want 1"
[ "$slots" = "7" ] && ok "seven slots registered" || bad "$slots slots registered, want 7"

sw=$(q "SELECT vbus_switchable FROM farm.hubs WHERE host_id='h-acceptance'")
[ "$sw" = "t" ] && ok "vbus_switchable read from the real file mode" \
                || bad "vbus_switchable=$sw; the mode on the disable files was not read"

pd=$(q "SELECT kind || '/' || control FROM farm.power_domains WHERE host_id='h-acceptance' LIMIT 1")
[ "$pd" = "per_port/uhubctl" ] && ok "power domain $pd" || bad "power domain is '$pd'"

kr=$(q "SELECT kernel_release FROM farm.hosts WHERE id='h-acceptance'")
[ "$kr" = "$(uname -r)" ] && ok "host row carries the running kernel: $kr" \
                          || bad "kernel_release='$kr', running $(uname -r)"

n1=$(curl -s -o /dev/null -w '%{http_code}' "http://$NODE_ADDR/node/v1/health")
n2=$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer acceptance' "http://$NODE_ADDR/node/v1/health")
[ "$n1" = "401" ] && ok "node /health without a token -> 401" || bad "node /health unauth -> $n1"
[ "$n2" = "200" ] && ok "node /health with a token -> 200" || bad "node /health auth -> $n2"

beat=$(q "SELECT count(*) FROM farm.component_heartbeat WHERE component = 'node:h-acceptance'")
[ "$beat" = "1" ] && ok "the agent beats under a per-host key" \
                  || bad "no per-host heartbeat row for this agent"

grep -q 'enrollment cycle' "$WORK/node.log" \
  && ok "enrollment ran: $(grep -m1 -o 'summary="[^"]*"' "$WORK/node.log")" \
  || bad "the enrollment loop never completed a cycle"

# --------------------------------------------------------------------------
step "result"
if [ "$fails" -eq 0 ]; then
  echo "  every check passed on $(uname -sr), PostgreSQL $($PSQL "$PGURL" -tAqc 'SHOW server_version' | head -1)"
  echo "  NOT proved here: USBDEVFS_RESET and uhubctl against real hardware (REC-03, HW-05)."
  echo "  exiting 0"
  exit 0
fi
echo "  $fails check(s) failed. Logs kept under $WORK (set KEEP_WORK=1 to keep them next time)."
echo "  exiting 1"
KEEP_WORK=1
exit 1
