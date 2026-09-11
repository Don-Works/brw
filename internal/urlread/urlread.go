// Package urlread fetches and extracts a page WITHOUT a browser.
//
// A large share of "read this page" work needs no tab, no profile and no
// Chrome: the daemon can negotiate for markdown, fall back to llms.txt, and
// otherwise extract the HTML itself. That removes a tab lease, a navigation and
// a settle from the common case.
//
// The read is deliberately UNAUTHENTICATED: it carries no cookies, no profile
// and no credentials. Anything behind a login must go through a real tab, which
// is also what keeps this from becoming a way to exfiltrate a session.
package urlread

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/readability"
)

// DefaultMaxBytes bounds what is pulled off the network before extraction. A
// read that needs more than this wants a tab and an artifact, not an inline
// reply.
const DefaultMaxBytes int64 = 4 << 20

// DefaultTimeout bounds the whole fetch including redirects.
const DefaultTimeout = 20 * time.Second

// acceptHeader asks for markdown first. Servers that publish a markdown
// rendering (Cloudflare-style endpoints and a growing number of docs sites)
// answer with prose that needs no extraction at all.
const acceptHeader = "text/markdown;q=1.0, text/plain;q=0.9, text/html;q=0.8, application/xhtml+xml;q=0.8"

// Options selects one no-browser read.
type Options struct {
	URL string `json:"url"`
	// LLMs fetches the origin's /llms.txt instead of the URL itself — the
	// site's own agent-facing summary, when it publishes one.
	LLMs      bool   `json:"llms"`
	MaxChars  int    `json:"max_chars"`
	Offset    int    `json:"offset"`
	Section   string `json:"section"`
	UserAgent string `json:"user_agent"`
	MaxBytes  int64  `json:"-"`
	Timeout   time.Duration
	// PolicyCheck, when set, gates the initial URL and EVERY redirect hop
	// against the daemon's navigation policy.
	PolicyCheck func(string) error `json:"-"`
}

// Result is a no-browser read plus how it was obtained.
type Result struct {
	readability.PageRead
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	FinalURL    string `json:"final_url,omitempty"`
	// Source records which strategy produced the prose: markdown (the server
	// served markdown directly), llms_txt, or html (extracted here).
	Source          string `json:"source"`
	Bytes           int    `json:"bytes"`
	Truncated       bool   `json:"fetch_truncated,omitempty"`
	Unauthenticated bool   `json:"unauthenticated"`
}

