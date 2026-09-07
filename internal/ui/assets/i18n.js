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
    'exec.force.reason': 'Reason — required because this device is leased',
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
    /* The masthead. "device-farmer" is the repository, the binary and the
     * database owner; it is not what a business calls the thing its fleet runs
     * on. The product name is capitalised like a product and the line under it
     * says what the page is for. Neither is a customer's name: this file has
     * no way to know one. */
    'app.name': 'Device Farmer',
    'app.role': 'Operations console',
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
    'header.density': 'Density',
    'header.densityTitle': 'Fit more on the screen, or give it more room to read',
    'header.densityComfortable': 'Comfortable',
    'header.densityCompact': 'Compact',

    /* The navigation rail. The groups are named for what an operator is doing,
     * not for which endpoint each view calls. */
    'nav.group.operations': 'Operations',
    'nav.group.maintenance': 'Maintenance',
    'nav.group.help': 'Help',
    'nav.collapse': 'Collapse the menu',
    'nav.expand': 'Expand the menu',

    /* The per-view page headers.
     *
     * A lede is a true sentence that teaches, not a label that repeats the tab
     * it is under. The model is the Leases axiom in index.html and the "this
     * device is free" paragraph in device.js: say what the view is for, and say
     * the one thing a newcomer would otherwise get wrong.
     *
     * The titles are their own keys and not the nav labels, even where the two
     * read the same today. A rail label has to survive a 240px column and a
     * page title does not — "Bulk" against "Bulk runs" is already the
     * difference — and tying them together would mean the shorter constraint
     * decided both. */
    'page.fleet.title': 'Fleet',
    'page.fleet.lede': 'Every device this farm can see, grouped by the host and the hub it is plugged into. Health is what the watchdog last measured — a healthy device can be busy, and an offline one can still be held.',
    'page.fleet.action': 'Recheck the fleet',
    'page.leases.title': 'Leases',
    'page.leases.lede': 'Who is holding which device, and until when. A lease is a claim on a device rather than a statement about its health, and this is the page to read before you take anything back from anyone.',
    'page.leases.action': 'Recheck the leases',
    'page.jobs.title': 'Jobs',
    'page.jobs.lede': 'Work waiting for a device, work running on one, and what each step printed. Submitting a job asks the control plane for a device that matches what you described; you never pick the handset yourself.',
    'page.jobs.action': 'Submit a job',
    'page.recovery.title': 'Recovery',
    'page.recovery.lede': 'Where a device that stopped answering gets its chances, and where you can see whether they worked. Nothing on this page ends a lease: recovery acts on behalf of whoever is holding the device, never instead of them.',
    'page.recovery.action': 'Recheck recovery',
    'page.bulk.title': 'Bulk runs',
    'page.bulk.lede': 'One command, many devices, and one record of what each of them answered. It is the page for a question you would otherwise have to ask one device at a time.',
    'page.bulk.action': 'Start a run',
    'page.events.title': 'Events',
    'page.events.lede': 'What the control plane and the operators did, newest first. It is the record to reach for when somebody asks why a device changed hands, and the server writes it — not this page.',
    'page.events.action': 'Load the newest entries',
    'page.docs.title': 'Documentation',
    'page.docs.lede': 'Help for whoever is on shift. It explains the words the other six pages use — a lease, a rung, a fence — and it describes this deployment rather than the product in general.',
    'page.docs.action': 'Recheck what this farm can do',
    'page.rechecking': 'Asking the API for this page again.',

    /* Connection state. These are about THIS page's connection to the API and
     * say nothing about any lease — see conn.downNote. */
    'conn.live': 'live',
    'conn.polling': 'polling',
    'conn.connecting': 'connecting',
    'conn.down': 'no connection',
    'conn.downNote': 'This page cannot reach the API. It says nothing about the farm: leases are held in PostgreSQL and are unaffected by a browser that lost its connection.',

    /* The command chooser, and the bulk form that mounts the same builder.
     * The catalogue's own entries are further up; these are the frame around
     * them. No command is here, in either dictionary, for the reason stated at
     * the top of this file. */
    'exec.commandLabel': 'The command, exactly as the handset will receive it',
    'exec.chooser.open': 'Pick a command…',
    'exec.chooser.filter': 'Filter by command or by what it does',
    'exec.chooser.tiers': 'Which tier of the catalogue to show',
    'exec.chooser.close': 'Close',
    'exec.chooser.none': 'Nothing in this tier matches that.',
    'exec.chooser.otherTier': '{n} in the other tier do — switch above.',
    'exec.bulk.preflight': 'What a fleet-wide command means here',
    'exec.bulk.note': 'Whatever is on the wire above goes to every device the selector matched, verbatim, a few per hub at a time. In the drawer a wrong command reaches one handset; here it reaches all of them.',
    'exec.bulk.fenceOn': 'this farm enforces the fence at the device. A bulk run is not refused up front the way one device is — it starts, and every target answers with a transport failure of its own',
    'exec.bulk.targets': 'decided by the selector above. A device holding a live lease is never commanded: it is recorded as skipped, with the lease that stopped it',
    'exec.bulk.answer': 'one result row per device, as each one answers or gives up',
    'exec.bulk.empty': 'Type a command, or pick one from the catalogue, before starting a run.',

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

    /* The reason field, and who is actually asking for it.
     *
     * Two labels, because there are two cases and the dashboard used to know
     * only one. Five of the six confirmable actions are refused by the server
     * with no reason; POST /api/v1/jobs/{id}/cancel is not. The label says
     * which case this is, in the server's terms — the page is reporting a rule,
     * not inventing one — and the suggestions turn the required case from a
     * sentence typed from nothing into one click. */
    'confirm.reason.required': 'Reason — the server refuses this action without one. It is written to farm.audit_log next to your name.',
    'confirm.reason.optional': 'Reason — optional. It is written to farm.audit_log if you give one.',
    'confirm.notSent': 'Nothing was sent.',
    'confirm.reasonMissing': 'POST /api/v1/{route} answers 400 without a reason. Type one, or pick a suggestion.',
    'confirm.suggest.label': 'Reason suggestions',
    'confirm.suggest.last': 'last: {reason}',
    'confirm.suggest.maintenance': 'scheduled maintenance',
    'confirm.suggest.hostUnhealthy': 'host is unhealthy',
    'confirm.suggest.agentRollout': 'agent rollout on this host',
    'confirm.suggest.backInService': 'fault fixed, back in service',
    'confirm.suggest.maintenanceDone': 'maintenance finished',
    'confirm.suggest.holderGone': 'holder is gone and the job is abandoned',
    'confirm.suggest.deviceNeeded': 'device needed for an urgent run',
    'confirm.suggest.leaseStuck': 'lease is stuck; the job will not finish',
    'confirm.suggest.adbOffline': 'device is offline in adb',
    'confirm.suggest.wedged': 'device is wedged; power is the last resort',
    'confirm.suggest.cableReplaced': 'cable replaced, fault fixed',
    'confirm.suggest.healthyAgain': 'device re-enumerated and is healthy',
    'confirm.suggest.notNeeded': 'no longer needed',
    'confirm.suggest.superseded': 'superseded by a newer run',
    'confirm.suggest.wrongTarget': 'submitted against the wrong target',

    /* The force step in the command box. The condition is the server's, from
     * internal/api/fleet.go: a reason is required BECAUSE the device is
     * leased, not because every command needs one. */
    'exec.force.reasonMissingHead': 'Nothing was sent.',
    'exec.force.reasonMissing': 'This device holds a live lease, and POST /api/v1/devices/{id}/exec answers 400 without a reason. Say why you are running this on a device somebody is using.',

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
    'confirm.reasonNote': 'Six weeks from now it is the only record of why.',
    'confirm.yes': 'Confirm',
    'confirm.no': 'Cancel',
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
    'exec.force.reason': 'Motivo — obrigatório porque este dispositivo está com uma lease',
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
    /* O cabeçalho da marca. Veja a nota no dicionário em inglês: o nome do
     * produto não é o nome do repositório. */
    'app.name': 'Device Farmer',
    'app.role': 'Console de operações',
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
    'header.density': 'Densidade',
    'header.densityTitle': 'Caber mais na tela, ou dar mais espaço para ler',
    'header.densityComfortable': 'Confortável',
    'header.densityCompact': 'Compacto',

    /* A barra de navegação. Os grupos têm o nome do que o operador está
     * fazendo, não do endpoint que cada visão chama. */
    'nav.group.operations': 'Operação',
    'nav.group.maintenance': 'Manutenção',
    'nav.group.help': 'Ajuda',
    'nav.collapse': 'Recolher o menu',
    'nav.expand': 'Expandir o menu',

    /* Os cabeçalhos de cada visão.
     *
     * Um lide é uma frase verdadeira que ensina, não um rótulo que repete a
     * aba acima dele. O modelo é o axioma das leases no index.html e o
     * parágrafo “este dispositivo está livre” do device.js: dizer para que
     * serve a visão, e dizer a única coisa que um recém-chegado erraria. */
    'page.fleet.title': 'Frota',
    'page.fleet.lede': 'Todo dispositivo que esta fazenda enxerga, agrupado pelo host e pelo hub em que está conectado. Saúde é o que o watchdog mediu por último — um dispositivo saudável pode estar ocupado, e um offline ainda pode estar preso a uma lease.',
    'page.fleet.action': 'Reconferir a frota',
    'page.leases.title': 'Leases',
    'page.leases.lede': 'Quem está segurando qual dispositivo, e até quando. Uma lease é uma reivindicação sobre o dispositivo, não uma afirmação sobre a saúde dele, e esta é a página para ler antes de tomar qualquer coisa de volta de alguém.',
    'page.leases.action': 'Reconferir as leases',
    'page.jobs.title': 'Jobs',
    'page.jobs.lede': 'Trabalho esperando por um dispositivo, trabalho rodando em um, e o que cada passo imprimiu. Enviar um job pede ao control plane um dispositivo que combine com o que você descreveu; você nunca escolhe o aparelho na mão.',
    'page.jobs.action': 'Enviar um job',
    'page.recovery.title': 'Recuperação',
    'page.recovery.lede': 'Onde um dispositivo que parou de responder ganha suas chances, e onde dá para ver se elas funcionaram. Nada nesta página encerra uma lease: a recuperação age em nome de quem segura o dispositivo, nunca no lugar dele.',
    'page.recovery.action': 'Reconferir a recuperação',
    'page.bulk.title': 'Execuções em massa',
    'page.bulk.lede': 'Um comando, muitos dispositivos, e um registro do que cada um respondeu. É a página para uma pergunta que você teria de fazer a um dispositivo de cada vez.',
    'page.bulk.action': 'Iniciar uma execução',
    'page.events.title': 'Eventos',
    'page.events.lede': 'O que o control plane e os operadores fizeram, do mais recente para o mais antigo. É o registro a consultar quando alguém pergunta por que um dispositivo trocou de mãos, e quem escreve é o servidor — não esta página.',
    'page.events.action': 'Carregar as entradas mais recentes',
    'page.docs.title': 'Documentação',
    'page.docs.lede': 'Ajuda para quem está de plantão. Explica as palavras que as outras seis páginas usam — lease, degrau, fence — e descreve este deployment, não o produto em geral.',
    'page.docs.action': 'Reconferir o que esta fazenda faz',
    'page.rechecking': 'Pedindo esta página à API de novo.',

    /* Estado da conexão. É sobre a conexão DESTA página com a API e não diz
     * nada sobre nenhuma lease — veja conn.downNote. */
    'conn.live': 'ao vivo',
    'conn.polling': 'consultando',
    'conn.connecting': 'conectando',
    'conn.down': 'sem conexão',
    'conn.downNote': 'Esta página não alcança a API. Isso não diz nada sobre a fazenda: as leases vivem no PostgreSQL e não são afetadas por um navegador que perdeu a conexão.',

    /* O seletor de comandos, e o formulário em massa que monta o mesmo widget.
     * As entradas do catálogo estão mais acima; isto é a moldura em volta
     * delas. Nenhum comando aparece aqui, em nenhum dos dois dicionários, pela
     * razão declarada no topo deste arquivo. */
    'exec.commandLabel': 'O comando, exatamente como o aparelho vai recebê-lo',
    'exec.chooser.open': 'Escolher um comando…',
    'exec.chooser.filter': 'Filtrar pelo comando ou pelo que ele faz',
    'exec.chooser.tiers': 'Qual camada do catálogo mostrar',
    'exec.chooser.close': 'Fechar',
    'exec.chooser.none': 'Nada nesta camada corresponde a isso.',
    'exec.chooser.otherTier': '{n} na outra camada correspondem — troque acima.',
    'exec.bulk.preflight': 'O que um comando para toda a frota significa aqui',
    'exec.bulk.note': 'O que estiver no fio acima vai para cada dispositivo que o seletor casou, literalmente, alguns por hub de cada vez. Na gaveta um comando errado alcança um aparelho; aqui alcança todos eles.',
    'exec.bulk.fenceOn': 'esta farm impõe a fence no aparelho. Uma execução em massa não é recusada de antemão como a de um dispositivo só — ela começa, e cada alvo responde com uma falha de transporte própria',
    'exec.bulk.targets': 'decidido pelo seletor acima. Um dispositivo com lease viva nunca é comandado: ele é registrado como pulado, com a lease que o impediu',
    'exec.bulk.answer': 'uma linha de resultado por dispositivo, conforme cada um responde ou desiste',
    'exec.bulk.empty': 'Digite um comando, ou escolha um do catálogo, antes de iniciar uma execução.',

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

    /* O campo motivo, e quem de fato está pedindo por ele.
     *
     * Dois rótulos, porque há dois casos e o painel só conhecia um. Cinco das
     * seis ações confirmáveis são recusadas pelo servidor sem motivo; POST
     * /api/v1/jobs/{id}/cancel não é. O rótulo diz qual é o caso, nos termos do
     * servidor — a página relata uma regra, não inventa uma — e as sugestões
     * transformam o caso obrigatório de uma frase escrita do zero em um
     * clique. */
    'confirm.reason.required': 'Motivo — o servidor recusa esta ação sem um. É escrito em farm.audit_log ao lado do seu nome.',
    'confirm.reason.optional': 'Motivo — opcional. É escrito em farm.audit_log se você der um.',
    'confirm.notSent': 'Nada foi enviado.',
    'confirm.reasonMissing': 'POST /api/v1/{route} responde 400 sem um motivo. Escreva um, ou escolha uma sugestão.',
    'confirm.suggest.label': 'Sugestões de motivo',
    'confirm.suggest.last': 'último: {reason}',
    'confirm.suggest.maintenance': 'manutenção programada',
    'confirm.suggest.hostUnhealthy': 'o host não está saudável',
    'confirm.suggest.agentRollout': 'atualização do agente neste host',
    'confirm.suggest.backInService': 'falha corrigida, de volta ao serviço',
    'confirm.suggest.maintenanceDone': 'manutenção concluída',
    'confirm.suggest.holderGone': 'o holder sumiu e o job foi abandonado',
    'confirm.suggest.deviceNeeded': 'dispositivo necessário para uma execução urgente',
    'confirm.suggest.leaseStuck': 'a lease travou; o job não vai terminar',
    'confirm.suggest.adbOffline': 'o dispositivo está offline no adb',
    'confirm.suggest.wedged': 'dispositivo travado; energia é o último recurso',
    'confirm.suggest.cableReplaced': 'cabo trocado, falha corrigida',
    'confirm.suggest.healthyAgain': 'o dispositivo reenumerou e está saudável',
    'confirm.suggest.notNeeded': 'não é mais necessário',
    'confirm.suggest.superseded': 'substituído por uma execução mais nova',
    'confirm.suggest.wrongTarget': 'enviado contra o alvo errado',

    /* O passo de forçar na caixa de comandos. A condição é a do servidor, de
     * internal/api/fleet.go: o motivo é obrigatório PORQUE o dispositivo está
     * com uma lease, não porque todo comando precisa de um. */
    'exec.force.reasonMissingHead': 'Nada foi enviado.',
    'exec.force.reasonMissing': 'Este dispositivo tem uma lease viva, e POST /api/v1/devices/{id}/exec responde 400 sem um motivo. Diga por que você está rodando isto num dispositivo que alguém está usando.',

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
    'confirm.reasonNote': 'Daqui a seis semanas é o único registro do porquê.',
    'confirm.yes': 'Confirmar',
    'confirm.no': 'Cancelar',
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
