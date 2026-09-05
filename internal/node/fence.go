package node

// The fence proxy, host side.
//
// internal/fenceproxy decides whether one ADB connection may proceed; this
// file is what puts that decision on the wire. When the agent is given TLS
// material it serves a fenceproxy.Server on an mTLS listener in front of the
// LOCAL ADB server, polls this host's fence floors through the pool it already
// owns, and advertises the proxy — not the ADB server — as the host's
// farm.hosts.adb_endpoint, so that every process in the cluster that dials a
// host dials the proxy.
//
// Two rules from the design document (docs/design/fence-proxy.md) are visible
// in the shape of the code:
//
//   - The proxy learns floors from ONE reader, hostFloors, and that reader has
//     one method. Nothing here can turn a refusal into a lease decision: the
//     agent still imports neither internal/lease nor anything that ends one.
//
//   - A listener that dies is restarted, with backoff, and the advertised
//     endpoint stays the proxy's throughout. A host whose proxy is down is a
//     host that is unreachable, on purpose: the alternative — falling back to
//     the unfenced ADB server — would mean the thing that enforces the fence
//     can be removed by making it crash.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/flaviopadilha/device-farmer/internal/fenceproxy"
)

const (
	// DefaultFenceBackoff is the initial delay before a fence listener that
	// died is bound again. It doubles up to DefaultFenceBackoffMax and resets
	// after a run of fenceHealthyRun, the same shape as the enrollment loop and
	// for the same reason: one bad afternoon must not leave the proxy on its
	// worst-case restart delay for the life of the process.
	DefaultFenceBackoff    = time.Second
	DefaultFenceBackoffMax = 30 * time.Second
	fenceHealthyRun        = time.Minute

	// fenceStopGrace is how long shutdown waits for the accept loop to report
	// back once the listener is closed. A quiet proxy returns at once; a busy
	// one is draining sessions that end with their sockets, and waiting here
	// for a six-hour transfer would hold the rest of the agent's shutdown
	// hostage to it. The process exit ends those sockets either way, and no
	// lease is affected by that.
	fenceStopGrace = 5 * time.Second

	// certRecheck bounds how often the certificate files are stat'd. Once a
	// second is invisible next to a TLS handshake and means a rotation lands
	// within a second of the file write.
	certRecheck = time.Second
)

// FenceConfig turns on the host-side fence proxy: an mTLS listener in front of
// Config.ADBEndpoint that admits a connection only while the fence it presents
// is at or above this host's farm.devices.fence_floor.
//
// The zero value is OFF, and off means nothing changes: the agent advertises
// ADBEndpoint and no fence is enforced at the device. CertFile, KeyFile and
// CAFile together turn it on; New refuses one or two of the three.
type FenceConfig struct {
	// CertFile and KeyFile are the proxy's own certificate and key. They are
	// re-read when the files change, so a rotation is a file write and never
	// a restart: restarting the proxy would sever every live connection on
	// the host, and a PKI operation must not be a data-path event.
	CertFile string
	KeyFile  string

	// CAFile holds the roots a client certificate must chain to. A client's
	// class — lease, maintenance, enroll — is read from its certificate's
	// farm://<class>/ URI SAN and never from anything the client says.
	CAFile string

	// Listen is the proxy's bind address. Required when the proxy is on.
	Listen string

	// Advertise is what the agent writes to farm.hosts.adb_endpoint instead
	// of ADBEndpoint while the proxy is on, so every reader of that column —
	// the jobrunner, the watchdog, the recovery ladder, the API — dials the
	// proxy. It must be reachable from the cluster. Empty means derive it from
	// Listen; see AdvertiseAddr.
	Advertise string

	// PollInterval is how often the proxy re-reads this host's floors: one
	// query per host per interval, never per connection. Defaults to
	// fenceproxy.DefaultPollInterval.
	PollInterval time.Duration

	// MaxStaleness is how old the last successful read may be and still admit
	// a NEW connection. It is FARM_NODE_SELF_FENCE_TIMEOUT, which
	// config.Validate keeps FARM_SLOT_REARM above. Defaults to
	// fenceproxy.DefaultMaxStaleness.
	MaxStaleness time.Duration

	// Floors overrides where the floors are read from. Nil means this host's
	// rows in farm.devices through Config.Pool; a test supplies a value.
	Floors fenceproxy.FenceSource
}

// Enabled reports whether all three PEM paths are set.
func (f FenceConfig) Enabled() bool {
	return f.CertFile != "" && f.KeyFile != "" && f.CAFile != ""
}

