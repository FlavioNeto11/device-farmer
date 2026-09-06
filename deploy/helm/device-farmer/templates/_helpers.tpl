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

{{/*
Full name of one role's workload, e.g. release-device-farmer-scheduler.

The 63-character cut falls on the FULLNAME, never on the component, and that is
the whole point of this helper. The usual `printf | trunc 63` idiom cuts the
TAIL — which here is the only part of the string that says which role, or which
host, this object belongs to. Measured against ci-values.yaml: at a 48-character
release name all nine Deployments and all four Services rendered under one
identical 62-character name, and `helm template` exited 0 while doing it. At 41
only the second host's watchdog disappeared, which is quieter and worse — that
is exactly the outcome checkHosts refuses a duplicate host id to prevent,
reached from the other end. Helm's own release-name limit is 53, so both are
legal input.

Trimming the prefix keeps every suffix intact, so the names stay distinct at any
release-name length Helm will accept. A component that cannot fit on its own is
refused rather than trimmed, because a trimmed suffix is the failure this helper
exists to stop.
*/}}
{{- define "device-farmer.componentName" -}}
{{- $component := .component | toString -}}
{{- $room := sub 62 (len $component) | int -}}
{{- if lt $room 1 -}}
{{- fail (printf "\ndevice-farmer: the name suffix %q is %d characters, and a Kubernetes name stops at 63.\n\nEven one character of the release name will not fit in front of it, so this\nobject cannot be given a name that is both complete and distinct from its\nsiblings. Shorten what the suffix is built from — a hosts[].id, almost\ncertainly.\n" $component (len $component)) -}}
{{- end -}}
{{- printf "%s-%s" (include "device-farmer.fullname" .ctx | trunc $room | trimSuffix "-") $component -}}
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
The statement, if any, by which this release asks to serve an OPEN API — or
nothing, which is the answer for every release that did not ask.

internal/api/auth_wiring.go accepts exactly three things as permission to
install the allow-all authenticator: FARM_API_ALLOW_ANONYMOUS=true,
FARM_API_AUTH=allow-all, and a loopback listener. The first two arrive through
config.extra, because templates/configmap.yaml owns the environment and there
is no auth.* value that reaches it. The third cannot be reached from this chart
at all: the ConfigMap pins FARM_API_ADDR to 0.0.0.0 so the api answers more than
its own probes, and openModeAllowed reads the socket rather than a promise about
it.

A FARM_API_ALLOW_ANONYMOUS that is neither true nor false is deliberately NOT
counted as a statement here. anonymousRequested refuses it by name and quotes
the value back, which sends the operator to the variable they actually mistyped
— better than a render-time refusal that talks about tokens they never touched.
*/}}
{{- define "device-farmer.openOnPurpose" -}}
{{- $extra := .Values.config.extra | default dict -}}
{{- $anon := get $extra "FARM_API_ALLOW_ANONYMOUS" | toString | trim | lower -}}
{{- $mode := get $extra "FARM_API_AUTH" | toString | trim | lower -}}
{{- if has $anon (list "true" "t" "1") -}}
config.extra.FARM_API_ALLOW_ANONYMOUS={{ $anon }}
{{- else if eq $mode "allow-all" -}}
config.extra.FARM_API_AUTH=allow-all
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

