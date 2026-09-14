package cdp

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// NetworkEnvironment is the part of a browser's network setup that can only be
// chosen when Chrome starts. Unlike the per-tab overrides in
// internal/browser/manager_environment.go, none of these can be changed over CDP
// on a running browser: the proxy, the certificate-error policy and the trusted
// keys are read once during network-service startup.
type NetworkEnvironment struct {
	// ProxyServer is Chrome's --proxy-server value: "host:port",
	// "scheme://host:port", or a per-scheme list such as
	// "https=proxy:8443;http=proxy:8080".
	ProxyServer string
	// ProxyBypassList is Chrome's --proxy-bypass-list value: semicolon-separated
	// hosts and patterns that go direct, for example "<local>;*.internal".
	ProxyBypassList string
	// IgnoreHTTPSErrors turns off certificate validation for the WHOLE browser.
	// It is opt-in per launch and reported in brw_identity, because a caller who
	// does not know it is on cannot tell a valid site from a
	// man-in-the-middled one.
	IgnoreHTTPSErrors bool
	// TrustedSPKI are base64 SHA-256 hashes of SubjectPublicKeyInfo blocks whose
	// certificate errors Chrome should ignore. See SPKIFingerprintsFromPEM for
	// what this does and does not amount to.
	TrustedSPKI []string
}

// Empty reports whether any launch-time network setting was requested.
func (n NetworkEnvironment) Empty() bool {
	return n.ProxyServer == "" && n.ProxyBypassList == "" && !n.IgnoreHTTPSErrors && len(n.TrustedSPKI) == 0
}

// Validate rejects values that would change Chrome's command line rather than
// one switch's value. Chrome's flag parser splits on whitespace, so a proxy
// value carrying a space is a way to smuggle a second switch past every guard in
// this package, including the real-profile check.
func (n NetworkEnvironment) Validate() error {
	for name, value := range map[string]string{
		"proxy server":      n.ProxyServer,
		"proxy bypass list": n.ProxyBypassList,
	} {
		if value == "" {
			continue
		}
		if strings.ContainsAny(value, " \t\r\n\"'") {
			return fmt.Errorf("%s %q contains whitespace or quotes; Chrome would read it as more than one switch", name, value)
		}
		if strings.HasPrefix(value, "-") {
			return fmt.Errorf("%s %q starts with a dash; that is a switch, not a value", name, value)
		}
	}
	if n.ProxyServer == "" && n.ProxyBypassList != "" {
		return errors.New("a proxy bypass list without a proxy server has nothing to bypass")
	}
	for _, hash := range n.TrustedSPKI {
		if strings.ContainsAny(hash, " \t\r\n,\"'") {
			return fmt.Errorf("SPKI hash %q contains whitespace, a comma or quotes", hash)
		}
		if _, err := base64.StdEncoding.DecodeString(hash); err != nil {
			return fmt.Errorf("SPKI hash %q is not base64: %w", hash, err)
		}
	}
	return nil
}

// networkArgs renders the switches for a launch. Order is stable so a test and a
// log line read the same way twice.
func networkArgs(n NetworkEnvironment) []string {
	var args []string
	if n.ProxyServer != "" {
		args = append(args, "--proxy-server="+n.ProxyServer)
	}
	if n.ProxyBypassList != "" {
		args = append(args, "--proxy-bypass-list="+n.ProxyBypassList)
	}
	if n.IgnoreHTTPSErrors {
		args = append(args, "--ignore-certificate-errors")
	}
	if len(n.TrustedSPKI) > 0 {
		args = append(args, "--ignore-certificate-errors-spki-list="+strings.Join(n.TrustedSPKI, ","))
	}
	return args
}

// SPKIFingerprintsFromPEM turns a PEM bundle into the base64 SHA-256
// SubjectPublicKeyInfo hashes Chrome's --ignore-certificate-errors-spki-list
// switch takes, so a private CA can be trusted for one launch without a flag
// that trusts everything.
//
// What this buys: Chrome stops reporting certificate errors for chains
// containing one of these public keys. A site signed by the named CA loads
// without --ignore-certificate-errors, so every OTHER certificate error in the
// session is still a real error the caller will see.
//
// What it is not: the CA is not installed anywhere. Chrome does not gain trust
// in it, it is told to ignore the errors that not trusting it produces — so the
// connection is an error Chrome was told to overlook, not a validated one, and
// nothing but this browser instance is affected. Installing a root into the
// profile's NSS database or the OS keychain is the only way to make the CA
// genuinely trusted, and brw deliberately does not write to either: a tool that
// silently adds roots to a user's trust store is a tool that can silently
// intercept their traffic long after it exits.
func SPKIFingerprintsFromPEM(bundle []byte) ([]string, error) {
	var out []string
	rest := bundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate from PEM bundle: %w", err)
		}
		sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
		out = append(out, base64.StdEncoding.EncodeToString(sum[:]))
	}
	if len(out) == 0 {
		return nil, errors.New("PEM bundle contains no CERTIFICATE block")
	}
	return out, nil
}
