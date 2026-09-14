package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// environmentRecorder is a controller that also holds the page-environment
// capability, so the routes can be exercised without a browser.
type environmentRecorder struct {
	fakeController
	geolocation browser.GeolocationOptions
	network     browser.NetworkConditionsOptions
	media       browser.MediaEmulationOptions
	headers     browser.ExtraHeadersOptions
	userAgent   browser.UserAgentOptions
	credentials browser.CredentialsOptions
	download    browser.DownloadPathOptions
}

func (c *environmentRecorder) SetGeolocation(_ context.Context, opts browser.GeolocationOptions) (browser.EnvironmentResult, error) {
	c.geolocation = opts
	return browser.EnvironmentResult{OK: true, TabID: opts.TabID}, nil
}

func (c *environmentRecorder) SetNetworkConditions(_ context.Context, opts browser.NetworkConditionsOptions) (browser.EnvironmentResult, error) {
	c.network = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func (c *environmentRecorder) EmulateMedia(_ context.Context, opts browser.MediaEmulationOptions) (browser.EnvironmentResult, error) {
	c.media = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func (c *environmentRecorder) SetExtraHeaders(_ context.Context, opts browser.ExtraHeadersOptions) (browser.EnvironmentResult, error) {
	c.headers = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func (c *environmentRecorder) SetUserAgent(_ context.Context, opts browser.UserAgentOptions) (browser.EnvironmentResult, error) {
	c.userAgent = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func (c *environmentRecorder) Authenticate(_ context.Context, opts browser.CredentialsOptions) (browser.EnvironmentResult, error) {
	c.credentials = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func (c *environmentRecorder) SetDownloadPath(_ context.Context, opts browser.DownloadPathOptions) (browser.EnvironmentResult, error) {
	c.download = opts
	return browser.EnvironmentResult{OK: true}, nil
}

// The CLI drives the HTTP API rather than MCP, so every environment tool needs
// its own route or the CLI cannot reach the capability at all.
func TestEnvironmentRoutesForwardTheirBodies(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		body  string
		check func(t *testing.T, ctrl *environmentRecorder)
	}{
		{
			name: "geolocation",
			path: "/api/page/geolocation",
			body: `{"latitude":51.5007,"longitude":-0.1246,"accuracy":25,"tab_id":"42"}`,
			check: func(t *testing.T, ctrl *environmentRecorder) {
				if ctrl.geolocation.Latitude == nil || *ctrl.geolocation.Latitude != 51.5007 || ctrl.geolocation.TabID != "42" {
					t.Fatalf("geolocation options = %+v", ctrl.geolocation)
				}
			},
		},
		{
			name: "network conditions",
			path: "/api/page/network_conditions",
			body: `{"offline":true}`,
			check: func(t *testing.T, ctrl *environmentRecorder) {
				if ctrl.network.Offline == nil || !*ctrl.network.Offline {
					t.Fatalf("network options = %+v", ctrl.network)
				}
			},
		},
		{
			name: "emulated media",
			path: "/api/page/emulate_media",
			body: `{"media":"print","color_scheme":"dark"}`,
			check: func(t *testing.T, ctrl *environmentRecorder) {
				if ctrl.media.Media != "print" || ctrl.media.ColorScheme != "dark" {
					t.Fatalf("media options = %+v", ctrl.media)
				}
			},
		},
		{
			name: "extra headers",
			path: "/api/page/extra_headers",
			body: `{"origins":[{"origin":"https://api.example.com","headers":{"Authorization":"Bearer fabricated"}}]}`,
			check: func(t *testing.T, ctrl *environmentRecorder) {
				if len(ctrl.headers.Origins) != 1 || ctrl.headers.Origins[0].Origin != "https://api.example.com" {
					t.Fatalf("header options = %+v", ctrl.headers)
				}
			},
		},
		{
			name: "user agent",
			path: "/api/page/user_agent",
			body: `{"user_agent":"Fabricated/1.0","accept_language":"fr-FR","platform":"FabricatedOS"}`,
			check: func(t *testing.T, ctrl *environmentRecorder) {
				if ctrl.userAgent.UserAgent != "Fabricated/1.0" || ctrl.userAgent.Platform != "FabricatedOS" {
					t.Fatalf("user agent options = %+v", ctrl.userAgent)
				}
			},
		},
		{
			name: "credentials",
			path: "/api/page/authenticate",
			body: `{"origin":"https://staging.example.com","username":"u","password":"p"}`,
			check: func(t *testing.T, ctrl *environmentRecorder) {
				if ctrl.credentials.Origin != "https://staging.example.com" || ctrl.credentials.Password != "p" {
					t.Fatalf("credential options = %+v", ctrl.credentials)
				}
			},
		},
		{
			name: "download path",
			path: "/api/browser/download_path",
			body: `{"path":"/tmp/brw-fixture-downloads"}`,
			check: func(t *testing.T, ctrl *environmentRecorder) {
				if ctrl.download.Path != "/tmp/brw-fixture-downloads" {
					t.Fatalf("download options = %+v", ctrl.download)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := &environmentRecorder{}
			server := New("", ctrl)
			req := httptest.NewRequest(http.MethodPost, tt.path, bytes.NewBufferString(tt.body))
			req.Header.Set("content-type", "application/json")
			rec := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var result browser.EnvironmentResult
			if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if !result.OK {
				t.Fatalf("result = %+v", result)
			}
			tt.check(t, ctrl)
		})
	}
}

// A transport without the capability has to say which transport it is and why,
// on the HTTP surface as well as over MCP.
func TestEnvironmentRoutesNameTheMissingCapability(t *testing.T) {
	paths := []string{
		"/api/page/geolocation",
		"/api/page/network_conditions",
		"/api/page/emulate_media",
		"/api/page/extra_headers",
		"/api/page/user_agent",
		"/api/page/authenticate",
		"/api/browser/download_path",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			server := New("", &fakeController{})
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
			req.Header.Set("content-type", "application/json")
			rec := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Error != browser.ErrEnvironmentUnsupported.Error() {
				t.Fatalf("error = %q, want the named capability error", body.Error)
			}
		})
	}
}

// The credential route is the one body that must never come back in an error.
// encoding/json quotes the input it choked on, and here that input is a
// password.
func TestAuthenticateRouteDoesNotEchoTheBodyOnADecodeError(t *testing.T) {
	ctrl := &environmentRecorder{}
	server := New("", ctrl)
	const fixtureCredential = "fabricated-pw-2b7e"
	req := httptest.NewRequest(http.MethodPost, "/api/page/authenticate",
		bytes.NewBufferString(`{"origin":"https://staging.example.com","password":`+fixtureCredential+`}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), fixtureCredential) {
		t.Fatalf("the rejected body was echoed back: %s", rec.Body.String())
	}
}
