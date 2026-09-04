{{/*
Shared template helpers.

Most helpers take a dict rather than a context, because nearly everything in
this chart is rendered once per role:

    {{ include "device-farmer.labels" (dict "ctx" $ "component" "api") }}

"ctx" is the root context and "component" is the farmd role.
*/}}

{{- define "device-farmer.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "device-farmer.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "device-farmer.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Full name of one role's workload, e.g. release-device-farmer-scheduler. */}}
{{- define "device-farmer.componentName" -}}
{{- printf "%s-%s" (include "device-farmer.fullname" .ctx) .component | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "device-farmer.selectorLabels" -}}
app.kubernetes.io/name: {{ include "device-farmer.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
{{- if .component }}
app.kubernetes.io/component: {{ .component }}
{{- end }}
{{- end -}}

{{- define "device-farmer.labels" -}}
helm.sh/chart: {{ include "device-farmer.chart" .ctx }}
{{ include "device-farmer.selectorLabels" . }}
app.kubernetes.io/version: {{ .ctx.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
app.kubernetes.io/part-of: device-farmer
{{- end -}}

{{- define "device-farmer.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
The pod-spec preamble every workload in this chart shares.

automountServiceAccountToken comes first and is always present: nothing in
farmd calls the Kubernetes API — it talks to Postgres — so there is no reason
for a token to be sitting in any of these containers. A ServiceAccount is
named only if the operator asked for one; this chart creates no RBAC.
*/}}
{{- define "device-farmer.podPreamble" -}}
automountServiceAccountToken: {{ .Values.serviceAccount.automountToken }}
{{- with .Values.serviceAccount.name }}
serviceAccountName: {{ . }}
{{- end }}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.priorityClassName }}
priorityClassName: {{ . }}
{{- end }}
{{- end -}}

{{- define "device-farmer.configMapName" -}}
{{- printf "%s-config" (include "device-farmer.fullname" .) -}}
{{- end -}}

{{/*
Where DATABASE_URL comes from. A DSN carries a password, so it is a Secret in
every case; there is no code path in this chart that puts it in a ConfigMap.
*/}}
{{- define "device-farmer.dbSecretName" -}}
{{- if .Values.database.existingSecret -}}
{{- .Values.database.existingSecret -}}
{{- else -}}
{{- printf "%s-db" (include "device-farmer.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "device-farmer.dbSecretKey" -}}
{{- if .Values.database.existingSecret -}}
{{- .Values.database.existingSecretKey -}}
{{- else -}}
DATABASE_URL
{{- end -}}
{{- end -}}

{{/*
The guard that keeps a half-configured release from ever reaching the cluster.
farmd itself refuses to boot without a DSN, but failing at render time is
better than failing in a CrashLoopBackOff: nothing has been created yet.

Both-set is refused as well, and it is the more dangerous of the two.
dbSecretName prefers existingSecret, so a release with both would mount the
existing Secret and drop database.dsn on the floor without a word — every
workload pointed at a database the operator did not name, while the one they
did name renders nowhere. Picking a winner here means one of the two authors
of that values file is wrong and is never told.
*/}}
{{- define "device-farmer.requireDatabase" -}}
{{- if and (not .Values.database.dsn) (not .Values.database.existingSecret) -}}
{{- fail "\ndevice-farmer: no database configured.\n\nSet one of:\n  --set database.dsn=postgres://user:pass@host:5432/device_farmer?sslmode=require\n  --set database.existingSecret=my-secret   (key: DATABASE_URL)\n\nEvery lease, fence and job in this system lives in that database. There is no\nembedded Postgres and no default DSN, because a control plane that silently\npoints at the wrong database is worse than one that will not start.\n" -}}
{{- end -}}
{{- if and .Values.database.dsn .Values.database.existingSecret -}}
{{- fail (printf "\ndevice-farmer: database.dsn and database.existingSecret are both set.\n\n  database.existingSecret = %s\n  database.dsn            = (set)\n\nOnly one of them can win, and the chart would silently take existingSecret:\nevery pod would read DATABASE_URL out of %q and the DSN you typed would never\nbe rendered anywhere. A control plane quietly pointed at a database nobody\nchose is the failure this chart refuses to have.\n\nDrop whichever one is stale. If you are moving from an inline DSN to a managed\nSecret, unset database.dsn in the same edit:\n\n  --set database.dsn=\"\" --set database.existingSecret=%s\n" .Values.database.existingSecret .Values.database.existingSecret .Values.database.existingSecret) -}}
{{- end -}}
{{- end -}}

{{/*
DATABASE_URL, always by reference, never inline in a pod spec.

"secretName" overrides where it is read from; the migration hook needs that,
because the release's own Secret does not exist yet while pre-install hooks
are running.
*/}}
{{- define "device-farmer.databaseEnv" -}}
{{- include "device-farmer.requireDatabase" .ctx -}}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ default (include "device-farmer.dbSecretName" .ctx) .secretName }}
      key: {{ include "device-farmer.dbSecretKey" .ctx }}
{{- end -}}

{{- define "device-farmer.authSecretName" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecret -}}
{{- else -}}
{{- printf "%s-auth" (include "device-farmer.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "device-farmer.authSecretKey" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecretKey -}}
{{- else -}}
FARM_API_TOKENS
{{- end -}}
{{- end -}}

{{/*
The same refusal as requireDatabase, for the same reason.

authSecretName prefers existingSecret, so a release with both would mount the
existing Secret and never render auth.tokens at all. That direction is worse
than the database one: an operator who edits auth.tokens to REVOKE a leaked
credential would see a clean upgrade, believe the token was withdrawn, and
leave it working. Nothing downstream can report the token that was never
rendered.
*/}}
{{- define "device-farmer.checkAuth" -}}
{{- if and .Values.auth.tokens .Values.auth.existingSecret -}}
{{- fail (printf "\ndevice-farmer: auth.tokens and auth.existingSecret are both set.\n\n  auth.existingSecret = %s\n  auth.tokens         = (set, and WOULD BE IGNORED)\n\nThe api reads FARM_API_TOKENS from %q. auth.tokens would render into no Secret\nand reach no pod, so editing it — to add a token, or to revoke one — would\nchange nothing while looking like it had.\n\nKeep the Secret and drop auth.tokens, or drop auth.existingSecret and let the\nchart write the Secret:\n\n  --set auth.tokens=\"\"                     # keep %s\n  --set auth.existingSecret=\"\"             # let the chart own the tokens\n" .Values.auth.existingSecret .Values.auth.existingSecret .Values.auth.existingSecret) -}}
{{- end -}}
{{- end -}}

{{/*
FARM_API_TOKENS, only where it is read. Absent, the API runs open — NOTES.txt
makes that impossible to miss.
*/}}
{{- define "device-farmer.authEnv" -}}
{{- if or .Values.auth.tokens .Values.auth.existingSecret -}}
- name: FARM_API_TOKENS
  valueFrom:
    secretKeyRef:
      name: {{ include "device-farmer.authSecretName" . }}
      key: {{ include "device-farmer.authSecretKey" . }}
{{- end }}
{{- end -}}

{{/*
The connection-pool floor.

internal/config accepts FARM_DB_MAX_CONNS >= 1, and it is right to: it does not
know which roles this pool will serve. This chart does, and one connection is a
deadlock it can see coming.

The scheduler and the reaper each pin ONE connection for the whole life of the
process — the session that holds their pg_try_advisory_lock leadership. With a
pool of one, the winner holds the only connection and its own sweep can never
get a second: the role comes up, logs that it acquired leadership, and then
allocates or reclaims nothing, forever, without an error anywhere. That is the
worst shape a failure can take in this system, so it is refused here rather
than discovered from a queue that stopped moving.
*/}}
{{- define "device-farmer.checkPool" -}}
{{- $max := .Values.config.db.maxConns | int -}}
{{- if lt $max 2 -}}
{{- fail (printf "\ndevice-farmer: config.db.maxConns is %d, and the minimum is 2.\n\nThe scheduler and the reaper each hold one connection for the entire life of\nthe process — the session carrying their leader-election advisory lock. A pool\nof one leaves the elected leader with nothing to work on: it reports that it\ntook leadership and then places no job and reclaims no lease, with no error to\nread. farmd accepts 1 because internal/config cannot know which role it is\nconfiguring; this chart deploys both roles from this one value.\n\nUse at least 2, and see the comment on config.db.maxConns in values.yaml for\nthe number the jobrunner wants.\n" $max) -}}
{{- end -}}
{{- end -}}

{{/*
hosts[] validation, in one place because two templates consume it.

templates/watchdog.yaml builds a Deployment name from hosts[].id and
templates/hosts.yaml builds a Service and an EndpointSlice name from it. Both
must reject the same values, and the checks used to live only in watchdog.yaml
— which meant watchdog.enabled=false let an id like "H_01" straight through
into a Service name that the API server rejects, halfway through an install
whose migration hook had already run. Validation that a value can switch off
is not validation.

farm.hosts.id is plain unconstrained text in the schema, so ids that are legal
in the database and illegal in Kubernetes are ordinary, not hypothetical.
*/}}
{{- define "device-farmer.checkHosts" -}}
{{- $seen := list -}}
{{- range $i, $host := .Values.hosts -}}
{{- if not $host.id -}}
{{- fail (printf "device-farmer: hosts[%d] has no id. It must equal farm.hosts.id — the value `farmd node` registers with FARM_HOST_ID on that machine — because every devpath is interpreted against it." $i) -}}
{{- end -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $host.id) -}}
{{- fail (printf "device-farmer: hosts[%d].id is %q, which is not a DNS-1123 label, and it becomes part of a Kubernetes resource name (a Deployment, and a Service if service.enabled). farm.hosts.id is unconstrained text, so this is legal in the database and illegal here. Rename the host in farm.hosts and in that machine's FARM_HOST_ID, or use lowercase letters, digits and dashes." $i $host.id) -}}
{{- end -}}
{{- if has $host.id $seen -}}
{{- fail (printf "device-farmer: host id %q appears twice in hosts[]. Both entries render the same Deployment name and the same Service name, so one of the two hosts silently loses its watchdog and stops being probed at all. Give each physical machine one entry." $host.id) -}}
{{- end -}}
{{- $seen = append $seen $host.id -}}
{{- if not $host.adbEndpoint -}}
{{- fail (printf "device-farmer: host %q has no adbEndpoint. It is the host:port the watchdog dials FROM THE CLUSTER; adb binds 127.0.0.1 unless the machine runs `adb -a -P 5037 nodaemon server`." $host.id) -}}
{{- end -}}
{{- if and $host.service $host.service.enabled -}}
{{- $addr := $host.service.address | default "" -}}
{{- if not $addr -}}
{{- fail (printf "device-farmer: host %q has service.enabled but no service.address. That is the address of the physical machine — an IP, or a DNS name resolvable from the cluster." $host.id) -}}
{{- end -}}
{{- if not (include "device-farmer.hostAddressKind" $addr) -}}
{{- fail (printf "device-farmer: host %q has service.address %q, which is neither an IP literal nor a bare DNS name.\n\nThe port belongs in service.port and the scheme belongs nowhere: this value becomes an EndpointSlice address or an ExternalName, and both are rejected by the API server if they carry a port, a scheme or a path.\n\n  address: 10.20.0.11      port: 5037\n  address: h01.lab.example.com" $host.id $addr) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Classifies a host address as "ipv4", "ipv6" or "dns", and renders nothing for
anything else. Both hosts.yaml and checkHosts ask, so that the object that gets
built and the guard that admits it can never disagree about what an address is.
*/}}
{{- define "device-farmer.hostAddressKind" -}}
{{- $a := . | toString -}}
{{- if regexMatch "^[0-9]{1,3}\\.[0-9]{1,3}\\.[0-9]{1,3}\\.[0-9]{1,3}$" $a -}}
ipv4
{{- else if and (contains ":" $a) (regexMatch "^[0-9a-fA-F:]+$" $a) -}}
ipv6
{{- else if regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" $a -}}
dns
{{- end -}}
{{- end -}}

{{/*
Seconds in a Go duration string, or nothing for a form this cannot read.

Helm has no duration parser and this only needs to compare two knobs, so it
reads the simple forms an operator actually writes and stays silent on the
rest. Declining to answer is deliberate: a guard that guessed at "1m30s" would
refuse a correct release, which is a worse outcome than not checking.
*/}}
{{- define "device-farmer.durationSeconds" -}}
{{- $d := . | toString | trim -}}
{{- if regexMatch "^[0-9]+s$" $d -}}
{{- trimSuffix "s" $d -}}
{{- else if regexMatch "^[0-9]+m$" $d -}}
{{- mul (trimSuffix "m" $d | int) 60 -}}
{{- else if regexMatch "^[0-9]+h$" $d -}}
{{- mul (trimSuffix "h" $d | int) 3600 -}}
{{- end -}}
{{- end -}}

{{/*
The api's drain window must outlast its drain.

FARM_SHUTDOWN_GRACE is the deadline api.Server.Run gives http.Server.Shutdown
(it is read by no other role). terminationGracePeriodSeconds is how long the
kubelet waits before SIGKILL. If the second is not larger than the first, the
process is killed while it is still draining, and the request most likely to be
in flight on a rolling deploy is the one that costs something: a renewal one
round trip from success. The lease survives — a failed renewal ends nothing —
but the holder burns TTL it did not have to.
*/}}
{{- define "device-farmer.checkDrainWindow" -}}
{{- $grace := include "device-farmer.durationSeconds" .Values.config.shutdownGrace -}}
{{- if $grace -}}
{{- $window := .Values.api.terminationGracePeriodSeconds | int -}}
{{- if le $window ($grace | int) -}}
{{- fail (printf "\ndevice-farmer: the api's drain window is not larger than its drain.\n\n  api.terminationGracePeriodSeconds = %d seconds\n  config.shutdownGrace              = %s (%d seconds)\n\nThe kubelet would SIGKILL the api while it was still draining. On every rolling\ndeploy and every node drain the request most likely to be cut off is the one\nthat matters: POST /api/v1/leases/{id}/renew, one round trip from success. No\nlease ends from that — a failed renewal ends nothing — but the holder pays TTL\nfor a deploy that should have cost it nothing.\n\nRaise api.terminationGracePeriodSeconds above %d, or lower config.shutdownGrace.\n" $window .Values.config.shutdownGrace ($grace | int) ($grace | int)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Every cross-value assertion, in one call.

configmap.yaml includes this, and configmap.yaml is the one template that
renders in every possible release — so no combination of enabled/disabled
values can leave a check unrun. Each guard is also cheap and idempotent,
because the checksum annotations re-render this template once per workload.
*/}}
{{- define "device-farmer.validate" -}}
{{- include "device-farmer.requireDatabase" . -}}
{{- include "device-farmer.checkAuth" . -}}
{{- include "device-farmer.checkPool" . -}}
{{- include "device-farmer.checkHosts" . -}}
{{- include "device-farmer.checkDrainWindow" . -}}
{{- end -}}

{{/* Everything that is not a credential, shared by every role. */}}
{{- define "device-farmer.envFrom" -}}
- configMapRef:
    name: {{ include "device-farmer.configMapName" . }}
{{- end -}}

{{/*
Annotations that roll a Deployment when its configuration changes. A lease
knob that only takes effect at the next unrelated restart is a lease knob that
means something different on every pod.
*/}}
{{- define "device-farmer.configChecksums" -}}
checksum/config: {{ include (print .Template.BasePath "/configmap.yaml") . | sha256sum }}
checksum/secret: {{ include (print .Template.BasePath "/secret.yaml") . | sha256sum }}
{{- end -}}

{{/*
Spread replicas over nodes. Only for the roles that actually have replicas —
a constraint on a single-replica leader-elected Deployment buys nothing and
can block its own rescheduling.
*/}}
{{- define "device-farmer.spread" -}}
- maxSkew: 1
  topologyKey: kubernetes.io/hostname
  # ScheduleAnyway, not DoNotSchedule: a farm with one node left should still
  # be running its API, not holding out for a topology it cannot have.
  whenUnsatisfiable: ScheduleAnyway
  labelSelector:
    matchLabels:
      {{- include "device-farmer.selectorLabels" . | nindent 6 }}
{{- end -}}

{{/* The artifact store volume, mounted only by the roles that read or write it. */}}
{{- define "device-farmer.artifactVolume" -}}
- name: artifacts
{{- if .Values.artifacts.persistence.enabled }}
  persistentVolumeClaim:
    claimName: {{ required "device-farmer: set artifacts.persistence.existingClaim to the name of a PersistentVolumeClaim, or turn artifacts.persistence.enabled off. The claim must be ReadWriteMany: the api writes an uploaded APK and a jobrunner on another node reads it back, so a ReadWriteOnce volume fails the install step of every job that lands on the wrong node." .Values.artifacts.persistence.existingClaim }}
{{- else }}
  # Per-pod. An artifact stored by one replica is invisible to the others;
  # see the comment on artifacts.persistence in values.yaml.
  emptyDir: {}
{{- end }}
{{- end -}}

{{- define "device-farmer.artifactVolumeMount" -}}
- name: artifacts
  mountPath: {{ .Values.artifacts.dir }}
{{- end -}}

{{/* The in-cluster URL of this release's API. */}}
{{- define "device-farmer.apiURL" -}}
{{- if .Values.config.apiBaseURL -}}
{{- .Values.config.apiBaseURL -}}
{{- else -}}
{{- printf "http://%s-api.%s.svc.cluster.local:%v" (include "device-farmer.fullname" .) .Release.Namespace .Values.api.service.port -}}
{{- end -}}
{{- end -}}