// fenceState is everything the proxy needs that outlives one listener.
type fenceState struct {
	cfg     FenceConfig
	cache   *fenceproxy.Cache
	tlsConf *tls.Config
	src     fenceproxy.FenceSource

	// listen is tls.Listen unless a test substitutes a listener that fails.
	listen  func(addr string) (net.Listener, error)
	backoff time.Duration

	mu   sync.Mutex
	addr string // the bound listener's address; empty while not listening
}

// newFenceState validates the proxy's configuration and loads its material.
// It returns nil, nil when the proxy is off.
func newFenceState(cfg Config, log *slog.Logger) (*fenceState, error) {
	f := cfg.Fence
	set := 0
	for _, p := range []string{f.CertFile, f.KeyFile, f.CAFile} {
		if p != "" {
			set++
		}
	}
	if set == 0 {
		return nil, nil
	}
	if set < 3 {
		return nil, fmt.Errorf("node: Config.Fence needs CertFile, KeyFile and CAFile "+
			"together and has %d of the three; a proxy with a certificate and no client "+
			"CA would admit anyone, and one with a CA and no certificate cannot listen", set)
	}
	if f.Listen == "" {
		return nil, errors.New("node: Config.Fence.Listen is required when the fence proxy is on")
	}

	certs, err := newCertReloader(f.CertFile, f.KeyFile, log)
	if err != nil {
		return nil, fmt.Errorf("node: fence proxy certificate: %w", err)
	}
	pem, err := os.ReadFile(f.CAFile)
	if err != nil {
		return nil, fmt.Errorf("node: fence proxy client CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("node: fence proxy client CA %s holds no PEM certificate; the "+
			"proxy would trust no client and refuse every connection to this host", f.CAFile)
	}

	if f.Advertise == "" {
		adv, err := AdvertiseAddr(f.Listen, cfg.ADBEndpoint, net.InterfaceAddrs)
		if err != nil {
			return nil, err
		}
		f.Advertise = adv
	}
	if f.PollInterval <= 0 {
		f.PollInterval = fenceproxy.DefaultPollInterval
	}
	if f.MaxStaleness <= 0 {
		f.MaxStaleness = fenceproxy.DefaultMaxStaleness
	}
	src := f.Floors
	if src == nil {
		src = hostFloors{pool: cfg.Pool, hostID: cfg.HostID, timeout: cfg.CallTimeout}
	}

	st := &fenceState{
		cfg:     f,
		cache:   fenceproxy.NewCache(nil),
		tlsConf: fenceproxy.ServerTLSConfig(certs.get, roots),
		src:     src,
		backoff: DefaultFenceBackoff,
	}
	st.listen = func(addr string) (net.Listener, error) {
		return tls.Listen("tcp", addr, st.tlsConf)
	}
	return st, nil
}

func (f *fenceState) setAddr(addr string) {
	f.mu.Lock()
	f.addr = addr
	f.mu.Unlock()
}

// FenceAddr reports the address the fence proxy is listening on, or "" while
// it is off or between listeners.
func (a *Agent) FenceAddr() string {
	if a.fence == nil {
		return ""
	}
	a.fence.mu.Lock()
	defer a.fence.mu.Unlock()
	return a.fence.addr
}

// advertisedEndpoint is what other processes dial to reach this host's
// devices: the fence proxy when it is on, the ADB server when it is not.
//
// It is the proxy's address even while the proxy is down. The endpoint is a
// promise about WHERE the fence is enforced, not a liveness report, and the
// one thing it must never do is point at the unfenced server because the
// fenced one stopped. Clients fail closed and retry; the listener loop brings
// the proxy back.
func (a *Agent) advertisedEndpoint() string {
	if a.fence != nil {
		return a.fence.cfg.Advertise
	}
	return a.cfg.ADBEndpoint
}

