/* i18n.js — this dashboard in English and in Brazilian Portuguese.
 *
 * # What is translated, and what deliberately is not
 *
 * The INTERFACE is translated: every word this page writes itself — navigation,
 * column headings, buttons, empty states, the sentences that explain a refusal.
 *
 * Three things are NOT, and each for a reason a reader should be able to check:
 *
 *  1. IDENTIFIERS. `farm.leases`, `release_reason`, `usb:3-1.4`, `no_disruption`,
 *     `FARM_LEASE_TTL`, role names, error codes. These are what the schema, the
 *     API and the logs say. An operator matches them by eye against a psql
 *     session or a log line, and a translated identifier is one they cannot
 *     find. The status vocabulary an operator reads on a chip — `held`,
 *     `healthy`, `quarantined` — is in this category: the word is the value in
 *     the column, and the page prints the value.
 *
 *  2. DATA FROM THE SERVER. A holder's name, a job's spec, a device's model, the
 *     output of a shell command. The page renders what it was given.
 *
 *  3. THE `ctl` COMMAND LINE. It is not this page, and its output is grepped by
 *     scripts. Translating a CLI's output breaks the scripts written against it,
 *     silently, on the machines least likely to be watched.
 *
 * # How a string gets here
 *
 * `t('key')` in JS, or `data-i18n="key"` on an element in index.html. For an
 * attribute rather than the text, `data-i18n-attr="placeholder:key title:other"`.
 *
 * EVERY KEY MUST EXIST IN BOTH DICTIONARIES. `TestEveryStringExistsInBothLanguages`
 * fails otherwise, and it fails loudly rather than letting a Portuguese reader
 * meet an English word with no explanation — which is what a silent fallback
 * would produce, one string at a time, forever.
 *
 * # Where the choice lives
 *
 * In localStorage, per browser, because it is a reading preference and not a
 * property of the farm: two operators sharing a control plane can want different
 * languages, and neither should be able to change the other's. The first visit
 * takes a guess from the browser's own setting, which is the only evidence
 * available at that point.
 */

const LANG_KEY = 'device-farmer.lang';
const LANGS = ['en', 'pt'];

/* Density lives here for one reason: this file is the only script that runs
 * before the first paint, and density has to be decided before it. A page that
 * paints comfortable and snaps to compact a frame later is worse than either.
 *
 * It is a reading preference, like the language, so it is stored the same way:
 * localStorage, per browser, never on the server. Two operators sharing a
 * control plane can want different densities and neither can change the
 * other's. */
const DENSITY_KEY = 'device-farmer.density';
const DENSITIES = ['comfortable', 'compact'];

/* The dictionaries.
 *
 * Ordered as the page is: chrome first, then per view, then the pieces shared
 * across views. A key is a path — `fleet.filters.health` — so that a reader
 * looking at a string on screen can find it, and so that a whole view's strings
 * sit together when somebody adds a third language. */
