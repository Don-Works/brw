package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/credential"
)

// defaultBrowserTimeout applies when a manifest sets none. Minting a cloud
// browser is an API call plus a cold start; ten seconds is generous for the
// first and short enough that a wedged provider is a startup failure rather
// than a daemon that never finishes starting.
const defaultBrowserTimeout = 10 * time.Second

// MaxSessionEnvelopeBytes bounds what a provider may print. The envelope is
// three short fields; anything larger is a program printing its help text.
const MaxSessionEnvelopeBytes = 64 << 10

// MaxSessionLifetime bounds the lifetime a provider may claim. A provider that
// says "this browser lives for a year" is not describing a session.
const MaxSessionLifetime = 24 * time.Hour

// MinSessionLifetime is the shortest claim brw will act on. Below this the
// session expires inside its own handshake, which is a misconfiguration
// reported at startup rather than an unexplained failure on the first tool
// call.
const MinSessionLifetime = time.Second

var (
	// ErrNoBrowserProvider is the fail-closed answer when a remote browser is
	// asked for and nothing is granted to supply one.
	ErrNoBrowserProvider = errors.New("no browser provider is configured: no loaded plugin holds the browser.provider capability")
	// ErrBrowserProviderRevoked separates "revoked" from "never configured", so
	// an operator who just revoked a plugin recognises their own action.
	ErrBrowserProviderRevoked = errors.New("the browser provider was revoked")
)

// sessionIDPattern is as narrow as a credential reference, and for the same
// reason: the id travels into a teardown argv slot. It is also the one field of
// the envelope the provider gets to choose freely, so it is the one an attacker
// who can answer for the provider would aim at.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,191}$`)

// Endpoint is a CDP websocket URL a provider minted.
//
// It is a type rather than a string so that every rendering path redacts. The
// path of a CDP websocket URL IS the authentication for that browser — Chrome's
// own /devtools/browser/<uuid> is a bearer token, and a hosted provider usually
// carries its key in the query — so a log line, a %v in an error, or a struct
// somebody later marshals would otherwise hand out the session.
type Endpoint struct {
	raw      string
	redacted string
}

// ParseEndpoint validates and wraps a websocket URL.
//
// Only ws and wss are accepted. brw hands the value straight to the CDP dialer
// without chromedp's usual /json/version discovery step, so an http URL here
// would send brw off to fetch a document from a host the provider named, and a
// non-network scheme would be a file read.
func ParseEndpoint(raw string) (Endpoint, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Endpoint{}, errors.New("browser provider returned an empty websocket URL")
	}
	if len(trimmed) > 4096 {
		return Endpoint{}, errors.New("browser provider returned a websocket URL longer than 4096 bytes")
	}
	if strings.ContainsAny(trimmed, "\x00\r\n\t ") {
		return Endpoint{}, errors.New("browser provider websocket URL contains whitespace or a control character")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		// The URL itself is never quoted back: a malformed one can still carry
		// the token that made it malformed.
		return Endpoint{}, errors.New("browser provider returned a websocket URL that does not parse")
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return Endpoint{}, fmt.Errorf("browser provider websocket URL has scheme %q; brw connects a CDP socket, so it must be ws or wss", parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return Endpoint{}, errors.New("browser provider websocket URL has no host")
	}
	if parsed.User != nil {
		// Userinfo is a credential written into a URL. It reaches the dialer,
		// the dialer's errors and anything that logs the endpoint, and brw
		// already has a mechanism for a provider credential that does none of
		// those things.
		return Endpoint{}, errors.New("browser provider websocket URL carries userinfo; pass a provider credential by reference through credential.read instead of embedding it in the URL")
	}
	return Endpoint{raw: trimmed, redacted: parsed.Scheme + "://" + parsed.Host}, nil
}

// Reveal returns the URL for the one caller allowed to use it: the CDP dialer.
func (e Endpoint) Reveal() string { return e.raw }

func (e Endpoint) Empty() bool { return e.raw == "" }

// The rendering paths all redact to scheme://host. fmt routes %v, %s and %q
// through Stringer, so an endpoint that reaches a log line or a formatted error
// names the provider's host and withholds the part that authenticates.
func (e Endpoint) String() string {
	if e.raw == "" {
		return ""
	}
	return e.redacted
}

