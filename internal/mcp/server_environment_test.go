package mcp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
)

// environmentToolNames is every tool backed by the page-environment capability.
// Kept as one list so a tool added to the capability and forgotten in the
// transport table fails a test rather than being advertised where it cannot run.
var environmentToolNames = []string{
	"brw_set_geolocation",
	"brw_set_network_conditions",
	"brw_emulate_media",
	"brw_set_extra_headers",
	"brw_set_user_agent",
	"brw_set_locale",
	"brw_authenticate",
	"brw_set_download_path",
}

func TestEnvironmentToolsAreNotAdvertisedOnTheExtensionBridge(t *testing.T) {
	bridge := NewWithToolProfile(nil, "all")
	bridge.SetIdentity(brwidentity.Identity{Transport: brwidentity.TransportExtensionBridge})
	advertisedOnBridge := advertisedNames(bridge)

	direct := NewWithToolProfile(nil, "all")
	direct.SetIdentity(brwidentity.Identity{Transport: brwidentity.TransportDirectCDP})
	advertisedOnDirect := advertisedNames(direct)

	for _, name := range environmentToolNames {
		if advertisedOnBridge[name] {
			t.Errorf("%s is advertised on the extension bridge, where it can only ever return a capability error", name)
		}
		if !advertisedOnDirect[name] {
			t.Errorf("%s is not advertised on direct CDP, where it works", name)
		}
	}
}

// An advertised-nowhere tool is still callable, so the refusal has to say which
// transport this is and why, not merely "unsupported".
func TestEnvironmentToolsReturnTheNamedCapabilityError(t *testing.T) {
	for _, name := range environmentToolNames {
		t.Run(name, func(t *testing.T) {
			input := lineJSON(t, map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  "tools/call",
				"params":  map[string]any{"name": name, "arguments": map[string]any{}},
			})
			var output bytes.Buffer
			// fakeController implements browser.Controller and nothing else, which
			// is exactly the shape of a transport without the capability.
			if err := New(fakeController{}).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			resp := parseLineResponse(t, output.Bytes())
			result, ok := resp["result"].(map[string]any)
			if !ok {
				t.Fatalf("no result in %#v", resp)
			}
			if isError, _ := result["isError"].(bool); !isError {
				t.Fatalf("%s succeeded on a transport without the capability: %#v", name, result)
			}
			content := result["content"].([]any)
			text := content[0].(map[string]any)["text"].(string)
			if text != browser.ErrEnvironmentUnsupported.Error() {
				t.Fatalf("error text = %q, want the named capability error %q", text, browser.ErrEnvironmentUnsupported.Error())
			}
		})
	}
}

// recordingEnvironmentController captures what each tool forwarded, so a schema
// whose field names drift from the options struct is caught here rather than by
// an agent whose override silently did nothing.
type recordingEnvironmentController struct {
	fakeController
	geolocation browser.GeolocationOptions
	network     browser.NetworkConditionsOptions
	media       browser.MediaEmulationOptions
	headers     browser.ExtraHeadersOptions
	userAgent   browser.UserAgentOptions
	locale      browser.LocaleOptions
	credentials browser.CredentialsOptions
	download    browser.DownloadPathOptions
}