const STRINGS = {
  en: {
    /* The command builder. Labels and explanations only — a command is never
     * translated, for the reason examples[].code is never translated: a
     * translated command does not run. The templates live in exec.js. */
    'exec.title': 'Run one ADB command',
    'exec.catalogue': 'Commands',
    'exec.run': 'Run',
    'exec.running': 'running…',
    'exec.refused': 'Refused:',
    'exec.tier.verified': 'Run by this farm',
    'exec.tier.verifiedNote': 'Each of these is sent to real handsets by this project, and names the file that sends it.',
    'exec.tier.unverified': 'Ordinary Android, not run here',
    'exec.tier.unverifiedNote': 'Widely used commands that nobody on this project has executed. On a simulated farm most of them answer with one blank line and exit 0.',
    'exec.unverified': 'not run by this farm',
    'exec.unverifiedWhy': 'No code in this repository sends this command. It is here because it is widely used, not because anybody here has watched it work.',
    'exec.verifiedWhy': 'The file that sends this exact command to a handset. A test reads that file and fails the build if the command is not in it.',
    'exec.group.identity': 'Identity',
    'exec.group.health': 'Health',
    'exec.group.packages': 'Packages',
    'exec.group.logs': 'Logs',
    'exec.group.storage': 'Storage',
    'exec.effect.read': 'reads',
    'exec.effect.readWhy': 'This command reports and changes nothing on the device.',
    'exec.effect.writes': 'changes the device',
    'exec.effect.writesWhy': 'This command leaves the handset different from how it found it.',
    'exec.freeform': 'Typed by you. This is not classified as reading or changing the device, and that is deliberate: no pattern over a shell command line can tell the two apart reliably — which is the same argument the fence proxy makes when it refuses to admit one at all.',
    'exec.wire': 'What goes on the wire',
    'exec.wireNote': 'Sent exactly like this. Nothing quotes it, escapes it or checks it — the handset\'s own /system/bin/sh does the word-splitting, the ; the | and the $( ).',
    'exec.preflight': 'What can refuse this, in the order the server asks',
    'exec.pf.command': 'a command',
    'exec.pf.commandOk': 'ready',
    'exec.pf.commandEmpty': 'you have not typed one',
    'exec.pf.fence': 'the fence',
    'exec.pf.fenceOn': 'this farm enforces the fence at the device — every command is refused, on every device',
    'exec.pf.fenceOff': 'not enforced at the device on this farm',
    'exec.pf.fenceUnknown': 'cannot be known from here right now',
    'exec.pf.slot': 'a physical position',
    'exec.pf.slotNone': 'this device is in no slot, so it has no address',
    'exec.pf.endpoint': 'the host\'s ADB server',
    'exec.pf.endpointNone': 'no endpoint recorded for this host',
    'exec.pf.lease': 'a live lease',
    'exec.pf.leaseHeld': 'held by {holder}, job {job} — needs force and a reason',
    'exec.pf.leaseFree': 'free, as of the last fleet read',
    'exec.pf.answer': 'the device answering',
    'exec.pf.answerUnknown': 'cannot be known until it is asked',
    'exec.pf.targets': 'which devices',
    'exec.pf.targetsBulk': 'decided by the selector; the fence refusal below does not cover bulk, which reports one 502 per target instead',
    'exec.timeout': 'Give up after (ms, at most {max}s)',
    'exec.reasonPlaceholder': 'why you are running this on a device somebody is using',
    'exec.force.head': 'This device holds a live lease',
    'exec.force.reason': 'Reason',
    'exec.force.audit': 'The reason and the command are written to farm.audit_log next to your name. Six weeks from now they are the only record of why.',
    'exec.force.holder': 'Holder {holder} is using this device now.',
    'exec.force.job': 'Job {job} is running on it.',
    'exec.force.tenant': 'It belongs to tenant {tenant}.',
    'exec.force.protected': 'The lease is protected: it is not reclaimed automatically, which is a sign somebody meant it.',
    'exec.force.noSignal': 'Running a command anyway can corrupt that job\'s run, and the holder gets no signal that it happened. The lease itself is not ended either way.',
    'exec.result.neverExited': 'The exit status never arrived. Whether the command finished on the device is UNKNOWN — it may still be running. No lease was affected.',
    'exec.result.exited0': 'Exited 0.',
    'exec.result.exitedN': 'Exited {code} on the device. No lease was affected.',
    'exec.result.silent': 'It printed nothing. That is a real outcome — and it is also what a simulated farm answers for a command its fake does not script.',
    'exec.result.truncated': 'Output hit the server\'s cap and the rest was discarded. What you see below is not all of it.',
    'exec.result.took': 'Took {ms} ms.',
    'exec.result.stderr': '--- stderr ---',
    'exec.result.nothing': '(no output)',
    'exec.landedAfterClose': 'The command you ran on {where} finished after you closed the drawer. It was not cancelled — see the Events tab for what it did.',
    'exec.cat.getprop.label': 'Read a build property',
    'exec.cat.getprop.why': 'Asks the handset for one of the eleven properties internal/enroll reads on every sighting.',
    'exec.cat.brandRead.label': 'Read the farm brand',
    'exec.cat.brandRead.why': 'The farm_uid this project writes onto a device so it can be recognised after a serial changes.',
    'exec.cat.battery.label': 'Battery, in full',
    'exec.cat.battery.why': 'The exact command internal/watchdog sends to every attached device once a minute.',
    'exec.cat.batteryLevel.label': 'Battery level only',
    'exec.cat.batteryLevel.why': 'The one line, for when the full dump is more than you want to read.',
    'exec.cat.idle.label': 'Deep idle state',
    'exec.cat.idle.why': 'Whether the device has dropped into deep doze, which changes what a job can expect of it.',
    'exec.cat.packages.label': 'Installed third-party packages',
    'exec.cat.packages.why': 'What has been installed on top of the system image.',
    'exec.cat.logcat.label': 'Last N log lines',
    'exec.cat.logcat.why': 'A bounded logcat dump. Bounded on purpose: the server caps captured output and discards the rest.',
    'exec.cat.bootCompleted.label': 'Has it finished booting',
    'exec.cat.bootCompleted.why': 'The property the reset ladder waits on after a reboot.',
    'exec.cat.df.label': 'Free space',
    'exec.cat.df.why': 'Filesystem usage. A full /data is a common cause of installs that fail for no obvious reason.',
    'exec.cat.ps.label': 'Running processes',
    'exec.cat.ps.why': 'Every process on the device.',
    'exec.cat.uptime.label': 'Uptime',
    'exec.cat.uptime.why': 'How long since the handset last booted.',
    'exec.cat.wmSize.label': 'Screen size',
    'exec.cat.wmSize.why': 'The display resolution, in device pixels — not the size a live screen streams at.',
    'exec.cat.wmDensity.label': 'Screen density',
    'exec.cat.wmDensity.why': 'Pixel density, which decides how large everything on the screen appears.',
    'exec.cat.dumpsysWindow.label': 'Displays',
    'exec.cat.dumpsysWindow.why': 'The window manager\'s view of every display attached.',
    'exec.cat.meminfo.label': 'Memory',
    'exec.cat.meminfo.why': 'Memory use across the system and every process.',
    'exec.cat.thermal.label': 'Thermal state',
    'exec.cat.thermal.why': 'What the device thinks of its own temperature, which is not the battery temperature the watchdog reads.',
    'exec.cat.wifi.label': 'Wi-Fi state',
    'exec.cat.wifi.why': 'The first forty lines of the Wi-Fi dump, which is where the connection state is.',
    'exec.cat.packagesAll.label': 'Every installed package',
    'exec.cat.packagesAll.why': 'System packages included. Long.',
    'exec.cat.packagePath.label': 'Where a package lives',
    'exec.cat.packagePath.why': 'The APK path for one package.',
    'exec.cat.dumpsysPackage.label': 'One package, in detail',
    'exec.cat.dumpsysPackage.why': 'Version, permissions and install state for one package.',
    'exec.cat.logcatCrash.label': 'The crash buffer',
    'exec.cat.logcatCrash.why': 'Only the crash log, which is usually what you wanted when an app disappeared.',
    'exec.cat.settingsGet.label': 'Read a setting',
    'exec.cat.settingsGet.why': 'One value out of the settings provider.',
    'exec.cat.lsTmp.label': 'What is in /data/local/tmp',
    'exec.cat.lsTmp.why': 'The directory this project pushes artifacts and markers into.',
    'exec.cat.topOnce.label': 'Top processes, once',
    'exec.cat.topOnce.why': 'A single non-interactive top, so it returns instead of streaming forever.',
    /* Header and navigation */
    'app.name': 'device-farmer',
    'app.skip': 'Skip to content',
    'nav.views': 'Views',
    'nav.fleet': 'Fleet',
    'nav.leases': 'Leases',
    'nav.jobs': 'Jobs',
    'nav.recovery': 'Recovery',
    'nav.bulk': 'Bulk',
    'nav.events': 'Events',
    'nav.docs': 'Docs',
    'header.searchLabel': 'Search devices, hosts, jobs and holders',
    'header.searchPlaceholder': 'Search  ( / )',
    'header.refresh': 'Refresh',
    'header.refreshTitle': 'Refetch every view now',
    'header.token': 'Token',
    'header.tokenTitle': 'Set the bearer token this browser sends to the API',
    'header.language': 'Language',
    'header.languageTitle': 'Read this dashboard in English or Portuguese',

    /* Connection state. These are about THIS page's connection to the API and
     * say nothing about any lease — see conn.downNote. */
    'conn.live': 'live',
    'conn.polling': 'polling',
    'conn.connecting': 'connecting',
    'conn.down': 'no connection',
    'conn.downNote': 'This page cannot reach the API. It says nothing about the farm: leases are held in PostgreSQL and are unaffected by a browser that lost its connection.',

    /* Filters, shared across views */
    'filter.host': 'Host',
    'filter.hub': 'Hub',
    'filter.health': 'Health',
    'filter.pool': 'Pool',
    'filter.lease': 'Lease',
    'filter.state': 'State',
    'filter.allHosts': 'all hosts',
    'filter.allHubs': 'all hubs',
    'filter.allPools': 'all pools',
    'filter.anyHealth': 'any health',
    'filter.anyLeaseState': 'any lease state',
    'filter.anyState': 'any state',
    'filter.clear': 'Clear',
    'filter.anyFault': 'not healthy (any fault)',

    /* Fleet */
    'count.total': 'total',
    'count.leased': 'leased',
    'count.free': 'free',
    'count.unhealthy': 'unhealthy',
    'count.quarantined': 'quarantined',
    'count.protected': 'protected',
    'count.devices': 'devices',
    'count.live': 'live',
    'count.expired': 'expired',
    'count.released': 'released',
    'count.running': 'running',
    'count.queued': 'queued',
    'count.failed': 'failed',
    'count.succeeded': 'succeeded',
    'fleet.total': 'total',
    'fleet.leased': 'leased',
    'fleet.free': 'free',
    'fleet.unhealthy': 'unhealthy',
    'fleet.quarantined': 'quarantined',
    'fleet.protected': 'protected',
    'fleet.showing': 'showing',
    'fleet.devices': 'devices',
    'fleet.liveLeases': 'live leases',
    'fleet.seen': 'seen',
    'fleet.drain': 'Drain',
    'fleet.showThatHub': 'Show that hub',
    'fleet.empty': 'No device matches these filters',
    'fleet.emptyDetail': 'Clear the filters, or widen them. Nothing is wrong with the farm.',

    /* Device drawer */
    'device.identity': 'Identity',
    'device.position': 'Physical position',
    'device.health': 'Health',
    'device.lease': 'Lease',
    'device.actions': 'Operator actions',
    'device.exec': 'Run one ADB command',
    'device.screen': 'Screen',
    'device.raw': 'Raw API row',
    'device.rawSummary': 'every field the API returned',
    'device.loading': 'Loading device…',
    'device.noLease': 'No live lease.',
    'device.noLeaseNote': 'This device is free. Health has nothing to do with that: an offline device can still be held, and a healthy one can be idle.',
    'device.close': 'Close',
    'device.run': 'Run',
    'device.powerCycle': 'Power-cycle slot',
    'device.drainHost': 'Drain host {host}',
    'device.refused': 'refused: {err}',

    /* The live screen */
    'screen.open': 'Open screen',
    'screen.close': 'Close screen',
    'screen.starting': 'Starting the server on the device…',
    'screen.streaming': 'Streaming {w}×{h}. Click and drag on the picture; the buttons below send keys.',
    'screen.ended': 'The stream ended ({why}). This says nothing about the lease: a screen session ends bytes, and the device is exactly as leased as it was before.',
    'screen.closedByOperator': 'closed by the operator',
    'screen.closedDrawer': 'the device drawer was closed',
    'screen.inputFailed': 'Input was not delivered: {err}',
    'screen.remedy': 'Remedy: {remedy}',
    'screen.noWebCodecs': 'This browser has no WebCodecs decoder, so a live screen cannot be shown here. The stream itself is fine: ctl device screen {id} --out screen.h264 records it.',
    'screen.key.back': 'Back',
    'screen.key.home': 'Home',
    'screen.key.recents': 'Recents',
    'screen.key.power': 'Power',

    /* Panel and table states */
    'state.loading': 'Loading…',
    'state.failed': 'Could not be loaded',
    'state.empty': 'Nothing here',
    'state.none': '—',

    /* Errors, by the code the API returns. The server also sends a message; it
     * is shown when a code has no entry here, so a new code degrades to English
     * prose rather than to nothing. */
    'error.unauthenticated': 'This API needs a credential. Set a token.',
    'error.forbidden': 'Your credential does not have the role this route needs.',
    'error.not_found': 'Not found.',
    'error.conflict': 'Refused: this would disturb something that is running.',
    'error.fenced': 'This lease is over. It is terminal — do not retry.',
    'error.transient': 'The database did not answer. The lease is untouched; retry.',
    'error.unavailable': 'This farm is not configured for that.',
    'error.timeout': 'Timed out.',
    'error.adb_error': 'The device did not answer. No lease was affected.',
    'error.bad_request': 'The request was refused as malformed.',
    'error.unreachable': 'The control plane is unreachable from this browser.',
    'error.internal': 'The control plane failed to answer.',
    'error.invalid_json': 'The request body is not valid JSON.',
    'error.check_violation': 'The database refused this: it violates a constraint the schema enforces.',
    'error.no_capacity': 'No device in this pool is free. Nothing failed; there is nothing to give yet.',
    'error.disruption_refused': 'Refused: this would disturb more than the live lease on that device permits. The refusal is recorded rather than quietly downgraded.',
    'error.host_agent': 'The host agent did not answer. No lease was affected.',
    'error.exec_not_admitted': 'This farm enforces the fence at the device, and an operator shell cannot be admitted through it. No safe pattern exists for an arbitrary command line.',
    'error.ui_not_mounted': 'This process serves the API but not the dashboard.',
    'error.client_closed': 'The client hung up before the answer was written.',

    /* Auth */
    'auth.needed': 'This API needs a credential.',
    'auth.setToken': 'Set API token',
    'auth.tokenTitle': 'API token',
    'auth.tokenNote': 'Stored in this tab only, and sent as a header — never in a URL.',
    'auth.save': 'Save',
    'auth.cancel': 'Cancel',

    /* Confirmations */
    'confirm.title': 'Confirm',
    'confirm.subject': 'Subject:',
    'confirm.reason': 'Reason',
    'confirm.reasonNote': 'Recorded in farm.audit_log next to your name. Six weeks from now it is the only record of why.',
    'confirm.yes': 'Confirm',
    'confirm.no': 'Cancel',

    /* ==================================================================
       THE OTHER FIVE VIEWS — Leases, Jobs, Recovery, Bulk and Events.

       The table-first and form-first pages. They are last in this file
       because they were last into it: the redesign that added these keys is
       the one that took the operational tables off a forced 900px minimum,
       gave the Jobs and Bulk forms their field groups, and turned the
       recovery ladder from seven unlabelled grid columns into a list that
       reads as one climb.

       WHAT IS AND IS NOT HERE. Column HEADINGS are here. Column VALUES are
       not: `held`, `suspect`, `power_domain`, `allow_soft_reset`, `refused`,
       `error`, `output`, `detail` are what the schema says and what an
       operator greps a log for, so the page prints them. A human sentence
       goes NEXT TO such a value — see `jobs.policy.*`, which is the enum
       value followed by what it means — and never in place of it.
       ================================================================== */

    /* Column headings. Shared on purpose: "Device" is the same heading on
       Leases, on a recovery attempt and on a bulk run's targets, and a second
       copy of a heading is a second place for the two languages to drift. */
    'col.state': 'State',
    'col.fence': 'Fence',
    'col.device': 'Device',
    'col.job': 'Job',
    'col.holder': 'Holder',
    'col.tenant': 'Tenant',
    'col.pool': 'Pool',
    'col.step': 'Step',
    'col.limits': 'Limits',
    'col.created': 'Created',
    'col.started': 'Started',
    'col.acquired': 'Acquired',
    'col.expires': 'Expires',
    'col.witness': 'Witness',
    'col.attempt': 'Attempt',
    'col.index': '#',
    'col.log': 'Log',
    'col.tier': 'Rung',
    'col.outcome': 'Outcome',
    'col.refusal': 'Refusal or detail',
    'col.scope': 'Scope',
    'col.subject': 'Subject',
    'col.reason': 'Reason',
    'col.opened': 'Opened',
    'col.progress': 'Progress',
    'col.command': 'Command',
    'col.output': 'Output',
    'col.by': 'By',
    'col.when': 'When',
    'col.source': 'Source',
    'col.kind': 'Kind',
    'col.actor': 'Actor',
    'col.detail': 'Detail',
    'col.actions': 'Actions',

    /* The second line of a cell. Every one of these was a column of its own
       until the tables were narrowed; the little label is what stops the value
       under the identifier from being a bare shape the reader has to decode. */
    'sub.queue': 'queue',
    'sub.tenant': 'tenant',
    'sub.by': 'by',
    'sub.heartbeat': 'heartbeat',
    'sub.reclaimable': 'reclaimable',
    'sub.expected': 'expected',
    'sub.started': 'started',
    'sub.finished': 'finished',
    'sub.kind': 'kind',
    'sub.exit': 'exit',
    'sub.took': 'took',
    'sub.rung': 'rung',
    'sub.host': 'host',
    'sub.source': 'source',
    'sub.selector': 'selector',
    'sub.timeout': 'timeout',
    'sub.policy': 'policy',
    'sub.job': 'job',

    /* Row actions. */
    'act.revoke': 'Revoke',
    'act.cancel': 'Cancel',
    'act.steps': 'Steps',
    'act.selected': 'Selected',
    'act.open': 'Open',
    'act.close': 'Close',

    /* Words a table says about itself. The unit letters in a relative time —
       s, m, h, d — are deliberately NOT translated: they are read as symbols
       down a column and a translated one would not line up. The direction is
       a word, and a word gets translated. */
    'word.none': 'none',
    'word.notReported': 'not reported by the API',
    'time.ago': '{v} ago',
    'time.in': 'in {v}',

    'panel.failed': 'Could not load this from the API.',
    'panel.loading': 'Loading from the API…',
    'panel.loadingDetail': 'Nothing is drawn until the server answers.',
    'trunc.title': 'The server capped this response.',
    'trunc.tail': 'Everything counted here counts only the rows that came back, not the farm.',
    'trunc.chip': 'truncated by the server',
    'form.badJSON': 'The {field} field is not valid JSON:',

    /* Leases */
    'leases.filter.live': 'live (held and suspect)',
    'leases.axiom.head': 'A lease ends when the job says so, when a deadline the user wrote down elapses, or when a human takes it back.',
    'leases.axiom.a': 'Nothing on this page ends one on its own. A',
    'leases.axiom.b': 'lease means the control plane has not heard a heartbeat from the holder — it does',
    'leases.axiom.not': 'not',
    'leases.axiom.c': 'mean the device is broken, the device has not been released, and a heartbeat that arrives later heals the lease at the same fence.',
    'leases.empty': 'No leases in this state.',
    'leases.emptyDetail': 'Every live lease appears here with its fence, its holder, its job, and the two server-computed instants that matter: when it becomes suspect, and the earliest the reaper may act.',
    'leases.narrow': 'Pick a single lease state to see the rest.',
    'leases.holderNotVisible': 'holder not visible',
    'leases.suspectAlertOnly': 'suspect is an alerting state only: the device is not released and the job may be running fine',
    'leases.holderTitle': 'audit only; the holder name confers no ownership',
    'leases.reclaimableTitle': 'the earliest instant the reaper may reclaim this lease. Computed by the server, never by this browser.',
    'leases.protectedSuspect': 'protected suspect',
    'leases.protectedSuspectTitle': 'protected and suspect: the reaper will not reclaim these; a human is expected to look',

    /* Jobs */
    'jobs.filter.all': 'all states',
    'jobs.empty': 'No jobs.',
    'jobs.emptyDetail': 'Queued and running jobs appear here as soon as one is submitted. Use the Submit a job form above this table.',
    'jobs.narrow': 'Pick a single job state to see the rest.',
    'jobs.protectedTitle': 'protected: the reaper will never reclaim this job\'s lease; a human is paged instead',
    'jobs.maxRuntimeTitle': 'max_runtime: the only user-supplied clock that may end a lease automatically',
    'jobs.expectedTitle': 'expected_duration: advisory. It ends nothing.',
    'jobs.cannotSubmit': 'Cannot submit:',
    'jobs.submitted': 'Job submitted',
    'jobs.rejected': 'The server rejected this job:',

    'jobs.steps.title': 'Steps of the selected job',
    'jobs.steps.hintA': 'Which step a job is on, and what it printed. Pick a job above; the rows come from',
    'jobs.steps.hintB': ', which is the same view',
    'jobs.steps.hintC': 'prints. A step that failed carries the exit code, the runner\'s message and the log it produced, so the answer to “which step failed and why” needs neither a terminal nor a database session.',

    /* The job form. Five groups, because eleven fields in one undifferentiated
       run is a form you can only fill in if you already knew the answer. */
    'jobs.form.title': 'Submit a job',
    'jobs.form.where': 'Where it runs',
    'jobs.form.pool': 'Pool',
    'jobs.form.poolHelp': 'The set of interchangeable devices the scheduler may pick from.',
    'jobs.form.queue': 'Queue',
    'jobs.form.queueHelp': 'Which line this job waits in. Queues are served independently, so a busy one does not hold up another.',
    'jobs.form.tenant': 'Tenant',
    'jobs.form.tenantHelp': 'Who the work belongs to. It appears on the lease and in every event this job writes.',
    'jobs.form.what': 'What it runs',
    'jobs.form.spec': 'Spec',
    'jobs.form.specHelp': 'JSON. The body the runner executes, passed through untouched. An empty object is a valid job.',
    'jobs.form.selector': 'Selector',
    'jobs.form.selectorHelp': 'JSON. Narrows the pool further — a model, a host, a label. Leave it empty to accept any device in the pool.',
    'jobs.form.clocks': 'How long it may take',
    'jobs.form.expected': 'Expected duration',
    'jobs.form.expectedHelp': 'Seconds. Advisory: it ends nothing.',
    'jobs.form.maxrt': 'Max runtime',
    'jobs.form.maxrtHelp': 'Seconds. A real deadline.',
    'jobs.form.maxrtNote': 'Max runtime is the only user-supplied clock allowed to end a lease automatically. Leave it empty and only the job or a human ends this lease.',
    'jobs.form.ttl': 'Lease TTL',
    'jobs.form.ttlHelp': 'Seconds, 600 or more. How long the lease survives without a heartbeat before it turns suspect.',
    'jobs.form.grace': 'Grace',
    'jobs.form.graceHelp': 'Seconds, 300 or more. How long a suspect lease is left alone before the reaper may reclaim it.',
    'jobs.form.guards': 'What may disturb it',
    'jobs.form.policy': 'Disruption policy',
    'jobs.form.policyHelp': 'The value before the dash is what the API stores and what ctl prints; the rest of the line is here to explain it. A recovery rung whose blast radius exceeds this policy is refused outright, never quietly downgraded.',
    'jobs.form.protected': 'Protected',
    'jobs.form.protectedNote': 'The reaper will never reclaim this lease. A human is paged instead.',
    'jobs.form.who': 'Who is asking',
    'jobs.form.by': 'Created by',
    'jobs.form.byPlaceholder': 'your name',
    'jobs.form.byHelp': 'Recorded on the job and on every event it writes. Six weeks from now it is how somebody finds out who asked.',
    'jobs.form.submit': 'Submit job',

    /* farm.jobs.disruption_policy. The value comes first and is never
       translated — it is what the API accepts and what ctl prints. The
       sentence after the dash is an addition, not a replacement. */
    'jobs.policy.portPowerCycle': 'allow_port_power_cycle — recovery may cut power to this device\'s USB port',
    'jobs.policy.softReset': 'allow_soft_reset — recovery may restart the device\'s adb, and nothing harder',
    'jobs.policy.noDisruption': 'no_disruption — recovery may not touch this device while the lease is live',

    /* The live step cell in the jobs table, fed by the event stream. */
    'step.noFrame': 'no step frame has arrived for this job on the event stream',
    'step.notLive': 'this column is fed by the event stream, which is not connected — the page is polling. Press Steps to read the step log over the API instead.',
    'step.notStarted': 'not started',
    'step.notStartedTitle': 'the control plane reports no step of this attempt has started yet',
    'step.noneRan': 'no steps ran',
    'step.noneRanTitle': 'this job reached a terminal state without a single step row: it never got as far as running one',
    'step.frameTitle': 'step {index} "{id}" ({kind}) is {state} on attempt {attempt}, as of the last event-stream frame',

    /* The step log panel. */
    'steps.none': 'No job selected.',
    'steps.noneDetail': 'Press Steps on a job above to see which step it is on, what it printed, and why it stopped.',
    'steps.failed': 'Could not read that job\'s steps.',
    'steps.loading': 'Loading the step log…',
    'steps.whichAttempt': 'Which attempt of this job to show',
    'steps.newest': 'newest attempt that ran',
    'steps.every': 'every attempt',
    'steps.attemptN': 'attempt {n}',
    'steps.attemptNoSteps': 'attempt {n} (no steps)',
    'steps.attempt': 'attempt',
    'steps.of': 'of',
    'steps.attemptCountTitle': 'how many placements this job has had, and how many it may have',
    'steps.showing': 'Showing',
    'steps.nFailed': '{n} failed',
    'steps.failedTitle': 'steps in failed or aborted',
    'steps.logsOmitted': '{n} logs omitted',
    'steps.logsOmittedTitle': 'the server dropped {n} log field(s) for its response size budget. Every step is here; some of their text is not. Pin a single attempt above to get it back.',
    'steps.narrow': 'Pin a single attempt with the selector above.',
    'steps.empty': 'No steps for this attempt.',
    'steps.emptyOther': 'This job has step rows for attempt {list}, and none for the one selected.',
    'steps.emptyNone': 'The runner writes a row per step as it executes one. A job that has not been placed on a device yet has none.',
    'steps.attemptColTitle': 'the placement this step belongs to; a retry writes a fresh set of rows',
    'steps.noExit': 'none',
    'steps.noExitTitle': 'the runner recorded no exit code for this step. That is not a zero: it is a step that never got one.',

    /* A step’s stored output, error and detail. The FIELD NAMES — error,
       output, detail — are printed as the API spells them. */
    'log.chars': '{n} chars',
    'log.charsCut': '{shown} of {n} chars — cut by the server',
    'log.whitespaceOnly': '(whitespace only, {n} characters stored)',
    'log.omitted': '{field} omitted',
    'log.storedChars': '({n} chars stored)',
    'log.omittedTitle': 'the response had already spent its size budget, so this {field} was dropped whole rather than cut to a fragment that would look complete. Show attempt {attempt} on its own to get it back.',
    'log.nothingStored': 'nothing stored',
    'log.nothingStoredTitle': 'the runner stored no output, no error and no detail for this step',
    'log.notRunTitle': 'this step has not run',

    /* Recovery */
    'recovery.ladder': 'The ladder',
    'recovery.hintA': 'Recovery climbs from the cheapest rung upward and acts',
    'recovery.hintEm': 'on behalf of',
    'recovery.hintB': 'the holder: the lease keeps its device, the lease clock keeps ticking and the fence never moves. A rung whose blast radius exceeds the live lease\'s disruption policy is refused, not quietly downgraded.',
    'recovery.attempts': 'Recent attempts',
    'recovery.quarantines': 'Open quarantines',
    'recovery.ladderEmpty': 'The ladder is empty.',
    'recovery.ladderEmptyDetail': 'Every rung the watchdog is allowed to climb appears here — its name, how far it reaches, the lease disruption policy it requires, its cooldown and its hourly budget.',
    'recovery.blastTitle': 'blast radius: how far past this one phone the rung reaches',
    'recovery.requires': 'Requires policy',
    'recovery.requiresTitle': 'a live lease must carry at least this disruption policy or the rung is refused',
    'recovery.cooldown': 'Cooldown',
    'recovery.cooldownTitle': 'how long this rung waits before it may be tried on the same subject again',
    'recovery.budget': 'Budget',
    'recovery.budgetTitle': 'the most attempts this rung may make in an hour, across the farm',
    'recovery.perHour': '{n} per hour',
    'recovery.disabled': 'disabled',
    'recovery.disabledTitle': 'this rung is switched off: the watchdog skips it and climbs past',
    'recovery.attemptsEmpty': 'No recovery attempts recorded.',
    'recovery.attemptsEmptyDetail': 'Each rung the watchdog climbs is logged here with its outcome, and a refused rung is shown with the reason it was refused.',
    'recovery.inFlight': 'in flight',
    'recovery.quarantinesEmpty': 'No open quarantines.',
    'recovery.quarantinesEmptyDetail': 'A quarantine appears here when the ladder stops scheduling to a device, a slot, a hub or a host and asks for a human. Closing one is an audited action.',
    'quarantine.unnamed': '{what}, id not reported',
    'quarantine.unnamedTitle': 'the API reported no identifier for this {what}',
    'quarantine.domainTitle': 'a whole power domain, located by {by}',
    'quarantine.automatic': 'automatic',
    'quarantine.operator': 'operator',

    /* Bulk */
    'bulk.runs': 'Runs',
    'bulk.selected': 'Selected run',
    'bulk.form.title': 'Run a command',
    'bulk.form.what': 'What to run, and where',
    'bulk.form.command': 'Command',
    'bulk.form.commandHelp': 'Sent to every matched device exactly as typed. The handset\'s own shell does the word-splitting.',
    'bulk.form.selector': 'Selector',
    'bulk.form.selectorHelp': 'JSON. Which devices this reaches. An empty pool matches every pool, so read this twice before starting a run.',
    'bulk.form.limits': 'Limits',
    'bulk.form.max': 'Max per hub',
    'bulk.form.maxHelp': 'Devices at once, per hub.',
    'bulk.form.timeout': 'Timeout',
    'bulk.form.timeoutHelp': 'Milliseconds, per device.',
    'bulk.form.limitsNote': 'Max per hub keeps a fleet-wide command from browning out one power domain.',
    'bulk.form.submit': 'Start run',
    'bulk.empty': 'No bulk runs yet.',
    'bulk.emptyDetail': 'A run started from the form above appears here, and its per-device results stream into the panel below as each target finishes.',
    'bulk.progressTitle': 'ok, error and skipped, out of the targets the selector matched',
    'bulk.perHub': '{n} per hub',
    'bulk.targets': 'targets',
    'bulk.runFailed': 'Could not load that run.',
    'bulk.noRun': 'No run selected.',
    'bulk.noRunDetail': 'Pick a run above to watch its per-device results.',
    'bulk.loadingRun': 'Loading that run…',
    'bulk.loadingRunDetail': 'Per-device results appear as the API reports them.',
    'bulk.noOutput': 'no output',
    'bulk.noTargets': 'This run has no targets yet.',
    'bulk.noTargetsDetail': 'The selector matched nothing, or the run has not expanded its target set.',
    'bulk.badSelector': 'Selector is not valid JSON:',
    'bulk.started': 'Bulk run started',
    'bulk.startedDetail': 'Results appear below as each device answers.',
    'bulk.rejected': 'The server rejected this run:',

    /* Events */
    'events.show': 'Show',
    'events.kind': 'Kind contains',
    'events.empty': 'No events.',
    'events.emptyDetail': 'The event log and the audit log are merged here newest first: every lease transition, every recovery attempt and every operator action with the human who typed the reason.',
    'events.narrow': 'This is the newest page only; raise Show to reach further back.',
  },

  pt: {
    /* O construtor de comandos. Só rótulos e explicações — um comando nunca é
     * traduzido, pelo mesmo motivo que examples[].code não é: um comando
     * traduzido não roda. Os templates ficam em exec.js. */
    'exec.title': 'Rodar um comando ADB',
    'exec.catalogue': 'Comandos',
    'exec.run': 'Rodar',
    'exec.running': 'rodando…',
    'exec.refused': 'Recusado:',
    'exec.tier.verified': 'Rodados por esta farm',
    'exec.tier.verifiedNote': 'Cada um destes é enviado a aparelhos reais por este projeto, e nomeia o arquivo que o envia.',
    'exec.tier.unverified': 'Android comum, não rodados aqui',
    'exec.tier.unverifiedNote': 'Comandos muito usados que ninguém neste projeto executou. Numa farm simulada a maioria responde com uma linha em branco e exit 0.',
    'exec.unverified': 'não rodado por esta farm',
    'exec.unverifiedWhy': 'Nenhum código deste repositório envia este comando. Ele está aqui por ser amplamente usado, não porque alguém aqui o viu funcionar.',
    'exec.verifiedWhy': 'O arquivo que envia exatamente este comando a um aparelho. Um teste lê esse arquivo e quebra o build se o comando não estiver nele.',
    'exec.group.identity': 'Identidade',
    'exec.group.health': 'Saúde',
    'exec.group.packages': 'Pacotes',
    'exec.group.logs': 'Logs',
    'exec.group.storage': 'Armazenamento',
    'exec.effect.read': 'lê',
    'exec.effect.readWhy': 'Este comando relata e não muda nada no aparelho.',
    'exec.effect.writes': 'muda o aparelho',
    'exec.effect.writesWhy': 'Este comando deixa o aparelho diferente de como o encontrou.',
    'exec.freeform': 'Digitado por você. Não é classificado como leitura nem como escrita, e isso é deliberado: nenhum padrão sobre uma linha de comando de shell distingue as duas de forma confiável — é o mesmo argumento que o fence proxy faz quando se recusa a admitir uma.',
    'exec.wire': 'O que vai pelo fio',
    'exec.wireNote': 'Enviado exatamente assim. Nada cita, escapa ou confere — o próprio /system/bin/sh do aparelho faz a separação de palavras, o ; o | e o $( ).',
    'exec.preflight': 'O que pode recusar isto, na ordem em que o servidor pergunta',
    'exec.pf.command': 'um comando',
    'exec.pf.commandOk': 'pronto',
    'exec.pf.commandEmpty': 'você ainda não digitou',
    'exec.pf.fence': 'a fence',
    'exec.pf.fenceOn': 'esta farm impõe a fence no aparelho — todo comando é recusado, em todo dispositivo',
    'exec.pf.fenceOff': 'não imposta no aparelho nesta farm',
    'exec.pf.fenceUnknown': 'não dá para saber daqui agora',
    'exec.pf.slot': 'uma posição física',
    'exec.pf.slotNone': 'este dispositivo não está em nenhum slot, então não tem endereço',
    'exec.pf.endpoint': 'o servidor ADB do host',
    'exec.pf.endpointNone': 'nenhum endpoint registrado para este host',
    'exec.pf.lease': 'uma lease viva',
    'exec.pf.leaseHeld': 'com {holder}, job {job} — precisa de force e um motivo',
    'exec.pf.leaseFree': 'livre, na última leitura da frota',
    'exec.pf.answer': 'o aparelho responder',
    'exec.pf.answerUnknown': 'não dá para saber antes de perguntar',
    'exec.pf.targets': 'quais dispositivos',
    'exec.pf.targetsBulk': 'decidido pelo seletor; a recusa por fence abaixo não cobre o modo em massa, que devolve um 502 por alvo',
    'exec.timeout': 'Desistir depois de (ms, no máximo {max}s)',
    'exec.reasonPlaceholder': 'por que você está rodando isto num dispositivo que alguém está usando',
    'exec.force.head': 'Este dispositivo tem uma lease viva',
    'exec.force.reason': 'Motivo',
    'exec.force.audit': 'O motivo e o comando são escritos em farm.audit_log ao lado do seu nome. Daqui a seis semanas são o único registro do porquê.',
    'exec.force.holder': 'O holder {holder} está usando este dispositivo agora.',
    'exec.force.job': 'O job {job} está rodando nele.',
    'exec.force.tenant': 'Pertence ao tenant {tenant}.',
    'exec.force.protected': 'A lease é protegida: não é retomada automaticamente, o que é sinal de que alguém quis assim.',
    'exec.force.noSignal': 'Rodar um comando mesmo assim pode corromper a execução daquele job, e o holder não recebe sinal nenhum de que isso aconteceu. A lease em si não termina de nenhum dos dois jeitos.',
    'exec.result.neverExited': 'O status de saída nunca chegou. Se o comando terminou no aparelho é DESCONHECIDO — pode ainda estar rodando. Nenhuma lease foi afetada.',
    'exec.result.exited0': 'Saiu com 0.',
    'exec.result.exitedN': 'Saiu com {code} no aparelho. Nenhuma lease foi afetada.',
    'exec.result.silent': 'Não imprimiu nada. Isso é um desfecho real — e é também o que uma farm simulada responde para um comando que o fake dela não roteiriza.',
    'exec.result.truncated': 'A saída bateu no teto do servidor e o resto foi descartado. O que está abaixo não é tudo.',
    'exec.result.took': 'Levou {ms} ms.',
    'exec.result.stderr': '--- stderr ---',
    'exec.result.nothing': '(sem saída)',
    'exec.landedAfterClose': 'O comando que você rodou em {where} terminou depois que você fechou a gaveta. Ele não foi cancelado — veja a aba Eventos para o que ele fez.',
    'exec.cat.getprop.label': 'Uma propriedade do aparelho',
    'exec.cat.getprop.why': 'Pergunta ao aparelho uma das onze propriedades que o internal/enroll lê a cada avistamento.',
    'exec.cat.brandRead.label': 'Ler a marca da farm',
    'exec.cat.brandRead.why': 'O farm_uid que este projeto escreve no aparelho para reconhecê-lo depois que um serial muda.',
    'exec.cat.battery.label': 'Bateria, completo',
    'exec.cat.battery.why': 'O comando exato que o internal/watchdog manda para cada dispositivo ligado, uma vez por minuto.',
    'exec.cat.batteryLevel.label': 'Só o nível da bateria',
    'exec.cat.batteryLevel.why': 'A única linha, para quando o dump completo é mais do que você quer ler.',
    'exec.cat.idle.label': 'Estado de idle profundo',
    'exec.cat.idle.why': 'Se o aparelho caiu em doze profundo, o que muda o que um job pode esperar dele.',
    'exec.cat.packages.label': 'Pacotes de terceiros instalados',
    'exec.cat.packages.why': 'O que foi instalado por cima da imagem de sistema.',
    'exec.cat.logcat.label': 'Últimas N linhas de log',
    'exec.cat.logcat.why': 'Um dump limitado do logcat. Limitado de propósito: o servidor tem um teto de saída e descarta o resto.',
    'exec.cat.bootCompleted.label': 'Terminou de dar boot',
    'exec.cat.bootCompleted.why': 'A propriedade que a escada de reset espera depois de um reboot.',
    'exec.cat.df.label': 'Espaço livre',
    'exec.cat.df.why': 'Uso dos sistemas de arquivos. Um /data cheio é causa comum de instalações que falham sem motivo aparente.',
    'exec.cat.ps.label': 'Processos rodando',
    'exec.cat.ps.why': 'Todos os processos do aparelho.',
    'exec.cat.uptime.label': 'Tempo ligado',
    'exec.cat.uptime.why': 'Há quanto tempo o aparelho deu boot pela última vez.',
    'exec.cat.wmSize.label': 'Tamanho da tela',
    'exec.cat.wmSize.why': 'A resolução do display, em pixels do aparelho — não o tamanho em que uma tela ao vivo transmite.',
    'exec.cat.wmDensity.label': 'Densidade da tela',
    'exec.cat.wmDensity.why': 'A densidade de pixels, que decide o tamanho aparente de tudo na tela.',
    'exec.cat.dumpsysWindow.label': 'Displays',
    'exec.cat.dumpsysWindow.why': 'A visão do window manager sobre cada display ligado.',
    'exec.cat.meminfo.label': 'Memória',
    'exec.cat.meminfo.why': 'Uso de memória no sistema e em cada processo.',
    'exec.cat.thermal.label': 'Estado térmico',
    'exec.cat.thermal.why': 'O que o aparelho acha da própria temperatura — que não é a temperatura de bateria que o watchdog lê.',
    'exec.cat.wifi.label': 'Estado do Wi-Fi',
    'exec.cat.wifi.why': 'As primeiras quarenta linhas do dump de Wi-Fi, onde está o estado da conexão.',
    'exec.cat.packagesAll.label': 'Todos os pacotes instalados',
    'exec.cat.packagesAll.why': 'Incluindo os pacotes de sistema. Longo.',
    'exec.cat.packagePath.label': 'Onde um pacote mora',
    'exec.cat.packagePath.why': 'O caminho do APK de um pacote.',
    'exec.cat.dumpsysPackage.label': 'Um pacote, em detalhe',
    'exec.cat.dumpsysPackage.why': 'Versão, permissões e estado de instalação de um pacote.',
    'exec.cat.logcatCrash.label': 'O buffer de crashes',
    'exec.cat.logcatCrash.why': 'Só o log de crashes, que costuma ser o que você queria quando um app sumiu.',
    'exec.cat.settingsGet.label': 'Ler uma configuração',
    'exec.cat.settingsGet.why': 'Um valor do provedor de configurações.',
    'exec.cat.lsTmp.label': 'O que há em /data/local/tmp',
    'exec.cat.lsTmp.why': 'O diretório onde este projeto empurra artefatos e marcadores.',
    'exec.cat.topOnce.label': 'Processos mais pesados, uma vez',
    'exec.cat.topOnce.why': 'Um top único e não interativo, para que ele termine em vez de transmitir para sempre.',
    /* Cabeçalho e navegação */
    'app.name': 'device-farmer',
    'app.skip': 'Ir para o conteúdo',
    'nav.views': 'Visões',
    'nav.fleet': 'Frota',
    'nav.leases': 'Leases',
    'nav.jobs': 'Jobs',
    'nav.recovery': 'Recuperação',
    'nav.bulk': 'Em massa',
    'nav.events': 'Eventos',
    'nav.docs': 'Docs',
    'header.searchLabel': 'Buscar dispositivos, hosts, jobs e holders',
    'header.searchPlaceholder': 'Buscar  ( / )',
    'header.refresh': 'Atualizar',
    'header.refreshTitle': 'Rebuscar todas as visões agora',
    'header.token': 'Token',
    'header.tokenTitle': 'Definir o bearer token que este navegador envia à API',
    'header.language': 'Idioma',
    'header.languageTitle': 'Ler este painel em inglês ou português',

    /* Estado da conexão. É sobre a conexão DESTA página com a API e não diz
     * nada sobre nenhuma lease — veja conn.downNote. */
    'conn.live': 'ao vivo',
    'conn.polling': 'consultando',
    'conn.connecting': 'conectando',
    'conn.down': 'sem conexão',
    'conn.downNote': 'Esta página não alcança a API. Isso não diz nada sobre a fazenda: as leases vivem no PostgreSQL e não são afetadas por um navegador que perdeu a conexão.',

    /* Filtros, comuns a várias visões */
    'filter.host': 'Host',
    'filter.hub': 'Hub',
    'filter.health': 'Saúde',
    'filter.pool': 'Pool',
    'filter.lease': 'Lease',
    'filter.state': 'Estado',
    'filter.allHosts': 'todos os hosts',
    'filter.allHubs': 'todos os hubs',
    'filter.allPools': 'todos os pools',
    'filter.anyHealth': 'qualquer saúde',
    'filter.anyLeaseState': 'qualquer estado de lease',
    'filter.anyState': 'qualquer estado',
    'filter.clear': 'Limpar',
    'filter.anyFault': 'com qualquer falha',

    /* Frota */
    'count.total': 'total',
    'count.leased': 'com lease',
    'count.free': 'livres',
    'count.unhealthy': 'com problema',
    'count.quarantined': 'em quarentena',
    'count.protected': 'protegidos',
    'count.devices': 'dispositivos',
    'count.live': 'vivas',
    'count.expired': 'expiradas',
    'count.released': 'liberadas',
    'count.running': 'rodando',
    'count.queued': 'na fila',
    'count.failed': 'falharam',
    'count.succeeded': 'sucesso',
    'fleet.total': 'total',
    'fleet.leased': 'com lease',
    'fleet.free': 'livres',
    'fleet.unhealthy': 'com problema',
    'fleet.quarantined': 'em quarentena',
    'fleet.protected': 'protegidos',
    'fleet.showing': 'mostrando',
    'fleet.devices': 'dispositivos',
    'fleet.liveLeases': 'leases vivas',
    'fleet.seen': 'visto',
    'fleet.drain': 'Drenar',
    'fleet.showThatHub': 'Mostrar esse hub',
    'fleet.empty': 'Nenhum dispositivo corresponde a estes filtros',
    'fleet.emptyDetail': 'Limpe os filtros, ou alargue-os. Não há nada errado com a fazenda.',

    /* Gaveta do dispositivo */
    'device.identity': 'Identidade',
    'device.position': 'Posição física',
    'device.health': 'Saúde',
    'device.lease': 'Lease',
    'device.actions': 'Ações de operador',
    'device.exec': 'Rodar um comando ADB',
    'device.screen': 'Tela',
    'device.raw': 'Linha crua da API',
    'device.rawSummary': 'todos os campos que a API devolveu',
    'device.loading': 'Carregando dispositivo…',
    'device.noLease': 'Nenhuma lease viva.',
    'device.noLeaseNote': 'Este dispositivo está livre. Saúde não tem relação com isso: um dispositivo offline pode estar com lease, e um saudável pode estar ocioso.',
    'device.close': 'Fechar',
    'device.run': 'Rodar',
    'device.powerCycle': 'Ciclar energia do slot',
    'device.drainHost': 'Drenar host {host}',
    'device.refused': 'recusado: {err}',

    /* A tela ao vivo */
    'screen.open': 'Abrir tela',
    'screen.close': 'Fechar tela',
    'screen.starting': 'Iniciando o servidor no dispositivo…',
    'screen.streaming': 'Transmitindo {w}×{h}. Clique e arraste na imagem; os botões abaixo enviam teclas.',
    'screen.ended': 'A transmissão terminou ({why}). Isso não diz nada sobre a lease: uma sessão de tela termina bytes, e o dispositivo continua exatamente tão alugado quanto estava.',
    'screen.closedByOperator': 'fechada pelo operador',
    'screen.closedDrawer': 'a gaveta do dispositivo foi fechada',
    'screen.inputFailed': 'O input não foi entregue: {err}',
    'screen.remedy': 'Como resolver: {remedy}',
    'screen.noWebCodecs': 'Este navegador não tem o decodificador WebCodecs, então a tela ao vivo não pode ser mostrada aqui. A transmissão em si está fina: ctl device screen {id} --out screen.h264 grava ela.',
    'screen.key.back': 'Voltar',
    'screen.key.home': 'Início',
    'screen.key.recents': 'Recentes',
    'screen.key.power': 'Ligar',

    /* Estados de painel e tabela */
    'state.loading': 'Carregando…',
    'state.failed': 'Não foi possível carregar',
    'state.empty': 'Nada aqui',
    'state.none': '—',

    /* Erros, pelo código que a API devolve. O servidor também manda uma
     * mensagem; ela aparece quando um código não tem entrada aqui, então um
     * código novo degrada para prosa em inglês em vez de para nada. */
    'error.unauthenticated': 'Esta API exige uma credencial. Defina um token.',
    'error.forbidden': 'Sua credencial não tem o papel que esta rota exige.',
    'error.not_found': 'Não encontrado.',
    'error.conflict': 'Recusado: isso perturbaria algo que está rodando.',
    'error.fenced': 'Esta lease acabou. É terminal — não tente de novo.',
    'error.transient': 'O banco não respondeu. A lease está intacta; tente de novo.',
    'error.unavailable': 'Esta fazenda não está configurada para isso.',
    'error.timeout': 'Tempo esgotado.',
    'error.adb_error': 'O dispositivo não respondeu. Nenhuma lease foi afetada.',
    'error.bad_request': 'A requisição foi recusada como malformada.',
    'error.unreachable': 'O control plane está inalcançável deste navegador.',
    'error.internal': 'O control plane falhou ao responder.',
    'error.invalid_json': 'O corpo da requisição não é um JSON válido.',
    'error.check_violation': 'O banco recusou isto: viola uma constraint que o schema impõe.',
    'error.no_capacity': 'Nenhum dispositivo deste pool está livre. Nada falhou; ainda não há o que entregar.',
    'error.disruption_refused': 'Recusado: isto perturbaria mais do que a lease viva nesse dispositivo permite. A recusa é registrada em vez de ser silenciosamente rebaixada.',
    'error.host_agent': 'O agente do host não respondeu. Nenhuma lease foi afetada.',
    'error.exec_not_admitted': 'Esta fazenda impõe a fence no dispositivo, e um shell de operador não pode ser admitido através dela. Não existe padrão seguro para uma linha de comando arbitrária.',
    'error.ui_not_mounted': 'Este processo serve a API, mas não o painel.',
    'error.client_closed': 'O cliente desligou antes de a resposta ser escrita.',

    /* Autenticação */
    'auth.needed': 'Esta API exige uma credencial.',
    'auth.setToken': 'Definir token da API',
    'auth.tokenTitle': 'Token da API',
    'auth.tokenNote': 'Guardado só nesta aba, e enviado como header — nunca numa URL.',
    'auth.save': 'Salvar',
    'auth.cancel': 'Cancelar',

    /* Confirmações */
    'confirm.title': 'Confirmar',
    'confirm.subject': 'Alvo:',
    'confirm.reason': 'Motivo',
    'confirm.reasonNote': 'Registrado em farm.audit_log ao lado do seu nome. Daqui a seis semanas é o único registro do porquê.',
    'confirm.yes': 'Confirmar',
    'confirm.no': 'Cancelar',

    /* ==================================================================
       AS OUTRAS CINCO TELAS — Leases, Jobs, Recovery, Bulk e Events.

       O vocabulário de domínio fica em inglês, como no resto deste arquivo:
       lease, fence, job, pool, hub, host, slot, reaper, watchdog. É o que a
       coluna guarda e o que o operador procura num log; traduzir isso daria
       ao leitor de português uma palavra que ele não acha em lugar nenhum.
       ================================================================== */

    'col.state': 'Estado',
    'col.fence': 'Fence',
    'col.device': 'Dispositivo',
    'col.job': 'Job',
    'col.holder': 'Detentor',
    'col.tenant': 'Tenant',
    'col.pool': 'Pool',
    'col.step': 'Passo',
    'col.limits': 'Limites',
    'col.created': 'Criado',
    'col.started': 'Início',
    'col.acquired': 'Adquirida',
    'col.expires': 'Expira',
    'col.witness': 'Testemunha',
    'col.attempt': 'Tentativa',
    'col.index': 'Nº',
    'col.log': 'Log',
    'col.tier': 'Degrau',
    'col.outcome': 'Desfecho',
    'col.refusal': 'Recusa ou detalhe',
    'col.scope': 'Escopo',
    'col.subject': 'Alvo',
    'col.reason': 'Motivo',
    'col.opened': 'Aberta',
    'col.progress': 'Progresso',
    'col.command': 'Comando',
    'col.output': 'Saída',
    'col.by': 'Por',
    'col.when': 'Quando',
    'col.source': 'Origem',
    'col.kind': 'Tipo',
    'col.actor': 'Autor',
    'col.detail': 'Detalhe',
    'col.actions': 'Ações',

    'sub.queue': 'fila',
    'sub.tenant': 'tenant',
    'sub.by': 'por',
    'sub.heartbeat': 'heartbeat',
    'sub.reclaimable': 'recuperável',
    'sub.expected': 'previsto',
    'sub.started': 'início',
    'sub.finished': 'fim',
    'sub.kind': 'tipo',
    'sub.exit': 'saída',
    'sub.took': 'levou',
    'sub.rung': 'degrau',
    'sub.host': 'host',
    'sub.source': 'origem',
    'sub.selector': 'seletor',
    'sub.timeout': 'timeout',
    'sub.policy': 'política',
    'sub.job': 'job',

    'act.revoke': 'Revogar',
    'act.cancel': 'Cancelar',
    'act.steps': 'Passos',
    'act.selected': 'Selecionado',
    'act.open': 'Abrir',
    'act.close': 'Fechar',

    'word.none': 'nenhuma',
    'word.notReported': 'não informado pela API',
    'time.ago': 'há {v}',
    'time.in': 'em {v}',

    'panel.failed': 'Não deu para carregar isto da API.',
    'panel.loading': 'Carregando da API…',
    'panel.loadingDetail': 'Nada é desenhado até o servidor responder.',
    'trunc.title': 'O servidor limitou esta resposta.',
    'trunc.tail': 'Tudo que é contado aqui conta só as linhas que voltaram, não a fazenda inteira.',
    'trunc.chip': 'truncado pelo servidor',
    'form.badJSON': 'O campo {field} não é JSON válido:',

    /* Leases */
    'leases.filter.live': 'vivas (held e suspect)',
    'leases.axiom.head': 'Uma lease termina quando o job diz que terminou, quando um prazo que o usuário escreveu se esgota, ou quando um humano a retoma.',
    'leases.axiom.a': 'Nada nesta página termina uma lease sozinho. Uma lease',
    'leases.axiom.b': 'significa que o plano de controle não ouviu heartbeat do detentor — isso',
    'leases.axiom.not': 'não',
    'leases.axiom.c': 'significa que o dispositivo quebrou, o dispositivo não foi liberado, e um heartbeat que chegue depois cura a lease na mesma fence.',
    'leases.empty': 'Nenhuma lease neste estado.',
    'leases.emptyDetail': 'Toda lease viva aparece aqui com sua fence, seu detentor, seu job e os dois instantes calculados pelo servidor que importam: quando ela vira suspect, e o mais cedo que o reaper pode agir.',
    'leases.narrow': 'Escolha um único estado de lease para ver o resto.',
    'leases.holderNotVisible': 'detentor não visível',
    'leases.suspectAlertOnly': 'suspect é só um estado de alerta: o dispositivo não foi liberado e o job pode estar rodando bem',
    'leases.holderTitle': 'só para auditoria; o nome do detentor não confere posse',
    'leases.reclaimableTitle': 'o instante mais cedo em que o reaper pode retomar esta lease. Calculado pelo servidor, nunca por este navegador.',
    'leases.protectedSuspect': 'protegidas e suspect',
    'leases.protectedSuspectTitle': 'protegidas e suspect: o reaper não vai retomá-las; espera-se que um humano olhe',

    /* Jobs */
    'jobs.filter.all': 'todos os estados',
    'jobs.empty': 'Nenhum job.',
    'jobs.emptyDetail': 'Jobs na fila e em execução aparecem aqui assim que um é enviado. Use o formulário Enviar um job acima desta tabela.',
    'jobs.narrow': 'Escolha um único estado de job para ver o resto.',
    'jobs.protectedTitle': 'protegido: o reaper nunca vai retomar a lease deste job; um humano é chamado no lugar',
    'jobs.maxRuntimeTitle': 'max_runtime: o único relógio dado pelo usuário que pode encerrar uma lease automaticamente',
    'jobs.expectedTitle': 'expected_duration: apenas indicativo. Não encerra nada.',
    'jobs.cannotSubmit': 'Não dá para enviar:',
    'jobs.submitted': 'Job enviado',
    'jobs.rejected': 'O servidor recusou este job:',

    'jobs.steps.title': 'Passos do job selecionado',
    'jobs.steps.hintA': 'Em que passo um job está, e o que ele imprimiu. Escolha um job acima; as linhas vêm de',
    'jobs.steps.hintB': ', que é a mesma visão que',
    'jobs.steps.hintC': 'imprime. Um passo que falhou traz o código de saída, a mensagem do runner e o log que ele produziu, então a resposta para “qual passo falhou e por quê” não exige nem terminal nem sessão de banco.',

    'jobs.form.title': 'Enviar um job',
    'jobs.form.where': 'Onde ele roda',
    'jobs.form.pool': 'Pool',
    'jobs.form.poolHelp': 'O conjunto de dispositivos intercambiáveis de onde o escalonador pode escolher.',
    'jobs.form.queue': 'Fila',
    'jobs.form.queueHelp': 'Em que fila este job espera. As filas são servidas de forma independente, então uma cheia não segura a outra.',
    'jobs.form.tenant': 'Tenant',
    'jobs.form.tenantHelp': 'De quem é o trabalho. Aparece na lease e em todo evento que este job escrever.',
    'jobs.form.what': 'O que ele roda',
    'jobs.form.spec': 'Spec',
    'jobs.form.specHelp': 'JSON. O corpo que o runner executa, repassado sem alteração. Um objeto vazio é um job válido.',
    'jobs.form.selector': 'Seletor',
    'jobs.form.selectorHelp': 'JSON. Estreita o pool ainda mais — um modelo, um host, um rótulo. Deixe vazio para aceitar qualquer dispositivo do pool.',
    'jobs.form.clocks': 'Quanto tempo ele pode levar',
    'jobs.form.expected': 'Duração prevista',
    'jobs.form.expectedHelp': 'Segundos. Indicativo: não encerra nada.',
    'jobs.form.maxrt': 'Tempo máximo',
    'jobs.form.maxrtHelp': 'Segundos. Um prazo de verdade.',
    'jobs.form.maxrtNote': 'O tempo máximo é o único relógio dado pelo usuário que pode encerrar uma lease automaticamente. Deixe vazio e só o job ou um humano encerra esta lease.',
    'jobs.form.ttl': 'TTL da lease',
    'jobs.form.ttlHelp': 'Segundos, 600 ou mais. Quanto tempo a lease sobrevive sem heartbeat antes de virar suspect.',
    'jobs.form.grace': 'Carência',
    'jobs.form.graceHelp': 'Segundos, 300 ou mais. Quanto tempo uma lease suspect fica em paz antes que o reaper possa retomá-la.',
    'jobs.form.guards': 'O que pode perturbá-lo',
    'jobs.form.policy': 'Política de perturbação',
    'jobs.form.policyHelp': 'O valor antes do travessão é o que a API guarda e o que o ctl imprime; o resto da linha está aqui para explicá-lo. Um degrau de recuperação cujo alcance excede esta política é recusado, nunca rebaixado em silêncio.',
    'jobs.form.protected': 'Protegido',
    'jobs.form.protectedNote': 'O reaper nunca vai retomar esta lease. Um humano é chamado no lugar.',
    'jobs.form.who': 'Quem está pedindo',
    'jobs.form.by': 'Criado por',
    'jobs.form.byPlaceholder': 'seu nome',
    'jobs.form.byHelp': 'Registrado no job e em todo evento que ele escrever. Daqui a seis semanas é assim que alguém descobre quem pediu.',
    'jobs.form.submit': 'Enviar job',

    'jobs.policy.portPowerCycle': 'allow_port_power_cycle — a recuperação pode cortar a energia da porta USB deste dispositivo',
    'jobs.policy.softReset': 'allow_soft_reset — a recuperação pode reiniciar o adb do dispositivo, e nada mais forte',
    'jobs.policy.noDisruption': 'no_disruption — a recuperação não pode tocar neste dispositivo enquanto a lease estiver viva',

    'step.noFrame': 'nenhum quadro de passo chegou para este job no fluxo de eventos',
    'step.notLive': 'esta coluna é alimentada pelo fluxo de eventos, que não está conectado — a página está fazendo polling. Aperte Passos para ler o log de passos pela API.',
    'step.notStarted': 'não começou',
    'step.notStartedTitle': 'o plano de controle informa que nenhum passo desta tentativa começou ainda',
    'step.noneRan': 'nenhum passo rodou',
    'step.noneRanTitle': 'este job chegou a um estado terminal sem uma única linha de passo: nunca chegou a rodar um',
    'step.frameTitle': 'o passo {index} "{id}" ({kind}) está {state} na tentativa {attempt}, segundo o último quadro do fluxo de eventos',

    'steps.none': 'Nenhum job selecionado.',
    'steps.noneDetail': 'Aperte Passos em um job acima para ver em que passo ele está, o que imprimiu e por que parou.',
    'steps.failed': 'Não deu para ler os passos desse job.',
    'steps.loading': 'Carregando o log de passos…',
    'steps.whichAttempt': 'Qual tentativa deste job mostrar',
    'steps.newest': 'tentativa mais recente que rodou',
    'steps.every': 'todas as tentativas',
    'steps.attemptN': 'tentativa {n}',
    'steps.attemptNoSteps': 'tentativa {n} (sem passos)',
    'steps.attempt': 'tentativa',
    'steps.of': 'de',
    'steps.attemptCountTitle': 'quantas colocações este job já teve, e quantas ele pode ter',
    'steps.showing': 'Mostrando',
    'steps.nFailed': '{n} falharam',
    'steps.failedTitle': 'passos em failed ou aborted',
    'steps.logsOmitted': '{n} logs omitidos',
    'steps.logsOmittedTitle': 'o servidor descartou {n} campo(s) de log por causa do orçamento de tamanho da resposta. Todos os passos estão aqui; parte do texto deles não. Fixe uma única tentativa acima para recuperá-lo.',
    'steps.narrow': 'Fixe uma única tentativa no seletor acima.',
    'steps.empty': 'Nenhum passo nesta tentativa.',
    'steps.emptyOther': 'Este job tem linhas de passo para a tentativa {list}, e nenhuma para a que está selecionada.',
    'steps.emptyNone': 'O runner escreve uma linha por passo conforme executa um. Um job que ainda não foi colocado em um dispositivo não tem nenhuma.',
    'steps.attemptColTitle': 'a colocação a que este passo pertence; uma nova tentativa escreve um conjunto novo de linhas',
    'steps.noExit': 'nenhum',
    'steps.noExitTitle': 'o runner não registrou código de saída para este passo. Isso não é um zero: é um passo que nunca recebeu um.',

    'log.chars': '{n} caracteres',
    'log.charsCut': '{shown} de {n} caracteres — cortado pelo servidor',
    'log.whitespaceOnly': '(só espaços em branco, {n} caracteres guardados)',
    'log.omitted': '{field} omitido',
    'log.storedChars': '({n} caracteres guardados)',
    'log.omittedTitle': 'a resposta já tinha gasto seu orçamento de tamanho, então este {field} foi descartado inteiro em vez de cortado em um fragmento que pareceria completo. Mostre a tentativa {attempt} sozinha para recuperá-lo.',
    'log.nothingStored': 'nada guardado',
    'log.nothingStoredTitle': 'o runner não guardou saída, erro nem detalhe para este passo',
    'log.notRunTitle': 'este passo não rodou',

    /* Recovery */
    'recovery.ladder': 'A escada',
    'recovery.hintA': 'A recuperação sobe do degrau mais barato para cima e age',
    'recovery.hintEm': 'em nome de',
    'recovery.hintB': 'quem detém a lease: a lease mantém seu dispositivo, o relógio da lease continua correndo e a fence nunca se move. Um degrau cujo alcance excede a política de perturbação da lease viva é recusado, não rebaixado em silêncio.',
    'recovery.attempts': 'Tentativas recentes',
    'recovery.quarantines': 'Quarentenas abertas',
    'recovery.ladderEmpty': 'A escada está vazia.',
    'recovery.ladderEmptyDetail': 'Todo degrau que o watchdog pode subir aparece aqui — seu nome, até onde ele alcança, a política de perturbação da lease que ele exige, seu tempo de espera e seu orçamento por hora.',
    'recovery.blastTitle': 'alcance: quão longe deste único aparelho o degrau chega',
    'recovery.requires': 'Exige política',
    'recovery.requiresTitle': 'uma lease viva precisa ter pelo menos esta política de perturbação ou o degrau é recusado',
    'recovery.cooldown': 'Espera',
    'recovery.cooldownTitle': 'quanto tempo este degrau espera antes de poder ser tentado de novo no mesmo alvo',
    'recovery.budget': 'Orçamento',
    'recovery.budgetTitle': 'o máximo de tentativas que este degrau pode fazer em uma hora, na fazenda inteira',
    'recovery.perHour': '{n} por hora',
    'recovery.disabled': 'desligado',
    'recovery.disabledTitle': 'este degrau está desligado: o watchdog pula e sobe adiante',
    'recovery.attemptsEmpty': 'Nenhuma tentativa de recuperação registrada.',
    'recovery.attemptsEmptyDetail': 'Cada degrau que o watchdog sobe é registrado aqui com seu desfecho, e um degrau recusado aparece com o motivo da recusa.',
    'recovery.inFlight': 'em andamento',
    'recovery.quarantinesEmpty': 'Nenhuma quarentena aberta.',
    'recovery.quarantinesEmptyDetail': 'Uma quarentena aparece aqui quando a escada para de escalonar para um dispositivo, um slot, um hub ou um host e pede um humano. Fechar uma é uma ação auditada.',
    'quarantine.unnamed': '{what}, id não informado',
    'quarantine.unnamedTitle': 'a API não informou identificador para este {what}',
    'quarantine.domainTitle': 'um domínio de energia inteiro, localizado por {by}',
    'quarantine.automatic': 'automática',
    'quarantine.operator': 'operador',

    /* Bulk */
    'bulk.runs': 'Execuções',
    'bulk.selected': 'Execução selecionada',
    'bulk.form.title': 'Rodar um comando',
    'bulk.form.what': 'O que rodar, e onde',
    'bulk.form.command': 'Comando',
    'bulk.form.commandHelp': 'Enviado a cada dispositivo correspondente exatamente como digitado. O shell do próprio aparelho faz a separação de palavras.',
    'bulk.form.selector': 'Seletor',
    'bulk.form.selectorHelp': 'JSON. Quais dispositivos isto alcança. Um pool vazio corresponde a todos os pools, então leia duas vezes antes de iniciar.',
    'bulk.form.limits': 'Limites',
    'bulk.form.max': 'Máximo por hub',
    'bulk.form.maxHelp': 'Dispositivos ao mesmo tempo, por hub.',
    'bulk.form.timeout': 'Timeout',
    'bulk.form.timeoutHelp': 'Milissegundos, por dispositivo.',
    'bulk.form.limitsNote': 'O máximo por hub evita que um comando na frota inteira derrube a energia de um domínio.',
    'bulk.form.submit': 'Iniciar execução',
    'bulk.empty': 'Nenhuma execução em massa ainda.',
    'bulk.emptyDetail': 'Uma execução iniciada no formulário acima aparece aqui, e os resultados por dispositivo entram no painel abaixo conforme cada alvo termina.',
    'bulk.progressTitle': 'ok, error e skipped, dentre os alvos que o seletor encontrou',
    'bulk.perHub': '{n} por hub',
    'bulk.targets': 'alvos',
    'bulk.runFailed': 'Não deu para carregar essa execução.',
    'bulk.noRun': 'Nenhuma execução selecionada.',
    'bulk.noRunDetail': 'Escolha uma execução acima para acompanhar os resultados por dispositivo.',
    'bulk.loadingRun': 'Carregando essa execução…',
    'bulk.loadingRunDetail': 'Os resultados por dispositivo aparecem conforme a API os informa.',
    'bulk.noOutput': 'sem saída',
    'bulk.noTargets': 'Esta execução ainda não tem alvos.',
    'bulk.noTargetsDetail': 'O seletor não encontrou nada, ou a execução ainda não expandiu seu conjunto de alvos.',
    'bulk.badSelector': 'O seletor não é JSON válido:',
    'bulk.started': 'Execução em massa iniciada',
    'bulk.startedDetail': 'Os resultados aparecem abaixo conforme cada dispositivo responde.',
    'bulk.rejected': 'O servidor recusou esta execução:',

    /* Events */
    'events.show': 'Mostrar',
    'events.kind': 'Tipo contém',
    'events.empty': 'Nenhum evento.',
    'events.emptyDetail': 'O log de eventos e o log de auditoria são mesclados aqui, mais recentes primeiro: toda transição de lease, toda tentativa de recuperação e toda ação de operador com o humano que digitou o motivo.',
    'events.narrow': 'Esta é só a página mais nova; aumente Mostrar para alcançar mais para trás.',
  },
};

