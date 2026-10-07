package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/credential"
)

const defaultBrowserTimeout = 10 * time.Second

// MaxSessionEnvelopeBytes bounds what a provider may print.
const MaxSessionEnvelopeBytes = 64 << 10

// MaxSessionLifetime bounds the lifetime a provider may claim.
const MaxSessionLifetime = 24 * time.Hour

// MinSessionLifetime is the shortest claim brw will act on.
const MinSessionLifetime = time.Second

var (
	// ErrNoBrowserProvider is the fail-closed answer when a remote browser is asked for and nothing is granted to supply one.
	ErrNoBrowserProvider = errors.New("no browser provider is configured: no loaded plugin holds the browser.provider capability")
	// ErrBrowserProviderRevoked separates "revoked" from "never configured", so an operator who just revoked a plugin recognises their own action.
	ErrBrowserProviderRevoked = errors.New("the browser provider was revoked")
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,191}$`)

// Endpoint is a CDP websocket URL a provider minted.
type Endpoint struct {
	raw      string
	redacted string
}

// ParseEndpoint validates and wraps a websocket URL.
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

		return Endpoint{}, errors.New("browser provider returned a websocket URL that does not parse")
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return Endpoint{}, fmt.Errorf("browser provider websocket URL has scheme %q; brw connects a CDP socket, so it must be ws or wss", parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return Endpoint{}, errors.New("browser provider websocket URL has no host")
	}
	if parsed.User != nil {

		return Endpoint{}, errors.New("browser provider websocket URL carries userinfo; pass a provider credential by reference through credential.read instead of embedding it in the URL")
	}
	return Endpoint{raw: trimmed, redacted: parsed.Scheme + "://" + parsed.Host}, nil
}

// Reveal returns the URL for the one caller allowed to use it: the CDP dialer.
func (e Endpoint) Reveal() string { return e.raw }

func (e Endpoint) Empty() bool { return e.raw == "" }

// The rendering paths all redact to scheme://host.
func (e Endpoint) String() string {
	if e.raw == "" {
		return ""
	}
	return e.redacted
}

func (e Endpoint) GoString() string { return e.String() }

// PlaintextToAnotherHost reports a ws:// endpoint whose host is not loopback.
func (e Endpoint) PlaintextToAnotherHost() bool {
	if e.raw == "" {
		return false
	}
	parsed, err := url.Parse(e.raw)
	if err != nil || parsed.Scheme != "ws" {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

func (e Endpoint) MarshalJSON() ([]byte, error) { return json.Marshal(e.String()) }

// BrowserSession is one browser a provider lent brw.
type BrowserSession struct {
	// ProviderID names the plugin that minted it, for the identity a daemon reports and for the error when a teardown fails.
	ProviderID string
	// Endpoint is the CDP websocket URL.
	Endpoint Endpoint
	// SessionID is the provider's own handle, and the only thing brw passes back to it at teardown.
	SessionID string
	// Lifetime is how long the provider says the browser lives.
	Lifetime time.Duration
}

type sessionEnvelope struct {
	WebSocketURL string `json:"websocket_url"`
	SessionID    string `json:"session_id"`
	ExpiresInMS  int64  `json:"expires_in_ms"`
}

// ParseSessionEnvelope decodes and validates one provider answer.
func ParseSessionEnvelope(raw []byte) (BrowserSession, error) {
	if len(raw) > MaxSessionEnvelopeBytes {
		return BrowserSession{}, fmt.Errorf("browser provider printed more than %d bytes", MaxSessionEnvelopeBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var envelope sessionEnvelope
	if err := decoder.Decode(&envelope); err != nil {

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
	switch {
	case envelope.ExpiresInMS <= 0:

		problems = append(problems, errors.New("browser provider must state expires_in_ms: a session with no stated lifetime cannot be reported as expired"))
	case envelope.ExpiresInMS < MinSessionLifetime.Milliseconds():
		problems = append(problems, fmt.Errorf("browser provider stated a lifetime under %s", MinSessionLifetime))
	case envelope.ExpiresInMS > MaxSessionLifetime.Milliseconds():
		problems = append(problems, fmt.Errorf("browser provider stated a lifetime over %s", MaxSessionLifetime))
	}
	if err := errors.Join(problems...); err != nil {
		return BrowserSession{}, err
	}
	return BrowserSession{Endpoint: endpoint, SessionID: envelope.SessionID, Lifetime: time.Duration(envelope.ExpiresInMS) * time.Millisecond}, nil
}

func newBrowserProvider(spec BrowserProviderSpec) (browserProvider, error) {
	timeout := defaultBrowserTimeout
	if spec.TimeoutMS > 0 {
		timeout = time.Duration(spec.TimeoutMS) * time.Millisecond
	}
	switch spec.Kind {
	case BrowserKindExec:
		if len(spec.Command) == 0 || len(spec.Teardown) == 0 {

			return nil, errors.New("the exec browser kind requires a command and a teardown")
		}
		program, err := checkProgramTrust("browser command program", spec.Command[0])
		if err != nil {
			return nil, err
		}

		teardownProgram, err := checkProgramTrust("browser teardown program", spec.Teardown[0])
		if err != nil {
			return nil, err
		}
		return &execBrowserProvider{
			command:         slices.Clone(spec.Command),
			teardown:        slices.Clone(spec.Teardown),
			program:         program,
			teardownProgram: teardownProgram,
			timeout:         timeout,
		}, nil
	default:
		return nil, fmt.Errorf("browser kind %q is not supported", spec.Kind)
	}
}

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

func (p *execBrowserProvider) run(ctx context.Context, what string, program trustedProgram, command []string, secret credential.Secret) ([]byte, error) {
	stdout, err := p.exec(ctx, what, program, command, secret)
	return stdout, credential.Scrub(err, secret)
}

func (p *execBrowserProvider) exec(ctx context.Context, what string, program trustedProgram, command []string, secret credential.Secret) ([]byte, error) {
	label := fmt.Sprintf("%s %q", what, program.declared)
	if err := checkProgramFileTrust(program.resolved, label); err != nil {
		return nil, err
	}
	argv := slices.Clone(command)

	argv[0] = program.resolved
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if !secret.Empty() {

		cmd.Stdin = strings.NewReader(secret.Reveal() + "\n")
	}

	cmd.Stderr = io.Discard
	cmd.WaitDelay = execWaitDelay
	stdout := &boundedBuffer{limit: MaxSessionEnvelopeBytes + 1}
	cmd.Stdout = stdout
	err := cmd.Run()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s timed out after %s", what, p.timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {

			return nil, fmt.Errorf("%s exited with status %d", what, exitErr.ExitCode())
		}
		return nil, fmt.Errorf("%s could not be run: %w", what, err)
	}
	return stdout.bytes(), nil
}
