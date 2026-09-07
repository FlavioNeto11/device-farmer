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

    /* Empty states.
     *
     * An empty panel used to say what was absent and then name the SQL object
     * it was absent from, at the same weight, with nothing to press. Both
     * halves survive here and the order is now the order a reader needs them:
     * the sentence a person can act on, then the button that acts, then
     * `empty.*.where` — the provenance, collapsed behind "Where this comes
     * from" for the reader who has started writing their own queries.
     *
     * The identifiers inside these sentences — farm.v_fleet, expires_at,
     * reclaimable_at, farm.job_steps, ctl job steps — are view names, column
     * names and a command line. They are NOT translated in either dictionary,
     * for the reason at the top of this file: they are what gets grepped. */
    'empty.where': 'Where this comes from',
    'empty.retry': 'Try again',
    'empty.openDocs': 'Open the docs',
    'empty.clearFilters': 'Clear the filters',
    'empty.failed': 'Could not load this from the API.',
    'empty.loading': 'Loading from the API…',
    'empty.loadingDetail': 'Nothing is drawn until the server answers.',
    'empty.fleet.filtered': 'No devices match.',
    'empty.fleet.filteredDetail': 'The farm has devices; the filters and the search box above are hiding all of them. Nothing is wrong with the farm.',
    /* {n} is what the API returned, which the server has ALREADY narrowed by
     * host, hub, health, pool and the search box. It is not the size of the
     * farm and this sentence must not say that it is. */
    'empty.fleet.hiddenDetail': 'The API returned {n} devices and the filters above are hiding every one of them.',
    'empty.fleet.none': 'This farm has no devices yet.',
    'empty.fleet.noneDetail': 'That is not the same as nothing matching a filter: nothing has ever been registered. A device appears here on its own, within a minute, once a watchdog running on a host sees it on a hub that host is watching. Nothing on this page can create one.',
    'empty.fleet.where': 'This grid shows every device in farm.v_fleet grouped by host and then by hub — rack slot, model, health, lease and battery.',
    'empty.leases.filtered': 'No leases in this state.',
    'empty.leases.filteredDetail': 'The farm may well be holding leases; none of them is in the state selected above.',
    'empty.leases.showAll': 'Show every lease state',
    'empty.leases.none': 'Nothing is holding a device right now.',
    'empty.leases.noneDetail': 'A lease is created when a job is placed on a device, or when a person acquires one by hand. An idle farm holds none, and that says nothing about the health of any device.',
    'empty.leases.openJobs': 'Go to Jobs',
    'empty.leases.where': 'Every live lease appears here with its fence, holder, job, and the two server-computed instants that matter: expires_at (when it becomes suspect) and reclaimable_at (the earliest the reaper may act).',
    'empty.jobs.filtered': 'No jobs in this state.',
    'empty.jobs.filteredDetail': 'Jobs exist; none of them is in the state selected above.',
    'empty.jobs.showAll': 'Show every state',
    'empty.jobs.none': 'No jobs.',
    'empty.jobs.noneDetail': 'A job is queued the moment it is submitted, and the scheduler then acquires a lease on a device its selector matches. Submitting one does not reserve a device; the scheduler does that, and it may wait.',
    'empty.jobs.submit': 'Submit a job',
    'empty.steps.none': 'No steps for this attempt.',
    'empty.steps.noneDetail': 'The runner writes one row per step as it executes it. A job that has not been placed on a device yet has none — this is a job waiting, not a job failing.',
    'empty.steps.otherAttempts': 'This job does have steps, filed under attempt {attempts}, and none under the one selected above. A retry writes a fresh set of rows rather than adding to the old ones.',
    'empty.steps.showAll': 'Show every attempt',
    'empty.steps.recheck': 'Check again',
    'empty.steps.where': 'The rows come from farm.job_steps, one per step per attempt, and are the same ones ctl job steps prints.',
    'empty.steps.noJob': 'No job selected.',
    'empty.steps.noJobDetail': 'Press Steps on a job above to see which step it is on, what it printed, and why it stopped.',
    'empty.steps.failed': 'Could not read the steps of that job.',
    'empty.steps.loading': 'Loading the step log…',
    'empty.tiers.none': 'The ladder is empty.',
    'empty.tiers.noneDetail': 'A farm whose migrations have run has nine rungs here. None at all means the recovery schema was never seeded, so the watchdog has nothing to climb and will recover nothing on its own.',
    'empty.tiers.where': 'farm.recovery_tiers rows 0 through 8 appear here — name, blast radius, the lease disruption policy each rung requires, its cooldown and its hourly budget.',
    'empty.attempts.none': 'No recovery attempts recorded.',
    'empty.attempts.noneDetail': 'The watchdog writes a row here every time it climbs a rung, and a refused rung is recorded with the reason it was refused. An empty list is good news, not a missing feature.',
    'empty.quarantines.none': 'No open quarantines.',
    'empty.quarantines.noneDetail': 'A quarantine opens when the ladder gives up on a device, slot, hub or host and asks for a person. None open means the ladder has given up on nothing.',
    'empty.bulk.none': 'No bulk runs yet.',
    'empty.bulk.noneDetail': 'A run sends one command to every device its selector matches, a few per hub at a time so that one power domain cannot brown out, and streams each answer back below.',
    'empty.bulk.start': 'Start a run',
    'empty.bulk.noRun': 'No run selected.',
    'empty.bulk.noRunDetail': 'Pick a run above to watch its per-device results.',
    'empty.bulk.loadingRun': 'Loading that run…',
    'empty.bulk.loadingRunDetail': 'Per-device results appear as the API reports them.',
    'empty.bulk.failedRun': 'Could not load that run.',
    'empty.bulk.noTargets': 'This run has no targets yet.',
    'empty.bulk.noTargetsDetail': 'The selector matched nothing, or the run has not expanded its target set.',
    'empty.events.none': 'No events.',
    'empty.events.noneDetail': 'Every lease transition, every recovery attempt and every operator action lands here, newest first, and stays. A farm that has not done anything yet has none.',
    'empty.events.where': 'farm.events and farm.audit_log are merged here newest first, and an audited row carries the person who typed the reason.',

    /* The first-visit card on Fleet. Three sentences and a door; see #first-run
     * in index.html for why it is not a tour. */
    'firstRun.title': 'What this page is',
    'firstRun.p1': 'This is the control plane for a rack of physical Android devices: what each one is doing, who is holding it, and what the watchdog did about the ones that stopped answering.',
    'firstRun.p2': 'Fleet, Leases and Jobs are the present tense — every device in the rack, who holds each one right now, and the work queued against them. Recovery, Bulk, Events and Docs are the record and the tools: what the watchdog tried, one command sent to many devices at once, the audited history of everything, and every word on this page defined.',
    'firstRun.p3': 'It reads the same PostgreSQL rows that ctl reads, so it is not a second source of truth — and every action that changes anything asks you to confirm it and to type a reason, which is kept.',
    'firstRun.docs': 'Open the docs',
    'firstRun.dismiss': 'Got it',
    'firstRun.dismissTitle': 'Hide this card in this browser. Everything it says is also in Docs, which is one key away.',

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

    /* The fleet summary strip, and the two chips a device card is allowed.
       `filter.liveLease` belongs in the Filters block above and is here because
       this file is being edited by eleven people at once and one contiguous
       insertion point is the difference between a merge and a morning. */
    'filter.liveLease': 'in use (held or suspect)',

    'fleet.sum.region': 'Fleet summary',
    'fleet.sum.attention': 'Needs attention',
    'fleet.sum.attentionSub': 'somebody should look at these',
    'fleet.sum.attentionHelp': 'Devices whose health is not healthy. Parked and retired are not counted: those are decisions somebody made, not faults. Click to show only these; click again to clear.',
    'fleet.sum.inUse': 'In use',
    'fleet.sum.inUseSub': 'held by a job or a person',
    'fleet.sum.inUseHelp': 'Devices carrying a live lease, held or suspect. A suspect lease is still a lease: the device is NOT free. Click to show only these; click again to clear.',
    'fleet.sum.free': 'Free',
    'fleet.sum.freeSub': 'nobody holds them',
    'fleet.sum.freeHelp': 'Devices with no live lease. Health has nothing to do with it: an offline device can still be free, and a free one can be broken. Click to show only these; click again to clear.',
    'fleet.sum.offline': 'Offline',
    'fleet.sum.offlineSub': 'not answering ADB',
    'fleet.sum.offlineHelp': 'Devices whose health reads exactly offline — a subset of the ones needing attention, and usually the cable or the hub port rather than the phone. Click to show only these; click again to clear.',
    'fleet.sum.total': 'Devices',
    'fleet.sum.totalSub': 'everything in this view',
    'fleet.sum.totalHelp': 'Every device the server returned for this view. Click to clear the health and lease filters.',
    'fleet.sum.allClear': 'Nothing needs attention right now.',
    'fleet.sum.wholeFarm': 'These five numbers are the whole farm, as the server counted it.',
    'fleet.sum.filtered': 'A filter is on: these five numbers count the {served} devices the server returned for it, not the whole farm.',
    'fleet.sum.onScreen': 'The grid below is showing {shown} of them.',
    'fleet.sum.truncated': 'The server capped this response, so these five numbers count only the rows that came back — not the farm.',

    'fleet.cond.healthy': 'The last probe answered and the device can be given work.',
    'fleet.cond.degraded': 'Probes are failing but the device still answers. It can still be given work; the recovery ladder is watching it.',
    'fleet.cond.offline': 'ADB cannot reach the device. Suspect the cable or the hub port before the handset.',
    'fleet.cond.missing': 'The device was recorded on this port and is no longer there at all.',
    'fleet.cond.unauthorized': 'The device answers but has not accepted this host key. Somebody has to tap Allow on the handset itself.',
    'fleet.cond.recovering': 'The recovery ladder is working on this device right now.',
    'fleet.cond.booting': 'The device is coming up. Give it a moment before reading anything into it.',
    'fleet.cond.quarantined': 'An open quarantine stops new work being scheduled here. It does not touch a live lease: whatever is running keeps running.',
    'fleet.cond.parked': 'Out of service on purpose — a charge limiter holding the battery, or an operator who wrote down why. Not a fault.',
    'fleet.cond.retired': 'Taken out of the farm for good. Not a fault.',
    'fleet.cond.unknown': 'Nothing has been recorded about this device yet, which is not the same as it being fine.',

    'fleet.avail.free': 'Free',
    'fleet.avail.inUse': 'In use',
    'fleet.avail.suspect': 'In use · suspect',
    'fleet.avail.protected': 'In use · protected',
    'fleet.avail.freeHelp': 'No live lease on this device. Anything may take it.',
    'fleet.avail.heldHelp': 'lease state held: the holder is heartbeating and the lease is being renewed.',
    'fleet.avail.suspectHelp': 'lease state suspect: no heartbeat from the holder. The device is NOT released and the job may be running fine.',
    'fleet.avail.protectedHelp': 'A protected lease. The reaper will never reclaim it: a lease ends when the job says so, when a deadline the user wrote down elapses, or when a human takes it back.',
    'fleet.avail.freeLine': 'Nobody holds it right now.',
    'fleet.avail.heldLine': '{holder} is using it.',
    'fleet.avail.suspectLine': '{holder} is using it, but stopped answering.',
    'fleet.avail.suspectProtectedLine': '{holder} is using it and stopped answering. The lease is protected, so only the job or a person ends it.',
    'fleet.avail.protectedLine': '{holder} is using it. Only the job or a person ends it.',
    'fleet.avail.someoneElse': 'Another tenant',

    'fleet.flag.label': 'Also true of this device:',
    'fleet.flag.open': 'Open the device to see it in full.',
    'fleet.flag.quarantine': 'quarantine open — {reason}',
    'fleet.flag.health': 'the health column reads {health}',
    'fleet.flag.dupSerial': 'this ADB serial is not unique in the farm, so address this device by devpath only',
    'fleet.flag.adminState': 'admin_state is {state} rather than enabled, so no new work is scheduled here',

    'fleet.tile.unknownModel': 'unknown model',
    'fleet.tile.unslottedHelp': 'This device has no rack_slot label, so nobody can be told where to walk. The USB path is all there is.',
    'fleet.tile.battery': 'Battery',
    'fleet.tile.noBattery': 'No battery level has been reported for this device.',

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

    /* The Fleet as a table: the mode switch, the column headings, and the words
     * a screen reader hears where a sighted reader sees an arrow.
     *
     * A column HEADING is interface and is translated. Nothing a cell holds is:
     * a rack slot, a model, a host, a hub path and a health value are all what
     * the API said, and an operator matches them by eye against a psql session
     * and a log line. */
    'fleet.view': 'How to show the fleet',
    'fleet.viewCards': 'Cards',
    'fleet.viewTable': 'Table',
    'fleet.viewCardsWhy': 'One card per device, grouped by host and then by hub — the order somebody walks the room in.',
    'fleet.viewTableWhy': 'One row per device, sortable by any column. This is the mode that stays readable on a large farm.',
    'fleet.col.slot': 'Rack slot',
    'fleet.col.model': 'Model',
    'fleet.col.where': 'Host / hub',
    'fleet.col.condition': 'Condition',
    'fleet.col.availability': 'Availability',
    'fleet.col.battery': 'Battery',
    'fleet.col.lastSeen': 'Last seen',
    'fleet.openDevice': 'Open {device}',
    'fleet.unslotted': 'no slot',
    'fleet.unslottedWhy': 'This device has no rack_slot label, so nobody can be told where to walk to reach it.',
    'fleet.unknownModel': 'model not reported',
    'fleet.dupSerial': 'dup serial',
    'fleet.dupSerialWhy': 'This ADB serial is not unique in this farm, so a command addressed by serial could reach either handset. Address this one by devpath.',
    'fleet.openQuarantine': 'an open quarantine record, with no reason written on it',
    'fleet.noHost': 'no host recorded',
    'fleet.noHub': 'no hub recorded',
    'table.sortable': 'not sorted by this column; activate to sort by it',
    'table.sortedAsc': 'sorted by this column, ascending; activate to reverse it',
    'table.sortedDesc': 'sorted by this column, descending; activate to reverse it',

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

    /* The device sheet — unit 6.
     *
     * The sentences below are the ones an operator reads instead of a uuid. The
     * `device.f.*` keys are the HUMAN half of every row; the machine half — the
     * column name — is printed underneath by kv() and is never translated,
     * because it is what the database, `ctl` and the log line say. */
    'device.tab.overview': 'Overview',
    'device.tab.health': 'Health',
    'device.tab.lease': 'Lease',
    'device.tab.screen': 'Screen',
    'device.tab.command': 'Command',
    'device.tab.raw': 'Raw',
    'device.tabsLabel': 'Device sections',
    'device.unnamedModel': 'No model reported by this device',
    'device.loadingDetail': 'Fetching {path} from the API.',
    'device.fetchFailed': 'Device detail could not be fetched ({err}); showing the fleet row instead.',

    /* The status line. Health and a lease are independent, so every sentence
     * here says which of the two it is talking about. An offline device can
     * still be held and a healthy one can be idle. */
    'device.say.freeHealthy': 'Healthy and free. Nobody is using this device.',
    'device.say.freeFault': 'Free, and health says {health}. Nobody is using this device; whether it can run a job is a separate question.',
    'device.say.freeOffService': 'Free, and out of service on purpose ({health}). Nobody is using it, and nobody is meant to.',
    'device.say.heldBySince': 'In use by {holder} since {since}.',
    'device.say.heldBy': 'In use by {holder}.',
    'device.say.heldSince': 'In use since {since}; the lease records no holder.',
    'device.say.heldByJobSince': 'In use since {since}, by job {job}; the lease records no holder.',
    'device.say.heldByJob': 'In use by job {job}; the lease records no holder.',
    'device.say.held': 'In use. The lease records neither a holder nor a start.',
    'device.say.expires': 'Expires in {when}.',
    'device.say.expiryPassed': 'Its expiry passed {when} ago.',
    'device.say.noExpiry': 'No expiry is recorded, so only the job or a human ends this lease.',
    'device.say.suspectNote': 'No heartbeat has arrived from the holder. The device is not released, and a heartbeat that arrives later heals the lease at the same fence.',
    'device.say.protectedNote': 'The lease is protected: nothing reclaims it automatically, and a human is paged instead.',

    'device.where': 'Where it is',
    'device.where.full': 'Rack slot {slot}, on host {host}, behind hub {hub}.',
    'device.where.noSlot': 'On host {host}, behind hub {hub}. No rack slot is recorded, so this handset has no physical address written down.',
    'device.where.noHub': 'Rack slot {slot}, on host {host}. No hub is recorded.',
    'device.where.hostOnly': 'On host {host}. Neither a rack slot nor a hub is recorded.',
    'device.where.slotOnly': 'Rack slot {slot}. No host is recorded, which is a device this farm cannot reach.',
    'device.where.noHost': 'Rack slot {slot}, behind hub {hub}. No host is recorded, which is a device this farm cannot reach.',
    'device.where.hubOnly': 'Behind hub {hub}. Neither a rack slot nor a host is recorded, and with no host this farm cannot reach it.',
    'device.where.nothing': 'Nowhere recorded: no rack slot, no host, no hub.',
    'device.availability': 'Can it be used',

    'device.identifiers': 'Identifiers',
    'device.identifiersNote': 'The strings this device is known by — in the database, in ADB and in a log line. Nothing here changes how it behaves; they are here to be matched and copied.',
    'device.copy': 'Copy',
    'device.copied': 'Copied',
    'device.copyFailed': 'This browser refused the clipboard; select the text and copy it.',
    'device.serialAmbiguous': 'not unique',
    'device.serialAmbiguousWhy': 'More than one device on this farm reports this ADB serial, so the serial alone does not address this handset.',

    'device.healthNote': 'Health is what the last check saw. It says nothing about who holds the device: an offline device can still be held, and a healthy one can be idle.',
    'device.leaseNote': 'A lease ends when the job says so, when a deadline the user wrote down elapses, or when a human takes it back. Nothing on this screen ends one on its own.',
    'device.screenNote': 'A session costs three ADB transports and a hardware encoder on the handset, so it starts when you ask for it. Closing this sheet stops it.',
    'device.rawNote': 'Exactly what the API returned for this device, before this page interpreted any of it.',
    'device.noQuarantine': 'none',
    'device.revoke': 'Revoke lease',
    'device.closeQuarantine': 'Close quarantine',

    /* Row labels. The column name beside each one is printed by kv() and is
     * never translated — see the note at the top of this file. */
    'device.f.pool': 'Pool',
    'device.f.adminState': 'Administrative state',
    'device.f.android': 'Android',
    'device.f.failureScore': 'Failure score',
    'device.f.farmUID': 'Farm UID',
    'device.f.deviceID': 'Device ID',
    'device.f.serial': 'ADB serial',
    'device.f.usbPath': 'USB path',
    'device.f.devpath': 'ADB devpath',
    'device.f.slotID': 'Slot',
    'device.f.slotState': 'Slot state',
    'device.f.labels': 'Labels',
    'device.f.health': 'Health',
    'device.f.healthSince': 'In this state since',
    'device.f.adbState': 'ADB state',
    'device.f.battery': 'Battery',
    'device.f.batteryTemp': 'Battery temperature',
    'device.f.consecBad': 'Consecutive failed checks',
    'device.f.nextRung': 'Next recovery rung',
    'device.f.lastSeen': 'Last seen',
    'device.f.quarantine': 'Quarantine',
    'device.f.leaseState': 'Lease state',
    'device.f.leaseID': 'Lease ID',
    'device.f.fence': 'Fence',
    'device.f.job': 'Job',
    'device.f.tenant': 'Tenant',
    'device.f.holder': 'Holder',
    'device.f.acquired': 'Acquired',
    'device.f.expires': 'Expires',
    'device.f.reclaimable': 'Reclaimable',

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

    /* The glossary. Seventeen words this page prints bare, each with the one
     * sentence a newcomer needs and the name the same thing has in the
     * database. terms.js builds the control, docs.js carries the long form.
     *
     * A `.short` states what the thing IS and then corrects the misconception
     * the reader is about to have. A gloss that only restates the word is the
     * failure this block exists to fix.
     *
     * A `.ident` is NEVER translated. It is byte-identical in both dictionaries
     * and TestNoTermIdentifierIsTranslated fails the build otherwise: the whole
     * argument for keeping these words in English is that an operator matches
     * them by eye against psql, ctl and a log line, and a translated column
     * name is one they cannot find. */
    'term.close': 'Close',
    'term.readMore': 'Read the full definition',
    'term.identLabel': 'In the database, in the API and in the logs',
    'term.identNote': 'That name never changes with the language on this page. It is what psql, ctl and a log line say, and matching them by eye is the whole reason this word is not translated.',

    'term.fence.short': 'A number stamped on a lease that says which holder is the current one. Ending a lease raises the device\'s floor above it, so every later call from the old holder is refused — but nothing in this build refuses an ADB command that carries a stale fence.',
    'term.fence.ident': 'farm.leases.fence',
    'term.witness.short': 'On-device proof that the HOLDER is still alive — a marker file its own agent touches — which buys a job that has lost the control plane more room before the reaper may reclaim it. It is not evidence about the device\'s health, and it is capped, so a wedged agent cannot hold a phone forever.',
    'term.witness.ident': 'farm.leases.witness_at',
    'term.holder.short': 'The name of the process that took the lease, kept for the audit log. Ownership is keyed on the job and not on this name: it confers nothing, and a replacement process re-attaches to the same lease at the same fence.',
    'term.holder.ident': 'farm.leases.holder',
    'term.suspect.short': 'The control plane has not heard a heartbeat from the holder since the lease\'s deadline passed. It does not mean the device is broken and nothing has been released: a heartbeat arriving later heals the lease at the same fence, with no work lost.',
    'term.suspect.ident': 'farm.leases.state',
    'term.protected.short': 'A lease the reaper will never reclaim: only the job or a human ends it. A job either asks for this or gets it for an expected duration over 30 minutes — it is no defence against an operator revoke, and none against max_runtime.',
    'term.protected.ident': 'farm.leases.protected',
    'term.guards.short': 'The two things a job declares to limit what may be done to it while it runs: whether the reaper may reclaim its lease, and the worst disruption recovery may inflict on its device. Both are copied onto the lease when it is acquired, so changing them on the job afterwards does not change a lease that is already live.',
    'term.guards.ident': 'farm.jobs.protected, farm.jobs.disruption_policy',
    'term.tenant.short': 'Who the work belongs to, and the boundary the API enforces: a tenant-scoped caller reads and releases its own tenant\'s leases and nobody else\'s. It does not decide which devices a job can have — the pool does.',
    'term.tenant.ident': 'farm.jobs.tenant_id',
    'term.pool.short': 'The named set of devices a job may be placed on; allocation only ever considers devices whose pool matches the job\'s. A job filed against a pool with nothing free waits — it never borrows from another pool.',
    'term.pool.ident': 'farm.devices.pool_id',
    'term.queue.short': 'One tenant\'s line of waiting jobs, with a priority and a device cap of its own. It decides the order work is placed in, not which device it lands on — the pool decides that.',
    'term.queue.ident': 'farm.jobs.queue_id',
    'term.disruptionPolicy.short': 'The worst thing a job will let recovery do to the device under it: no_disruption, allow_soft_reset or allow_port_power_cycle. A rung that needs more than this is refused outright and written down — never quietly downgraded to a cheaper one.',
    'term.disruptionPolicy.ident': 'farm.jobs.disruption_policy',
    'term.rung.short': 'One step of the recovery ladder, from observing at rung 0 to draining a whole host at rung 8. The ladder climbs from the cheapest rung upward and acts on behalf of the holder: the lease keeps its device, its clock keeps ticking and the fence never moves.',
    'term.rung.ident': 'farm.recovery_tiers.tier',
    'term.blastRadius.short': 'What else a rung disturbs besides the one device: device, power_domain, hub or host. Every live lease inside that radius has to permit the rung, so it is usually a neighbour\'s job rather than your own that refuses a power cycle.',
    'term.blastRadius.ident': 'farm.recovery_tiers.blast_radius',
    'term.quarantine.short': 'An open row saying a fault was found at some scope — device, slot, power domain, hub or host — which stops new allocations there. Live leases are untouched; close it because the fault is fixed, not to clear the screen, or the watchdog will simply open it again.',
    'term.quarantine.ident': 'farm.quarantines',
    'term.drain.short': 'Marking a host so the allocator places no new lease on it. It ends nothing: the leases already running there keep their devices and run to completion, and no part of a drain releases them.',
    'term.drain.ident': 'farm.hosts.admin_state',
    'term.devpath.short': 'The USB position a command is addressed to — bus, hub and port, in the usb:3-1.4 form. Hardware actions use it and never a serial: OEM serials collide, and a command addressed by serial can land on a healthy phone holding somebody else\'s six-hour lease.',
    'term.devpath.ident': 'farm.slots.adb_devpath',
    'term.adminState.short': 'A decision a human or the recovery ladder made about a device: enabled, disabled, quarantined or retired. It is not health — a perfectly healthy phone that is disabled is still refused by the allocator, and the watchdog has no permission to write this column.',
    'term.adminState.ident': 'farm.devices.admin_state',
    'term.slotState.short': 'Whether a physical position may be scheduled: active, disabled or maintenance. A slot is never deleted because the phone in it stopped answering — only the port itself vanishing retires one, and even then the row stays, so a six-month-old lease still resolves to a place a human can walk to.',
    'term.slotState.ident': 'farm.slots.state',

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

    /* Estados vazios. Os identificadores dentro destas frases — farm.v_fleet,
     * expires_at, reclaimable_at, farm.job_steps, ctl job steps — não são
     * traduzidos: são o que se procura no psql e no log. */
    'empty.where': 'De onde isto vem',
    'empty.retry': 'Tentar de novo',
    'empty.openDocs': 'Abrir a documentação',
    'empty.clearFilters': 'Limpar os filtros',
    'empty.failed': 'Não foi possível carregar isto da API.',
    'empty.loading': 'Carregando da API…',
    'empty.loadingDetail': 'Nada é desenhado até o servidor responder.',
    'empty.fleet.filtered': 'Nenhum dispositivo corresponde.',
    'empty.fleet.filteredDetail': 'A fazenda tem dispositivos; os filtros e a busca acima estão escondendo todos eles. Não há nada errado com a fazenda.',
    'empty.fleet.hiddenDetail': 'A API devolveu {n} dispositivos e os filtros acima estão escondendo todos eles.',
    'empty.fleet.none': 'Esta fazenda ainda não tem dispositivos.',
    'empty.fleet.noneDetail': 'Isso não é o mesmo que nada corresponder a um filtro: nada nunca foi registrado. Um dispositivo aparece aqui sozinho, dentro de um minuto, assim que um watchdog rodando em um host o enxergar em um hub que aquele host observa. Nada nesta página cria um.',
    'empty.fleet.where': 'Esta grade mostra cada dispositivo em farm.v_fleet agrupado por host e depois por hub — posição no rack, modelo, saúde, lease e bateria.',
    'empty.leases.filtered': 'Nenhuma lease neste estado.',
    'empty.leases.filteredDetail': 'A fazenda pode muito bem estar segurando leases; nenhuma delas está no estado selecionado acima.',
    'empty.leases.showAll': 'Mostrar todos os estados de lease',
    'empty.leases.none': 'Nada está segurando um dispositivo agora.',
    'empty.leases.noneDetail': 'Uma lease nasce quando um job é colocado em um dispositivo, ou quando uma pessoa adquire uma na mão. Uma fazenda ociosa não tem nenhuma, e isso não diz nada sobre a saúde de nenhum dispositivo.',
    'empty.leases.openJobs': 'Ir para Jobs',
    'empty.leases.where': 'Cada lease viva aparece aqui com seu fence, holder, job, e os dois instantes calculados pelo servidor que importam: expires_at (quando ela vira suspect) e reclaimable_at (o mais cedo que o reaper pode agir).',
    'empty.jobs.filtered': 'Nenhum job neste estado.',
    'empty.jobs.filteredDetail': 'Existem jobs; nenhum deles está no estado selecionado acima.',
    'empty.jobs.showAll': 'Mostrar todos os estados',
    'empty.jobs.none': 'Nenhum job.',
    'empty.jobs.noneDetail': 'Um job entra na fila no instante em que é submetido, e então o escalonador adquire uma lease em um dispositivo que o selector dele casar. Submeter um job não reserva um dispositivo; quem reserva é o escalonador, e ele pode esperar.',
    'empty.jobs.submit': 'Submeter um job',
    'empty.steps.none': 'Nenhum passo para esta tentativa.',
    'empty.steps.noneDetail': 'O runner escreve uma linha por passo conforme executa cada um. Um job que ainda não foi colocado em um dispositivo não tem nenhuma — isto é um job esperando, não um job falhando.',
    'empty.steps.otherAttempts': 'Este job tem passos, sim, arquivados sob a tentativa {attempts}, e nenhum sob a que está selecionada acima. Uma nova tentativa escreve um conjunto novo de linhas em vez de somar às antigas.',
    'empty.steps.showAll': 'Mostrar todas as tentativas',
    'empty.steps.recheck': 'Ver de novo',
    'empty.steps.where': 'As linhas vêm de farm.job_steps, uma por passo por tentativa, e são as mesmas que ctl job steps imprime.',
    'empty.steps.noJob': 'Nenhum job selecionado.',
    'empty.steps.noJobDetail': 'Aperte Steps em um job acima para ver em que passo ele está, o que ele imprimiu e por que parou.',
    'empty.steps.failed': 'Não foi possível ler os passos desse job.',
    'empty.steps.loading': 'Carregando o log de passos…',
    'empty.tiers.none': 'A escada está vazia.',
    'empty.tiers.noneDetail': 'Uma fazenda cujas migrations rodaram tem nove degraus aqui. Nenhum degrau significa que o schema de recuperação nunca foi semeado, então o watchdog não tem o que subir e não vai recuperar nada sozinho.',
    'empty.tiers.where': 'As linhas 0 a 8 de farm.recovery_tiers aparecem aqui — nome, raio de destruição, a disruption policy de lease que cada degrau exige, seu cooldown e seu orçamento por hora.',
    'empty.attempts.none': 'Nenhuma tentativa de recuperação registrada.',
    'empty.attempts.noneDetail': 'O watchdog escreve uma linha aqui toda vez que sobe um degrau, e um degrau recusado é registrado com o motivo da recusa. Uma lista vazia é boa notícia, não um recurso faltando.',
    'empty.quarantines.none': 'Nenhuma quarentena aberta.',
    'empty.quarantines.noneDetail': 'Uma quarentena abre quando a escada desiste de um dispositivo, slot, hub ou host e pede uma pessoa. Nenhuma aberta significa que a escada não desistiu de nada.',
    'empty.bulk.none': 'Nenhuma execução em massa ainda.',
    'empty.bulk.noneDetail': 'Uma execução manda um comando para cada dispositivo que o selector casar, alguns por hub de cada vez para que um domínio de energia não caia, e devolve cada resposta abaixo.',
    'empty.bulk.start': 'Iniciar uma execução',
    'empty.bulk.noRun': 'Nenhuma execução selecionada.',
    'empty.bulk.noRunDetail': 'Escolha uma execução acima para acompanhar os resultados por dispositivo.',
    'empty.bulk.loadingRun': 'Carregando essa execução…',
    'empty.bulk.loadingRunDetail': 'Os resultados por dispositivo aparecem conforme a API os informa.',
    'empty.bulk.failedRun': 'Não foi possível carregar essa execução.',
    'empty.bulk.noTargets': 'Esta execução ainda não tem alvos.',
    'empty.bulk.noTargetsDetail': 'O selector não casou com nada, ou a execução ainda não expandiu seu conjunto de alvos.',
    'empty.events.none': 'Nenhum evento.',
    'empty.events.noneDetail': 'Cada transição de lease, cada tentativa de recuperação e cada ação de operador cai aqui, da mais recente para a mais antiga, e fica. Uma fazenda que ainda não fez nada não tem nenhum.',
    'empty.events.where': 'farm.events e farm.audit_log são mesclados aqui da linha mais recente para a mais antiga, e uma linha auditada carrega a pessoa que digitou o motivo.',

    /* O cartão da primeira visita, na Frota. Três frases e uma porta. */
    'firstRun.title': 'O que é esta página',
    'firstRun.p1': 'Este é o plano de controle de um rack de dispositivos Android físicos: o que cada um está fazendo, quem o está segurando, e o que o watchdog fez com os que pararam de responder.',
    'firstRun.p2': 'Fleet, Leases e Jobs são o presente — cada dispositivo do rack, quem segura cada um agora, e o trabalho na fila para eles. Recovery, Bulk, Events e Docs são o registro e as ferramentas: o que o watchdog tentou, um comando enviado a muitos dispositivos de uma vez, o histórico auditado de tudo, e cada palavra desta página definida.',
    'firstRun.p3': 'Ela lê as mesmas linhas do PostgreSQL que o ctl lê, então não é uma segunda fonte da verdade — e toda ação que muda alguma coisa pede que você confirme e digite um motivo, que fica guardado.',
    'firstRun.docs': 'Abrir a documentação',
    'firstRun.dismiss': 'Entendi',
    'firstRun.dismissTitle': 'Esconder este cartão neste navegador. Tudo o que ele diz também está em Docs, que fica a uma tecla de distância.',

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

    /* A faixa de resumo da frota, e os dois chips que um card de dispositivo
       pode ter. `filter.liveLease` pertence ao bloco de Filtros acima e está
       aqui porque onze pessoas editam este arquivo ao mesmo tempo, e um único
       ponto de inserção contíguo é a diferença entre um merge e uma manhã. */
    'filter.liveLease': 'em uso (held ou suspect)',

    'fleet.sum.region': 'Resumo da frota',
    'fleet.sum.attention': 'Precisa de atenção',
    'fleet.sum.attentionSub': 'alguém precisa olhar',
    'fleet.sum.attentionHelp': 'Dispositivos cuja saúde não é healthy. Parked e retired não entram: são decisões que alguém tomou, não falhas. Clique para ver só estes; clique de novo para limpar.',
    'fleet.sum.inUse': 'Em uso',
    'fleet.sum.inUseSub': 'com lease de um job ou de uma pessoa',
    'fleet.sum.inUseHelp': 'Dispositivos com lease viva, held ou suspect. Uma lease suspect ainda é uma lease: o dispositivo NÃO está livre. Clique para ver só estes; clique de novo para limpar.',
    'fleet.sum.free': 'Livres',
    'fleet.sum.freeSub': 'ninguém está com eles',
    'fleet.sum.freeHelp': 'Dispositivos sem lease viva. Saúde não tem nada a ver com isso: um dispositivo offline pode estar livre, e um livre pode estar quebrado. Clique para ver só estes; clique de novo para limpar.',
    'fleet.sum.offline': 'Offline',
    'fleet.sum.offlineSub': 'não respondem ao ADB',
    'fleet.sum.offlineHelp': 'Dispositivos cuja saúde é exatamente offline — um subconjunto dos que precisam de atenção, e normalmente é o cabo ou a porta do hub, não o aparelho. Clique para ver só estes; clique de novo para limpar.',
    'fleet.sum.total': 'Dispositivos',
    'fleet.sum.totalSub': 'tudo o que está nesta visão',
    'fleet.sum.totalHelp': 'Todos os dispositivos que o servidor devolveu para esta visão. Clique para limpar os filtros de saúde e de lease.',
    'fleet.sum.allClear': 'Nada precisa de atenção agora.',
    'fleet.sum.wholeFarm': 'Estes cinco números são a fazenda inteira, como o servidor contou.',
    'fleet.sum.filtered': 'Há um filtro ligado: estes cinco números contam os {served} dispositivos que o servidor devolveu para ele, não a fazenda inteira.',
    'fleet.sum.onScreen': 'A grade abaixo está mostrando {shown} deles.',
    'fleet.sum.truncated': 'O servidor limitou esta resposta, então estes cinco números contam só as linhas que voltaram — não a fazenda.',

    'fleet.cond.healthy': 'A última sonda respondeu e o dispositivo pode receber trabalho.',
    'fleet.cond.degraded': 'As sondas estão falhando mas o dispositivo ainda responde. Ele continua podendo receber trabalho; a escada de recuperação está de olho nele.',
    'fleet.cond.offline': 'O ADB não alcança o dispositivo. Suspeite do cabo ou da porta do hub antes do aparelho.',
    'fleet.cond.missing': 'O dispositivo estava registrado nesta porta e não está mais lá.',
    'fleet.cond.unauthorized': 'O dispositivo responde mas não aceitou a chave deste host. Alguém precisa tocar em Permitir no próprio aparelho.',
    'fleet.cond.recovering': 'A escada de recuperação está trabalhando neste dispositivo agora.',
    'fleet.cond.booting': 'O dispositivo está subindo. Espere um momento antes de concluir qualquer coisa.',
    'fleet.cond.quarantined': 'Uma quarentena aberta impede que trabalho novo seja escalonado aqui. Ela não toca em nenhuma lease viva: o que está rodando continua rodando.',
    'fleet.cond.parked': 'Fora de serviço de propósito — um limitador de carga segurando a bateria, ou um operador que escreveu o motivo. Não é falha.',
    'fleet.cond.retired': 'Tirado da fazenda de vez. Não é falha.',
    'fleet.cond.unknown': 'Nada foi registrado sobre este dispositivo ainda, o que não é o mesmo que estar tudo bem.',

    'fleet.avail.free': 'Livre',
    'fleet.avail.inUse': 'Em uso',
    'fleet.avail.suspect': 'Em uso · suspect',
    'fleet.avail.protected': 'Em uso · protegida',
    'fleet.avail.freeHelp': 'Nenhuma lease viva neste dispositivo. Qualquer um pode pegá-lo.',
    'fleet.avail.heldHelp': 'lease state held: quem está com ele manda heartbeat e a lease vai sendo renovada.',
    'fleet.avail.suspectHelp': 'lease state suspect: nenhum heartbeat de quem está com ele. O dispositivo NÃO foi liberado e o job pode estar rodando bem.',
    'fleet.avail.protectedHelp': 'Uma lease protegida. O coletor nunca a recolhe: uma lease termina quando o job diz, quando um prazo que a pessoa escreveu vence, ou quando alguém a toma de volta.',
    'fleet.avail.freeLine': 'Ninguém está com ele agora.',
    'fleet.avail.heldLine': '{holder} está usando.',
    'fleet.avail.suspectLine': '{holder} está usando, mas parou de responder.',
    'fleet.avail.suspectProtectedLine': '{holder} está usando e parou de responder. A lease é protegida, então só o job ou uma pessoa encerra.',
    'fleet.avail.protectedLine': '{holder} está usando. Só o job ou uma pessoa encerra.',
    'fleet.avail.someoneElse': 'Outro inquilino',

    'fleet.flag.label': 'Também vale para este dispositivo:',
    'fleet.flag.open': 'Abra o dispositivo para ver por inteiro.',
    'fleet.flag.quarantine': 'quarentena aberta — {reason}',
    'fleet.flag.health': 'a coluna health diz {health}',
    'fleet.flag.dupSerial': 'este serial ADB não é único na fazenda, então enderece este dispositivo só pelo devpath',
    'fleet.flag.adminState': 'admin_state é {state} em vez de enabled, então nenhum trabalho novo é escalonado aqui',

    'fleet.tile.unknownModel': 'modelo desconhecido',
    'fleet.tile.unslottedHelp': 'Este dispositivo não tem rótulo rack_slot, então ninguém pode ser mandado até ele. O caminho USB é tudo o que existe.',
    'fleet.tile.battery': 'Bateria',
    'fleet.tile.noBattery': 'Nenhum nível de bateria foi reportado para este dispositivo.',

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

    /* A frota como tabela: o seletor de modo, os títulos de coluna, e as palavras
     * que um leitor de tela ouve onde um leitor que enxerga vê uma seta.
     *
     * O TÍTULO da coluna é interface e é traduzido. Nada do que a célula contém
     * é: posição no rack, modelo, host, caminho do hub e valor de saúde são o
     * que a API disse, e o operador confere isso a olho contra uma sessão psql
     * e contra uma linha de log. */
    'fleet.view': 'Como mostrar a frota',
    'fleet.viewCards': 'Cartões',
    'fleet.viewTable': 'Tabela',
    'fleet.viewCardsWhy': 'Um cartão por dispositivo, agrupado por host e depois por hub — a ordem em que alguém caminha pela sala.',
    'fleet.viewTableWhy': 'Uma linha por dispositivo, ordenável por qualquer coluna. É o modo que continua legível numa fazenda grande.',
    'fleet.col.slot': 'Posição no rack',
    'fleet.col.model': 'Modelo',
    'fleet.col.where': 'Host / hub',
    'fleet.col.condition': 'Condição',
    'fleet.col.availability': 'Disponibilidade',
    'fleet.col.battery': 'Bateria',
    'fleet.col.lastSeen': 'Visto por último',
    'fleet.openDevice': 'Abrir {device}',
    'fleet.unslotted': 'sem posição',
    'fleet.unslottedWhy': 'Este dispositivo não tem rótulo rack_slot, então ninguém pode ser informado até onde caminhar para alcançá-lo.',
    'fleet.unknownModel': 'modelo não informado',
    'fleet.dupSerial': 'serial repetido',
    'fleet.dupSerialWhy': 'Este serial ADB não é único nesta fazenda, então um comando endereçado por serial pode chegar a qualquer um dos dois aparelhos. Enderece este por devpath.',
    'fleet.openQuarantine': 'um registro de quarentena aberto, sem motivo anotado',
    'fleet.noHost': 'nenhum host registrado',
    'fleet.noHub': 'nenhum hub registrado',
    'table.sortable': 'não ordenado por esta coluna; acione para ordenar por ela',
    'table.sortedAsc': 'ordenado por esta coluna, crescente; acione para inverter',
    'table.sortedDesc': 'ordenado por esta coluna, decrescente; acione para inverter',

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

    /* A folha do dispositivo — unidade 6.
     *
     * As frases abaixo são o que um operador lê no lugar de um uuid. As chaves
     * `device.f.*` são a metade HUMANA de cada linha; a metade da máquina — o
     * nome da coluna — é impressa embaixo por kv() e nunca é traduzida, porque
     * é o que o banco, o `ctl` e a linha de log dizem. */
    'device.tab.overview': 'Visão geral',
    'device.tab.health': 'Saúde',
    'device.tab.lease': 'Lease',
    'device.tab.screen': 'Tela',
    'device.tab.command': 'Comando',
    'device.tab.raw': 'Cru',
    'device.tabsLabel': 'Seções do dispositivo',
    'device.unnamedModel': 'Nenhum modelo informado por este dispositivo',
    'device.loadingDetail': 'Buscando {path} na API.',
    'device.fetchFailed': 'Não foi possível buscar o detalhe do dispositivo ({err}); mostrando a linha da frota no lugar.',

    /* A linha de estado. Saúde e lease são independentes, então cada frase aqui
     * diz de qual das duas está falando. Um dispositivo offline pode continuar
     * com lease, e um saudável pode estar ocioso. */
    'device.say.freeHealthy': 'Saudável e livre. Ninguém está usando este dispositivo.',
    'device.say.freeFault': 'Livre, e a saúde diz {health}. Ninguém está usando este dispositivo; se ele consegue rodar um job é outra pergunta.',
    'device.say.freeOffService': 'Livre, e fora de serviço de propósito ({health}). Ninguém está usando, e ninguém deveria.',
    'device.say.heldBySince': 'Em uso por {holder} desde {since}.',
    'device.say.heldBy': 'Em uso por {holder}.',
    'device.say.heldSince': 'Em uso desde {since}; a lease não registra quem segura.',
    'device.say.heldByJobSince': 'Em uso desde {since}, pelo job {job}; a lease não registra quem segura.',
    'device.say.heldByJob': 'Em uso pelo job {job}; a lease não registra quem segura.',
    'device.say.held': 'Em uso. A lease não registra nem quem segura nem quando começou.',
    'device.say.expires': 'Expira em {when}.',
    'device.say.expiryPassed': 'O prazo dela passou há {when}.',
    'device.say.noExpiry': 'Nenhum prazo registrado, então só o job ou uma pessoa encerra esta lease.',
    'device.say.suspectNote': 'Nenhum heartbeat chegou de quem segura a lease. O dispositivo não foi liberado, e um heartbeat que chegue depois cura a lease no mesmo fence.',
    'device.say.protectedNote': 'A lease é protegida: nada a recupera automaticamente, e uma pessoa é acionada no lugar.',

    'device.where': 'Onde ele está',
    'device.where.full': 'Slot de rack {slot}, no host {host}, atrás do hub {hub}.',
    'device.where.noSlot': 'No host {host}, atrás do hub {hub}. Nenhum slot de rack registrado, então este aparelho não tem endereço físico anotado.',
    'device.where.noHub': 'Slot de rack {slot}, no host {host}. Nenhum hub registrado.',
    'device.where.hostOnly': 'No host {host}. Nem slot de rack nem hub registrados.',
    'device.where.slotOnly': 'Slot de rack {slot}. Nenhum host registrado, o que é um dispositivo que esta fazenda não alcança.',
    'device.where.noHost': 'Slot de rack {slot}, atrás do hub {hub}. Nenhum host registrado, o que é um dispositivo que esta fazenda não alcança.',
    'device.where.hubOnly': 'Atrás do hub {hub}. Nem slot de rack nem host registrados, e sem host esta fazenda não alcança ele.',
    'device.where.nothing': 'Nenhum lugar registrado: sem slot de rack, sem host, sem hub.',
    'device.availability': 'Dá para usar',

    'device.identifiers': 'Identificadores',
    'device.identifiersNote': 'Os textos pelos quais este dispositivo é conhecido — no banco, no ADB e numa linha de log. Nada aqui muda o comportamento dele; estão aqui para serem conferidos e copiados.',
    'device.copy': 'Copiar',
    'device.copied': 'Copiado',
    'device.copyFailed': 'Este navegador recusou a área de transferência; selecione o texto e copie.',
    'device.serialAmbiguous': 'não é único',
    'device.serialAmbiguousWhy': 'Mais de um dispositivo nesta fazenda informa este serial ADB, então o serial sozinho não endereça este aparelho.',

    'device.healthNote': 'Saúde é o que a última verificação viu. Ela não diz nada sobre quem segura o dispositivo: um dispositivo offline pode continuar com lease, e um saudável pode estar ocioso.',
    'device.leaseNote': 'Uma lease termina quando o job diz, quando um prazo que a pessoa escreveu se esgota, ou quando alguém a toma de volta. Nada nesta tela encerra uma por conta própria.',
    'device.screenNote': 'Uma sessão custa três transportes ADB e um codificador de hardware no aparelho, então ela começa quando você pede. Fechar esta folha para a sessão.',
    'device.rawNote': 'Exatamente o que a API devolveu para este dispositivo, antes desta página interpretar qualquer coisa.',
    'device.noQuarantine': 'nenhuma',
    'device.revoke': 'Revogar lease',
    'device.closeQuarantine': 'Fechar quarentena',

    /* Rótulos das linhas. O nome da coluna ao lado de cada um é impresso por
     * kv() e nunca é traduzido — veja a nota no topo deste arquivo. */
    'device.f.pool': 'Pool',
    'device.f.adminState': 'Estado administrativo',
    'device.f.android': 'Android',
    'device.f.failureScore': 'Pontuação de falha',
    'device.f.farmUID': 'UID da fazenda',
    'device.f.deviceID': 'ID do dispositivo',
    'device.f.serial': 'Serial ADB',
    'device.f.usbPath': 'Caminho USB',
    'device.f.devpath': 'Devpath ADB',
    'device.f.slotID': 'Slot',
    'device.f.slotState': 'Estado do slot',
    'device.f.labels': 'Rótulos',
    'device.f.health': 'Saúde',
    'device.f.healthSince': 'Neste estado desde',
    'device.f.adbState': 'Estado do ADB',
    'device.f.battery': 'Bateria',
    'device.f.batteryTemp': 'Temperatura da bateria',
    'device.f.consecBad': 'Verificações falhas seguidas',
    'device.f.nextRung': 'Próximo degrau de recuperação',
    'device.f.lastSeen': 'Visto pela última vez',
    'device.f.quarantine': 'Quarentena',
    'device.f.leaseState': 'Estado da lease',
    'device.f.leaseID': 'ID da lease',
    'device.f.fence': 'Fence',
    'device.f.job': 'Job',
    'device.f.tenant': 'Tenant',
    'device.f.holder': 'Quem segura',
    'device.f.acquired': 'Adquirida',
    'device.f.expires': 'Expira',
    'device.f.reclaimable': 'Recuperável',

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

    /* O glossário. Dezessete palavras que esta página imprime cruas, cada uma
     * com a frase que quem chega precisa e o nome que a mesma coisa tem no
     * banco. terms.js monta o controle, docs.js carrega a forma longa.
     *
     * Um `.short` diz o que a coisa É e depois corrige o mal-entendido que o
     * leitor está prestes a ter. Uma definição que só repete a palavra é
     * exatamente a falha que este bloco existe para consertar.
     *
     * Um `.ident` NUNCA é traduzido. É byte a byte igual nos dois dicionários e
     * o TestNoTermIdentifierIsTranslated quebra o build se não for: o motivo
     * inteiro de manter estas palavras em inglês é que um operador as compara a
     * olho com o psql, o ctl e uma linha de log, e um nome de coluna traduzido é
     * um nome que ele não acha. */
    'term.close': 'Fechar',
    'term.readMore': 'Ler a definição completa',
    'term.identLabel': 'No banco, na API e nos logs',
    'term.identNote': 'Esse nome não muda com o idioma desta página. É o que o psql, o ctl e uma linha de log dizem, e poder compará-los a olho é o motivo inteiro de esta palavra não ser traduzida.',

    'term.fence.short': 'Um número gravado na lease que diz qual holder é o atual. Terminar uma lease sobe o piso do dispositivo acima dele, então toda chamada posterior do holder antigo é recusada — mas nada neste build recusa um comando ADB que carregue um fence velho.',
    'term.fence.ident': 'farm.leases.fence',
    'term.witness.short': 'Prova no próprio aparelho de que o HOLDER continua vivo — um arquivo marcador que o agente dele toca — e que compra mais folga para um job que perdeu o control plane antes que o reaper possa retomar o dispositivo. Não é evidência sobre a saúde do aparelho, e tem teto, para que um agente travado não segure um telefone para sempre.',
    'term.witness.ident': 'farm.leases.witness_at',
    'term.holder.short': 'O nome do processo que pegou a lease, guardado para o log de auditoria. A posse é chaveada no job e não neste nome: ele não confere nada, e um processo substituto faz reattach na mesma lease e no mesmo fence.',
    'term.holder.ident': 'farm.leases.holder',
    'term.suspect.short': 'O control plane não ouve um heartbeat do holder desde que o prazo da lease passou. Não significa que o dispositivo está quebrado e nada foi liberado: um heartbeat que chegue depois cura a lease no mesmo fence, sem perder trabalho.',
    'term.suspect.ident': 'farm.leases.state',
    'term.protected.short': 'Uma lease que o reaper nunca vai retomar: só o job ou um humano a termina. Um job pede por isso, ou ganha por ter duração esperada acima de 30 minutos — não protege contra um revoke de operador, nem contra o max_runtime.',
    'term.protected.ident': 'farm.leases.protected',
    'term.guards.short': 'As duas coisas que um job declara para limitar o que pode ser feito com ele enquanto roda: se o reaper pode retomar a lease dele, e a pior perturbação que o recovery pode causar no dispositivo. As duas são copiadas para a lease na aquisição, então mudá-las no job depois não muda uma lease que já está viva.',
    'term.guards.ident': 'farm.jobs.protected, farm.jobs.disruption_policy',
    'term.tenant.short': 'De quem é o trabalho, e a fronteira que a API impõe: um chamador com escopo de tenant lê e libera as leases do próprio tenant e de mais ninguém. Não decide quais dispositivos um job pode ter — quem decide é o pool.',
    'term.tenant.ident': 'farm.jobs.tenant_id',
    'term.pool.short': 'O conjunto nomeado de dispositivos em que um job pode ser colocado; a alocação só considera dispositivos cujo pool é o do job. Um job enviado a um pool sem nada livre espera — nunca toma emprestado de outro pool.',
    'term.pool.ident': 'farm.devices.pool_id',
    'term.queue.short': 'A fila de jobs esperando de um tenant, com prioridade e teto de dispositivos próprios. Decide a ordem em que o trabalho é colocado, não em qual dispositivo ele cai — isso quem decide é o pool.',
    'term.queue.ident': 'farm.jobs.queue_id',
    'term.disruptionPolicy.short': 'O pior que um job deixa o recovery fazer com o dispositivo embaixo dele: no_disruption, allow_soft_reset ou allow_port_power_cycle. Uma rung que precisa de mais do que isso é recusada de vez e registrada — nunca rebaixada em silêncio para uma mais barata.',
    'term.disruptionPolicy.ident': 'farm.jobs.disruption_policy',
    'term.rung.short': 'Um degrau da escada de recovery, de observar na rung 0 até drenar um host inteiro na rung 8. A escada sobe do degrau mais barato para cima e age em nome do holder: a lease mantém o dispositivo, o relógio dela continua correndo e o fence nunca se move.',
    'term.rung.ident': 'farm.recovery_tiers.tier',
    'term.blastRadius.short': 'O que mais uma rung perturba além daquele dispositivo: device, power_domain, hub ou host. Toda lease viva dentro desse raio precisa permitir a rung, então quem recusa um ciclo de energia costuma ser o job do vizinho, e não o seu.',
    'term.blastRadius.ident': 'farm.recovery_tiers.blast_radius',
    'term.quarantine.short': 'Uma linha aberta dizendo que uma falha foi encontrada em algum escopo — device, slot, power domain, hub ou host — e que impede novas alocações ali. Leases vivas não são tocadas; feche porque a falha foi corrigida, e não para limpar a tela, ou o watchdog simplesmente abre de novo.',
    'term.quarantine.ident': 'farm.quarantines',
    'term.drain.short': 'Marcar um host para que o alocador não coloque nenhuma lease nova nele. Não termina nada: as leases que já rodam ali mantêm seus dispositivos e vão até o fim, e nenhuma parte de um drain as libera.',
    'term.drain.ident': 'farm.hosts.admin_state',
    'term.devpath.short': 'A posição USB para a qual um comando é endereçado — barramento, hub e porta, na forma usb:3-1.4. As ações de hardware usam isto e nunca um serial: seriais de fábrica colidem, e um comando endereçado por serial pode cair num telefone saudável que segura a lease de seis horas de outra pessoa.',
    'term.devpath.ident': 'farm.slots.adb_devpath',
    'term.adminState.short': 'Uma decisão que um humano ou a escada de recovery tomou sobre um dispositivo: enabled, disabled, quarantined ou retired. Não é saúde — um telefone perfeitamente saudável que está disabled continua sendo recusado pelo alocador, e o watchdog não tem permissão para escrever nesta coluna.',
    'term.adminState.ident': 'farm.devices.admin_state',
    'term.slotState.short': 'Se uma posição física pode ser escalada: active, disabled ou maintenance. Um slot nunca é apagado porque o telefone nele parou de responder — só a porta em si sumir aposenta um, e mesmo aí a linha fica, para que uma lease de seis meses atrás ainda resolva para um lugar até onde um humano pode caminhar.',
    'term.slotState.ident': 'farm.slots.state',

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