/* pickInitialLang guesses once, from the only evidence there is.
 *
 * localStorage first, because a choice already made outranks a guess. Then the
 * browser's own languages: a reader whose browser is set to pt-BR is told, by
 * their own configuration, which language they read. Anything else is English,
 * because English is what every string in this file is guaranteed to have. */
function pickInitialLang() {
  try {
    const saved = localStorage.getItem(LANG_KEY);
    if (LANGS.includes(saved)) return saved;
  } catch (_) { /* private mode: fall through to the browser's setting */ }

  const wanted = navigator.languages || [navigator.language || ''];
  for (const tag of wanted) {
    const base = String(tag).toLowerCase().split('-')[0];
    if (LANGS.includes(base)) return base;
  }
  return 'en';
}

let lang = pickInitialLang();

/* currentLang is what everything else reads. */
function currentLang() { return lang; }

/* setLang switches the page.
 *
 * It re-applies every marked element, sets the document's own lang attribute —
 * which is what a screen reader uses to choose a voice, and the one part of this
 * that is not cosmetic — and announces the change so that views holding their
 * own rendered DOM can redraw. docs.js listens for it to refetch content in the
 * new language. */
function setLang(next) {
  if (!LANGS.includes(next) || next === lang) return;
  lang = next;
  try { localStorage.setItem(LANG_KEY, next); } catch (_) { /* not fatal */ }
  document.documentElement.lang = next === 'pt' ? 'pt-BR' : 'en';
  applyTranslations(document);
  window.dispatchEvent(new CustomEvent('languagechange', { detail: { lang: next } }));
}

