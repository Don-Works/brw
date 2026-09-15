package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/devtools"
	"github.com/Don-Works/brw/internal/devtools/axe"
)

// auditFixture fails three axe rules on purpose. #bbbbbb on white is about
// 1.9:1 where 4.5:1 is required, the image has no alt text, and the button has
// no accessible name.
const auditFixture = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>MCP audit fixture</title></head><body>
<h1>MCP audit fixture</h1>
<p id="low-contrast" style="color:#bbbbbb;background-color:#ffffff">Unreadable on purpose.</p>
<img id="no-alt" width="30" height="30" src="data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7">
<button id="no-name"></button>
</body></html>`

// auditSummaryCeiling is what "bounded" has to mean to be worth anything: the
// summary must stay small enough to read in a model's context regardless of how
// large the audit was. The full report for this fixture is already several
// times this.
const auditSummaryCeiling = 8 << 10

// newLiveAuditServer wires the real pieces the audit tool needs — a headless
// Chrome, a real artifact store on disk, and the MCP server in front of both —
// because what this file is testing is the handoff between them.
func newLiveAuditServer(t *testing.T) (*Server, string) {
	t.Helper()
	chromePath, err := cdp.FindChrome("")
	if err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	profile := browsertest.NewProfile(t)
	manager, err := browser.New(ctx, browser.Config{
		ChromePath:  chromePath,
		UserDataDir: profile.Dir(),
		Headless:    true,
		Timeout:     45 * time.Second,
	})
	if err != nil {
		t.Skipf("headless Chrome did not start: %v", err)
	}
	profile.StopWith(func() { _ = manager.Close() })

	store, err := artifact.NewStore(artifact.Config{
		Root:             filepath.Join(t.TempDir(), "artifacts"),
		MaxArtifactBytes: 8 << 20,
		MaxTotalBytes:    32 << 20,
		TTL:              time.Hour,
	})
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	service, err := artifact.NewService(store, manager)
	if err != nil {
		t.Fatalf("artifact service: %v", err)
	}

	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, auditFixture)
	}))
	t.Cleanup(fixture.Close)
	if _, err := manager.Open(ctx, fixture.URL); err != nil {
		t.Fatalf("open the fixture: %v", err)
	}

	server := New(manager)
	server.SetArtifactAPI(service)
	return server, fixture.URL
}

// TestAccessibilityAuditPutsTheReportInAnArtifactAndAnswersWithASummary is the
// whole arrangement in one test: real page, real engine, real store. The tool
// answer must be small and must not contain the report; the artifact must
// contain it and be readable through the artifact tools.
func TestAccessibilityAuditPutsTheReportInAnArtifactAndAnswersWithASummary(t *testing.T) {
	server, fixtureURL := newLiveAuditServer(t)

	answer := callToolJSON(t, server, "brw_a11y_audit", `{}`)
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("re-encode the answer: %v", err)
	}
	if len(encoded) > auditSummaryCeiling {
		t.Fatalf("the summary is %d bytes, want at most %d — the artifact exists so this stays small", len(encoded), auditSummaryCeiling)
	}
	if _, leaked := answer["report"]; leaked {
		t.Fatal("the tool answer carries the full axe report; it belongs in the artifact")
	}
	if url, _ := answer["url"].(string); url != fixtureURL+"/" && url != fixtureURL {
		t.Errorf("url = %q, want the fixture page %q", url, fixtureURL)
	}

	rules, _ := answer["rules"].([]any)
	found := map[string]bool{}
	for _, entry := range rules {
		rule, _ := entry.(map[string]any)
		id, _ := rule["id"].(string)
		found[id] = true
		if id != "color-contrast" {
			continue
		}
		refs, _ := rule["refs"].([]any)
		if len(refs) == 0 {
			t.Errorf("color-contrast came back with no refs: %v", rule)
		}
	}
	if !found["color-contrast"] {
		t.Fatalf("the deliberately unreadable paragraph was not reported; rules = %v", rules)
	}

	handle, _ := answer["artifact"].(map[string]any)
	if handle == nil {
		t.Fatal("no artifact handle in the answer; the full report was not stored")
	}
	artifactID, _ := handle["artifact_id"].(string)
	if artifactID == "" {
		t.Fatalf("artifact handle has no id: %v", handle)
	}
	size, _ := handle["size_bytes"].(float64)
	if int(size) <= len(encoded) {
		t.Errorf("stored report is %d bytes and the summary is %d; the report should be the larger document", int(size), len(encoded))
	}

	// The handle has to be usable through the ordinary artifact tools, or it is
	// just a string. Reading it back is the only proof of that.
	chunk := callToolJSON(t, server, "brw_artifact_read",
		fmt.Sprintf(`{"artifact_id":%q,"max_bytes":%d}`, artifactID, artifact.MaxReadBytes))
	text, _ := chunk["text"].(string)
	if !strings.Contains(text, "color-contrast") {
		t.Errorf("the stored report does not name the contrast failure: %.400s", text)
	}
	if !strings.Contains(text, `"brw_ref"`) {
		t.Error("the stored report carries no brw_ref annotations")
	}
	// The rules the summary had no room for still have to be in the report;
	// otherwise the artifact is a copy of the summary and buys nothing.
	if !strings.Contains(text, "image-alt") {
		t.Error("the stored report does not mention the missing alt text")
	}
}

// TestAccessibilityAuditAnswersWithItsPageEffectsAndHonoursATTL: the audit is
// read-shaped but it injects an engine and stores the raw HTML of every failing
// element. Both are the caller's business, and neither is visible from the
// counts.
func TestAccessibilityAuditAnswersWithItsPageEffectsAndHonoursATTL(t *testing.T) {
	server, _ := newLiveAuditServer(t)

	answer := callToolJSON(t, server, "brw_a11y_audit", `{"rules":["color-contrast"]}`)
	effects, _ := answer["page_effects"].(string)
	for _, want := range []string{"data-brw-ref", "window.axe", axe.Version} {
		if !strings.Contains(effects, want) {
			t.Errorf("page_effects = %q, want it to mention %q", effects, want)
		}
	}

	// A caller on a page holding real data has to be able to bound how long the
	// report survives. The store here keeps artifacts for an hour.
	bounded := callToolJSON(t, server, "brw_a11y_audit", `{"rules":["color-contrast"],"ttl_seconds":120}`)
	handle, _ := bounded["artifact"].(map[string]any)
	if handle == nil {
		t.Fatalf("no artifact handle with a ttl: %v", bounded)
	}
	expires, _ := handle["expires_at"].(string)
	at, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		t.Fatalf("expires_at = %q: %v", expires, err)
	}
	if left := time.Until(at); left > 10*time.Minute {
		t.Fatalf("the report expires in %v with ttl_seconds:120; the caller's retention was ignored", left)
	}
}

// TestAccessibilityAuditSaysSoWhenThereIsNowhereToStoreTheReport: a daemon run
// with --artifact-dir off still has to answer, and has to say the full report
// is gone rather than imply the summary is everything axe found.
func TestAccessibilityAuditSaysSoWhenThereIsNowhereToStoreTheReport(t *testing.T) {
	server, _ := newLiveAuditServer(t)
	server.SetArtifactAPI(nil)

	answer := callToolJSON(t, server, "brw_a11y_audit", `{}`)
	if _, stored := answer["artifact"]; stored {
		t.Fatal("an artifact handle came back with no store configured")
	}
	note, _ := answer["note"].(string)
	if !strings.Contains(note, "no artifact store") {
		t.Fatalf("note = %q, want it to say the full report was not kept", note)
	}
	if violations, _ := answer["violations"].(float64); violations < 1 {
		t.Fatalf("violations = %v, want the fixture's deliberate failures still reported", violations)
	}
}

// TestVitalsAndHighlightDispatchThroughTheToolSurface covers the other two
// tools end to end over the MCP call path, including the reversibility the
// highlight tool promises in its description.
func TestVitalsAndHighlightDispatchThroughTheToolSurface(t *testing.T) {
	server, _ := newLiveAuditServer(t)

	vitals := callToolJSON(t, server, "brw_vitals", `{"settle_ms":400}`)
	if ttfb, ok := vitals["ttfb_ms"].(float64); !ok || ttfb <= 0 {
		t.Fatalf("ttfb_ms = %v, want a real time-to-first-byte", vitals["ttfb_ms"])
	}
	ratings, _ := vitals["ratings"].(map[string]any)
	if ratings["ttfb"] == "" || ratings["ttfb"] == nil {
		t.Errorf("ratings = %v, want every metric labelled", ratings)
	}

	audit := callToolJSON(t, server, "brw_a11y_audit", `{"rules":["color-contrast"],"max_refs":1}`)
	rules, _ := audit["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("scoped audit returned %d rules, want exactly the one asked for", len(rules))
	}
	rule, _ := rules[0].(map[string]any)
	refs, _ := rule["refs"].([]any)
	if len(refs) != 1 {
		t.Fatalf("max_refs:1 returned %d refs", len(refs))
	}
	ref, _ := refs[0].(string)

	marked := callToolJSON(t, server, "brw_highlight", fmt.Sprintf(`{"ref":%q,"color":"blue"}`, ref))
	if active, _ := marked["active"].(float64); active != 1 {
		t.Fatalf("highlight active = %v, want 1", marked["active"])
	}
	reversible, _ := marked["reversible"].(string)
	if !strings.Contains(reversible, devtools.HighlightHostID) {
		t.Errorf("reversible = %q, want it to name what was added", reversible)
	}

	cleared := callToolJSON(t, server, "brw_highlight", `{"clear":true}`)
	if active, _ := cleared["active"].(float64); active != 0 {
		t.Fatalf("active = %v after a clear, want 0", cleared["active"])
	}
	if ok, _ := cleared["cleared"].(bool); !ok {
		t.Fatalf("cleared = %v, want the overlay reported gone", cleared["cleared"])
	}
}

// TestDevtoolsToolsAreAdvertisedWithWhatTheyPromise pins the three catalogue
// entries and the specific claims an agent is entitled to rely on.
func TestDevtoolsToolsAreAdvertisedWithWhatTheyPromise(t *testing.T) {
	tests := []struct {
		name       string
		properties []string
		claims     []string
	}{
		{
			name:       "brw_vitals",
			properties: []string{"settle_ms", "tab_id"},
			claims: []string{"LCP", "CLS", "INP", "TTFB",
				// Both are things the code now does and an agent would
				// otherwise have to discover from a surprising number.
				"interactions counts distinct interactions", "named in unavailable"},
		},
		{
			name:       "brw_a11y_audit",
			properties: []string{"tags", "rules", "include_passes", "max_rules", "max_refs", "ttl_seconds", "tab_id"},
			claims: []string{"artifact", "embedded in the brw binary", "fetches nothing over the network",
				// The audit installs half a megabyte of engine and leaves it
				// there, and stores the raw HTML of every failing element.
				// Both were advertised away and are now stated.
				"NOT effect-free", "left installed as window.axe", "no redaction"},
		},
		{
			name:       "brw_highlight",
			properties: []string{"ref", "refs", "clear", "label", "color", "duration_ms", "scroll", "tab_id"},
			claims:     []string{devtools.HighlightHostID, "clear:true removes it", "never restyled"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			definition := toolByName(t, tt.name)
			description, _ := definition["description"].(string)
			for _, claim := range tt.claims {
				if !strings.Contains(description, claim) {
					t.Errorf("%s description does not say %q: %q", tt.name, claim, description)
				}
			}
			props := toolProperties(t, tt.name)
			for _, property := range tt.properties {
				if _, ok := props[property]; !ok {
					t.Errorf("%s does not advertise %q, so an agent cannot use it", tt.name, property)
				}
			}
		})
	}

	// The tag list belongs to one place. Burying it in prose while the sibling
	// colour field is generated from HighlightColorNames() is how the two
	// drift, and how AuditTagNames ends up exported and called from nowhere.
	tagsSchema, _ := toolProperties(t, "brw_a11y_audit")["tags"].(map[string]any)
	tagsDescription, _ := tagsSchema["description"].(string)
	for _, tag := range devtools.AuditTagNames() {
		if !strings.Contains(tagsDescription, tag) {
			t.Errorf("the tags schema does not name %q: %q", tag, tagsDescription)
		}
	}
	// Examples, not an enum: the run forwards any tag it is given, so a closed
	// list in the schema would advertise a restriction the code does not have.
	if _, closed := tagsSchema["enum"]; closed {
		t.Error("the tags schema declares an enum, but the audit forwards any tag it is given")
	}

	// Every tool in the catalogue has to be reachable from the switch; these
	// three went in from a separate file, which is exactly how a tool gets
	// advertised and never wired.
	for _, name := range []string{"brw_vitals", "brw_a11y_audit", "brw_highlight"} {
		result, rpcErr := New(&fakeController{}).callTool(context.Background(), name, json.RawMessage(`{}`))
		if rpcErr != nil {
			t.Fatalf("%s: rpc error %v", name, rpcErr)
		}
		payload, _ := result.(map[string]any)
		if payload["isError"] != true {
			t.Fatalf("%s on a transport without the capability answered %v, want a named capability error", name, payload)
		}
		content, _ := payload["content"].([]toolContent)
		if len(content) == 0 || !strings.Contains(content[0].Text, "does not support developer observations") {
			t.Fatalf("%s error text = %v, want the named capability error", name, content)
		}
	}
}
