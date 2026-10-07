package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const minBearerTokenLen = 32

// readBearerTokenFile reads a token from an absolute, non-symlink 0600 file.
func readBearerTokenFile(flagName, path string) (string, error) {
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("--%s must be an absolute path", flagName)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("--%s: cannot inspect token file", flagName)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("--%s: token must be a non-symlink regular file readable only by its owner", flagName)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("--%s: cannot open token file", flagName)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(raw) > 4096 {
		return "", fmt.Errorf("--%s: cannot read token file", flagName)
	}
	token := strings.TrimRight(string(raw), "\r\n")
	if err := validateBearerToken(token); err != nil {
		return "", fmt.Errorf("--%s: %w", flagName, err)
	}
	return token, nil
}

func validateBearerToken(token string) error {
	if len(token) < minBearerTokenLen {
		return fmt.Errorf("token must contain at least %d characters", minBearerTokenLen)
	}
	for _, char := range token {
		if char < 0x21 || char > 0x7e {
			return errors.New("token must contain only printable ASCII without whitespace")
		}
	}
	return nil
}

// resolveUpstreamToken returns the bearer token a proxy sends upstream: the
// file wins over BRW_UPSTREAM_TOKEN, which exists so a supervisor that injects
// credentials as environment variables need not write them to disk.
func resolveUpstreamToken(file, envValue string) (string, error) {
	if strings.TrimSpace(file) != "" {
		return readBearerTokenFile("upstream-token-file", file)
	}
	envValue = strings.TrimSpace(envValue)
	if envValue == "" {
		return "", nil
	}
	if err := validateBearerToken(envValue); err != nil {
		return "", fmt.Errorf("BRW_UPSTREAM_TOKEN: %w", err)
	}
	return envValue, nil
}

// httpBindIsLoopback reports whether an --http address listens on loopback only.
func httpBindIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