/* ------------------------------------------------------------------ *
 * Density
 * ------------------------------------------------------------------ */

/* pickInitialDensity. A stored choice outranks everything. There is no
 * browser signal for "how much do you want on screen", so the default is the
 * comfortable one — see the density law in tokens.css for why that is the
 * default and not the fallback. */
function pickInitialDensity() {
  try {
    const saved = localStorage.getItem(DENSITY_KEY);
    if (DENSITIES.includes(saved)) return saved;
  } catch (_) { /* private mode */ }
  return 'comfortable';
}

let density = pickInitialDensity();

function currentDensity() { return density; }

/* setDensity retunes the whole page by writing one attribute.
 *
 * Nothing re-renders. Every rule that spaces anything reads a component token
 * from tokens.css, and the [data-density="compact"] block redefines those
 * tokens — so the browser restyles in place, keeping scroll position, focus,
 * open dialogs and any in-flight command. That is the entire reason density is
 * a token swap rather than a class every component has to know about. */
function setDensity(next) {
  if (!DENSITIES.includes(next) || next === density) return;
  density = next;
  try { localStorage.setItem(DENSITY_KEY, next); } catch (_) { /* not fatal */ }
  document.documentElement.setAttribute('data-density', next);
  window.dispatchEvent(new CustomEvent('densitychange', { detail: { density: next } }));
}

