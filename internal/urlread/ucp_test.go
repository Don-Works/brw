package urlread

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

const ucpProfileFixture = `{"ucp":{"version":"2026-08-25","services":{"dev.ucp.shopping":[{"version":"2026-08-25","transport":"rest","endpoint":"/do-not-fetch","schema":"/do-not-fetch"},{"transport":"mcp"},{"transport":"rest"}]},"capabilities":{"dev.ucp.shopping.order":[{"version":"2026-08-25"}],"dev.ucp.shopping.checkout":[{"version":"2026-08-25"},{"version":"2026-08-25"},{"version":"2026-01-11"}]},"payment_handlers":{"com.example.processor_tokenizer":[{"id":"processor_tokenizer","config":{"secret":"NEVER-RETURN"},"schema":"/do-not-fetch"}],"com.google.pay":[{"id":"gpay_1234"},{"id":"gpay_1234"}]},"signing_keys":[{"secret":"NEVER-RETURN"}]}}`

func TestUCPProfileDeclaredSummary(t *testing.T) {
	got := parseUCPProfile([]byte(ucpProfileFixture), false)
	want := &UCPProfile{Status: "parsed", Version: "2026-08-25", Capabilities: []UCPCapability{
		{Name: "dev.ucp.shopping.checkout", Version: "2026-01-11"},
		{Name: "dev.ucp.shopping.checkout", Version: "2026-08-25"},
		{Name: "dev.ucp.shopping.order", Version: "2026-08-25"},
	}, Transports: []string{"mcp", "rest"}, PaymentHandlers: []UCPPaymentHandler{
		{Name: "com.example.processor_tokenizer", ID: "processor_tokenizer"},
		{Name: "com.google.pay", ID: "gpay_1234"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"NEVER-RETURN", "signing_keys", "schema", "config", "endpoint"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("summary includes excluded field %q", secret)
		}
	}
}

func TestUCPProfileInvalidAndUnavailable(t *testing.T) {
	for _, body := range []string{`{`, `[]`, `null`, `{}`, `{"version":"2026-01"}`, `{"ucp":[]}`, `{"ucp":{"version":123}}`, `{"ucp":{"version":"v","capabilities":[]}}`, `{"ucp":{"version":"v","capabilities":{"checkout":{}}}}`, `{"ucp":{"version":"v","services":{"shopping":[{"transport":false}]}}}`, `{"ucp":{"version":"v","payment_handlers":{"pay":[{"id":{}}]}}}`, `{"ucp":{"version":"v"}} trailing`, "\xff"} {
		if got := parseUCPProfile([]byte(body), false); got.Status != "invalid" {
			t.Errorf("body %q: %+v", body, got)
		}
	}
	for _, value := range []struct {
		body      []byte
		oversized bool
	}{
		{[]byte(ucpProfileFixture), true}, {[]byte(strings.Repeat(" ", ucpProbeMaxBytes+1)), false},
	} {
		if got := parseUCPProfile(value.body, value.oversized); !reflect.DeepEqual(got, &UCPProfile{Status: "unavailable", Truncated: true}) {
			t.Errorf("oversize: %+v", got)
		}
	}
}

func TestUCPProfileBoundsAndDeterminism(t *testing.T) {
	caps := map[string][]ucpDeclaration{}
	handlers := map[string][]ucpDeclaration{}
	services := map[string][]ucpDeclaration{}
	for i := 19; i >= 0; i-- {
		name := fmt.Sprintf("%02d", i) + strings.Repeat("界", 170)
		caps[name] = []ucpDeclaration{{Version: strings.Repeat("版", 170)}}
		handlers[name] = []ucpDeclaration{{ID: strings.Repeat("号", 170)}}
		services[name] = []ucpDeclaration{{Transport: fmt.Sprintf("%02d", i) + strings.Repeat("運", 170)}}
	}
	body, err := json.Marshal(map[string]any{"ucp": map[string]any{"version": strings.Repeat("版", 170), "capabilities": caps, "services": services, "payment_handlers": handlers}})
	if err != nil {
		t.Fatal(err)
	}
	got := parseUCPProfile(body, false)
	if got.Status != "parsed" || !got.Truncated || len(got.Capabilities) != 12 || len(got.Transports) != 8 || len(got.PaymentHandlers) != 8 {
		t.Fatalf("bounds: %+v", got)
	}
	fields := []string{got.Version}
	for _, v := range got.Capabilities {
		fields = append(fields, v.Name, v.Version)
	}
	for _, v := range got.PaymentHandlers {
		fields = append(fields, v.Name, v.ID)
	}
	fields = append(fields, got.Transports...)
	for _, v := range fields {
		if utf8.RuneCountInString(v) > 160 {
			t.Fatal("unbounded string")
		}
	}
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(got, parseUCPProfile(body, false)) {
			t.Fatal("nondeterministic output")
		}
	}
	encoded, _ := json.Marshal(got)
	if len(encoded) > 42000 {
		t.Fatalf("summary too large: %d", len(encoded))
	}
	if !strings.HasPrefix(got.Capabilities[0].Name, "00") || !strings.HasPrefix(got.Capabilities[11].Name, "11") {
		t.Fatal("capabilities not sorted before cap")
	}
}

