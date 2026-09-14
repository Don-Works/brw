package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

// maxHeaderOrigins and maxHeadersPerOrigin bound the per-tab header table. The
// table is consulted inside the Fetch.requestPaused handler, which runs once per
// request, so an unbounded table would make every subresource pay for it.
const (
	maxHeaderOrigins    = 20
	maxHeadersPerOrigin = 20
)

// ErrEnvironmentUnsupported is returned for every page-environment override by
// transports that cannot apply one.
//
// These are DevTools Protocol session overrides — Emulation.setGeolocationOverride,
// Network.overrideNetworkState, Emulation.setEmulatedMedia,
// Emulation.setUserAgentOverride, Fetch request interception and
// Browser.setDownloadBehavior. chrome.debugger sessions owned by the extension
// bridge cannot hold them: the extension attaches and detaches per operation, and
// a detach drops every override the session installed, so an override applied
// through the bridge would silently evaporate between two tool calls. Failing
// loudly is the only honest answer.
var ErrEnvironmentUnsupported = errors.New("page environment overrides (geolocation, network conditions, emulated media, per-origin headers, user agent, HTTP credentials, download path) are not supported on the extension-bridge transport: they are DevTools Protocol session overrides that do not survive the extension's attach/detach cycle; use a direct-CDP profile")

// EnvironmentController is an optional transport capability covering the page
// environment an agent may need to fake: where the browser claims to be, whether
// it has a network, what media it renders for, which extra headers and
// credentials it sends, what it calls itself, and where its downloads land.
//
// It is a separate interface rather than part of Controller for the same reason
// DialogController is: a transport that has not implemented it degrades to a
// named capability error instead of failing to compile.
type EnvironmentController interface {
	SetGeolocation(context.Context, GeolocationOptions) (EnvironmentResult, error)
	SetNetworkConditions(context.Context, NetworkConditionsOptions) (EnvironmentResult, error)
	EmulateMedia(context.Context, MediaEmulationOptions) (EnvironmentResult, error)
	SetExtraHeaders(context.Context, ExtraHeadersOptions) (EnvironmentResult, error)
	SetUserAgent(context.Context, UserAgentOptions) (EnvironmentResult, error)
	Authenticate(context.Context, CredentialsOptions) (EnvironmentResult, error)
	SetDownloadPath(context.Context, DownloadPathOptions) (EnvironmentResult, error)
}

// EnvironmentResult is the shared reply for every page-environment override. The
// applied block is the RESOLVED override rather than the request, so a caller can
// see the accuracy or throughput a defaulted field ended up with.
type EnvironmentResult struct {
	OK          bool                     `json:"ok"`
	TabID       string                   `json:"tab_id,omitempty"`
	Cleared     bool                     `json:"cleared,omitempty"`
	Geolocation *GeolocationConfig       `json:"geolocation,omitempty"`
	Network     *NetworkConditionsConfig `json:"network_conditions,omitempty"`
	Media       *MediaEmulationConfig    `json:"media,omitempty"`
	// ExtraHeaders echoes the declared origins and header NAMES. Values are never
	// echoed: the common use is a bearer token, and a result that repeats it puts
	// it in the agent transcript, the MCP client's log and the usage ledger's
	// caller-side copy.
	ExtraHeaders  []OriginHeaderNames    `json:"extra_headers,omitempty"`
	UserAgent     *UserAgentConfig       `json:"user_agent,omitempty"`
	Authenticated *AuthenticationOutcome `json:"authentication,omitempty"`
	DownloadPath  string                 `json:"download_path,omitempty"`
	Message       string                 `json:"message,omitempty"`
}

// GeolocationOptions overrides the position navigator.geolocation reports.
type GeolocationOptions struct {
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
	Accuracy  *float64 `json:"accuracy,omitempty"`
	Clear     bool     `json:"clear,omitempty"`
	TabID     string   `json:"tab_id,omitempty"`
}

// GeolocationConfig is the resolved override.
type GeolocationConfig struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Accuracy  float64 `json:"accuracy"`
}

// defaultGeolocationAccuracy is what Chrome's own DevTools sensors panel uses.
const defaultGeolocationAccuracy = 100

