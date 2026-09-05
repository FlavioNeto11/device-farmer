// Package ctl is the operator command line for a device farm.
//
// # Everything goes through the API
//
// ctl opens no database connection and holds no credential Postgres would
// accept. Every command below is an HTTP request against the same /api/v1
// surface the dashboard and any SDK client use.
//
// That is a constraint, not an accident of layering. An operator tool with a
// private path into the schema is a tool whose behaviour no other client can
// reproduce, and — worse — a tool whose refusals can be walked around by the
// one person most likely to be in a hurry at three in the morning. "This
// device holds a live lease; running a command on it can corrupt that job's
// run" is a sentence the API says. A ctl that could reach past it to an UPDATE
// would eventually be used to. So ctl cannot develop privileges the API does
// not have, and a ctl that works is evidence the API works.
//
// The one exception is `ctl validate`, which parses a spec with
// internal/jobspec and never leaves the machine. Validating locally is not a
// private privilege: it is the same library the server runs, and refusing to
// submit a spec that cannot be valid saves a round trip without deciding
// anything the server would have decided differently.
//
// # Destructive commands
//
// A command that can disturb hardware somebody else is using — cancel, revoke,
// drain, exec, bulk — requires --reason, prints exactly what it is about to
// disturb (rack positions first), and then asks. With no terminal on stdin
// there is nobody to ask, so it refuses unless --yes was passed. The reason is
// not decoration: it lands in farm.audit_log next to the operator's name, and
// six weeks later it is the only record of why a job's device went away.
//
// Nothing in this package ends a lease as a side effect of a transport
// failure. `ctl lease revoke` ends one because a human typed it and said why;
// `ctl job cancel` ends one because the job is over. A timeout talking to the
// API, a device that will not answer a shell command, a stream that drops — all
// are reported and none release anything.
//
// # Exit codes
//
//	0  the command did what it said
//	1  it failed
//	2  the invocation was wrong
//	3  the remote REFUSED the action (HTTP 409)
//	4  the run completed and at least one target in it failed
//
// 3 is separate from 1 because a refusal is an answer, not a crash. "no
// healthy device matched", "this job is already terminal", "this power cycle
// would disturb a live lease" are the control plane working correctly. A
// script that retries on 1 and stops on 3 is a script that does not fight the
// farm.
//
// 4 is separate from 1 because "nine of sixty phones errored" and "could not
// reach the API" are different problems with different next steps, and a
// script that could not tell them apart would re-run a fleet-wide command to
// recover from a transport blip. A partial outcome is a completed run: the
// server has every target's row, and nothing about a failed target released
// anything.
//
// # Configuration
//
//	FARM_API_URL     base URL of the control plane API (config.EnvAPIBaseURL)
//	FARM_API_TOKEN   bearer token, optional; --token overrides it
package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

// EnvAPIToken names the environment variable holding the bearer token.
//
// It lives here rather than in internal/config because it is a credential: a
// token in the Config struct would be one field away from appearing in
// Summary(), in a log line at startup, or in a panic dump. ctl reads it,
// attaches it to a request header, and never stores it anywhere else.
const EnvAPIToken = "FARM_API_TOKEN"

// apiPrefix is the versioned root every route below hangs from.
const apiPrefix = "/api/v1"

// defaultTimeout bounds an ordinary request. The long-lived ones — the event
// stream, an artifact upload, a bulk run being followed — deliberately do not
// use it; see Client.
const defaultTimeout = 30 * time.Second

// maxErrorBody bounds how much of a failed response is read before giving up
// on parsing it. An error envelope is small; anything larger is a proxy's HTML
// error page, and buffering a megabyte of it helps nobody.
const maxErrorBody = 1 << 20

// maxResponseBody bounds a successful response.
//
// It is generous because the fleet listing legitimately carries five thousand
// devices, and it exists because without it the size of this process is a
// property of what the far end decides to send. A wedged server, a proxy
// looping, or something that is not this API at all would otherwise grow ctl
// until the machine gave out — and the failure would be an OOM kill rather
// than a sentence naming the endpoint.
const maxResponseBody = 64 << 20