The other two refusals here are about a release that configured no credentials
at all. This chart cannot render one that works: the api role reads
internal/api/auth_wiring.go, which will not install the allow-all authenticator
on a listener the whole network can reach, and the ConfigMap gives it exactly
that listener. Rendering it anyway costs five minutes of `--wait` and then
"Available: 0/2", with nothing in the timeout that mentions a token — the
message an operator needs is in a pod log they have to know to go and read.
*/}}
{{- define "device-farmer.checkAuth" -}}
{{- $configured := or .Values.auth.tokens .Values.auth.existingSecret -}}
{{- $open := include "device-farmer.openOnPurpose" . -}}
{{- if and .Values.auth.tokens .Values.auth.existingSecret -}}
{{- fail (printf "\ndevice-farmer: auth.tokens and auth.existingSecret are both set.\n\n  auth.existingSecret = %s\n  auth.tokens         = (set, and WOULD BE IGNORED)\n\nThe api reads FARM_API_TOKENS from %q. auth.tokens would render into no Secret\nand reach no pod, so editing it — to add a token, or to revoke one — would\nchange nothing while looking like it had.\n\nKeep the Secret and drop auth.tokens, or drop auth.existingSecret and let the\nchart write the Secret:\n\n  --set auth.tokens=\"\"                     # keep %s\n  --set auth.existingSecret=\"\"             # let the chart own the tokens\n" .Values.auth.existingSecret .Values.auth.existingSecret .Values.auth.existingSecret) -}}
{{- end -}}
{{- if and (not $configured) (not $open) -}}
{{- fail (printf "\ndevice-farmer: no API credentials, and this is not a deployment where running\nopen is safe.\n\n  auth.tokens         = (empty)\n  auth.existingSecret = (empty)\n  FARM_API_ADDR       = 0.0.0.0:%v   (set by templates/configmap.yaml)\n\nThe api would not start. It refuses to serve an unauthenticated control plane on\na listener the network can reach, so every api pod would be in\nCrashLoopBackOff while `helm install --wait` sat there until its own timeout.\n\nChoose one of three, all of which install:\n\n  # tokens, written by the chart into a Secret of its own\n  --set auth.tokens='<token>:operator:ci'\n\n  # tokens you already keep somewhere else, under the key FARM_API_TOKENS\n  --set auth.existingSecret=farm-api-tokens\n\n  # an OPEN control plane, on purpose — an evaluation farm, a demo, a laptop\n  --set config.extra.FARM_API_ALLOW_ANONYMOUS=true\n\nA list of more than one token belongs in a values file: helm's --set splits on\ncommas, so the second entry arrives as a key with no value.\n\nThe third is a real answer and not a formality, but write it knowing what it\ngrants. With no authenticator every caller that can reach port %v holds the\noperator role: it can revoke a live lease, cancel somebody's job, drain a host\nand cut power to a USB port that is holding a running job. That is the one\nconfiguration in which a stranger ends six hours of work, so it is spelled out\nrather than reached by leaving a value empty.\n" .Values.api.service.port .Values.api.service.port) -}}
{{- end -}}
{{- if and $configured $open -}}
{{- fail (printf "\ndevice-farmer: this release lists API credentials AND asks for an open API.\n\n  %s\n  %s\n\nfarmd refuses that pair at startup rather than picking a winner, and the chart\nrefuses it here for the same reason one CrashLoopBackOff earlier: whichever half\nloses is silently ignored, and half the time the half that loses is the one\nasking for authentication.\n\nDrop the opt-in to require credentials, or drop the credentials to serve open:\n\n  --set config.extra.FARM_API_ALLOW_ANONYMOUS=null\n  --set auth.tokens=\"\" --set auth.existingSecret=\"\"\n" (ternary (printf "auth.existingSecret = %s" (.Values.auth.existingSecret | toString)) "auth.tokens         = (set)" (ne (.Values.auth.existingSecret | toString) "")) $open) -}}
{{- end -}}
{{- end -}}