// NormalizeGeolocationOptions resolves and range-checks a geolocation request.
func NormalizeGeolocationOptions(opts GeolocationOptions) (GeolocationConfig, bool, error) {
	if opts.Clear {
		return GeolocationConfig{}, true, nil
	}
	if opts.Latitude == nil || opts.Longitude == nil {
		return GeolocationConfig{}, false, errors.New("geolocation requires both latitude and longitude, or clear:true to remove the override")
	}
	cfg := GeolocationConfig{
		Latitude:  *opts.Latitude,
		Longitude: *opts.Longitude,
		Accuracy:  defaultGeolocationAccuracy,
	}
	if opts.Accuracy != nil {
		cfg.Accuracy = *opts.Accuracy
	}
	if cfg.Latitude < -90 || cfg.Latitude > 90 {
		return GeolocationConfig{}, false, fmt.Errorf("latitude %v is outside the valid range -90 to 90", cfg.Latitude)
	}
	if cfg.Longitude < -180 || cfg.Longitude > 180 {
		return GeolocationConfig{}, false, fmt.Errorf("longitude %v is outside the valid range -180 to 180", cfg.Longitude)
	}
	if cfg.Accuracy < 0 {
		return GeolocationConfig{}, false, fmt.Errorf("accuracy %v is negative; accuracy is a radius in metres", cfg.Accuracy)
	}
	return cfg, false, nil
}

// NetworkConditionsOptions throttles or disconnects the page's network.
type NetworkConditionsOptions struct {
	Offline *bool `json:"offline,omitempty"`
	// LatencyMS is the minimum delay from request sent to response headers
	// received.
	LatencyMS *float64 `json:"latency_ms,omitempty"`
	// DownloadThroughput and UploadThroughput are bytes per second. -1 disables
	// throttling on that direction, which is also the cleared state.
	DownloadThroughput *float64 `json:"download_throughput,omitempty"`
	UploadThroughput   *float64 `json:"upload_throughput,omitempty"`
	Clear              bool     `json:"clear,omitempty"`
	TabID              string   `json:"tab_id,omitempty"`
}

// NetworkConditionsConfig is the resolved override.
type NetworkConditionsConfig struct {
	Offline            bool    `json:"offline"`
	LatencyMS          float64 `json:"latency_ms"`
	DownloadThroughput float64 `json:"download_throughput"`
	UploadThroughput   float64 `json:"upload_throughput"`
}

// unthrottled is the value CDP reads as "no limit on this direction".
const unthrottled = -1

// NormalizeNetworkConditions resolves a throttling request. Clearing and an
// all-defaults request produce the same config, which is what restores a normal
// network.
func NormalizeNetworkConditions(opts NetworkConditionsOptions) (NetworkConditionsConfig, bool, error) {
	cfg := NetworkConditionsConfig{
		DownloadThroughput: unthrottled,
		UploadThroughput:   unthrottled,
	}
	if opts.Clear {
		return cfg, true, nil
	}
	if opts.Offline != nil {
		cfg.Offline = *opts.Offline
	}
	if opts.LatencyMS != nil {
		cfg.LatencyMS = *opts.LatencyMS
	}
	if opts.DownloadThroughput != nil {
		cfg.DownloadThroughput = *opts.DownloadThroughput
	}
	if opts.UploadThroughput != nil {
		cfg.UploadThroughput = *opts.UploadThroughput
	}
	if cfg.LatencyMS < 0 {
		return NetworkConditionsConfig{}, false, fmt.Errorf("latency_ms %v is negative", cfg.LatencyMS)
	}
	if cfg.DownloadThroughput < 0 && cfg.DownloadThroughput != unthrottled {
		return NetworkConditionsConfig{}, false, fmt.Errorf("download_throughput %v is negative; pass -1 for no limit", cfg.DownloadThroughput)
	}
	if cfg.UploadThroughput < 0 && cfg.UploadThroughput != unthrottled {
		return NetworkConditionsConfig{}, false, fmt.Errorf("upload_throughput %v is negative; pass -1 for no limit", cfg.UploadThroughput)
	}
	return cfg, false, nil
}

