package extensionbridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func tokenInBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	token, _ := body["token"].(string)
	return token
}

var secFetchSiteDomain = []struct {
	name string

	header string
	absent bool

	browserSet bool
	served     bool
	why        string
}{
	{
		name: "absent", absent: true, browserSet: false, served: true,
		why: "a non-browser local client sends no Sec-Fetch-* at all; brwctl doctor is one, and refusing it would refuse a real caller to inconvenience nobody",
	},
	{
		name: "none", header: "none", browserSet: true, served: true,
		why: "the measured value for both the MV3 service worker's fetch and an extension page's fetch: no document initiated the request",
	},
	{
		name: "same-origin", header: "same-origin", browserSet: false, served: false,
		why: "the bridge serves no HTML of its own, so nothing can be same-origin with it; a caller claiming to be is not a browser",
	},
	{
		name: "same-site", header: "same-site", browserSet: true, served: false,
		why: "a page served by another local server on 127.0.0.1 is same-site with the daemon and can cause a GET carrying no Origin; it is a document, and not this extension",
	},
	{
		name: "cross-site", header: "cross-site", browserSet: true, served: false,
		why: "a page on the internet reaching the loopback daemon, measured as the value a no-cors fetch and a <script src> both arrive with",
	},
	{
		name: "an undefined value", header: "future-value", browserSet: false, served: false,
		why: "default deny: a value this guard does not recognise is not the extension's",
	},
}

func TestStatusTokenSecFetchSiteDomainHasAVerdictForEveryValue(t *testing.T) {

	origins := []struct{ name, origin string }{
		{"no Origin", ""},
		{"the configured extension", "chrome-extension://" + configuredExtnID},
	}

	servedValues := 0
	for _, value := range secFetchSiteDomain {
		if value.why == "" {
			t.Fatalf("Sec-Fetch-Site value %q has no written reason for its verdict", value.name)
		}
		if value.served && !value.absent {
			servedValues++
		}
		for _, origin := range origins {
			t.Run(value.name+"/"+origin.name, func(t *testing.T) {
				b := New("", 5*time.Second, configuredExtnID)
				b.SetAuthToken(tokenIssueSecret)
				req := httptest.NewRequest(http.MethodGet, "/status", nil)
				req.Host = tokenIssueLoopback
				if !value.absent {
					req.Header.Set("Sec-Fetch-Site", value.header)
				}
				if origin.origin != "" {
					req.Header.Set("Origin", origin.origin)
				}
				rec := httptest.NewRecorder()
				b.handleStatus(rec, req)
				got := tokenInBody(t, rec)
				if (got != "") != value.served {
					t.Fatalf("token served = %v, want %v (%s)", got != "", value.served, value.why)
				}
			})
		}
	}

	if servedValues != 1 {
		t.Fatalf("%d Sec-Fetch-Site values are served, want exactly one (%q)", servedValues, initiatorSecFetchSite)
	}
	for _, value := range secFetchSiteDomain {
		if value.served && !value.absent && value.header != initiatorSecFetchSite {
			t.Fatalf("value %q is served but is not the measured initiator value %q", value.header, initiatorSecFetchSite)
		}
	}
}

func TestConsentSurfaceUsesTheSameInitiatorRule(t *testing.T) {
	for _, path := range []string{"/consent", "/consent/revoke"} {
		for _, value := range secFetchSiteDomain {
			if value.served {
				continue
			}
			t.Run(path+"/"+value.name, func(t *testing.T) {
				b := New("", 5*time.Second, configuredExtnID)
				b.SetAuthToken(tokenIssueSecret)
				method := http.MethodGet
				if path == "/consent/revoke" {
					method = http.MethodPost
				}
				req := httptest.NewRequest(method, path, nil)
				req.Host = tokenIssueLoopback
				req.Header.Set("Sec-Fetch-Site", value.header)
				rec := httptest.NewRecorder()
				if path == "/consent" {
					b.handleConsent(rec, req)
				} else {
					b.handleConsentRevoke(rec, req)
				}
				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s answered %d to a %s caller, want 403 (%s)", path, rec.Code, value.name, value.why)
				}
			})
		}
	}
}