var (
	// ErrUsage means the invocation was wrong: exit 2.
	ErrUsage = errors.New("usage")

	// ErrRefused means the remote declined the action with 409: exit 3. It is
	// not a failure of ctl and not a failure of the API — it is the answer.
	ErrRefused = errors.New("the remote refused this action")

	// ErrPartial means the run completed and some of what it addressed
	// failed: exit 4. The invocation worked, the server has the full
	// per-target record, and no lease was touched by any of the failures.
	// Wrap it with the count, so the message says how much of the run this
	// is about.
	ErrPartial = errors.New("partial")
)

// ExitCode maps an error from Run onto this package's documented exit codes.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrUsage):
		return 2
	case errors.Is(err, ErrRefused):
		return 3
	case errors.Is(err, ErrPartial):
		return 4
	default:
		return 1
	}
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client is an HTTP client for the control plane API.
//
// The embedded http.Client carries NO timeout of its own. The event stream is
// open for as long as an operator watches it and an artifact upload is bounded
// by the size of an APK, not by a clock; a client-wide timeout would sever both
// mid-flight. Ordinary requests get a deadline from a per-call context instead,
// which is the difference between "this request is slow" and "every request is
// capped at thirty seconds forever".
type Client struct {
	base    *url.URL
	token   string
	hc      *http.Client
	timeout time.Duration
	agent   string
}

// NewClient builds a client for baseURL. token may be empty, for a farm whose
// API runs without authentication.
func NewClient(baseURL, token string, timeout time.Duration) (*Client, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return nil, fmt.Errorf("%w: no API base URL; set %s or pass --url",
			ErrUsage, config.EnvAPIBaseURL)
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: --url %q is not a URL: %v", ErrUsage, baseURL, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: --url %q names no host", ErrUsage, baseURL)
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		base:    u,
		token:   strings.TrimSpace(token),
		hc:      &http.Client{},
		timeout: timeout,
		agent:   "farmd-ctl",
	}, nil
}

// BaseURL reports the root this client talks to, for the banner a destructive
// command prints before it asks.
func (c *Client) BaseURL() string { return c.base.String() }

// urlFor resolves a route against the base URL. p arrives already
// percent-encoded, because the ids in it went through url.PathEscape.
//
// It deliberately does not use path.Join, which CLEANS what it builds: an id an
// operator typed as ".." would delete the segment before it, so `ctl device ..`
// would quietly GET /api/v1 and — far worse — a destructive POST would be sent
// to a different URL than the one the confirmation named.
//
// It also does not assign to u.Path, which is the DECODED field: url.URL would
// then escape the percent signs already in p a second time, and an id
// containing a space would be looked up as the literal text "a%20b" and
// reported as missing. Parsing p back gives Path and RawPath that agree, which
// is what makes one round of encoding survive to the wire.
func (c *Client) urlFor(p string, q url.Values) string {
	u := *c.base
	joined := strings.TrimSuffix(u.EscapedPath(), "/") + "/" + strings.TrimPrefix(p, "/")
	if ref, err := url.Parse(joined); err == nil {
		u.Path, u.RawPath = ref.Path, ref.RawPath
	} else {
		// p is not a decodable path. Send it as written rather than inventing
		// a different request: the server answers for what was actually asked.
		u.Path, u.RawPath = joined, ""
	}
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// RemoteError is a non-2xx response, decoded from the API's error envelope.
//
// Code is carried separately from Status because two of this API's most
// important answers share a status with unrelated conditions: 409 is both "no
// capacity", an ordinary scheduling outcome, and "this action would disturb a
// live lease", a refusal a human has to read.
type RemoteError struct {
	Status    int
	Code      string
	Message   string
	Detail    json.RawMessage
	Method    string
	Path      string
	RequestID string
}

func (e *RemoteError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	out := fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.Path, e.Status, e.Code, msg)
	if e.RequestID != "" {
		out += " (request " + e.RequestID + ")"
	}
	return out
}

// Is makes a 409 satisfy errors.Is(err, ErrRefused), which is what turns it
// into exit 3 without every call site having to check a status by hand.
func (e *RemoteError) Is(target error) bool {
	return target == ErrRefused && e.Status == http.StatusConflict
}