// MediaEmulationOptions forces the CSS media type and user-preference media
// features a page renders against.
type MediaEmulationOptions struct {
	Media         string `json:"media,omitempty"`
	ColorScheme   string `json:"color_scheme,omitempty"`
	ReducedMotion string `json:"reduced_motion,omitempty"`
	Clear         bool   `json:"clear,omitempty"`
	TabID         string `json:"tab_id,omitempty"`
}

// MediaEmulationConfig is the resolved override. An empty field is left to the
// browser rather than forced.
type MediaEmulationConfig struct {
	Media         string `json:"media,omitempty"`
	ColorScheme   string `json:"color_scheme,omitempty"`
	ReducedMotion string `json:"reduced_motion,omitempty"`
}

// NormalizeMediaEmulation validates the media type and feature values. The
// accepted values are the CSS ones so a caller can copy them straight out of the
// media query being tested.
func NormalizeMediaEmulation(opts MediaEmulationOptions) (MediaEmulationConfig, bool, error) {
	if opts.Clear {
		return MediaEmulationConfig{}, true, nil
	}
	cfg := MediaEmulationConfig{
		Media:         strings.ToLower(strings.TrimSpace(opts.Media)),
		ColorScheme:   strings.ToLower(strings.TrimSpace(opts.ColorScheme)),
		ReducedMotion: strings.ToLower(strings.TrimSpace(opts.ReducedMotion)),
	}
	switch cfg.Media {
	case "", "screen", "print":
	default:
		return MediaEmulationConfig{}, false, fmt.Errorf("unknown media type %q: use screen or print", opts.Media)
	}
	switch cfg.ColorScheme {
	case "", "light", "dark", "no-preference":
	default:
		return MediaEmulationConfig{}, false, fmt.Errorf("unknown color_scheme %q: use light, dark, or no-preference", opts.ColorScheme)
	}
	switch cfg.ReducedMotion {
	case "", "reduce", "no-preference":
	default:
		return MediaEmulationConfig{}, false, fmt.Errorf("unknown reduced_motion %q: use reduce or no-preference", opts.ReducedMotion)
	}
	if cfg.Media == "" && cfg.ColorScheme == "" && cfg.ReducedMotion == "" {
		return MediaEmulationConfig{}, false, errors.New("media emulation needs at least one of media, color_scheme, or reduced_motion, or clear:true to remove the override")
	}
	return cfg, false, nil
}

// ExtraHeadersOptions declares extra request headers and the origins allowed to
// receive them.
type ExtraHeadersOptions struct {
	Origins []OriginHeaders `json:"origins,omitempty"`
	Clear   bool            `json:"clear,omitempty"`
	TabID   string          `json:"tab_id,omitempty"`
}

// OriginHeaders binds a header set to exactly one origin.
type OriginHeaders struct {
	Origin  string            `json:"origin"`
	Headers map[string]string `json:"headers"`
}

// OriginHeaderNames is the echo shape: which origin carries which header names,
// with the values withheld.
type OriginHeaderNames struct {
	Origin  string   `json:"origin"`
	Headers []string `json:"headers"`
}