// AdvertiseAddr decides what a proxied host writes to farm.hosts.adb_endpoint
// when nothing was configured for it (FARM_FENCE_ADVERTISE unset).
//
// The listen address is the starting point and only its host part is in
// question: a proxy bound to a specific address advertises exactly that, even
// loopback — an operator who bound the listener to 127.0.0.1 has a single-box
// farm and meant it. A proxy bound to every interface (":5038", "0.0.0.0:5038",
// "[::]:5038") has to name one, in this order:
//
//   - the host part of adbEndpoint, when it is a specific, non-loopback
//     address. The operator has already written this machine's routable
//     address there — deploy/helm/README.md tells them to — and the cluster
//     reaches the ADB server through it today;
//   - otherwise the first global-unicast address of a local interface, IPv4
//     before IPv6, which is what "this machine's IP" means to whoever reads
//     the row.
//
// Loopback is never CHOSEN: written into farm.hosts by inference it is an
// endpoint no other machine can dial, on a row that reads as a healthy host.
// When nothing qualifies the error says to set the variable.
func AdvertiseAddr(listen, adbEndpoint string, interfaceAddrs func() ([]net.Addr, error)) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("node: fence proxy listen address %q: %w", listen, err)
	}
	if host != "" && !isUnspecified(host) {
		return listen, nil
	}

	if h, _, err := net.SplitHostPort(adbEndpoint); err == nil && h != "" && !isUnspecified(h) && !isLoopback(h) {
		return net.JoinHostPort(h, port), nil
	}

	addrs, err := interfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("node: fence proxy: listing this host's addresses to advertise "+
			"one: %w; set FARM_FENCE_ADVERTISE", err)
	}
	var v6 net.IP
	for _, addr := range addrs {
		ipn, ok := addr.(*net.IPNet)
		if !ok || !ipn.IP.IsGlobalUnicast() {
			continue
		}
		if ip4 := ipn.IP.To4(); ip4 != nil {
			return net.JoinHostPort(ip4.String(), port), nil
		}
		if v6 == nil {
			v6 = ipn.IP
		}
	}
	if v6 != nil {
		return net.JoinHostPort(v6.String(), port), nil
	}
	return "", fmt.Errorf("node: fence proxy listens on %q and this host has no address "+
		"other machines could reach it at; set FARM_FENCE_ADVERTISE to the host:port the "+
		"cluster dials", listen)
}

func isUnspecified(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// The certificate, re-read on rotation
// ---------------------------------------------------------------------------

// certReloader hands the TLS stack the proxy's certificate and picks up a
// rotation from disk without a restart. Design document, section 9.2: a
// restart severs every live connection on the host, so a PKI operation must
// not be one.
type certReloader struct {
	certFile, keyFile string
	log               *slog.Logger
	recheck           time.Duration

	mu      sync.Mutex
	cert    *tls.Certificate
	certMod time.Time
	keyMod  time.Time
	checked time.Time
}

func newCertReloader(certFile, keyFile string, log *slog.Logger) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, log: log, recheck: certRecheck}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// load reads both files. The modification times are taken BEFORE the read, so
// a write that lands between the stat and the read is seen again on the next
// check rather than missed.
func (r *certReloader) load() error {
	cs, err := os.Stat(r.certFile)
	if err != nil {
		return err
	}
	ks, err := os.Stat(r.keyFile)
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}
	r.cert, r.certMod, r.keyMod = &cert, cs.ModTime(), ks.ModTime()
	return nil
}

func (r *certReloader) changed() bool {
	cs, err := os.Stat(r.certFile)
	if err != nil {
		return false
	}
	ks, err := os.Stat(r.keyFile)
	if err != nil {
		return false
	}
	return !cs.ModTime().Equal(r.certMod) || !ks.ModTime().Equal(r.keyMod)
}

// get is tls.Config.GetCertificate. A rotation that does not parse keeps the
// previous certificate in service and is logged: a broken file on disk must
// not take a working listener down, and the operator who wrote it is the one
// who needs to hear about it.
func (r *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := time.Now(); now.Sub(r.checked) >= r.recheck {
		r.checked = now
		if r.changed() {
			if err := r.load(); err != nil {
				r.log.Warn("the fence proxy's certificate files changed and do not load; "+
					"the previous certificate stays in service", "err", err)
			} else {
				r.log.Info("fence proxy certificate reloaded from disk")
			}
		}
	}
	return r.cert, nil
}

// ---------------------------------------------------------------------------
// The loop
// ---------------------------------------------------------------------------