// remoteError decodes a failed response.
func remoteError(method, p string, resp *http.Response) error {
	out := &RemoteError{
		Status:    resp.StatusCode,
		Code:      "http_" + fmt.Sprint(resp.StatusCode),
		Method:    method,
		Path:      p,
		RequestID: resp.Header.Get("X-Request-Id"),
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))

	var envelope struct {
		Error struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Detail  json.RawMessage `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" {
		out.Code = envelope.Error.Code
		out.Message = envelope.Error.Message
		out.Detail = envelope.Error.Detail
		return out
	}
	// Not the API's envelope: a proxy, a redirect to a login page, the wrong
	// port. Show what actually came back rather than inventing a code.
	out.Message = strings.TrimSpace(clip(string(body), 400))
	if out.Message == "" {
		out.Message = "the response carried no body"
	}
	return out
}

// newRequest builds a request carrying this client's headers.
func (c *Client) newRequest(ctx context.Context, method, p string, q url.Values, body io.Reader, contentType string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.urlFor(p, q), body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, p, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.agent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// roundTrip sends a request and returns the response with its body still open.
// The caller closes it. A non-2xx status is consumed here and returned as a
// *RemoteError.
func (c *Client) roundTrip(req *http.Request, p string) (*http.Response, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, p, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, remoteError(req.Method, p, resp)
	}
	return resp, nil
}

// do issues a request and returns the response with its body still open.
func (c *Client) do(ctx context.Context, method, p string, q url.Values, body io.Reader, contentType string) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, p, q, body, contentType)
	if err != nil {
		return nil, err
	}
	return c.roundTrip(req, p)
}

// readJSON runs a request under the client's ordinary deadline and returns the
// whole body.
func (c *Client) readJSON(ctx context.Context, method, p string, q url.Values, body io.Reader, contentType string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.do(ctx, method, p, q, body, contentType)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return readBounded(resp.Body, method, p)
}

// readBounded reads a response body under maxResponseBody and fails loudly at
// the cap instead of returning a truncated document, which would surface as an
// unexplained JSON syntax error somewhere in the middle of a fleet listing.
func readBounded(r io.Reader, method, p string) (json.RawMessage, error) {
	buf, err := io.ReadAll(io.LimitReader(r, maxResponseBody+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read response: %w", method, p, err)
	}
	if int64(len(buf)) > maxResponseBody {
		return nil, fmt.Errorf("%s %s: the response is larger than %s and was not read; "+
			"narrow the request with a filter or --limit", method, p, bytesCell(maxResponseBody))
	}
	return json.RawMessage(buf), nil
}

// Get reads a JSON resource.
func (c *Client) Get(ctx context.Context, p string, q url.Values) (json.RawMessage, error) {
	return c.readJSON(ctx, http.MethodGet, p, q, nil, "")
}

// Post sends a JSON body and reads a JSON response. A nil body sends "{}",
// which the API decodes into the zero request rather than rejecting.
func (c *Client) Post(ctx context.Context, p string, body any) (json.RawMessage, error) {
	return c.PostQuery(ctx, p, nil, body)
}

// PostQuery is Post with a query string, for the routes whose switches live
// in the URL rather than the body — the artifact sweep reads ?apply and
// ?reason there, because its body is nothing and its dry run must be the
// request a mistyped command sends.
func (c *Client) PostQuery(ctx context.Context, p string, q url.Values, body any) (json.RawMessage, error) {
	payload := []byte("{}")
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request for %s: %w", p, err)
		}
	}
	return c.readJSON(ctx, http.MethodPost, p, q, bytes.NewReader(payload), "application/json")
}

// Delete removes a resource and reads the JSON reply.
func (c *Client) Delete(ctx context.Context, p string, q url.Values) (json.RawMessage, error) {
	return c.readJSON(ctx, http.MethodDelete, p, q, nil, "")
}

// Stream opens a long-lived response — the SSE event stream — with no
// client-side deadline. It ends when the context ends or the server closes.
func (c *Client) Stream(ctx context.Context, p string, q url.Values) (io.ReadCloser, error) {
	req, err := c.newRequest(ctx, http.MethodGet, p, q, nil, "")
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.roundTrip(req, p)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Upload streams a file as the request body.
//
// The artifact IS the body. The API reads kind, name and the declared digest
// from the query string and hands the body straight to the content-addressed
// store, so wrapping the file in a multipart envelope would file the envelope
// as the artifact and give it a hash no spec could ever reference.
//
// Nothing is buffered — an APK is hundreds of megabytes and an OBB can be
// gigabytes, and holding one in memory would make the size of an artifact a
// property of ctl's resident set. size is sent as Content-Length so an upload
// over the server's cap is refused in one round trip rather than after
// streaming the whole thing; a caller that cannot know the size ahead of time
// passes -1 and the body goes out chunked. There is no client-side deadline
// for the same reason the event stream has none: the context is the only clock.
func (c *Client) Upload(ctx context.Context, p string, q url.Values, body io.Reader, size int64) (json.RawMessage, error) {
	req, err := c.newRequest(ctx, http.MethodPost, p, q, body, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	req.ContentLength = size

	resp, err := c.roundTrip(req, p)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return readBounded(resp.Body, http.MethodPost, p)
}

// fetch reads a resource and decodes it, returning the raw body too so a
// command can hand the API's own bytes to -o json instead of re-encoding a
// lossy copy of them.
func fetch[T any](ctx context.Context, c *Client, p string, q url.Values) (T, json.RawMessage, error) {
	var out T
	raw, err := c.Get(ctx, p, q)
	if err != nil {
		return out, nil, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, raw, fmt.Errorf("GET %s: the response did not decode: %w", p, err)
	}
	return out, raw, nil
}

// send posts a body and decodes the reply, keeping the raw bytes for -o json.
func send[T any](ctx context.Context, c *Client, p string, body any) (T, json.RawMessage, error) {
	var out T
	raw, err := c.Post(ctx, p, body)
	if err != nil {
		return out, nil, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, raw, fmt.Errorf("POST %s: the response did not decode: %w", p, err)
	}
	return out, raw, nil
}

// ---------------------------------------------------------------------------
// Invocation
// ---------------------------------------------------------------------------

// session is the process-level context of one ctl invocation: where output
// goes, and whether there is a human on the other end of stdin.
type session struct {
	cfg *config.Config
	out io.Writer
	err io.Writer
	in  io.Reader
	tty bool
}

// env is one command's working context, built after its flags are parsed.
//
// The per-request deadline is not repeated here: it belongs to the Client and
// is applied there, so there is exactly one place that decides how long a
// request may take.
type env struct {
	*session
	client *Client
	out    *Printer
	format Format
	yes    bool
	reason string
}

// globals are the flags every command accepts.
type globals struct {
	url     string
	token   string
	output  string
	timeout time.Duration
	yes     bool
	reason  string
}

// bind registers the global flags on fs. They are registered per command
// rather than once before dispatch so that `ctl fleet --host h1 -o json` works
// with flags on either side of the arguments, and so `ctl help fleet` can show
// the whole set a command accepts.
func (g *globals) bind(fs *flag.FlagSet) {
	fs.StringVar(&g.url, "url", "", "control plane API base URL (default $"+config.EnvAPIBaseURL+")")
	fs.StringVar(&g.token, "token", "", "bearer token (default $"+EnvAPIToken+")")
	fs.StringVar(&g.output, "o", "table", "output format: table or json")
	fs.StringVar(&g.output, "output", "table", "output format: table or json")
	fs.DurationVar(&g.timeout, "timeout", defaultTimeout, "per-request timeout")
}

// bindDestructive adds the two flags a command that disturbs hardware needs.
func (g *globals) bindDestructive(fs *flag.FlagSet) {
	fs.StringVar(&g.reason, "reason", "", "why (required; recorded in farm.audit_log)")
	fs.BoolVar(&g.yes, "yes", false, "skip the confirmation prompt; required when stdin is not a terminal")
}

// open resolves configuration and builds the command's environment.
//
// Precedence is flag, then the loaded configuration, then the environment,
// then the compiled-in default — so a one-off `--url` never has to be exported
// and an exported FARM_API_URL never has to be unset.
func (s *session) open(g *globals) (*env, error) {
	base := strings.TrimSpace(g.url)
	if base == "" && s.cfg != nil {
		base = s.cfg.APIBaseURL
	}
	if base == "" {
		base = strings.TrimSpace(os.Getenv(config.EnvAPIBaseURL))
	}
	if base == "" {
		base = config.DefaultAPIBaseURL
	}

	token := strings.TrimSpace(g.token)
	if token == "" {
		token = strings.TrimSpace(os.Getenv(EnvAPIToken))
	}

	format, err := ParseFormat(g.output)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(base, token, g.timeout)
	if err != nil {
		return nil, err
	}
	return &env{
		session: s,
		client:  client,
		out:     NewPrinter(s.out, format),
		format:  format,
		yes:     g.yes,
		reason:  strings.TrimSpace(g.reason),
	}, nil
}

// warn writes to stderr. Warnings never go to stdout: a truncated listing or a
// failed enrichment has to stay visible when the result is being piped into jq.
func (e *env) warnf(format string, args ...any) {
	fmt.Fprintf(e.err, format+"\n", args...)
}

// ---------------------------------------------------------------------------
// The destructive gate
// ---------------------------------------------------------------------------

// requireReason refuses a destructive command that did not say why.
//
// The reason reaches farm.audit_log beside the operator's name. Weeks later it
// is the only surviving explanation of why somebody's device went away, and a
// blank one turns a post-mortem into an archaeology exercise.
func (e *env) requireReason(action string) error {
	if e.reason != "" {
		return nil
	}
	return fmt.Errorf("%w: %s needs --reason; it is written to farm.audit_log next to your name "+
		"and is the only record of why this happened", ErrUsage, action)
}

// confirm renders the blast radius and asks for approval.
//
// The detail block is printed to stderr before the question, always — including
// when --yes was passed, so an operator who scripted the flag still has the
// list of what was disturbed in their terminal scrollback and in their CI log.
// With no terminal on stdin there is nobody to ask, and guessing "yes" on
// behalf of an absent human is how an automation ends eleven running jobs.
func (e *env) confirm(headline string, detail *Fields) error {
	fmt.Fprintf(e.err, "\n%s\n", headline)
	if detail != nil && detail.Len() > 0 {
		fmt.Fprintln(e.err)
		if err := detail.Render(e.err); err != nil {
			return err
		}
	}
	fmt.Fprintf(e.err, "\nreason: %s\ntarget: %s\n", e.reason, e.client.BaseURL())

	if e.yes {
		fmt.Fprintln(e.err, "proceeding (--yes)")
		return nil
	}
	if !e.tty {
		return errNoConfirmer
	}

	fmt.Fprint(e.err, "\nType \"yes\" to proceed: ")
	answer, empty, err := readLine(e.in)
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if empty {
		// Stat said character device but the read ended immediately, which is
		// what `< /dev/null` looks like: /dev/null and NUL are both character
		// devices, so no stat can tell them from a terminal. An instant EOF
		// can, and it means the same thing a pipe does — nobody is here.
		return errNoConfirmer
	}
	if strings.TrimSpace(answer) != "yes" {
		return errors.New("cancelled; nothing was disturbed")
	}
	return nil
}

// unknownOutcome annotates a destructive write whose reply never arrived.
//
// A revoke that commits and then loses its connection is indistinguishable
// from one that never reached the server: both surface here as a transport
// error. Reporting only "the request failed" invites a retry, and the retry
// gets a 409 from a lease this very command already ended — which reads as the
// control plane refusing something it in fact did.
//
// So the outcome is named as unknown and the operator is pointed at the read
// that settles it. Nothing is retried automatically: re-sending a write that
// ends somebody's run is not a decision this tool makes on a socket error. A
// *RemoteError is passed through untouched, because there the server answered
// and the state is known.
func (e *env) unknownOutcome(err error, check string) error {
	var remote *RemoteError
	if errors.As(err, &remote) {
		return err
	}
	e.warnf("the reply never arrived, so whether this was applied is UNKNOWN. Nothing was "+
		"retried and nothing here released anything. Settle it with: %s", check)
	return err
}

// errNoConfirmer is the refusal for a destructive command with no human on the
// other end of stdin.
var errNoConfirmer = fmt.Errorf("%w: stdin is not a terminal, so there is nobody to confirm "+
	"this with. Re-run with --yes once the list above is what you meant to disturb", ErrUsage)

// readLine reads one line from r without buffering past it, so a command that
// reads a confirmation does not swallow input a later one might want.
//
// empty reports that the reader ended before yielding a single byte, which is
// the difference between an operator pressing Return and there being no
// operator at all.
func readLine(r io.Reader) (line string, empty bool, err error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return b.String(), false, nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return b.String(), b.Len() == 0, nil
			}
			return b.String(), b.Len() == 0, err
		}
	}
}

// isTerminal reports whether f is a character device, which is as close to
// "there is a human here" as the standard library alone gets on either
// platform. It is deliberately the optimistic half of the check: a false
// positive costs one unanswered prompt, which confirm turns into the same
// refusal a pipe gets, while a false negative would make every interactive
// revoke demand --yes and train operators to pass it by reflex.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

type command struct {
	name    string
	args    string
	summary string
	run     func(context.Context, *session, []string) error
}

// commands is the dispatch table and the help text, in the order help prints
// them: look at the farm, then at one thing in it, then change something.
var commands = []command{
	{"fleet", "[--host h] [--hub p] [--health s] [--pool p]", "every device, grouped by host and hub", cmdFleet},
	{"device", "<id|farm_uid> | exec <id> -- <command>", "one device in detail, or a shell command on it", cmdDevice},
	{"hosts", "", "the hosts, with what a drain would have to wait for", cmdHosts},
	{"host", "drain|undrain <id> --reason r", "stop or resume placement on a host", cmdHost},
	{"jobs", "[--state s] [--pool p] [--queue q]", "the work", cmdJobs},
	{"job", "<id> | cancel <id> | steps <id> | attempts <id>", "one job, end one, or watch it run", cmdJob},
	// submit and validate now ask the SERVER. The previous pair parsed with a
	// private copy of internal/jobspec and never called POST
	// /api/v1/specs/validate, so the CLI could accept a spec the server would
	// refuse — or refuse one it would have taken.
	{"submit", "-f spec.json --pool p --queue q --tenant t [--profile p] [--reset-tier t] [--max-attempts n] [--selector k=v]", "validate against the server, then file it", cmdSpecSubmit},
	{"validate", "-f spec.json", "ask the server whether this spec can run", cmdSpecValidate},
	{"kinds", "", "the step vocabulary this server accepts", cmdSpecKinds},
	{"resets", "--profile p [--tier t]", "exactly what a reset tier will run, before it runs", cmdSpecResets},
	{"leases", "[--state s] [--host h] [--device d]", "who holds what", cmdLeases},
	{"lease", "revoke <id> --reason r", "take a device back from its holder", cmdLease},
	{"reaper", "[disable|enable --reason r]", "the kill switch for automatic reclamation", cmdReaper},
	{"recovery", "[--outcome o --tier n --hub id --since d]", "the ladder, recent attempts, open quarantines", cmdRecovery},
	{"quarantine", "open --scope s --id x --reason r | close <id> --reason r", "take a device, slot, power domain, hub or host out of allocation, or put it back", cmdQuarantine},
	{"park", "<id|farm_uid> --reason r", "hold a device out of service on purpose; its lease is untouched", cmdPark},
	{"unpark", "<id|farm_uid> [--reason r]", "put a parked device back in service", cmdUnpark},
	// Top-level rather than a `slot` sub-verb: the slot is the address, but
	// the action is a rung of the recovery ladder with a hub-sized blast
	// radius, and it belongs beside the other things that disturb hardware.
	{"power", "<slot id> --reason r", "cycle VBUS on one slot through the host agent", cmdSlotPower},
	{"bulk", "--selector k=v -- <command>", "one command across a selector, streamed", cmdBulk},
	{"artifacts", "| gc [--apply] | delete <sha> --reason r", "the artifact store: list it, reclaim its disk, forget one", cmdArtifacts},
	{"push", "<file> [--kind k] [--name n]", "upload an artifact", cmdPush},
	{"watch", "", "follow the live event stream", cmdWatch},
}

// Run executes one ctl invocation. cfg may be nil, in which case the base URL
// comes from the environment or the compiled-in default.
//
// The returned error is classified by ExitCode; callers that want the number
// directly should use Main.
func Run(ctx context.Context, cfg *config.Config, args []string, stdout, stderr io.Writer) error {
	return runWith(ctx, cfg, args, stdout, stderr, os.Stdin)
}

// Main is Run plus the exit-code mapping, for a caller that will hand the
// result straight to os.Exit.
func Main(ctx context.Context, cfg *config.Config, args []string, stdout, stderr io.Writer) int {
	err := Run(ctx, cfg, args, stdout, stderr)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(stderr, "ctl: interrupted")
		return 1
	default:
		fmt.Fprintf(stderr, "ctl: %v\n", err)
		return ExitCode(err)
	}
}

func runWith(ctx context.Context, cfg *config.Config, args []string, stdout, stderr io.Writer, stdin *os.File) error {
	s := &session{
		cfg: cfg,
		out: stdout,
		err: stderr,
		in:  stdin,
		tty: isTerminal(stdin),
	}
	if len(args) == 0 {
		usage(stderr)
		return ErrUsage
	}

	name, rest := args[0], args[1:]
	switch name {
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	}
	for _, c := range commands {
		if c.name == name {
			err := c.run(ctx, s, rest)
			if errors.Is(err, flag.ErrHelp) {
				// -h on a subcommand. The flag package has already printed the
				// whole flag set; asking how to use a command is not a misuse
				// of it, and exiting 2 here would make `ctl lease revoke -h`
				// fail a script that runs it to find out what it needs.
				return nil
			}
			return err
		}
	}
	fmt.Fprintf(stderr, "ctl: unknown command %q\n\n", name)
	usage(stderr)
	return ErrUsage
}

func usage(w io.Writer) {
	fmt.Fprint(w, `farmd ctl — the operator command line, driven entirely through the HTTP API.

Usage:
  farmd ctl <command> [flags]

Commands:
`)
	width := 0
	for _, c := range commands {
		if n := len(c.name) + 1 + len(c.args); n > width {
			width = n
		}
	}
	if width > 52 {
		width = 52
	}
	for _, c := range commands {
		sig := c.name
		if c.args != "" {
			sig += " " + c.args
		}
		fmt.Fprintf(w, "  %-*s  %s\n", width, clip(sig, width), c.summary)
	}
	fmt.Fprintf(w, `
Global flags (accepted by every command):
  --url string        API base URL           (default $%s, else %s)
  --token string      bearer token           (default $%s)
  -o, --output f      table or json          (default table)
  --timeout d         per-request timeout    (default %s)

Destructive commands additionally require:
  --reason string     why; recorded in farm.audit_log next to your name
  --yes               skip the prompt; mandatory when stdin is not a terminal

Exit codes:
  0  success
  1  failure
  2  usage
  3  the remote refused the action (HTTP 409) — an answer, not a crash
  4  partial — the run completed and at least one target in it failed

A lease ends when its job ends, when a deadline the user wrote down elapses, or
when a human revokes it. Nothing in this tool ends one for any other reason: a
timeout here, a device that will not answer, a stream that drops are all
reported and none of them release anything.
`, config.EnvAPIBaseURL, config.DefaultAPIBaseURL, EnvAPIToken, defaultTimeout)
}

// ---------------------------------------------------------------------------
// Argument handling
// ---------------------------------------------------------------------------

// newFlags builds a flag set that reports its own errors as ErrUsage.
func newFlags(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("ctl "+name, flag.ContinueOnError)
	fs.SetOutput(out)
	return fs
}

// parseArgs parses flags that may appear anywhere among the positional
// arguments, and returns the positional ones in order.
//
// Go's flag package stops at the first non-flag argument, which would make
// `ctl job cancel <id> --reason "..."` silently ignore the reason on a
// DESTRUCTIVE command. Re-parsing what is left after each positional is the
// standard permutation and removes that failure entirely.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				// Passed through unwrapped: runWith turns it into a success,
				// because the flag package has already printed what was asked
				// for and nothing went wrong.
				return nil, err
			}
			return nil, fmt.Errorf("%w: %v", ErrUsage, err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// splitCommand divides an argument list at the first standalone "--".
//
// It runs BEFORE flag parsing, so everything after the separator survives
// untouched: `ctl device exec X --reason r -- am force-stop --user 0 com.x`
// sends the whole `am force-stop --user 0 com.x` to the device, with its own
// flags intact and unparsed by ctl.
func splitCommand(args []string) (head, tail []string, found bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

// repeatable collects a flag given more than once, for --selector.
type repeatable []string

func (r *repeatable) String() string { return strings.Join(*r, ",") }

func (r *repeatable) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("empty value")
	}
	*r = append(*r, v)
	return nil
}

// usageErrf builds a usage error with the command's own syntax attached, so a
// mistyped invocation answers itself instead of sending the operator to --help.
func usageErrf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUsage, fmt.Sprintf(format, args...))
}
