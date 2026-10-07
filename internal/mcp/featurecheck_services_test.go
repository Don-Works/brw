package mcp_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/agentskill"
	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/baseline"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/devtools"
	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/sessionstate"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/Don-Works/brw/internal/testbed"
)

func featureServices(t *testing.T, h *featureHarness) *artifact.Service {
	t.Helper()
	store, err := artifact.NewStore(artifact.Config{
		Root: filepath.Join(t.TempDir(), "artifacts"), MaxArtifactBytes: 8 << 20,
		MaxTotalBytes: 32 << 20, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := artifact.NewService(store, h.Manager)
	if err != nil {
		t.Fatal(err)
	}
	h.Server.SetArtifactAPI(service)
	return service
}

func featurePrivateDir(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func init() {
	registerFeatureCase("services-artifacts", []string{"brw_artifact_capture", "brw_artifact_info", "brw_artifact_read", "brw_artifact_search", "brw_artifact_delete"}, featureArtifactRoundTrip)
	registerFeatureCase("services-audit-vitals", []string{"brw_a11y_audit", "brw_vitals"}, featureAuditVitals)
	registerFeatureCase("services-recipes", []string{"brw_recipe_search", "brw_recipe_run"}, featureRecipeCapture)
	registerFeatureCase("services-screenshots", []string{"brw_screenshot", "brw_screenshot_element", "brw_screenshot_save"}, featureScreenshots)
	registerFeatureCase("services-baseline", []string{"brw_baseline"}, featureBaseline)
	registerFeatureCase("services-state", []string{"brw_state"}, featureState)
	registerFeatureCase("services-discovery", []string{"brw_tools", "brw_skill"}, featureDiscovery)
	registerFeatureCase("services-trace-console", []string{"brw_trace", "brw_clear_trace", "brw_console"}, featureTraceConsole)
	registerFeatureCase("services-plan-cancel", []string{"brw_plan", "brw_cancel"}, featurePlanCancellation)
	registerFeatureCase("services-check-commit", []string{"brw_check", "brw_commit"}, featureCheckCommit)
	registerFeatureCase("services-approval", []string{"brw_approval_status", "brw_approval_resume"}, featureApproval)
	registerFeatureCase("services-files", []string{"brw_set_download_path", "brw_downloads", "brw_upload_file"}, featureFiles)
	registerFeatureCase("services-page-tools", []string{"brw_page_tools", "brw_call_page_tool", "brw_page_tool_result", "brw_page_tool_cancel"}, featurePageTools)
	registerFeatureCase("services-diff", []string{"brw_diff"}, featureDiff)
	registerFeatureCase("services-read-url", []string{"brw_read_url"}, featureReadURL)
}

func featureDecode[T any](t *testing.T, r featureResult) T {
	t.Helper()
	var result T
	if err := json.Unmarshal([]byte(r.Text), &result); err != nil {
		t.Fatalf("decode MCP text: %v; %s", err, r.Text)
	}
	return result
}

func featureArtifactRoundTrip(t *testing.T, h *featureHarness) {
	service := featureServices(t, h)
	meta := featureDecode[artifact.Meta](t, h.OK(t, "brw_artifact_capture", map[string]any{"kind": "text"}))
	h.Check(t, "brw_artifact_capture", meta.ID != "" && meta.SizeBytes > 0 && meta.Kind == "text", "capture metadata: %+v", meta)
	info := featureDecode[artifact.Meta](t, h.OK(t, "brw_artifact_info", map[string]any{"artifact_id": meta.ID}))
	h.Check(t, "brw_artifact_info", info.ID == meta.ID && info.SHA256 == meta.SHA256 && info.SizeBytes == meta.SizeBytes, "stored metadata differs: %+v", info)
	chunk := featureDecode[artifact.Chunk](t, h.OK(t, "brw_artifact_read", map[string]any{"artifact_id": meta.ID, "max_bytes": 8192}))
	fact := h.State(t).Reading.Facts["site_code"]
	h.Check(t, "brw_artifact_read", chunk.Encoding == "utf-8" && chunk.TotalBytes == meta.SizeBytes && strings.Contains(chunk.Text, fact), "captured page fact absent: %+v", chunk)
	hits := featureDecode[[]artifact.TextHit](t, h.OK(t, "brw_artifact_search", map[string]any{"artifact_id": meta.ID, "query": fact, "limit": 3}))
	h.Check(t, "brw_artifact_search", len(hits) > 0 && len(hits) <= 3 && strings.Contains(hits[0].Excerpt, fact), "search missed fixture fact: %+v", hits)
	payload, _, _, err := service.Store().Read(meta.ID, 0, int(meta.SizeBytes))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	h.Check(t, "brw_artifact_capture", hex.EncodeToString(sum[:]) == meta.SHA256, "artifact digest does not match captured bytes")
	h.OK(t, "brw_artifact_delete", map[string]any{"artifact_id": meta.ID})
	_, err = service.Store().Info(meta.ID)
	after := h.Call(t, "brw_artifact_info", map[string]any{"artifact_id": meta.ID})
	h.Check(t, "brw_artifact_delete", err != nil && after.IsError, "deleted artifact remained available")
}

func featureAuditVitals(t *testing.T, h *featureHarness) {
	service := featureServices(t, h)
	audit := featureDecode[devtools.AuditResult](t, h.OK(t, "brw_a11y_audit", map[string]any{"selector": "#forms", "rules": []string{"label"}, "include_passes": true}))
	h.Check(t, "brw_a11y_audit", audit.Artifact != nil && audit.Artifact.ID != "" && strings.HasPrefix(audit.URL, h.Fixture.URL()), "audit did not identify owned page and stored report: %+v", audit)
	data, _, _, err := service.Store().Read(audit.Artifact.ID, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	report, _ = report["report"].(map[string]any)
	engine, _ := report["testEngine"].(map[string]any)
	h.Check(t, "brw_a11y_audit", engine["version"] == "4.14.0" && report["passes"] != nil, "full axe report missing exact engine/passes: %s", data)
	vitals := featureDecode[devtools.Vitals](t, h.OK(t, "brw_vitals", map[string]any{"settle_ms": 1}))
	h.Check(t, "brw_vitals", strings.HasPrefix(vitals.URL, h.Fixture.URL()) && vitals.DOMContentLoadedMS != nil && vitals.TTFBMS != nil && *vitals.TTFBMS >= 0, "navigation timing absent: %+v", vitals)
}

func featureRecipeCapture(t *testing.T, h *featureHarness) {
	artifacts := featureServices(t, h)
	value := recipe.Recipe{SchemaVersion: recipe.SchemaVersion, ID: "fixture.report", Version: "1.0.0", Name: "Owned verified report", Description: "Capture the current verified field report", Intents: []string{"verified report"}, Origins: []string{h.Fixture.URL()}, Risk: "read_only", Steps: []recipe.Step{{ID: "evidence", Action: "capture", Capture: &recipe.CaptureSpec{Kind: "text"}}}}
	root := featurePrivateDir(t)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "report.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	provider, err := recipe.NewDirectoryProvider(h.Ctx, recipe.DirectoryConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	service, err := recipe.NewService(provider, recipe.Runner{Surface: &recipe.BrowserSurface{Browser: h.Manager, Artifacts: artifacts}})
	if err != nil {
		t.Fatal(err)
	}
	h.Server.SetRecipeAPI(service)
	matches := featureDecode[[]recipe.Match](t, h.OK(t, "brw_recipe_search", map[string]any{"query": "verified report", "origin": h.Fixture.URL(), "limit": 3}))
	h.Check(t, "brw_recipe_search", len(matches) == 1 && matches[0].ID == value.ID && len(matches[0].Digest) == 64, "stored recipe not discovered: %+v", matches)
	request := recipe.RunRequest{ID: matches[0].ID, Version: matches[0].Version, Digest: matches[0].Digest}
	before := h.State(t)
	refused := h.Call(t, "brw_recipe_run", map[string]any{"id": request.ID, "version": request.Version, "digest": request.Digest})
	after := h.State(t)
	h.Check(t, "brw_recipe_run", refused.IsError && strings.HasPrefix(refused.Text, "approval_split_required:") && reflect.DeepEqual(before.ActionCounts, after.ActionCounts) && before.FormState == after.FormState, "whole recipe approval did not refuse before page effects: %s", refused.Text)
	run, err := service.RunRecipe(browser.WithTabID(h.Ctx, h.TabID), request)
	if err != nil {
		t.Fatal(err)
	}
	h.Check(t, "brw_recipe_run", run.Status == "done" && len(run.Artifacts) == 1 && run.RecipeDigest == matches[0].Digest, "recipe did not complete capture: %+v", run)
	captured, _, _, err := artifacts.Store().Read(run.Artifacts[0].ID, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(captured)
	h.Check(t, "brw_recipe_run", strings.Contains(string(captured), h.State(t).Reading.Facts["site_code"]) && hex.EncodeToString(sum[:]) == run.Artifacts[0].SHA256, "service recipe artifact lacks actual page fact or byte digest")
	h.status["brw_recipe_run"] = "policy_refused"
}

func featureImage(t *testing.T, r featureResult) image.Config {
	t.Helper()
	blocks, _ := r.Envelope["content"].([]any)
	for _, block := range blocks {
		item, _ := block.(map[string]any)
		if item["type"] != "image" {
			continue
		}
		text, _ := item["data"].(string)
		data, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			t.Fatal(err)
		}
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		return config
	}
	t.Fatal("MCP screenshot has no image block")
	return image.Config{}
}

func featureScreenshots(t *testing.T, h *featureHarness) {
	whole := featureImage(t, h.OK(t, "brw_screenshot", nil))
	h.Check(t, "brw_screenshot", whole.Width > 500 && whole.Height > 300, "invalid owned viewport image: %+v", whole)
	ref := h.Ref(t, "Fixture note")
	element := featureImage(t, h.OK(t, "brw_screenshot_element", map[string]any{"ref": ref}))
	geometry := h.OK(t, "brw_evaluate", map[string]any{"expression": "document.getElementById('note').getBoundingClientRect().toJSON()"})
	width, _ := geometry.Data["width"].(float64)
	height, _ := geometry.Data["height"].(float64)
	h.Check(t, "brw_screenshot_element", math.Abs(float64(element.Width)-width) <= 1 && math.Abs(float64(element.Height)-height) <= 1, "element crop differs from actual DOM geometry: %+v vs %s", element, geometry.Text)
	t.Setenv("BRW_SCREENSHOT_ALLOW_OUTSIDE_HOME", "1")
	path := filepath.Join(t.TempDir(), "owned.png")
	result := h.OK(t, "brw_screenshot_save", map[string]any{"save_path": path, "format": "png", "ref": ref, "preview": "none"})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	h.Check(t, "brw_screenshot_save", result.Data["sha256"] == hex.EncodeToString(sum[:]) && config.Width > 0 && config.Height > 0 && result.Data["path"] == path, "saved bytes/metadata differ: %s", result.Text)
}

func featureBaseline(t *testing.T, h *featureHarness) {
	store, err := baseline.NewStore(featurePrivateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	h.Server.SetBaselineStore(store)
	args := map[string]any{"action": "update", "recipe_digest": strings.Repeat("a", 64), "step_index": 0, "pixel_tolerance": 1}
	recorded := h.OK(t, "brw_baseline", args)
	h.Check(t, "brw_baseline", recorded.Data["status"] == baseline.StatusRecorded, "baseline not stored: %s", recorded.Text)
	args["action"] = "list"
	listed := h.OK(t, "brw_baseline", args)
	h.Check(t, "brw_baseline", listed.Data["count"] == float64(1), "stored environment not listed: %s", listed.Text)
	args["action"] = "check"
	matched := h.OK(t, "brw_baseline", args)
	h.Check(t, "brw_baseline", matched.Data["status"] == baseline.StatusMatch, "unchanged owned page failed comparison: %s", matched.Text)
	h.Event(t, "reading")
	changed := h.OK(t, "brw_baseline", args)
	h.Check(t, "brw_baseline", changed.Data["status"] == baseline.StatusDiff && changed.Data["failed"] == true, "changed report did not produce ARIA regression: %s", changed.Text)
	args["action"] = "delete"
	h.OK(t, "brw_baseline", args)
	args["action"] = "list"
	gone := h.OK(t, "brw_baseline", args)
	h.Check(t, "brw_baseline", gone.Data["count"] == float64(0), "deleted baseline remained listed")
}

func featureState(t *testing.T, h *featureHarness) {
	store, err := sessionstate.NewStore(sessionstate.Config{Root: featurePrivateDir(t), Key: bytes.Repeat([]byte{'s'}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	h.Manager.SetSessionStateStore(store)
	t.Cleanup(func() { h.Manager.SetSessionStateStore(nil) })
	h.OK(t, "brw_open", map[string]any{"url": h.Fixture.URL() + "/api/cookies"})
	saved := featureDecode[browser.SessionStateResult](t, h.OK(t, "brw_state", map[string]any{"action": "save", "origins": []string{h.Fixture.URL()}}))
	h.Check(t, "brw_state", saved.Snapshot != nil && saved.Snapshot.CookieCount >= 2, "cookies not sealed: %+v", saved)
	listed := featureDecode[browser.SessionStateResult](t, h.OK(t, "brw_state", map[string]any{"action": "list"}))
	h.Check(t, "brw_state", len(listed.Snapshots) == 1 && listed.Snapshots[0].ID == saved.Snapshot.ID, "saved state not listed")
	opened := h.OK(t, "brw_open_incognito", map[string]any{"url": h.Fixture.URL() + "/?frames=none"})
	tab, _ := opened.Data["tab"].(map[string]any)
	contextID, _ := tab["context_id"].(string)
	if contextID == "" {
		contextID, _ = opened.Data["context_id"].(string)
	}
	if contextID == "" {
		t.Fatalf("incognito context absent: %s", opened.Text)
	}
	incognitoTab, _ := tab["id"].(string)
	isolated := featureDecode[browser.CookieResult](t, h.OK(t, "brw_cookies", map[string]any{"action": "list", "tab_id": incognitoTab, "url": h.Fixture.URL() + "/?phase=before"}))
	h.Check(t, "brw_state", len(isolated.Cookies) == 0, "new incognito context inherited profile cookies")
	restored := featureDecode[browser.SessionStateResult](t, h.OK(t, "brw_state", map[string]any{"action": "restore", "snapshot_id": saved.Snapshot.ID, "context_id": contextID, "origins": []string{h.Fixture.URL()}}))
	h.Check(t, "brw_state", restored.RestoredCookies >= 2, "sealed cookies did not restore: %+v", restored)
	cookies := featureDecode[browser.CookieResult](t, h.OK(t, "brw_cookies", map[string]any{"action": "list", "tab_id": incognitoTab, "url": h.Fixture.URL() + "/?phase=after"}))
	found := map[string]bool{}
	expected := map[string]string{"testbed_public": "synthetic", "testbed_private": "synthetic-http-only"}
	for _, cookie := range cookies.Cookies {
		found[cookie.Name] = cookie.Value == expected[cookie.Name] && cookie.HTTPOnly == (cookie.Name == "testbed_private")
	}
	h.Check(t, "brw_state", found["testbed_public"] && found["testbed_private"], "sealed synthetic cookies absent from restored isolated context")
	h.OK(t, "brw_state", map[string]any{"action": "delete", "snapshot_id": saved.Snapshot.ID})
	revoked := featureDecode[browser.SessionStateResult](t, h.OK(t, "brw_state", map[string]any{"action": "list"}))
	h.Check(t, "brw_state", len(revoked.Snapshots) == 0, "revoked state still listed")
	h.OK(t, "brw_close_context", map[string]any{"context_id": contextID})
}

func featureDiscovery(t *testing.T, h *featureHarness) {
	h.UseServer(t, mcp.NewWithToolProfile(h.Manager, "auto"))
	before := h.RPC(t, "tools/list", nil)["tools"].([]any)
	search := h.OK(t, "brw_tools", map[string]any{"query": "artifact capture read"})
	after := h.RPC(t, "tools/list", nil)["tools"].([]any)
	h.Check(t, "brw_tools", len(after) > len(before) && strings.Contains(search.Text, "brw_artifact_capture"), "discovery did not unlock concrete tools: %s", search.Text)
	notified := false
	for _, notification := range h.Notifications() {
		notified = notified || notification["method"] == "notifications/tools/list_changed"
	}
	h.Check(t, "brw_tools", notified, "auto catalogue emitted no list-change notification")
	skill := h.OK(t, "brw_skill", nil)
	h.Check(t, "brw_skill", strings.Contains(skill.Data["content"].(string), "brw_snapshot") && strings.Contains(skill.Data["content"].(string), "approval") && len(skill.Data["content"].(string)) <= 16<<10 && skill.Data["source"] == agentskill.SourceEmbedded, "served skill is incomplete or unbounded")
}

func featureTraceConsole(t *testing.T, h *featureHarness) {
	h.OK(t, "brw_clear_trace", nil)
	ref := h.Ref(t, "Fixture note")
	value := "Feature trace note"
	h.OK(t, "brw_fill", map[string]any{"ref": ref, "value": value})
	h.Wait(t, func(s testbed.State) bool { return s.FormState.Note == value })
	trace := h.OK(t, "brw_trace", nil)
	h.Check(t, "brw_trace", strings.Contains(trace.Text, value) && strings.Contains(trace.Text, ref), "trace lost the real fill: %s", trace.Text)
	h.OK(t, "brw_clear_trace", nil)
	cleared := featureDecode[browser.TraceResult](t, h.OK(t, "brw_trace", nil))
	h.Check(t, "brw_clear_trace", len(cleared.Entries) == 0, "trace was not cleared")
	marker := "Feature console marker"
	h.OK(t, "brw_evaluate", map[string]any{"expression": "console.log('Feature console marker'); true"})
	console := h.OK(t, "brw_console", map[string]any{"pattern": marker, "clear": false})
	h.Check(t, "brw_console", strings.Contains(console.Text, marker) && console.Data["returned"] == float64(1), "real console event not returned: %s", console.Text)
}

func featurePlanCancellation(t *testing.T, h *featureHarness) {
	ref := h.Ref(t, "Fixture note")
	value := "Feature completed prefix"
	id := h.StartRPC(t, "tools/call", map[string]any{"name": "brw_plan", "arguments": map[string]any{"tab_id": h.TabID, "steps": []map[string]any{{"action": "fill", "ref": ref, "value": value}, {"action": "wait", "condition": "selector:#feature-never-ready", "timeout_ms": 20000}}}, "_meta": map[string]any{"progressToken": "feature-plan-progress"}})
	h.Wait(t, func(s testbed.State) bool { return s.FormState.Note == value })
	cancel := h.OK(t, "brw_cancel", map[string]any{"tab_id": h.TabID})
	h.Check(t, "brw_cancel", cancel.Data["cancelled"] == float64(1), "active plan was not signalled: %s", cancel.Text)
	result := h.AwaitRPC(t, id)
	data, _ := result["structuredContent"].(map[string]any)
	encoded, _ := json.Marshal(data)
	var plan browser.PlanResult
	if err := json.Unmarshal(encoded, &plan); err != nil {
		t.Fatal(err)
	}
	h.Check(t, "brw_plan", result["isError"] != true && plan.Cancelled && plan.StepsCompleted == 1 && len(plan.Steps) == 1 && h.State(t).FormState.Note == value, "cancel lost completed real prefix: %s", encoded)
}

func featureCheckCommit(t *testing.T, h *featureHarness) {
	ref := h.Ref(t, "Include appendix")
	h.OK(t, "brw_check", map[string]any{"ref": ref, "checked": true})
	got := h.OK(t, "brw_evaluate", map[string]any{"expression": "document.getElementById('agree').checked"})
	h.Check(t, "brw_check", got.Text == "true", "checkbox not checked: %s", got.Text)
	h.OK(t, "brw_check", map[string]any{"ref": ref, "checked": true})
	stable := h.OK(t, "brw_evaluate", map[string]any{"expression": "Boolean(document.getElementById('agree').checked)"})
	h.Check(t, "brw_check", stable.Text == "true", "idempotent check toggled control")
	note := h.Ref(t, "Fixture note")
	h.OK(t, "brw_fill", map[string]any{"ref": note, "value": "Feature committed note"})
	h.Wait(t, func(s testbed.State) bool { return s.FormState.Note == "Feature committed note" })
	h.OK(t, "brw_commit", map[string]any{"ref": note})
	state := h.Wait(t, func(s testbed.State) bool { return s.FormState.DraftSaved })
	h.Check(t, "brw_commit", state.FormState.Note == "Feature committed note" && state.ActionCounts["save-draft"] == 1, "field commit did not submit actual retained draft exactly once: %+v", state.FormState)
}

func featureApproval(t *testing.T, h *featureHarness) {
	h.RequireApprovals(t)
	ref := h.Ref(t, "Delete account")
	args := map[string]any{"ref": ref, "tab_id": h.TabID}
	denied := h.Call(t, "brw_click", args)
	id, _ := denied.Data["approval_id"].(string)
	if !denied.IsError || id == "" {
		t.Fatalf("real approval was not required: %s", denied.Text)
	}
	status := h.OK(t, "brw_approval_status", map[string]any{"approval_id": id})
	h.Check(t, "brw_approval_status", strings.Contains(status.Text, "pending") && !h.State(t).FormState.AccountDeleted, "pending approval allowed effect: %s", status.Text)
	resumed := h.approveResult(t, "brw_click", args, denied)
	h.Wait(t, func(s testbed.State) bool { return s.FormState.AccountDeleted })
	h.Check(t, "brw_approval_resume", !resumed.IsError && h.State(t).ActionCounts["delete-account"] == 1, "approved exact action did not execute once")
	consumed := h.Call(t, "brw_approval_resume", map[string]any{"approval_id": id, "tool": "brw_click", "arguments": args})
	h.Check(t, "brw_approval_resume", consumed.IsError && h.State(t).ActionCounts["delete-account"] == 1, "consumed approval replayed effect")
}

func featureFiles(t *testing.T, h *featureHarness) {
	root := featurePrivateDir(t)
	routing := h.OK(t, "brw_set_download_path", map[string]any{"path": root})
	h.Check(t, "brw_set_download_path", !routing.IsError, "owned download routing refused")
	h.OK(t, "brw_click", map[string]any{"ref": h.Ref(t, "Download fixture file")})
	var downloaded browser.DownloadsResult
	deadline := time.Now().Add(8 * time.Second)
	for {
		downloaded = featureDecode[browser.DownloadsResult](t, h.OK(t, "brw_downloads", nil))
		if len(downloaded.Downloads) > 0 && downloaded.Downloads[0].State == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("download did not finish: %+v", downloaded)
		}
		time.Sleep(40 * time.Millisecond)
	}
	data, err := os.ReadFile(downloaded.Downloads[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	h.Check(t, "brw_downloads", hex.EncodeToString(sum[:]) == h.State(t).DownloadSHA256 && filepath.Dir(downloaded.Downloads[0].Path) == root, "downloaded owned payload/path differ")
	h.Check(t, "brw_set_download_path", filepath.Dir(downloaded.Downloads[0].Path) == root, "actual browser download ignored requested destination")
	path := filepath.Join(t.TempDir(), "feature-upload.txt")
	body := []byte("Owned feature upload bytes.")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	h.OK(t, "brw_upload_file", map[string]any{"ref": h.Ref(t, "Fixture upload"), "path": path})
	h.OK(t, "brw_click", map[string]any{"ref": h.Ref(t, "Upload fixture file")})
	state := h.Wait(t, func(s testbed.State) bool { return s.LastUpload != nil })
	uploadSum := sha256.Sum256(body)
	h.Check(t, "brw_upload_file", state.LastUpload.Bytes == int64(len(body)) && state.LastUpload.SHA256 == hex.EncodeToString(uploadSum[:]) && state.LastUpload.Filename == filepath.Base(path), "actual uploaded bytes differ: %+v", state.LastUpload)
}

func featurePageTools(t *testing.T, h *featureHarness) {
	listing := h.OK(t, "brw_page_tools", nil)
	h.Check(t, "brw_page_tools", strings.Contains(listing.Text, "fixture_read_report"), "owned page tool not registered: %s", listing.Text)
	read := featureDecode[snapshot.PageToolInvocation](t, h.OK(t, "brw_call_page_tool", map[string]any{"name": "fixture_read_report", "arguments": map[string]any{}}))
	h.Check(t, "brw_call_page_tool", read.OK && strings.Contains(string(read.Result), h.State(t).Reading.Facts["site_code"]), "page tool lost actual ground truth: %+v", read)
	expression := `(function(){const runtime=document.modelContext||navigator.modelContext;runtime.registerTool({name:'feature_delayed_read',description:'Read owned synthetic delayed evidence',inputSchema:{type:'object',properties:{nonce:{type:'integer'}},additionalProperties:false},annotations:{readOnlyHint:true},execute:async()=>{await new Promise(resolve=>setTimeout(resolve,1000));return {marker:'feature-delayed-evidence'}}});return true})()`
	h.OK(t, "brw_evaluate", map[string]any{"expression": expression})
	started := featureDecode[snapshot.PageToolInvocation](t, h.OK(t, "brw_call_page_tool", map[string]any{"name": "feature_delayed_read", "arguments": map[string]any{"nonce": 1}, "detach": true}))
	finished := featureDecode[snapshot.PageToolInvocation](t, h.OK(t, "brw_page_tool_result", map[string]any{"invocation_id": started.ID, "timeout_ms": 3000}))
	h.Check(t, "brw_page_tool_result", finished.OK && finished.ID == started.ID && strings.Contains(string(finished.Result), "feature-delayed-evidence"), "detached result lost identity/evidence: %+v", finished)
	second := featureDecode[snapshot.PageToolInvocation](t, h.OK(t, "brw_call_page_tool", map[string]any{"name": "feature_delayed_read", "arguments": map[string]any{"nonce": 2}, "detach": true}))
	cancelled := featureDecode[snapshot.PageToolInvocation](t, h.OK(t, "brw_page_tool_cancel", map[string]any{"invocation_id": second.ID}))
	h.Check(t, "brw_page_tool_cancel", cancelled.ID == second.ID && cancelled.Cancelled, "detached cancellation lost identity: %+v", cancelled)
	time.Sleep(1200 * time.Millisecond)
	terminal := featureDecode[snapshot.PageToolInvocation](t, h.OK(t, "brw_page_tool_result", map[string]any{"invocation_id": second.ID, "timeout_ms": 1}))
	h.Check(t, "brw_page_tool_cancel", terminal.ID == second.ID && terminal.Status == snapshot.PageToolCancelled && !terminal.OK && len(terminal.Result) == 0, "cancelled invocation published delayed evidence: %+v", terminal)
	args := map[string]any{"name": "fixture_delete_account", "arguments": map[string]any{}}
	denied := h.Call(t, "brw_call_page_tool", args)
	h.Check(t, "brw_call_page_tool", denied.IsError && denied.Data["code"] == "approval_required" && !h.State(t).FormState.AccountDeleted && h.State(t).ActionCounts["webmcp-mutation"] == 0, "consequential page tool bypassed approval: %s", denied.Text)
	approved := h.approveResult(t, "brw_call_page_tool", args, denied)
	state := h.Wait(t, func(s testbed.State) bool { return s.FormState.AccountDeleted })
	h.Check(t, "brw_call_page_tool", !approved.IsError && state.ActionCounts["webmcp-mutation"] == 1, "approved exact page tool did not execute once: %s", approved.Text)
}

func featureDiff(t *testing.T, h *featureHarness) {
	h.OK(t, "brw_diff", map[string]any{"action": "mark"})
	h.Event(t, "reading")
	delta := h.OK(t, "brw_diff", map[string]any{"action": "compare"})
	h.Check(t, "brw_diff", delta.Data["changed"] == true && delta.Data["text_changed"] == true, "revised report not detected: %s", delta.Text)
}

func featureReadURL(t *testing.T, h *featureHarness) {
	before := h.State(t).ActionCounts["save-draft"]
	read := h.OK(t, "brw_read_url", map[string]any{"url": h.Fixture.URL() + "/frame?version=1", "max_chars": 4000})
	h.Check(t, "brw_read_url", strings.Contains(read.Text, "Child fixture") && h.State(t).ActionCounts["save-draft"] == before, "owned static page missing or read caused writes: %s", read.Text)
}
