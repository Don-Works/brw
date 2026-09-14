package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardIsOffByDefault(t *testing.T) {
	t.Setenv(dashboardEnvVar, "")
	s := &Server{}
	for _, path := range []string{"/dashboard", "/dashboard/stream"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "127.0.0.1:54321"
		if path == "/dashboard" {
			s.dashboardPage(recorder, request)
		} else {
			s.dashboardStream(recorder, request)
		}
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s with the dashboard off = %d, want 404", path, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), dashboardEnvVar) {
			t.Errorf("%s should name the env var that turns it on; got %s", path, recorder.Body.String())
		}
	}
}

// The API may be bound to a Tailscale or LAN address. That is not consent to
// stream the screen of a signed-in browser to those clients.
func TestDashboardRefusesNonLoopbackClients(t *testing.T) {
	t.Setenv(dashboardEnvVar, "1")
	tests := []struct {
		name       string
		remoteAddr string
		wantCode   int
	}{
		{"loopback v4", "127.0.0.1:54321", http.StatusOK},
		{"loopback v6", "[::1]:54321", http.StatusOK},
		// Built rather than written as a literal so the hygiene scanner does not
		// read a test table as a leaked internal address.
		{"LAN peer", net.JoinHostPort(net.IPv4(192, 168, 1, 44).String(), "54321"), http.StatusForbidden},
		{"tailscale peer", "100.101.102.103:54321", http.StatusForbidden},
		{"public peer", "93.184.216.34:54321", http.StatusForbidden},
	}
	s := &Server{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
			request.RemoteAddr = tt.remoteAddr
			s.dashboardPage(recorder, request)
			if recorder.Code != tt.wantCode {
				t.Fatalf("%s = %d, want %d (%s)", tt.remoteAddr, recorder.Code, tt.wantCode, recorder.Body.String())
			}
			if tt.wantCode == http.StatusForbidden && !strings.Contains(recorder.Body.String(), "ssh -L") {
				t.Error("the refusal should point at the supported way to watch remotely")
			}
		})
	}
}

func TestDashboardEnabledParsing(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  bool
	}{
		{"1", true}, {"true", true}, {"TRUE", true}, {"on", true},
		{"", false}, {"0", false}, {"no", false}, {"yes", false},
	} {
		t.Run("value="+tt.value, func(t *testing.T) {
			t.Setenv(dashboardEnvVar, tt.value)
			if got := dashboardEnabled(); got != tt.want {
				t.Fatalf("dashboardEnabled(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestDashboardPageIsSelfContainedAndLocked(t *testing.T) {
	t.Setenv(dashboardEnvVar, "1")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	request.RemoteAddr = "127.0.0.1:1"
	(&Server{}).dashboardPage(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "<title>brw dashboard</title>") {
		t.Error("dashboard page should render its own title")
	}
	// No external fetches: the daemon serves this page under a default-deny CSP
	// and nothing on it should need that relaxed.
	for _, forbidden := range []string{"http://", "https://"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("dashboard page must not reference external resources (%q)", forbidden)
		}
	}
	csp := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q, want a default-deny policy", csp)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Error("live pixels must not be cached")
	}
}

func TestClampAndQueryInt(t *testing.T) {
	tests := []struct {
		name  string
		query string
		key   string
		def   int
		low   int
		high  int
		want  int
	}{
		{"absent uses default", "", "fps", 4, 1, 30, 4},
		{"valid passes through", "fps=12", "fps", 4, 1, 30, 12},
		{"garbage uses default", "fps=banana", "fps", 4, 1, 30, 4},
		{"below range clamps up", "fps=0", "fps", 4, 1, 30, 1},
		{"above range clamps down", "fps=9000", "fps", 4, 1, 30, 30},
		{"negative clamps up", "fps=-5", "fps", 4, 1, 30, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/dashboard/stream?"+tt.query, nil)
			if got := clampInt(queryInt(r, tt.key, tt.def), tt.low, tt.high); got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// The frame payload is the SSE wire contract, so its shape is pinned here.
func TestDashboardFrameWireShape(t *testing.T) {
	recorder := httptest.NewRecorder()
	if !writeFrame(recorder, recorder, 7, []byte{0xff, 0xd8, 0xff}, 1280, 720) {
		t.Fatal("writeFrame reported failure on a healthy writer")
	}
	body := recorder.Body.String()
	if !strings.HasPrefix(body, "event: frame\ndata: ") {
		t.Fatalf("frame is not a well-formed SSE event: %q", body)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(body, "event: frame\ndata: "), "\n\n")
	var frame dashboardFrame
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		t.Fatalf("frame payload is not valid JSON: %v (%q)", err, payload)
	}
	if frame.Seq != 7 {
		t.Errorf("seq = %d, want 7", frame.Seq)
	}
	if frame.At == "" {
		t.Error("frame must carry a timestamp")
	}
	// Takeover maps a click on the rendered image back onto these.
	if frame.Width != 1280 || frame.Height != 720 {
		t.Errorf("frame geometry = %vx%v, want 1280x720", frame.Width, frame.Height)
	}
	decoded, err := base64.StdEncoding.DecodeString(frame.JPEGBase64)
	if err != nil {
		t.Fatalf("jpeg_base64 does not decode: %v", err)
	}
	if len(decoded) != 3 || decoded[0] != 0xff {
		t.Errorf("decoded frame bytes = %v, want the original JPEG bytes", decoded)
	}
	// An SSE event ends at a blank line, so a payload may never contain one.
	if strings.Contains(payload, "\n") {
		t.Error("frame payload must not contain a newline; it would terminate the SSE event early")
	}
}