// Fetch performs the read.
func Fetch(ctx context.Context, opts Options) (Result, error) {
	raw := strings.TrimSpace(opts.URL)
	if raw == "" {
		return Result{}, errors.New("url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	target, err := url.Parse(raw)
	if err != nil {
		return Result{}, fmt.Errorf("invalid url %q: %w", opts.URL, err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return Result{}, fmt.Errorf("brw_read_url only fetches http(s); %q is not supported (use a tab for other schemes)", target.Scheme)
	}
	if opts.LLMs {
		target = target.ResolveReference(&url.URL{Path: "/llms.txt"})
	}
	if opts.PolicyCheck != nil {
		if err := opts.PolicyCheck(target.String()); err != nil {
			return Result{}, err
		}
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: guardedTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			// A redirect is a fresh destination and is re-gated. Without this a
			// permitted URL could bounce the read anywhere.
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to unsupported scheme %q refused", req.URL.Scheme)
			}
			if opts.PolicyCheck != nil {
				return opts.PolicyCheck(req.URL.String())
			}
			return nil
		},
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Result{}, err
	}
	request.Header.Set("Accept", acceptHeader)
	request.Header.Set("Accept-Encoding", "identity")
	userAgent := strings.TrimSpace(opts.UserAgent)
	if userAgent == "" {
		userAgent = "brw/urlread (+https://brw.donworks.co.uk)"
	}
	request.Header.Set("User-Agent", userAgent)

	response, err := client.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("fetch %s: %w", target.Redacted(), err)
	}
	defer response.Body.Close()

	limited := io.LimitReader(response.Body, maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", target.Redacted(), err)
	}
	truncated := int64(len(body)) > maxBytes
	if truncated {
		body = body[:maxBytes]
	}

	finalURL := response.Request.URL.String()
	contentType := response.Header.Get("Content-Type")
	result := Result{
		Status:          response.StatusCode,
		ContentType:     contentType,
		FinalURL:        finalURL,
		Bytes:           len(body),
		Truncated:       truncated,
		Unauthenticated: true,
	}
	if response.StatusCode >= 400 {
		return result, fmt.Errorf("fetch %s: HTTP %d", target.Redacted(), response.StatusCode)
	}

	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	var read readability.PageRead
	switch {
	case opts.LLMs:
		result.Source = "llms_txt"
		read = readability.FromMarkdown(finalURL, "", string(body))
	case mediaType == "text/markdown" || mediaType == "text/x-markdown":
		result.Source = "markdown"
		read = readability.FromMarkdown(finalURL, "", string(body))
	case mediaType == "text/plain":
		// Plain text is already prose. Treating it as markdown keeps heading
		// indexing for the common case of a text file with # headings.
		result.Source = "markdown"
		read = readability.FromMarkdown(finalURL, "", string(body))
	default:
		result.Source = "html"
		read, err = readability.FromHTML(finalURL, body)
		if err != nil {
			return result, fmt.Errorf("extract %s: %w", target.Redacted(), err)
		}
	}

	// Same windowing as an in-tab read, so offset/max_chars/section behave
	// identically on both paths.
	read = readability.Window(read, readability.ReadOptions{
		MaxChars: opts.MaxChars,
		Offset:   opts.Offset,
		Section:  opts.Section,
	})
	result.PageRead = read
	return result, nil
}

// guardedTransport validates the RESOLVED address at connect time rather than
// the hostname up front. Checking the hostname alone is defeated by a name that
// resolves to a blocked address, and by DNS rebinding between the check and the
// dial; checking at dial time is the only point where the real destination is
// known.
func guardedTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			if err := checkDialIP(ip.IP); err != nil {
				lastErr = err
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err != nil {
				lastErr = err
				continue
			}
			return conn, nil
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no usable address for %s", host)
		}
		return nil, lastErr
	}
	return transport
}

// checkDialIP refuses addresses that have no legitimate "read a web page"
// meaning and exist mainly as a way to turn a URL read into a request against
// infrastructure the caller could not otherwise reach.
//
// Loopback and RFC1918 private addresses are deliberately ALLOWED: brw is a
// local development tool and reading http://localhost:3000 or a LAN host is a
// first-class use. The navigation policy (--allowed-domains) is the control for
// narrowing that further.
func checkDialIP(ip net.IP) error {
	switch {
	case ip.IsUnspecified():
		return fmt.Errorf("refusing to fetch the unspecified address %s", ip)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// 169.254.169.254 (cloud instance metadata) lives here.
		return fmt.Errorf("refusing to fetch link-local address %s: this range carries cloud instance metadata, not web pages", ip)
	case ip.IsMulticast():
		return fmt.Errorf("refusing to fetch multicast address %s", ip)
	case ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("refusing to fetch interface-local address %s", ip)
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10 carrier-grade NAT, 192.0.0.0/24 IETF protocol
		// assignments, 198.18.0.0/15 benchmarking, 240.0.0.0/4 reserved.
		switch {
		case v4[0] == 100 && v4[1]&0xc0 == 64:
			return fmt.Errorf("refusing to fetch carrier-grade NAT address %s", ip)
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0:
			return fmt.Errorf("refusing to fetch reserved address %s", ip)
		case v4[0] == 198 && v4[1]&0xfe == 18:
			return fmt.Errorf("refusing to fetch benchmarking address %s", ip)
		case v4[0] >= 240:
			return fmt.Errorf("refusing to fetch reserved address %s", ip)
		}
	}
	return nil
}