{{/*
FARM_API_TOKENS, only where it is read. Absent, this release asked for an open
API by name — checkAuth refuses every other way of arriving here — and NOTES.txt
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

The scheduler, the reaper and the janitor each pin ONE connection for the whole
life of the process — the session that holds their pg_try_advisory_lock
leadership. With a pool of one, the winner holds the only connection and its
own sweep can never get a second: the role comes up, logs that it acquired
leadership, and then allocates, reclaims or closes nothing, forever, without an
error anywhere. That is the worst shape a failure can take in this system, so
it is refused here rather than discovered from a queue that stopped moving.
*/}}
{{- define "device-farmer.checkPool" -}}
{{- $max := .Values.config.db.maxConns | int -}}
{{- if lt $max 2 -}}
{{- fail (printf "\ndevice-farmer: config.db.maxConns is %d, and the minimum is 2.\n\nThe scheduler, the reaper and the janitor each hold one connection for the\nentire life of the process — the session carrying their leader-election\nadvisory lock. A pool of one leaves the elected leader with nothing to work on:\nit reports that it took leadership and then places no job, reclaims no lease\nand closes no orphan, with no error to read. farmd accepts 1 because\ninternal/config cannot know which role it is configuring; this chart deploys\nall three roles from this one value.\n\nUse at least 2, and see the comment on config.db.maxConns in values.yaml for\nthe number the jobrunner wants.\n" $max) -}}
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
{{/*
The same collision, arrived at by length rather than by typo.

device-farmer.componentName trims the release prefix instead of the suffix so
that two hosts can never share a name, which needs the suffix plus a joining
dash plus at least one character of prefix to fit in 63 — a suffix of 61. The
longest suffix an id produces is the watchdog Deployment's, so that is the one
checked here, and it is checked whether or not watchdog.enabled is on: an id is
supposed to survive turning the watchdog back on.
*/}}
{{- $idMax := sub 61 (len "watchdog-") | int -}}
{{- if gt (len $host.id) $idMax -}}
{{- fail (printf "device-farmer: hosts[%d].id is %d characters and the limit is %d. It becomes the tail of a Deployment name (%q) and of a Service name, and a Kubernetes name stops at 63 — so past %d characters the id itself is what gets cut, and two hosts whose ids differ only after the cut land on one Deployment and one Service. That is the duplicate-id failure above, reached by length. Give the machine a shorter id in farm.hosts and in its FARM_HOST_ID." $i (len $host.id) $idMax (printf "watchdog-%s" $host.id) $idMax) -}}
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
The Secret that holds the cluster's fence-proxy material: the release's own
when the PEMs are inline, the operator's when existingSecret is named.
*/}}
{{- define "device-farmer.fenceSecretName" -}}
{{- if .Values.fenceProxy.existingSecret -}}
{{- .Values.fenceProxy.existingSecret -}}
{{- else -}}
{{- printf "%s-fence" (include "device-farmer.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
fenceProxy validation (U10).

The proxy runs on the device hosts; what the cluster holds is the material its
clients need to reach one. Three things are refused here, because each of them
renders cleanly and leaves a farm that LOOKS fenced:

  - one or two of ca/cert/key: a client with a certificate and no CA trusts no
    host; one with a CA and no certificate is refused by every host;
  - material given while enabled is false, or inline PEMs next to an
    existingSecret: one of the two would be dropped without a word;
  - enabled with no extraVolumes/extraVolumeMounts entry that puts THIS Secret
    at fenceProxy.mountPath: a Secret nobody mounts reaches no pod.
*/}}
{{- define "device-farmer.checkFence" -}}
{{- $fp := .Values.fenceProxy -}}
{{- $inline := 0 -}}
{{- range list $fp.ca $fp.cert $fp.key -}}
{{- if . -}}{{- $inline = add1 $inline -}}{{- end -}}
{{- end -}}
{{- if and (gt $inline 0) (lt $inline 3) -}}
{{- fail (printf "\ndevice-farmer: fenceProxy has %d of ca, cert and key, and they are set together or not\nat all. A client with a certificate and no CA trusts no host; one with a CA and no\ncertificate is refused by every host. Set all three, or name an existingSecret that\nholds ca.crt, tls.crt and tls.key.\n" $inline) -}}
{{- end -}}
{{- if and (eq $inline 3) $fp.existingSecret -}}
{{- fail "\ndevice-farmer: fenceProxy has inline ca/cert/key AND an existingSecret. The chart would\nmount the existingSecret and drop the inline material without a word. Set one or the\nother.\n" -}}
{{- end -}}
{{- if and (not $fp.enabled) (or (eq $inline 3) $fp.existingSecret) -}}
{{- fail "\ndevice-farmer: fenceProxy has material (inline PEMs or an existingSecret) but\nfenceProxy.enabled is false, so nothing would be rendered or mounted and every client\nwould keep dialling the hosts without a certificate. Set fenceProxy.enabled: true, or\nremove the material.\n" -}}
{{- end -}}
{{- if $fp.enabled -}}
{{- if and (eq $inline 0) (not $fp.existingSecret) -}}
{{- fail "\ndevice-farmer: fenceProxy.enabled is true and there is nothing to mount: give ca, cert\nand key inline, or name an existingSecret holding ca.crt, tls.crt and tls.key.\n" -}}
{{- end -}}
{{- $secret := include "device-farmer.fenceSecretName" . -}}
{{- $volume := "" -}}
{{- range .Values.extraVolumeMounts -}}
{{- if eq (.mountPath | toString) ($fp.mountPath | toString) -}}{{- $volume = .name | toString -}}{{- end -}}
{{- end -}}
{{- $mounted := false -}}
{{- range .Values.extraVolumes -}}
{{- if and $volume (eq (.name | toString) $volume) .secret (eq (.secret.secretName | toString) $secret) -}}{{- $mounted = true -}}{{- end -}}
{{- end -}}
{{- if not $mounted -}}
{{- fail (printf "\ndevice-farmer: fenceProxy.enabled is true but no extraVolumes/extraVolumeMounts pair\nputs the Secret %q at\n\n  %s\n\nThe chart holds the cluster's fence material in that Secret and mounts it through\nthe one mechanism every farmd pod shares; without the mount it reaches no pod while\nlooking configured. Add:\n\n  extraVolumes:\n    - name: fence\n      secret:\n        secretName: %s\n  extraVolumeMounts:\n    - name: fence\n      mountPath: %s\n      readOnly: true\n\nor change fenceProxy.mountPath to where you mount it.\n" $secret $fp.mountPath $secret $fp.mountPath) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The two ends of the metrics wire, which come from different places and can
disagree with nothing to notice.

templates/configmap.yaml sets FARM_METRICS_ADDR as the literal ":9090" — it
reads no value — while templates/metrics-service.yaml renders both port and
targetPort from metricsService.port. Moving the value moves the Service and
leaves every farmd process on 9090: with a ServiceMonitor in front of it every
target goes up == 0, every farm_* series stops, and the render, the lint and
the other guards all say nothing. Silence is what observability looks like when
it is broken, which is why this one is refused rather than warned about.

IF THE CONFIGMAP EVER READS metricsService.port, DELETE THIS FIRST HALF in the
same edit: it exists only because that line is a literal, and left behind it
would refuse a release that had become correct.

The second half is the same shape one level up. A ServiceMonitor selects
app.kubernetes.io/component=metrics, and the only object that ever carries that
label is the headless Service; without it the monitor sits in the cluster
selecting nothing. Prometheus discovers zero targets, so not even up == 0 fires
— there is no target to be down — and every rule in the PrometheusRule returns
no data, which reads exactly like a quiet farm.
*/}}
{{- define "device-farmer.checkMetrics" -}}
{{- if .Values.metricsService.enabled -}}
{{- $port := .Values.metricsService.port | int -}}
{{- if ne $port 9090 -}}
{{- fail (printf "\ndevice-farmer: metricsService.port is %d, and every farmd process would still be\nlistening on 9090.\n\n  metricsService.port          = %d   (Service port and targetPort)\n  FARM_METRICS_ADDR            = :9090 (a literal in templates/configmap.yaml)\n\nThe two ends of that wire come from different places. The Service would point\nat a port nothing binds, every ServiceMonitor target would go up == 0, and all\nfarm_* series would stop — with no error anywhere, because a scrape that finds\nnothing looks the same as a farm with nothing to say.\n\nLeave metricsService.port at 9090. If your site needs another number, it has to\nchange in templates/configmap.yaml too, and then this guard is what to delete.\n" $port $port) -}}
{{- end -}}
{{- end -}}
{{- if and .Values.serviceMonitor.enabled (not .Values.metricsService.enabled) -}}
{{- fail "\ndevice-farmer: serviceMonitor.enabled is true and metricsService.enabled is false.\n\nThe ServiceMonitor selects the Service labelled app.kubernetes.io/component=metrics,\nand that Service is the one metricsService renders. Without it the monitor is an\nobject in the cluster that selects nothing: Prometheus discovers zero targets, so\nno up == 0 fires either — there is no target to be down — and every rule in the\nPrometheusRule returns no data. Coverage that reads as coverage and is not.\n\nTurn metricsService.enabled back on, or turn serviceMonitor.enabled off and\nscrape these pods whichever way your cluster already does.\n" -}}
{{- end -}}
{{- end -}}

{{/*
The two things about the migration hook that render cleanly and then fail in the
cluster with nothing readable to go on.

NAME. templates/migrate-job.yaml builds "<fullname>-migrate" whole — it is the
one name in this chart that is not trimmed to fit — and Kubernetes copies a
Job's name into a job-name label on its pod template. Label VALUES stop at 63
where names do not, so past that the API server rejects the Job at admission
("spec.template.labels: Invalid value: ...: must be no more than 63 bytes") and
the install dies at the hook, with an error that names a label rather than a
release.

ORDER. Helm runs hooks in weight order and the Secret carrying the DSN is the
string literal "-10" in templates/secret.yaml, so it cannot move with this
value. A Job that does not sort strictly after it has a secretKeyRef that
resolves to nothing: the pod sits in CreateContainerConfigError, no container
ever starts, and `kubectl logs` has nothing at all to show while Helm waits out
its hook timeout. Equal weights are refused along with lower ones, because at
equal weight the order is Helm's to choose and a coin flip is not an ordering.
The constraint only exists when the chart owns that Secret — with
database.existingSecret the credential is already in the namespace before any
hook runs and the weights are free.
*/}}
{{- define "device-farmer.checkMigrate" -}}
{{- if .Values.migrate.enabled -}}
{{- $job := printf "%s-migrate" (include "device-farmer.fullname" .) -}}
{{- if gt (len $job) 63 -}}
{{- fail (printf "\ndevice-farmer: the migration hook Job would be named\n\n  %s\n\nwhich is %d characters. Kubernetes copies a Job's name into a job-name label on\nits pod template, and a label value stops at 63 even though a name does not — so\nthe API server rejects the Job at admission and the install fails at the hook,\nbefore any schema is touched, with an error that talks about a label.\n\nThe name is <fullname>-migrate, so the release name (or fullnameOverride) has to\nleave room for those %d characters: a fullname of %d or fewer. Every other name\nin this chart trims the release prefix to fit; this one is built whole.\n" $job (len $job) (len "-migrate") (sub 63 (len "-migrate") | int)) -}}
{{- end -}}
{{- if and (not .Values.database.existingSecret) (le (.Values.migrate.hookWeight | int) -10) -}}
{{- fail (printf "\ndevice-farmer: migrate.hookWeight is %d, and the Secret carrying the DSN is -10.\n\nHelm runs hooks in weight order, so the Job would not be guaranteed to come\nafter the Secret its DATABASE_URL is read from — below -10 it certainly does\nnot, and at -10 the order is Helm's to pick. Whenever it loses, the secretKeyRef\nresolves to nothing: the pod stops at CreateContainerConfigError with no\ncontainer and therefore no logs, and Helm fails the release once its hook\ntimeout runs out, having migrated nothing and left nothing to read.\n\nThat -10 is a literal in templates/secret.yaml and cannot follow this value.\nUse a weight above -10; the default is -5.\n" (.Values.migrate.hookWeight | int)) -}}
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
{{- include "device-farmer.checkFence" . -}}
{{- include "device-farmer.checkMetrics" . -}}
{{- include "device-farmer.checkMigrate" . -}}
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