// fenceLoop serves the fence proxy until ctx ends.
//
// Two goroutines: the poller, which is the proxy's only channel to the control
// plane and refreshes the floors on a timer, and the accept loop. The accept
// loop is restarted with backoff whenever its listener dies, because this
// process sits on the path of every ADB byte on the host and the advertised
// endpoint is the proxy's: while it is down nothing on this host is reachable,
// and that is the intended failure. See advertisedEndpoint.
func (a *Agent) fenceLoop(ctx context.Context) error {
	if a.fence == nil {
		<-ctx.Done()
		return nil
	}
	f := a.fence

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.cache.Poll(ctx, gaugedSource{f.src}, f.cfg.PollInterval, a.log)
	}()
	defer wg.Wait()

	pol := fenceproxy.DefaultPolicy()
	pol.MaxStaleness = f.cfg.MaxStaleness
	srv := &fenceproxy.Server{
		Cache:  f.cache,
		Policy: pol,
		DialUpstream: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{Timeout: a.cfg.CallTimeout}).DialContext(ctx, "tcp", a.cfg.ADBEndpoint)
		},
		// The accepted connection is wrapped to count it; the handshake needs
		// the *tls.Conn underneath.
		Identify: func(c net.Conn) (fenceproxy.Identity, error) {
			if cc, ok := c.(*countedConn); ok {
				c = cc.Conn
			}
			return fenceproxy.IdentityFromConn(c)
		},
		Log: a.log,
	}

	backoff := f.backoff
	for {
		started := time.Now()
		err := a.serveFence(ctx, srv)
		if ctx.Err() != nil {
			return nil
		}
		fenceProxyRestarts.Inc()
		if time.Since(started) >= fenceHealthyRun {
			backoff = f.backoff
		}
		a.log.Error("the fence proxy listener stopped; restarting it. Until it is back nothing "+
			"on this host is reachable: the advertised endpoint is the proxy, and it stays the "+
			"proxy on purpose rather than falling back to the unfenced adb server",
			"err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > DefaultFenceBackoffMax {
			backoff = DefaultFenceBackoffMax
		}
	}
}

// serveFence binds one listener and serves it until it dies or ctx ends.
func (a *Agent) serveFence(ctx context.Context, srv *fenceproxy.Server) error {
	f := a.fence
	ln, err := f.listen(f.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", f.cfg.Listen, err)
	}
	addr := ln.Addr().String()
	f.setAddr(addr)
	fenceProxyUp.Set(1)
	defer func() {
		f.setAddr("")
		fenceProxyUp.Set(0)
	}()
	a.log.Info("fence proxy listening", "addr", addr, "advertised", f.cfg.Advertise,
		"upstream", a.cfg.ADBEndpoint, "poll_every", f.cfg.PollInterval,
		"max_staleness", f.cfg.MaxStaleness)

	// Serve waits for every session before it returns, and a session lasts as
	// long as its transfer. Watching the listener directly is what lets a dead
	// accept loop be replaced NOW while the old sessions drain behind it.
	wl := &watchedListener{Listener: ln, failed: make(chan error, 1)}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, wl) }()

	select {
	case err := <-wl.failed:
		return fmt.Errorf("accept on %s: %w", addr, err)
	case err := <-done:
		if ctx.Err() != nil {
			return nil
		}
		return err
	case <-ctx.Done():
		// Serve closes the listener itself when ctx ends.
		select {
		case <-done:
		case <-time.After(fenceStopGrace):
			a.log.Info("the fence proxy stopped accepting; connections still open through it "+
				"end with this process, and no lease is affected by that",
				"open", wl.live.Load())
		}
		return nil
	}
}

// watchedListener reports the first accept failure on a channel, before it is
// returned to Serve, and counts the connections it handed out.
type watchedListener struct {
	net.Listener
	failed chan error
	once   sync.Once
	live   atomic.Int64
}

func (l *watchedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		l.once.Do(func() { l.failed <- err })
		return nil, err
	}
	l.live.Add(1)
	fenceProxyConnections.Inc()
	return &countedConn{Conn: c, l: l}, nil
}

type countedConn struct {
	net.Conn
	l    *watchedListener
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() {
		c.l.live.Add(-1)
		fenceProxyConnections.Dec()
	})
	return c.Conn.Close()
}

// gaugedSource records how many positions the last successful read covered.
// It is observational; the snapshot passes through untouched.
type gaugedSource struct{ inner fenceproxy.FenceSource }

func (g gaugedSource) Floors(ctx context.Context) (fenceproxy.Snapshot, error) {
	snap, err := g.inner.Floors(ctx)
	if err == nil {
		fencePositions.Set(float64(len(snap.Floors)))
	}
	return snap, err
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var (
	fenceProxyUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "node", Name: "fence_proxy_up",
		Help: "1 while the fence proxy is listening. 0 on a host that advertises the " +
			"proxy is a host nothing can reach until the listener is back; 0 on a host " +
			"with no TLS material configured is the proxy being off.",
	})

	fenceProxyRestarts = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "farm", Subsystem: "node", Name: "fence_proxy_restarts_total",
		Help: "Times the fence proxy listener died or could not bind and was retried. " +
			"Each one is a window in which this host refused every connection.",
	})

	fenceProxyConnections = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "node", Name: "fence_proxy_connections",
		Help: "Client connections currently open through the fence proxy, admitted or " +
			"still being decided.",
	})

	fencePositions = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "farm", Subsystem: "node", Name: "fence_positions",
		Help: "Positions on this host whose fence floor the proxy read in its last " +
			"successful poll. A lease-class connection to a position outside this " +
			"set is refused as unknown, never as fenced.",
	})
)
