# DeviceFarmerAPIAuthOpen

**Severity:** critical · **Group:** `device-farmer.control-plane`

```promql
max by (authenticator) (farm_api_auth_open) == 1    for 10m
```

## What fired

The API is serving with an authenticator that authenticates nothing. Every
caller that can reach the port is handed the **operator** role.

## What it means

The operator role can:

- **revoke a lease** — take a phone away from a running job and destroy its work
- **drain a host** — stop placement across an entire machine
- **cut power to a USB port** that is holding a live job
- run arbitrary shell commands on any device, in bulk, by selector

There is no rate limit and no second factor in front of any of that. This is the
one configuration in which an anonymous stranger who can route to the Service
can destroy somebody's work, and it is why this is a critical page rather than a
finding in a report.

It is also, importantly, **not an accident**. `internal/api/auth_wiring.go`
refuses every ambiguous case: a network listener with no tokens does not start,
`FARM_API_AUTH=bearer` with an empty token list does not start, and a manifest
that both lists credentials and asks for anonymous access does not start.
Reaching this state took somebody explicitly writing `FARM_API_AUTH=allow-all`
or `FARM_API_ALLOW_ANONYMOUS=true`, or binding to loopback with no tokens.

The startup log said so at `WARN` the moment it happened. That line scrolled out
of the buffer the same afternoon. `farm_api_auth_open` does not.

## What is NOT wrong

- **A demo or evaluation farm running open on purpose, with no real devices.**
  This is what the mode is for. Silence the alert for that release, or set
  `prometheusRule.enabled: false` on it — do not "fix" it by deleting the rule
  from the chart for everyone.
- **`/metrics` and `/healthz` answering without a token.** Deliberate, and
  unrelated. An operator debugging a farm whose auth is the broken thing still
  needs to see its state. Put those behind a NetworkPolicy, not behind a token.
- **A single replica reporting it during a rollout.** The gauge is set once at
  process construction, so a mixed rollout shows both values for a few minutes.
  The 10m `for:` covers that.

## First three checks

**1. Confirm it from the server itself, not from the manifest.** The server
reports the authenticator it actually installed:

```sh
curl -s "$FARM_API_URL/api/v1/capabilities" | jq '.auth'   # needs no token
```

And, more bluntly — if this returns anything without a token, the farm is open:

```sh
curl -s -o /dev/null -w '%{http_code}\n' "$FARM_API_URL/api/v1/leases"
```

`200` means open. `401` means the alert is stale or reading a different release.

**2. Find out how long it has been open, and whether anyone used it.** Every
privileged action is audited, and an open farm's audit rows carry the anonymous
subject rather than a person:

```sh
psql "$PGURL" -c "
SELECT at, actor, action, subject, reason
  FROM farm.audit_log
 WHERE at > now() - interval '7 days'
 ORDER BY at DESC LIMIT 50"
psql "$PGURL" -c "
SELECT actor, count(*), min(at), max(at)
  FROM farm.audit_log
 WHERE at > now() - interval '30 days'
 GROUP BY actor ORDER BY 2 DESC"
```

An `actor` of `demo-operator` or `anonymous` on a production farm is somebody
having used the open door, deliberately or not. Also check for work that was
destroyed while it was open:

```sh
psql "$PGURL" -c "
SELECT ended_at, lease_id, job_id, tenant_id, release_reason, ended_by
  FROM farm.v_lease_endings
 WHERE release_reason = 'operator_revoked' AND ended_at > now() - interval '7 days'
 ORDER BY ended_at DESC"
```

**3. Find where the configuration came from.**

```sh
kubectl -n <ns> get configmap <release>-config -o yaml | grep -i -E 'FARM_API_AUTH|ANONYMOUS'
kubectl -n <ns> get deploy <release>-api -o yaml | grep -A3 -i -E 'FARM_API_AUTH|ALLOW_ANONYMOUS|FARM_API_TOKENS'
helm -n <ns> get values <release> | grep -A6 '^auth:'
```

The likeliest source is `config.extra` in a values file, inherited from a base
manifest that was written for a laptop.

## The fix

Give the release credentials. Either inline:

```sh
helm -n <ns> upgrade <release> deploy/helm/device-farmer --reuse-values \
  --set auth.tokens='<token>:operator:<name>,<token2>:tenant:<team>' \
  --set config.extra.FARM_API_AUTH=null \
  --set config.extra.FARM_API_ALLOW_ANONYMOUS=null
```

or, better, out of the values file entirely — put `FARM_API_TOKENS` in a Secret
and point `auth.existingSecret` at it. Note that setting **both** `auth.tokens`
and `auth.existingSecret` is refused at render time on purpose: `existingSecret`
would win silently, so editing `auth.tokens` to revoke a leaked credential would
look like a clean upgrade and change nothing.

At least one credential must hold `operator` or `admin`, or revoke, drain, slot
power, bulk exec and quarantine close all become unreachable — the API warns
about this at startup, and a wedged job's device could then not be taken back by
hand.

**Rolling the API does not disturb a single lease.** SIGTERM drains in-flight
requests; it does not release. Holders that miss a renewal during the restart
record `kind="transient"`, which proves nothing and aborts nothing.

## When to escalate

- **The port is reachable from outside the cluster** — an Ingress, a LoadBalancer
  Service, a NodePort. Then it is not "misconfigured", it is exposed, and it is a
  security incident with a timeline that starts at the first `WARN` in the api
  log.
- **The audit log shows revokes you cannot account for.** Work was destroyed by
  somebody unidentified.
- **You cannot find where the setting comes from.** Do not paper over it by
  adding tokens on top; if a base manifest is re-asserting `allow-all`, the next
  deploy reopens the farm.
