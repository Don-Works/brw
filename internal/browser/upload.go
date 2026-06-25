package browser

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// maxUploadFetchBytes caps how much the daemon will pull from a remote URL into
// a temp file, guarding against an unbounded download exhausting host disk.
const maxUploadFetchBytes = 64 << 20 // 64 MiB

// uploadFetchTimeout bounds the whole remote-fetch so a slow/hung server can't
// pin the operation indefinitely.
const uploadFetchTimeout = 60 * time.Second

// errBlockedUploadHost is returned when an upload url resolves to an address the
// daemon must never fetch from (SSRF guard).
var errBlockedUploadHost = errors.New("upload url resolves to a private, loopback, or link-local address, which is blocked to prevent SSRF; use path/bytes_base64 for host-local files")

func NormalizeUploadPaths(opts snapshot.UploadOptions) ([]string, error) {
	paths := make([]string, 0, len(opts.Paths)+1)
	if strings.TrimSpace(opts.Path) != "" {
		paths = append(paths, opts.Path)
	}
	paths = append(paths, opts.Paths...)
	if len(paths) == 0 {
		return nil, errors.New("path or paths is required")
	}

	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		if strings.HasPrefix(path, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("upload file %q: %w", abs, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("upload file %q is a directory", abs)
		}
		out = append(out, abs)
	}
	if len(out) == 0 {
		return nil, errors.New("path or paths is required")
	}
	return out, nil
}

// ResolveUploadPaths returns the absolute file paths to set on a file input,
// materializing inline bytes or a remote URL into temp files on the daemon host
// when requested. Exactly one source must be supplied: path/paths (local files
// on the browser host), bytes_base64 (inline contents), or url (remote fetch).
//
// The returned cleanup func removes any temp files created for bytes_base64/url
// sources and must always be called (it is a no-op when only local paths were
// used). cleanup is safe to call even when an error is returned.
func ResolveUploadPaths(ctx context.Context, opts snapshot.UploadOptions) (paths []string, cleanup func(), err error) {
	hasPath := strings.TrimSpace(opts.Path) != "" || len(opts.Paths) > 0
	hasBytes := strings.TrimSpace(opts.BytesBase64) != ""
	hasURL := strings.TrimSpace(opts.URL) != ""

	sources := 0
	for _, present := range []bool{hasPath, hasBytes, hasURL} {
		if present {
			sources++
		}
	}
	switch {
	case sources == 0:
		return nil, noopCleanup, errors.New("one of path/paths, bytes_base64, or url is required")
	case sources > 1:
		return nil, noopCleanup, errors.New("provide exactly one of path/paths, bytes_base64, or url")
	}

	if hasPath {
		p, err := NormalizeUploadPaths(opts)
		return p, noopCleanup, err
	}

	var temps []string
	cleanup = func() {
		for _, t := range temps {
			_ = os.Remove(t)
		}
	}

	if hasBytes {
		tmp, err := writeUploadTemp(opts.BytesBase64, opts.Filename)
		if err != nil {
			cleanup()
			return nil, noopCleanup, err
		}
		temps = append(temps, tmp)
		return temps, cleanup, nil
	}

	// hasURL
	tmp, err := fetchUploadTemp(ctx, opts.URL, opts.Filename)
	if err != nil {
		cleanup()
		return nil, noopCleanup, err
	}
	temps = append(temps, tmp)
	return temps, cleanup, nil
}

func noopCleanup() {}

// uploadTempFile creates a temp file in the daemon's temp dir whose basename
// preserves filename (so the page sees a sensible name), falling back to a
// generic name when filename is empty.
func uploadTempFile(filename string) (*os.File, error) {
	name := filepath.Base(strings.TrimSpace(filename))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "upload"
	}
	dir, err := os.MkdirTemp("", "brw-upload-")
	if err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return f, nil
}

// decodeUploadBytes decodes inline base64, rejecting input that would exceed max
// decoded bytes BEFORE allocating the full output (base64 is 4 chars per 3
// bytes), mirroring the remote-fetch cap so an inline upload can't exhaust host
// memory/disk. max <= 0 disables the cap.
func decodeUploadBytes(b64 string, max int) ([]byte, error) {
	trimmed := strings.TrimSpace(b64)
	if max > 0 && int64(len(trimmed)) > (int64(max)/3+1)*4 {
		return nil, fmt.Errorf("bytes_base64 exceeds %d byte decoded limit", max)
	}
	data, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("decode bytes_base64: %w", err)
	}
	if max > 0 && len(data) > max {
		return nil, fmt.Errorf("bytes_base64 exceeds %d byte limit", max)
	}
	return data, nil
}

func writeUploadTemp(b64, filename string) (string, error) {
	data, err := decodeUploadBytes(b64, maxUploadFetchBytes)
	if err != nil {
		return "", err
	}
	f, err := uploadTempFile(filename)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// ssrfSafeClient returns an http.Client whose dialer rejects any connection
// whose resolved IP is loopback, link-local, private (RFC1918/ULA), CGNAT, or
// unspecified. Validating at dial time (rather than parsing the URL host) is
// what makes this robust against DNS rebinding AND HTTP redirects: every hop,
// including a 302 to http://169.254.169.254/, is re-resolved and re-checked, so
// neither a redirect nor a hostname that resolves to an internal IP can reach
// cloud metadata or internal services.
func ssrfSafeClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: uploadFetchTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, err
				}
				for _, ip := range ips {
					if blockedFetchIP(ip.IP) {
						return nil, errBlockedUploadHost
					}
				}
				// Dial the already-resolved IP so the address can't change between
				// our check and the connect (TOCTOU / rebinding).
				return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
			},
		},
	}
}

// blockedFetchIP is the SSRF predicate the dialer consults; it is a variable
// only so tests can relax it to exercise the download mechanics against a
// loopback httptest server. Production always uses isBlockedFetchIP.
var blockedFetchIP = isBlockedFetchIP

// isBlockedFetchIP reports whether ip is in a range the daemon must never fetch
// an upload from. Link-local (169.254.0.0/16, fe80::/10) is the cloud-metadata
// range (169.254.169.254); the rest are internal/loopback targets.
func isBlockedFetchIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsPrivate() {
		return true
	}
	// Carrier-grade NAT 100.64.0.0/10 — not covered by IsPrivate but internal.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

func fetchUploadTemp(ctx context.Context, rawURL, filename string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("url scheme %q not supported (use http or https)", u.Scheme)
	}
	if filename == "" {
		if base := filepath.Base(u.Path); base != "" && base != "." && base != "/" {
			filename = base
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := ssrfSafeClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch url: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch url: unexpected status %s", resp.Status)
	}

	f, err := uploadTempFile(filename)
	if err != nil {
		return "", err
	}
	// Limit the copy so a misbehaving server cannot fill the host disk; +1 lets
	// us detect an over-limit body rather than silently truncating it.
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxUploadFetchBytes+1))
	if err != nil {
		f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if n > maxUploadFetchBytes {
		f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("fetch url: file exceeds %d byte limit", maxUploadFetchBytes)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