func (e Endpoint) GoString() string { return e.String() }

func (e Endpoint) MarshalJSON() ([]byte, error) { return json.Marshal(e.String()) }

// BrowserSession is one browser a provider lent brw.
type BrowserSession struct {
	// ProviderID names the plugin that minted it, for the identity a daemon
	// reports and for the error when a teardown fails.
	ProviderID string
	// Endpoint is the CDP websocket URL. Redacted on every rendering path.
	Endpoint Endpoint
	// SessionID is the provider's own handle, and the only thing brw passes
	// back to it at teardown.
	SessionID string
	// Lifetime is how long the provider says the browser lives. brw refuses to
	// start a new operation past it rather than letting the socket fail with a
	// protocol error nobody can attribute.
	Lifetime time.Duration
}

// sessionEnvelope is what a provider prints on stdout. Strictly decoded:
// a misspelled field is an error, not a silently defaulted session.
type sessionEnvelope struct {
	WebSocketURL string `json:"websocket_url"`
	SessionID    string `json:"session_id"`
	ExpiresInMS  int64  `json:"expires_in_ms"`
}

// ParseSessionEnvelope decodes and validates one provider answer. Exported so
// an operator writing a provider can be pointed at the exact contract, and so
// the envelope rules are tested without running a program.
func ParseSessionEnvelope(raw []byte) (BrowserSession, error) {
	if len(raw) > MaxSessionEnvelopeBytes {
		return BrowserSession{}, fmt.Errorf("browser provider printed more than %d bytes", MaxSessionEnvelopeBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var envelope sessionEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		// Never the decoder's message: it quotes the offending input, and the
		// offending input is a document containing a session token.
		return BrowserSession{}, errors.New("browser provider did not print a valid session envelope: expected one JSON object with websocket_url, session_id and expires_in_ms")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return BrowserSession{}, errors.New("browser provider printed trailing output after its session envelope")
	}
	var problems []error
	endpoint, err := ParseEndpoint(envelope.WebSocketURL)
	if err != nil {
		problems = append(problems, err)
	}
	if !sessionIDPattern.MatchString(envelope.SessionID) {
		problems = append(problems, errors.New("browser provider session_id must be letters, digits, dot, dash, underscore or colon; it is substituted into the teardown argv"))
	}
	lifetime := time.Duration(envelope.ExpiresInMS) * time.Millisecond
	switch {
	case envelope.ExpiresInMS <= 0:
		// A provider that states no lifetime is stating that brw should use the
		// browser forever, and brw has no way to notice when that stops being
		// true. Saying so is the provider's job.
		problems = append(problems, errors.New("browser provider must state expires_in_ms: a session with no stated lifetime cannot be reported as expired"))
	case lifetime < MinSessionLifetime:
		problems = append(problems, fmt.Errorf("browser provider stated a lifetime under %s", MinSessionLifetime))
	case lifetime > MaxSessionLifetime:
		problems = append(problems, fmt.Errorf("browser provider stated a lifetime over %s", MaxSessionLifetime))
	}
	if err := errors.Join(problems...); err != nil {
		return BrowserSession{}, err
	}
	return BrowserSession{Endpoint: endpoint, SessionID: envelope.SessionID, Lifetime: lifetime}, nil
}

func newBrowserProvider(spec BrowserProviderSpec) (browserProvider, error) {
	timeout := defaultBrowserTimeout
	if spec.TimeoutMS > 0 {
		timeout = time.Duration(spec.TimeoutMS) * time.Millisecond
	}
	switch spec.Kind {
	case BrowserKindExec:
		if len(spec.Command) == 0 || len(spec.Teardown) == 0 {
			// Unreachable through Load, which validates the manifest first. Kept
			// so a future caller cannot reach the indexes below on an empty argv.
			return nil, errors.New("the exec browser kind requires a command and a teardown")
		}
		program, err := checkProgramTrust("browser command program", spec.Command[0])
		if err != nil {
			return nil, err
		}
		// The teardown binary is held to the same rule as the mint binary. It
		// runs as the daemon's user too, and a check that covered only the one
		// an operator reads first is not the boundary docs/plugins.md claims.
		teardownProgram, err := checkProgramTrust("browser teardown program", spec.Teardown[0])
		if err != nil {
			return nil, err
		}
		return &execBrowserProvider{
			command:         append([]string(nil), spec.Command...),
			teardown:        append([]string(nil), spec.Teardown...),
			program:         program,
			teardownProgram: teardownProgram,
			timeout:         timeout,
		}, nil
	default:
		return nil, fmt.Errorf("browser kind %q is not supported", spec.Kind)
	}
}

// execBrowserProvider runs a fixed argv and reads one session envelope from its
// stdout. It is the reference implementation: whatever cloud service an
// operator uses, the auth story is their program's, not brw's.
type execBrowserProvider struct {
	command         []string
	teardown        []string
	program         trustedProgram
	teardownProgram trustedProgram
	timeout         time.Duration
}

func (p *execBrowserProvider) open(ctx context.Context, secret credential.Secret) (BrowserSession, error) {
	stdout, err := p.run(ctx, "browser command program", p.program, p.command, secret)
	if err != nil {
		return BrowserSession{}, err
	}
	session, err := ParseSessionEnvelope(stdout)
	if err != nil {
		// The envelope can hold the credential (a token pasted into the URL),
		// so the validation error is scrubbed like any other.
		return BrowserSession{}, credential.Scrub(err, secret)
	}
	return session, nil
}

func (p *execBrowserProvider) release(ctx context.Context, sessionID string, secret credential.Secret) error {
	if !sessionIDPattern.MatchString(sessionID) {
		return errors.New("browser provider session id is not releasable")
	}
	argv := substituteToken(p.teardown, SessionToken, sessionID)
	_, err := p.run(ctx, "browser teardown program", p.teardownProgram, argv, secret)
	return err
}

// run execs one provider call and scrubs the credential out of whatever comes
// back. Nothing here is built from the secret, so the scrub is a net rather
// than the boundary — the boundary is that stderr is discarded and the value
// only ever reaches the child's stdin.
func (p *execBrowserProvider) run(ctx context.Context, what string, program trustedProgram, command []string, secret credential.Secret) ([]byte, error) {
	stdout, err := p.exec(ctx, what, program, command, secret)
	return stdout, credential.Scrub(err, secret)
}

// exec is run's body. The credential goes on stdin: an argv is readable by
// every process on the machine, and this is the difference between "passed by
// reference" and "passed by reference and then printed in ps".
func (p *execBrowserProvider) exec(ctx context.Context, what string, program trustedProgram, command []string, secret credential.Secret) ([]byte, error) {
	label := fmt.Sprintf("%s %q", what, program.declared)
	if err := checkProgramFileTrust(program.resolved, label); err != nil {
		return nil, err
	}
	argv := append([]string(nil), command...)
	// The path the loader resolved and checked, never the declared name looked
	// up again here: re-resolving would let a link moved since startup choose
	// the program.
	argv[0] = program.resolved
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if !secret.Empty() {
		// One line, so a provider can `read key` from stdin. The reader is
		// closed by exec once the value is written.
		cmd.Stdin = strings.NewReader(secret.Reveal() + "\n")
	}
	// stderr to the void rather than captured: exec.ExitError would retain it,
	// and a provider that prints its key on the failure path would put it in a
	// retained buffer.
	cmd.Stderr = io.Discard
	cmd.WaitDelay = execWaitDelay
	stdout := &boundedBuffer{limit: MaxSessionEnvelopeBytes + 1}
	cmd.Stdout = stdout
	err := cmd.Run()
	// Checked before the run error so an expired deadline reads as a timeout on
	// every path, including the race where Run returns nil as the deadline
	// passes.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s timed out after %s", what, p.timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Deliberately without the provider's stderr: see above.
			return nil, fmt.Errorf("%s exited with status %d", what, exitErr.ExitCode())
		}
		return nil, fmt.Errorf("%s could not be run: %w", what, err)
	}
	return stdout.bytes(), nil
}