// NormalizeExtraHeaders canonicalizes each origin and rejects header names that
// cannot be sent.
func NormalizeExtraHeaders(opts ExtraHeadersOptions) ([]OriginHeaders, bool, error) {
	if opts.Clear {
		return nil, true, nil
	}
	if len(opts.Origins) == 0 {
		return nil, false, errors.New("extra headers need at least one origin entry, or clear:true to remove them")
	}
	if len(opts.Origins) > maxHeaderOrigins {
		return nil, false, fmt.Errorf("at most %d origins may carry extra headers", maxHeaderOrigins)
	}
	out := make([]OriginHeaders, 0, len(opts.Origins))
	seen := map[string]bool{}
	for _, entry := range opts.Origins {
		origin, err := CanonicalOrigin(entry.Origin)
		if err != nil {
			return nil, false, err
		}
		if seen[origin] {
			return nil, false, fmt.Errorf("origin %s is declared twice; merge its headers into one entry", origin)
		}
		seen[origin] = true
		if len(entry.Headers) == 0 {
			return nil, false, fmt.Errorf("origin %s declares no headers", origin)
		}
		if len(entry.Headers) > maxHeadersPerOrigin {
			return nil, false, fmt.Errorf("origin %s declares more than the maximum of %d headers", origin, maxHeadersPerOrigin)
		}
		headers := make(map[string]string, len(entry.Headers))
		for name, value := range entry.Headers {
			trimmed := strings.TrimSpace(name)
			if err := validHeaderName(trimmed); err != nil {
				return nil, false, err
			}
			if forgeableHeaders[strings.ToLower(trimmed)] {
				return nil, false, fmt.Errorf("%w: %s", ErrForgeableHeader, trimmed)
			}
			if strings.ContainsAny(value, "\r\n") {
				return nil, false, fmt.Errorf("header %s carries a newline, which would let it inject a second header", trimmed)
			}
			headers[trimmed] = value
		}
		out = append(out, OriginHeaders{Origin: origin, Headers: headers})
	}
	return out, false, nil
}

// ErrForgeableHeader names the headers the per-origin table must not carry.
var ErrForgeableHeader = errors.New("this header cannot be set from the per-origin header table: it changes which server or which request body the message is for, behind the checks the navigation policy already made")

// forgeableHeaders are the headers Fetch.continueRequest sends verbatim and that
// change what the request MEANS rather than what it carries. Host picks a
// different virtual host from the one containmentVerdict evaluated out of the
// request URL, so the table would be a way around a confining navigation policy;
// Content-Length and Transfer-Encoding desynchronise the body brw's interceptor
// forwards from the one the server reads.
var forgeableHeaders = map[string]bool{
	"host":              true,
	"content-length":    true,
	"transfer-encoding": true,
}

// validHeaderName rejects anything outside RFC 7230's token grammar. A header
// name with a space or colon in it is not sent as a header; it corrupts the
// request line and the failure surfaces far from the call that caused it.
func validHeaderName(name string) error {
	if name == "" {
		return errors.New("header name is empty")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return fmt.Errorf("header name %q contains %q, which is not a valid header-name character", name, r)
		}
	}
	return nil
}

// HeaderNames reduces a declared header set to its names, sorted so the echo is
// stable across calls.
func HeaderNames(entries []OriginHeaders) []OriginHeaderNames {
	out := make([]OriginHeaderNames, 0, len(entries))
	for _, entry := range entries {
		names := make([]string, 0, len(entry.Headers))
		for name := range entry.Headers {
			names = append(names, name)
		}
		sort.Strings(names)
		out = append(out, OriginHeaderNames{Origin: entry.Origin, Headers: names})
	}
	return out
}

// UserAgentOptions overrides what the browser calls itself, in the User-Agent
// header, navigator.userAgent, Accept-Language and navigator.platform.
type UserAgentOptions struct {
	UserAgent      string `json:"user_agent,omitempty"`
	AcceptLanguage string `json:"accept_language,omitempty"`
	Platform       string `json:"platform,omitempty"`
	Clear          bool   `json:"clear,omitempty"`
	TabID          string `json:"tab_id,omitempty"`
}

// UserAgentConfig is the resolved override.
type UserAgentConfig struct {
	UserAgent      string `json:"user_agent"`
	AcceptLanguage string `json:"accept_language,omitempty"`
	Platform       string `json:"platform,omitempty"`
}

// NormalizeUserAgent resolves a user-agent request.
func NormalizeUserAgent(opts UserAgentOptions) (UserAgentConfig, bool, error) {
	if opts.Clear {
		return UserAgentConfig{}, true, nil
	}
	cfg := UserAgentConfig{
		UserAgent:      strings.TrimSpace(opts.UserAgent),
		AcceptLanguage: strings.TrimSpace(opts.AcceptLanguage),
		Platform:       strings.TrimSpace(opts.Platform),
	}
	if cfg.UserAgent == "" {
		return UserAgentConfig{}, false, errors.New("user_agent is required, or clear:true to restore the browser's own")
	}
	// CDP sends these verbatim into request headers, so a newline here is a
	// header-injection primitive against every origin the page touches.
	if strings.ContainsAny(cfg.UserAgent, "\r\n") || strings.ContainsAny(cfg.AcceptLanguage, "\r\n") {
		return UserAgentConfig{}, false, errors.New("user_agent and accept_language must not contain newlines")
	}
	return cfg, false, nil
}

