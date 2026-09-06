{{/*
The startup probe the roles whose only listener is the metrics port share.

They all serve GET /healthz on FARM_METRICS_ADDR — cmd/farmd wraps every
long-running role in withMetrics — so this is the one probe that can be asked
of them without lying. It is deliberately the ONLY one: the comment above the
container in templates/reaper.yaml says why a livenessProbe on that same
endpoint would be a false alarm in one direction and a false comfort in the
other.

WHAT IT BUYS. A startupProbe stops being evaluated the moment it first
succeeds, so it cannot restart a role that is working. What it catches is the
boot that never gets as far as the listener — a wrong image, an argument this
binary does not know, a configuration it refuses — which today comes up
Running, exports nothing, and is noticed only by whoever reads
farm.component_heartbeat.

WHY IT IS GATED ON THE PORT ACTUALLY BEING BOUND. A startupProbe that never
succeeds kills the container, so aiming one at a port nothing binds converts a
working role into a CrashLoopBackOff. FARM_METRICS_ADDR can be switched off —
internal/config accepts the literal "off" — through config.extra or through a
role's own extraEnv, which beats the ConfigMap because env wins over envFrom;
and a metrics bind that fails is deliberately NOT fatal in the binary, for the
reason cmd/farmd/roles.go gives: a reaper that will not start is the only
automatic release path gone. Rendering the probe only when nothing in these
values takes that variable away keeps both of those decisions intact — a role
that switches its own metrics off silently gets no probe instead of a crash
loop.
*/}}
{{- define "device-farmer.metricsAddrOverridden" -}}
{{- if .ctx.Values.config.extra -}}
{{- if hasKey .ctx.Values.config.extra "FARM_METRICS_ADDR" -}}yes{{- end -}}
{{- end -}}
{{- range (index .ctx.Values .component).extraEnv -}}
{{- if eq (.name | toString) "FARM_METRICS_ADDR" -}}yes{{- end -}}
{{- end -}}
{{- end -}}

{{- define "device-farmer.startupProbe" -}}
{{- $probe := (index .ctx.Values .component).startupProbe -}}
{{- if $probe -}}
{{- if and $probe.enabled (not (include "device-farmer.metricsAddrOverridden" .)) -}}
startupProbe:
  httpGet:
    path: /healthz
    # A number and never a name. These containers declare no ports at all —
    # only the api does — and an httpGet naming a port the container never
    # declared does not resolve to anything.
    port: {{ .ctx.Values.metricsService.port }}
  periodSeconds: {{ $probe.periodSeconds }}
  failureThreshold: {{ $probe.failureThreshold }}
{{- end -}}
{{- end -}}
{{- end -}}