/* t looks a key up in the current language.
 *
 * A MISSING KEY RETURNS THE KEY ITSELF, loudly and visibly, rather than falling
 * back to English. A silent English fallback is how a half-translated interface
 * survives for years: nobody can tell a deliberate English word from a forgotten
 * one. `nav.fleet` on screen is ugly and gets fixed;
 * TestEveryStringExistsInBothLanguages is what stops it reaching a reader at
 * all.
 *
 * vars interpolate as {name}. They are substituted with String(), never parsed,
 * so a value that happens to contain braces cannot introduce a second
 * substitution. */
function t(key, vars) {
  const table = STRINGS[lang] || STRINGS.en;
  let s = table[key];
  if (s === undefined) return key;
  if (vars) {
    for (const k of Object.keys(vars)) {
      s = s.split('{' + k + '}').join(String(vars[k]));
    }
  }
  return s;
}

/* applyTranslations fills in every element marked in the HTML.
 *
 * `data-i18n` sets textContent. `data-i18n-attr` sets attributes, as
 * space-separated `attribute:key` pairs — which is how a placeholder, a title
 * and an aria-label get translated without a wrapper element each.
 *
 * textContent rather than innerHTML, here as everywhere in this app: a
 * dictionary is a file in this repository today, and the day somebody loads one
 * from anywhere else is the day innerHTML becomes a script injection. */
function applyTranslations(root) {
  const scope = root || document;
  for (const el of scope.querySelectorAll('[data-i18n]')) {
    el.textContent = t(el.getAttribute('data-i18n'));
  }
  for (const el of scope.querySelectorAll('[data-i18n-attr]')) {
    for (const pair of el.getAttribute('data-i18n-attr').split(/\s+/)) {
      const at = pair.indexOf(':');
      if (at <= 0) continue;
      el.setAttribute(pair.slice(0, at), t(pair.slice(at + 1)));
    }
  }
}

/* The keys, for the guard test and for anybody adding a language. */
function i18nKeys(which) { return Object.keys(STRINGS[which] || {}); }

/* Applied as early as the parser reaches this file, so the first paint is
 * already in the right language rather than flashing English and correcting
 * itself. The DOM below this script does not exist yet, so a second pass runs
 * on DOMContentLoaded. */
document.documentElement.lang = lang === 'pt' ? 'pt-BR' : 'en';
document.documentElement.setAttribute('data-density', density);
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', () => applyTranslations(document));
} else {
  applyTranslations(document);
}