func TestFetchUCPProfileReusesDiscoveryRequest(t *testing.T) {
	for _, tt := range []struct{ name, body, status string }{
		{"valid", ucpProfileFixture, "parsed"},
		{"malformed", "{", "invalid"},
		{"oversize", `{"ucp":{"version":"v"},"padding":"` + strings.Repeat("x", ucpProbeMaxBytes) + `"}`, "unavailable"},
		{"exact_bound", ucpProfileFixture + strings.Repeat(" ", ucpProbeMaxBytes-len(ucpProfileFixture)), "parsed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			counts := map[string]int{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				counts[r.URL.Path]++
				mu.Unlock()
				switch r.URL.Path {
				case "/page":
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, "<html><body>"+articleBody+"</body></html>")
				case "/.well-known/ucp":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, tt.body)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			got, err := Fetch(context.Background(), Options{URL: srv.URL + "/page"})
			if err != nil {
				t.Fatal(err)
			}
			s := got.AgentSurfaces
			if s == nil || s.UCP != srv.URL+"/.well-known/ucp" || s.UCPProfile == nil || s.UCPProfile.Status != tt.status {
				t.Fatalf("surfaces: %+v", s)
			}
			mu.Lock()
			defer mu.Unlock()
			wantPaths := []string{"/page", "/page.md", "/llms.txt", "/.well-known/api-catalog", "/.well-known/ai-catalog.json", "/.well-known/ucp"}
			if len(counts) != len(wantPaths) {
				t.Fatalf("extra requests: %v", counts)
			}
			for _, path := range wantPaths {
				if counts[path] != 1 {
					t.Fatalf("request count %s: %d", path, counts[path])
				}
			}
		})
	}
}

func TestUCPProfileOmittedWithoutDeclaration(t *testing.T) {
	var empty *AgentSurfaces
	if !empty.empty() || !(&AgentSurfaces{}).empty() {
		t.Fatal("nil/empty compatibility changed")
	}
	encoded, err := json.Marshal(&AgentSurfaces{})
	if err != nil || string(encoded) != "{}" {
		t.Fatalf("empty output %s: %v", encoded, err)
	}
	srv := fixtureServer(t, map[string]fixtureRoute{"/page": {contentType: "text/html", body: "<html><body>" + articleBody + "</body></html>"}})
	got, err := Fetch(context.Background(), Options{URL: srv.URL + "/page"})
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentSurfaces.UCP != "" || got.AgentSurfaces.UCPProfile != nil {
		t.Fatalf("absent UCP: %+v", got.AgentSurfaces)
	}
}