// CredentialsOptions supplies HTTP authentication for ONE navigation.
//
// There is deliberately no "store these credentials" call. A daemon that held a
// password between calls would have to keep it in memory for the life of the
// session, hand it to every 401 the page provoked, and survive in a heap dump —
// so the credential is armed for the navigation this call performs and dropped
// before the call returns.
//
// That covers the DAEMON only. Once a challenge has been answered, Chrome keeps
// the credential in its own HTTP-auth cache and re-sends it for that origin for
// the rest of the browser session, including for pages a human opens in a
// visible profile; CDP has no command to clear that cache. An incognito context
// is the only way to bound it, because disposing the context discards the cache
// with it.
type CredentialsOptions struct {
	Origin   string `json:"origin"`
	Username string `json:"username"`
	Password string `json:"password"`
	// URL is the page to load with the credential armed. It must be inside
	// Origin; omitted, the origin itself is loaded.
	URL   string `json:"url,omitempty"`
	TabID string `json:"tab_id,omitempty"`
}

// AuthenticationOutcome reports what the armed credential actually did, so a
// caller can tell "signed in" from "the server never asked".
type AuthenticationOutcome struct {
	Origin     string `json:"origin"`
	URL        string `json:"url"`
	Challenged bool   `json:"challenged"`
	Answered   int    `json:"answered"`
	// BrowserCached says the browser now holds the credential itself. brw's copy
	// is already gone when this result is built; Chrome's HTTP-auth cache is not,
	// and nothing in CDP can empty it. Reported rather than hidden because on a
	// persistent profile it means every later load of that origin is
	// authenticated, by this agent or by a human in the same browser.
	BrowserCached bool `json:"browser_cached"`
}

// NormalizeCredentials validates the credential request and returns the
// canonical origin and the URL to load.
func NormalizeCredentials(opts CredentialsOptions) (origin, target string, err error) {
	origin, err = CanonicalOrigin(opts.Origin)
	if err != nil {
		return "", "", err
	}
	if opts.Username == "" && opts.Password == "" {
		return "", "", errors.New("credentials need a username or a password")
	}
	if strings.ContainsAny(opts.Username, "\r\n") || strings.ContainsAny(opts.Password, "\r\n") {
		return "", "", errors.New("username and password must not contain newlines")
	}
	target = strings.TrimSpace(opts.URL)
	if target == "" {
		return origin, origin + "/", nil
	}
	// The URL must be inside the origin the credential is scoped to, or the
	// credential would be armed for a navigation it was never meant to cover.
	targetOrigin, err := CanonicalOrigin(target)
	if err != nil {
		return "", "", err
	}
	if targetOrigin != origin {
		return "", "", fmt.Errorf("url %s is on origin %s, not the declared credential origin %s", target, targetOrigin, origin)
	}
	return origin, target, nil
}

// DownloadPathOptions redirects completed downloads to a directory the caller
// names.
type DownloadPathOptions struct {
	Path  string `json:"path,omitempty"`
	Clear bool   `json:"clear,omitempty"`
}

// CanonicalOrigin reduces a URL or origin to scheme://host[:port], lowercased,
// with the scheme's default port removed so http://example.com:80 and
// http://example.com are one origin rather than two.
func CanonicalOrigin(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("origin is empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("origin %q is not a URL: %w", raw, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("origin %q must start with http:// or https://", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("origin %q has no host", raw)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("origin %q has no host", raw)
	}
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + net.JoinHostPort(host, port), nil
	}
	// url.Hostname strips an IPv6 literal's brackets; without them the result is
	// not a URL and would never match an intercepted request.
	if strings.Contains(host, ":") {
		return scheme + "://[" + host + "]", nil
	}
	return scheme + "://" + host, nil
}