func (c *recordingEnvironmentController) SetGeolocation(_ context.Context, opts browser.GeolocationOptions) (browser.EnvironmentResult, error) {
	c.geolocation = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func (c *recordingEnvironmentController) SetNetworkConditions(_ context.Context, opts browser.NetworkConditionsOptions) (browser.EnvironmentResult, error) {
	c.network = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func (c *recordingEnvironmentController) EmulateMedia(_ context.Context, opts browser.MediaEmulationOptions) (browser.EnvironmentResult, error) {
	c.media = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func (c *recordingEnvironmentController) SetExtraHeaders(_ context.Context, opts browser.ExtraHeadersOptions) (browser.EnvironmentResult, error) {
	c.headers = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func (c *recordingEnvironmentController) SetUserAgent(_ context.Context, opts browser.UserAgentOptions) (browser.EnvironmentResult, error) {
	c.userAgent = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func (c *recordingEnvironmentController) SetLocale(_ context.Context, opts browser.LocaleOptions) (browser.EnvironmentResult, error) {
	c.locale = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func (c *recordingEnvironmentController) Authenticate(_ context.Context, opts browser.CredentialsOptions) (browser.EnvironmentResult, error) {
	c.credentials = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func (c *recordingEnvironmentController) SetDownloadPath(_ context.Context, opts browser.DownloadPathOptions) (browser.EnvironmentResult, error) {
	c.download = opts
	return browser.EnvironmentResult{OK: true}, nil
}

func TestEnvironmentToolsForwardTheirArguments(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		args  map[string]any
		check func(t *testing.T, ctrl *recordingEnvironmentController)
	}{
		{
			name: "geolocation",
			tool: "brw_set_geolocation",
			args: map[string]any{"latitude": 51.5007, "longitude": -0.1246, "accuracy": 25},
			check: func(t *testing.T, ctrl *recordingEnvironmentController) {
				if ctrl.geolocation.Latitude == nil || *ctrl.geolocation.Latitude != 51.5007 {
					t.Fatalf("latitude = %v", ctrl.geolocation.Latitude)
				}
				if ctrl.geolocation.Longitude == nil || *ctrl.geolocation.Longitude != -0.1246 {
					t.Fatalf("longitude = %v", ctrl.geolocation.Longitude)
				}
				if ctrl.geolocation.Accuracy == nil || *ctrl.geolocation.Accuracy != 25 {
					t.Fatalf("accuracy = %v", ctrl.geolocation.Accuracy)
				}
			},
		},
		{
			name: "network conditions",
			tool: "brw_set_network_conditions",
			args: map[string]any{"offline": true, "latency_ms": 150, "download_throughput": -1},
			check: func(t *testing.T, ctrl *recordingEnvironmentController) {
				if ctrl.network.Offline == nil || !*ctrl.network.Offline {
					t.Fatalf("offline = %v", ctrl.network.Offline)
				}
				if ctrl.network.LatencyMS == nil || *ctrl.network.LatencyMS != 150 {
					t.Fatalf("latency = %v", ctrl.network.LatencyMS)
				}
			},
		},
		{
			name: "emulated media",
			tool: "brw_emulate_media",
			args: map[string]any{"media": "print", "color_scheme": "dark", "reduced_motion": "reduce"},
			check: func(t *testing.T, ctrl *recordingEnvironmentController) {
				if ctrl.media.Media != "print" || ctrl.media.ColorScheme != "dark" || ctrl.media.ReducedMotion != "reduce" {
					t.Fatalf("media options = %+v", ctrl.media)
				}
			},
		},
		{
			name: "extra headers",
			tool: "brw_set_extra_headers",
			args: map[string]any{"origins": []any{map[string]any{
				"origin":  "https://api.example.com",
				"headers": map[string]any{"Authorization": "Bearer fabricated"},
			}}},
			check: func(t *testing.T, ctrl *recordingEnvironmentController) {
				if len(ctrl.headers.Origins) != 1 {
					t.Fatalf("origins = %+v", ctrl.headers.Origins)
				}
				entry := ctrl.headers.Origins[0]
				if entry.Origin != "https://api.example.com" || entry.Headers["Authorization"] != "Bearer fabricated" {
					t.Fatalf("origin entry = %+v", entry)
				}
			},
		},
		{
			name: "user agent",
			tool: "brw_set_user_agent",
			args: map[string]any{"user_agent": "Fabricated/1.0", "accept_language": "fr-FR", "platform": "FabricatedOS"},
			check: func(t *testing.T, ctrl *recordingEnvironmentController) {
				if ctrl.userAgent.UserAgent != "Fabricated/1.0" || ctrl.userAgent.AcceptLanguage != "fr-FR" || ctrl.userAgent.Platform != "FabricatedOS" {
					t.Fatalf("user agent options = %+v", ctrl.userAgent)
				}
			},
		},
		{
			name: "locale",
			tool: "brw_set_locale",
			args: map[string]any{"locale": "en-GB", "timezone": "Europe/London"},
			check: func(t *testing.T, ctrl *recordingEnvironmentController) {
				if ctrl.locale.Locale != "en-GB" || ctrl.locale.Timezone != "Europe/London" {
					t.Fatalf("locale options = %+v", ctrl.locale)
				}
			},
		},
		{
			name: "credentials",
			tool: "brw_authenticate",
			args: map[string]any{"origin": "https://staging.example.com", "username": "u", "password": "p", "url": "https://staging.example.com/x"},
			check: func(t *testing.T, ctrl *recordingEnvironmentController) {
				if ctrl.credentials.Origin != "https://staging.example.com" || ctrl.credentials.Username != "u" || ctrl.credentials.Password != "p" {
					t.Fatalf("credential options = %+v", ctrl.credentials)
				}
				if ctrl.credentials.URL != "https://staging.example.com/x" {
					t.Fatalf("credential url = %q", ctrl.credentials.URL)
				}
			},
		},
		{
			name: "download path",
			tool: "brw_set_download_path",
			args: map[string]any{"path": "/tmp/brw-fixture-downloads"},
			check: func(t *testing.T, ctrl *recordingEnvironmentController) {
				if ctrl.download.Path != "/tmp/brw-fixture-downloads" {
					t.Fatalf("download path = %q", ctrl.download.Path)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := &recordingEnvironmentController{}
			input := lineJSON(t, map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  "tools/call",
				"params":  map[string]any{"name": tt.tool, "arguments": tt.args},
			})
			var output bytes.Buffer
			if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			resp := parseLineResponse(t, output.Bytes())
			result, ok := resp["result"].(map[string]any)
			if !ok {
				t.Fatalf("no result in %#v", resp)
			}
			if isError, _ := result["isError"].(bool); isError {
				t.Fatalf("%s failed: %#v", tt.tool, result)
			}
			tt.check(t, ctrl)
		})
	}
}

// brw_authenticate decodes strictly like every other tool: a misspelled
// "password" has to be an argument error, not an empty password, a challenge
// that is never answered and a caller left to work out why the server "never
// asked". Strictness and not echoing the body are independent, so the error is
// still the constant one rather than the decoder's, which quotes the JSON it
// choked on — and here that JSON is the credential.
func TestAuthenticateIsStrictAboutFieldsAndStillNeverEchoesThem(t *testing.T) {
	const fixtureCredential = "fabricated-pw-8f14"
	tests := []struct {
		name      string
		args      map[string]any
		wantError bool
	}{
		{
			name: "a misspelled password",
			args: map[string]any{"origin": "https://staging.example.com", "username": "u", "passwrd": fixtureCredential},
			// A field this tool does not define cannot be silently dropped: the
			// call would load the page unauthenticated and say so obscurely.
			wantError: true,
		},
		{
			name:      "the fields the tool defines",
			args:      map[string]any{"origin": "https://staging.example.com", "username": "u", "password": fixtureCredential},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := &recordingEnvironmentController{}
			input := lineJSON(t, map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  "tools/call",
				"params":  map[string]any{"name": "brw_authenticate", "arguments": tt.args},
			})
			var output bytes.Buffer
			if err := New(ctrl).Serve(context.Background(), strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), fixtureCredential) {
				t.Fatalf("the password was echoed back to the caller in %s", output.String())
			}
			resp := parseLineResponse(t, output.Bytes())
			_, failed := resp["error"]
			if failed != tt.wantError {
				t.Fatalf("response = %#v, want error %v", resp, tt.wantError)
			}
			if tt.wantError && ctrl.credentials.Origin != "" {
				t.Fatalf("the call reached the controller as %+v despite the unknown field", ctrl.credentials)
			}
		})
	}
}
