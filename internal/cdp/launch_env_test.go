package cdp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestLaunchArgsCarryTheNetworkEnvironment(t *testing.T) {
	tests := []struct {
		name    string
		env     NetworkEnvironment
		want    []string
		unwant  []string
		wantErr bool
	}{
		{
			name:   "no network settings adds no switches",
			env:    NetworkEnvironment{},
			unwant: []string{"--proxy-server", "--proxy-bypass-list", "--ignore-certificate-errors", "--ignore-certificate-errors-spki-list"},
		},
		{
			name: "proxy with a bypass list",
			env:  NetworkEnvironment{ProxyServer: "http://127.0.0.1:8080", ProxyBypassList: "<local>;*.internal"},
			want: []string{"--proxy-server=http://127.0.0.1:8080", "--proxy-bypass-list=<local>;*.internal"},
		},
		{
			name:   "ignore https errors is opt-in",
			env:    NetworkEnvironment{IgnoreHTTPSErrors: true},
			want:   []string{"--ignore-certificate-errors"},
			unwant: []string{"--ignore-certificate-errors-spki-list="},
		},
		{
			name: "trusted keys are one comma-separated switch",
			env:  NetworkEnvironment{TrustedSPKI: []string{"aGFzaC1vbmU=", "aGFzaC10d28="}},
			want: []string{"--ignore-certificate-errors-spki-list=aGFzaC1vbmU=,aGFzaC10d28="},
			// Naming one CA must not turn validation off for everything else.
			unwant: []string{"--ignore-certificate-errors="},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := launchArgs(LaunchConfig{UserDataDir: "/tmp/brw-fixture", Network: tt.env}, 9999)
			joined := strings.Join(args, " ")
			for _, want := range tt.want {
				if !containsArg(args, want) {
					t.Errorf("launch args are missing %q; got %s", want, joined)
				}
			}
			for _, unwanted := range tt.unwant {
				for _, arg := range args {
					if strings.HasPrefix(arg, unwanted) {
						t.Errorf("launch args contain %q, which was not asked for; got %s", arg, joined)
					}
				}
			}
		})
	}
}

// An operator's own --chrome-arg has to win, because it is the escape hatch when
// brw's own switch is wrong for their setup.
func TestExplicitChromeArgOverridesTheNetworkEnvironment(t *testing.T) {
	args := launchArgs(LaunchConfig{
		UserDataDir: "/tmp/brw-fixture",
		Network:     NetworkEnvironment{ProxyServer: "http://127.0.0.1:8080"},
		Args:        []string{"--proxy-server=http://127.0.0.1:9090"},
	}, 9999)
	first, last := -1, -1
	for i, arg := range args {
		if strings.HasPrefix(arg, "--proxy-server=") {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first == last {
		t.Fatalf("expected both proxy switches on the command line, got %v", args)
	}
	if args[last] != "--proxy-server=http://127.0.0.1:9090" {
		t.Fatalf("last --proxy-server is %q; Chrome keeps the last value, so the operator's own switch must come last", args[last])
	}
}

// Chrome's command line is whitespace-separated, so a value carrying a space is
// a way to append a switch nobody reviewed.
func TestNetworkEnvironmentRejectsSmuggledSwitches(t *testing.T) {
	tests := []struct {
		name string
		env  NetworkEnvironment
	}{
		{"space in the proxy", NetworkEnvironment{ProxyServer: "http://127.0.0.1:8080 --unsafely-treat-insecure-origin-as-secure=http://evil"}},
		{"newline in the bypass list", NetworkEnvironment{ProxyServer: "http://127.0.0.1:8080", ProxyBypassList: "<local>\n--headless"}},
		{"proxy that is itself a switch", NetworkEnvironment{ProxyServer: "--headless"}},
		{"bypass list with no proxy", NetworkEnvironment{ProxyBypassList: "<local>"}},
		{"SPKI hash that is not base64", NetworkEnvironment{TrustedSPKI: []string{"not base64!"}}},
		{"SPKI hash carrying the list separator", NetworkEnvironment{TrustedSPKI: []string{"aGFzaA==,aGFzaA=="}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.env.Validate(); err == nil {
				t.Fatalf("Validate() accepted %+v", tt.env)
			}
		})
	}
}

func TestSPKIFingerprintsMatchTheCertificatesPublicKey(t *testing.T) {
	certPEM, cert := selfSignedCertificate(t)

	fingerprints, err := SPKIFingerprintsFromPEM(certPEM)
	if err != nil {
		t.Fatalf("SPKIFingerprintsFromPEM: %v", err)
	}
	if len(fingerprints) != 1 {
		t.Fatalf("got %d fingerprints, want 1", len(fingerprints))
	}
	// Chrome hashes the DER SubjectPublicKeyInfo and base64s it; anything else
	// produces a switch Chrome accepts and silently never matches.
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	if want := base64.StdEncoding.EncodeToString(sum[:]); fingerprints[0] != want {
		t.Fatalf("fingerprint = %q, want the base64 SHA-256 of the SubjectPublicKeyInfo %q", fingerprints[0], want)
	}
	if err := (NetworkEnvironment{TrustedSPKI: fingerprints}).Validate(); err != nil {
		t.Fatalf("a fingerprint this package produced was rejected by its own validation: %v", err)
	}
}

func TestSPKIFingerprintsRejectABundleWithNoCertificate(t *testing.T) {
	notACert := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("fabricated")})
	if _, err := SPKIFingerprintsFromPEM(notACert); err == nil {
		t.Fatal("a PEM bundle with no CERTIFICATE block was accepted")
	}
	if _, err := SPKIFingerprintsFromPEM([]byte("not pem at all")); err == nil {
		t.Fatal("a non-PEM input was accepted")
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// selfSignedCertificate builds a throwaway certificate in-process, so no
// fixture file and no machine-specific material is involved.
func selfSignedCertificate(t *testing.T) ([]byte, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "brw test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), parsed
}
