package mcp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/siteconsent"
)

func init() {
	registerFeatureCase("capabilities_incognito_authentication", []string{"brw_open_incognito", "brw_close_context", "brw_authenticate"}, featureIncognitoAuthentication)
	registerFeatureCase("capabilities_cookie_attributes", []string{"brw_cookies"}, featureCookieAttributes)
	registerFeatureCase("capabilities_clipboard_owned_headless", []string{"brw_clipboard"}, featureClipboard)
	registerFeatureCase("capabilities_device_viewport", []string{"brw_emulate_device"}, featureDevice)
	registerFeatureCase("capabilities_media_preferences", []string{"brw_emulate_media"}, featureMedia)
	registerFeatureCase("capabilities_geolocation_permission", []string{"brw_set_geolocation"}, featureGeolocation)
	registerFeatureCase("capabilities_locale_timezone", []string{"brw_set_locale"}, featureLocale)
	registerFeatureCase("capabilities_network_offline_recovery", []string{"brw_set_network_conditions"}, featureNetworkConditions)
	registerFeatureCase("capabilities_user_agent_restore", []string{"brw_set_user_agent"}, featureUserAgent)
	registerFeatureCase("capabilities_header_origin_isolation", []string{"brw_set_extra_headers"}, featureExtraHeaders)
	registerFeatureCase("capabilities_init_script_lifecycle", []string{"brw_init_script"}, featureInitScript)
	registerFeatureCase("capabilities_touch_events", []string{"brw_touch"}, featureTouch)
	registerFeatureCase("capabilities_profile_artifacts", []string{"brw_profile"}, featureProfile)
	registerFeatureCase("capabilities_react_bounded_fixture", []string{"brw_react"}, featureReact)
	registerFeatureCase("capabilities_network_capture_and_resources", []string{"brw_network_capture", "brw_network_requests"}, featureNetworkCapture)
	registerFeatureCase("capabilities_replay_redirect_body_window", []string{"brw_replay_request"}, featureReplayRequest)
	registerFeatureCase("capabilities_route_fulfill_abort_clear", []string{"brw_route"}, featureRoute)
	registerFeatureCase("capabilities_web_storage", []string{"brw_storage"}, featureStorage)
	registerFeatureCase("capabilities_window_geometry", []string{"brw_window_bounds", "brw_window_resize"}, featureWindow)
	registerFeatureCase("capabilities_tab_groups_cdp_refusal", []string{"brw_group_tabs", "brw_list_tab_groups", "brw_ungroup_tabs"}, featureTabGroups)
	registerFeatureCase("capabilities_notification_unavailable", []string{"brw_notify"}, featureNotifyUnavailable)
}

func capabilityEval(t *testing.T, h *featureHarness, expression string) map[string]any {
	t.Helper()
	result := h.OK(t, "brw_evaluate", map[string]any{"expression": fmt.Sprintf("(void %d,(%s))", h.called["brw_evaluate"], expression)})
	var value map[string]any
	if err := json.Unmarshal([]byte(result.Text), &value); err != nil {
		t.Fatalf("fixture observation is not an object: %v: %s", err, result.Text)
	}
	return value
}

func capabilityArray(t *testing.T, result featureResult) []map[string]any {
	t.Helper()
	var envelope struct {
		Requests []map[string]any `json:"requests"`
		Returned int              `json:"returned"`
	}
	if err := json.Unmarshal([]byte(result.Text), &envelope); err != nil {
		t.Fatalf("fixture result is not a network envelope: %v: %s", err, result.Text)
	}
	if envelope.Returned != len(envelope.Requests) {
		t.Fatalf("network result count differs from rows: %s", result.Text)
	}
	return envelope.Requests
}

func capabilityObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T: %v", value, value)
	}
	return object
}

func featureIncognitoAuthentication(t *testing.T, h *featureHarness) {
	h.OK(t, "brw_storage", map[string]any{"action": "set", "key": "feature-context", "value": "normal-owned-fixture"})
	opened := h.OK(t, "brw_open_incognito", map[string]any{"url": h.Fixture.URL() + "/?frames=none"})
	tab := capabilityObject(t, opened.Data["tab"])
	id, contextID := tab["id"], tab["context_id"]
	h.Check(t, "brw_open_incognito", id != h.TabID && id != nil && contextID != nil && contextID != "", "incognito did not return an isolated tab/context: %s", opened.Text)
	isolated := h.OK(t, "brw_storage", map[string]any{"tab_id": id, "action": "get", "key": "feature-context"})
	h.Check(t, "brw_open_incognito", isolated.Data["value"] == nil && isolated.Data["key"] == "feature-context", "incognito inherited normal storage: %s", isolated.Text)
	auth := h.OK(t, "brw_authenticate", map[string]any{"tab_id": id, "origin": h.Fixture.URL(), "url": h.Fixture.URL() + "/fixture/basic-auth", "username": "fixture", "password": "fixture"})
	outcome := capabilityObject(t, auth.Data["authentication"])
	h.Check(t, "brw_authenticate", outcome["challenged"] == true && outcome["answered"].(float64) > 0 && outcome["browser_cached"] == true, "owned authentication did not answer the known challenge: %s", auth.Text)
	page := h.OK(t, "brw_get", map[string]any{"tab_id": id, "what": "text"})
	h.Check(t, "brw_authenticate", strings.Contains(page.Text, "Authenticated local fixture"), "authenticated page did not return known text: %s", page.Text)
	h.OK(t, "brw_close_context", map[string]any{"context_id": contextID})
	tabs, err := h.Manager.ListTabs(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	present := false
	for _, tab := range tabs {
		present = present || tab.ID == id
	}
	h.Check(t, "brw_close_context", !present, "disposed context retained its tab %v", id)
	normal := h.OK(t, "brw_storage", map[string]any{"action": "get", "key": "feature-context"})
	h.Check(t, "brw_close_context", normal.Data["value"] == "normal-owned-fixture", "disposing incognito changed normal storage: %s", normal.Text)
	h.OK(t, "brw_storage", map[string]any{"action": "remove", "key": "feature-context"})
}

func featureCookieAttributes(t *testing.T, h *featureHarness) {
	capabilityEval(t, h, `(async()=>{await fetch('/api/cookies');return {ready:true}})()`)
	listed := h.OK(t, "brw_cookies", map[string]any{"action": "list", "name": "testbed_private"})
	rows, _ := listed.Data["cookies"].([]any)
	h.Check(t, "brw_cookies", len(rows) == 1 && capabilityObject(t, rows[0])["http_only"] == true, "HTTP-only fixture missing from CDP cookies: %s", listed.Text)
	visible := capabilityEval(t, h, `({cookies:document.cookie})`)
	h.Check(t, "brw_cookies", !strings.Contains(fmt.Sprint(visible["cookies"]), "testbed_private"), "HTTP-only cookie exposed to JavaScript: %v", visible)
	set := h.OK(t, "brw_cookies", map[string]any{"action": "set", "name": "feature_http_only", "value": "owned-cookie", "url": h.Fixture.URL(), "path": "/", "http_only": true, "same_site": "strict"})
	cookie := capabilityObject(t, set.Data["cookie"])
	h.Check(t, "brw_cookies", cookie["value"] == "owned-cookie" && cookie["http_only"] == true && strings.EqualFold(fmt.Sprint(cookie["same_site"]), "strict"), "cookie attributes differed: %s", set.Text)
	h.OK(t, "brw_cookies", map[string]any{"action": "delete", "name": "feature_http_only", "url": h.Fixture.URL(), "path": "/"})
	remaining := h.OK(t, "brw_cookies", map[string]any{"action": "list", "name": "feature_http_only"})
	h.Check(t, "brw_cookies", remaining.Data["count"] == float64(0), "deleted cookie remained: %s", remaining.Text)
}

func featureClipboard(t *testing.T, h *featureHarness) {
	const text = "owned headless fixture clipboard"
	h.OK(t, "brw_clipboard", map[string]any{"action": "write", "text": text})
	read := h.OK(t, "brw_clipboard", map[string]any{"action": "read"})
	h.Check(t, "brw_clipboard", read.Data["text"] == text, "headless clipboard read differed: %s", read.Text)
	h.OK(t, "brw_clipboard", map[string]any{"action": "revoke"})
	permission := capabilityEval(t, h, `(async()=>({state:(await navigator.permissions.query({name:'clipboard-read'})).state}))()`)
	h.Check(t, "brw_clipboard", permission["state"] != "granted", "clipboard grant survived revoke: %v", permission)
}

func featureDevice(t *testing.T, h *featureHarness) {
	h.OK(t, "brw_emulate_device", map[string]any{"device": "responsive", "width": 420, "height": 760, "device_scale_factor": 2, "mobile": false, "touch": true, "max_touch_points": 3})
	observed := capabilityEval(t, h, `({width:innerWidth,height:innerHeight,dpr:devicePixelRatio,touch:navigator.maxTouchPoints})`)
	h.Check(t, "brw_emulate_device", observed["width"] == float64(420) && observed["height"] == float64(760) && observed["dpr"] == float64(2) && observed["touch"] == float64(3), "device override differs from actual viewport: %v", observed)
	cleared := h.OK(t, "brw_emulate_device", map[string]any{"clear": true})
	h.Check(t, "brw_emulate_device", cleared.Data["cleared"] == true, "device reset not reported: %s", cleared.Text)
}

func featureMedia(t *testing.T, h *featureHarness) {
	h.OK(t, "brw_emulate_media", map[string]any{"media": "print", "color_scheme": "dark", "reduced_motion": "reduce"})
	observed := capabilityEval(t, h, `({print:matchMedia('print').matches,dark:matchMedia('(prefers-color-scheme:dark)').matches,reduced:matchMedia('(prefers-reduced-motion:reduce)').matches})`)
	h.Check(t, "brw_emulate_media", observed["print"] == true && observed["dark"] == true && observed["reduced"] == true, "media overrides did not affect queries: %v", observed)
	h.OK(t, "brw_emulate_media", map[string]any{"clear": true})
	reset := capabilityEval(t, h, `({print:matchMedia('print').matches})`)
	h.Check(t, "brw_emulate_media", reset["print"] == false, "print override survived reset: %v", reset)
}

func featureGeolocation(t *testing.T, h *featureHarness) {
	before := capabilityEval(t, h, `(async()=>({state:(await navigator.permissions.query({name:'geolocation'})).state}))()`)
	h.OK(t, "brw_set_geolocation", map[string]any{"latitude": 51.5, "longitude": -0.12, "accuracy": 7})
	location := capabilityEval(t, h, `new Promise(resolve=>navigator.geolocation.getCurrentPosition(p=>resolve({latitude:p.coords.latitude,longitude:p.coords.longitude,accuracy:p.coords.accuracy}),e=>resolve({error:e.code}),{timeout:4000}))`)
	h.Check(t, "brw_set_geolocation", location["latitude"] == 51.5 && location["longitude"] == -0.12 && location["accuracy"] == float64(7), "permission/position override was unreachable: %v", location)
	h.OK(t, "brw_set_geolocation", map[string]any{"clear": true})
	after := capabilityEval(t, h, `(async()=>({state:(await navigator.permissions.query({name:'geolocation'})).state}))()`)
	h.Check(t, "brw_set_geolocation", after["state"] == before["state"], "clear did not restore the original permission: before=%v after=%v", before, after)
}

func featureLocale(t *testing.T, h *featureHarness) {
	before := capabilityEval(t, h, `({zone:Intl.DateTimeFormat().resolvedOptions().timeZone})`)
	h.OK(t, "brw_set_locale", map[string]any{"locale": "de-DE", "timezone": "Europe/Berlin"})
	observed := capabilityEval(t, h, `({zone:Intl.DateTimeFormat().resolvedOptions().timeZone,number:new Intl.NumberFormat().format(1234.5)})`)
	h.Check(t, "brw_set_locale", observed["zone"] == "Europe/Berlin" && observed["number"] == "1.234,5", "locale/timezone differed: %v", observed)
	h.OK(t, "brw_set_locale", map[string]any{"clear": true})
	after := capabilityEval(t, h, `({zone:Intl.DateTimeFormat().resolvedOptions().timeZone})`)
	h.Check(t, "brw_set_locale", after["zone"] == before["zone"], "timezone survived reset: %v", after)
}

func featureNetworkConditions(t *testing.T, h *featureHarness) {
	h.OK(t, "brw_set_network_conditions", map[string]any{"offline": true})
	failed := capabilityEval(t, h, `(async()=>{try{await fetch('/fixture/status/200?offline-fixture=1',{cache:'no-store'});return {failed:false}}catch(e){return {failed:true,online:navigator.onLine}}})()`)
	h.Check(t, "brw_set_network_conditions", failed["failed"] == true, "offline request reached the network: %v", failed)
	h.OK(t, "brw_set_network_conditions", map[string]any{"clear": true})
	recovered := capabilityEval(t, h, `(async()=>({status:(await fetch('/fixture/status/200?online-fixture=1',{cache:'no-store'})).status}))()`)
	h.Check(t, "brw_set_network_conditions", recovered["status"] == float64(200), "network did not recover: %v", recovered)
}

func featureUserAgent(t *testing.T, h *featureHarness) {
	before := capabilityEval(t, h, `({ua:navigator.userAgent})`)
	h.OK(t, "brw_set_user_agent", map[string]any{"user_agent": "brw-owned-feature/1.0", "accept_language": "fr-FR,fr;q=0.9", "platform": "OwnedFixture"})
	observed := capabilityEval(t, h, `({ua:navigator.userAgent,platform:navigator.platform})`)
	h.Check(t, "brw_set_user_agent", observed["ua"] == "brw-owned-feature/1.0" && observed["platform"] == "OwnedFixture", "user-agent override differed: %v", observed)
	h.OK(t, "brw_set_user_agent", map[string]any{"clear": true})
	after := capabilityEval(t, h, `({ua:navigator.userAgent})`)
	h.Check(t, "brw_set_user_agent", after["ua"] == before["ua"], "user agent survived reset: %v", after)
}

func featureExtraHeaders(t *testing.T, h *featureHarness) {
	server := func() *httptest.Server {
		owned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", h.Fixture.URL())
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"header": r.Header.Get("X-Owned-Feature")})
		}))
		t.Cleanup(owned.Close)
		for _, scope := range []siteconsent.Scope{siteconsent.ScopeRead, siteconsent.ScopeAct} {
			if _, err := h.Consent.Allow(siteconsent.GrantOptions{Origin: owned.URL, Scope: scope, Actor: "owned-fixture-test"}); err != nil {
				t.Fatal(err)
			}
		}
		return owned
	}
	allowed, unrelated := server(), server()
	set := h.OK(t, "brw_set_extra_headers", map[string]any{"origins": []any{map[string]any{"origin": allowed.URL, "headers": map[string]string{"X-Owned-Feature": "fixture-header-value"}}}})
	h.Check(t, "brw_set_extra_headers", !strings.Contains(set.Text, "fixture-header-value"), "header value echoed in configuration result: %s", set.Text)
	for _, check := range []struct{ url, want string }{{allowed.URL, "fixture-header-value"}, {unrelated.URL, ""}} {
		observed := capabilityEval(t, h, fmt.Sprintf(`fetch(%q).then(r=>r.json())`, check.url))
		h.Check(t, "brw_set_extra_headers", observed["header"] == check.want, "header origin isolation failed at %s: %v", check.url, observed)
	}
	h.OK(t, "brw_set_extra_headers", map[string]any{"clear": true})
	cleared := capabilityEval(t, h, fmt.Sprintf(`fetch(%q).then(r=>r.json())`, allowed.URL))
	h.Check(t, "brw_set_extra_headers", cleared["header"] == "", "cleared header remained: %v", cleared)
}

func featureInitScript(t *testing.T, h *featureHarness) {
	added := h.OK(t, "brw_init_script", map[string]any{"action": "add", "source": "window.__ownedFeatureInit = document.readyState"})
	id := capabilityObject(t, added.Data["added"])["id"]
	listed := h.OK(t, "brw_init_script", map[string]any{"action": "list"})
	scripts, _ := listed.Data["scripts"].([]any)
	h.Check(t, "brw_init_script", id != nil && len(scripts) == 1 && capabilityObject(t, scripts[0])["id"] == id, "init script registration differed: %s", listed.Text)
	h.OK(t, "brw_navigate_to", map[string]any{"url": h.Fixture.URL() + "/?frames=none&init-phase=registered"})
	observed := capabilityEval(t, h, `({phase:window.__ownedFeatureInit})`)
	h.Check(t, "brw_init_script", observed["phase"] == "loading", "init script missed document initialization: %v", observed)
	h.OK(t, "brw_init_script", map[string]any{"action": "remove", "id": id})
	h.OK(t, "brw_navigate_to", map[string]any{"url": h.Fixture.URL() + "/?frames=none&init-phase=removed"})
	removed := capabilityEval(t, h, `({absent:typeof window.__ownedFeatureInit==='undefined'})`)
	h.Check(t, "brw_init_script", removed["absent"] == true, "removed init script still ran: %v", removed)
}

func featureTouch(t *testing.T, h *featureHarness) {
	h.OK(t, "brw_emulate_device", map[string]any{"device": "responsive", "width": 500, "height": 800, "mobile": false, "touch": true})
	capabilityEval(t, h, `(()=>{window.__ownedTouches={start:0,move:0,end:0};const pad=document.getElementById('pointer-pad');pad.scrollIntoView({block:'center'});for(const [kind,key] of [['touchstart','start'],['touchmove','move'],['touchend','end']])pad.addEventListener(kind,()=>window.__ownedTouches[key]++);return {ready:true}})()`)
	point := capabilityEval(t, h, `(()=>{const b=document.getElementById('pointer-pad').getBoundingClientRect();return {x:b.x+b.width/2,y:b.y+b.height/2}})()`)
	h.OK(t, "brw_touch", map[string]any{"action": "tap", "x": point["x"], "y": point["y"]})
	tapped := capabilityEval(t, h, `window.__ownedTouches`)
	h.Check(t, "brw_touch", tapped["start"] == float64(1) && tapped["end"] == float64(1), "touch tap did not dispatch both native events: %v", tapped)
	h.OK(t, "brw_touch", map[string]any{"action": "swipe", "x": point["x"], "y": point["y"], "to_x": point["x"].(float64) + 20, "to_y": point["y"], "duration_ms": 120})
	swiped := capabilityEval(t, h, `window.__ownedTouches`)
	h.Check(t, "brw_touch", swiped["start"] == float64(2) && swiped["end"] == float64(2) && swiped["move"].(float64) > 0, "swipe did not dispatch interpolated native events: %v", swiped)
	h.OK(t, "brw_emulate_device", map[string]any{"clear": true})
}

func featureProfile(t *testing.T, h *featureHarness) {
	service := featureServices(t, h)
	for _, kind := range []string{"cpu", "trace"} {
		started := h.OK(t, "brw_profile", map[string]any{"action": "start", "kind": kind})
		h.Check(t, "brw_profile", started.Data["running"] == true && started.Data["kind"] == kind, "capture failed to start: %s", started.Text)
		capabilityEval(t, h, `(()=>{let sum=0;for(let i=0;i<100000;i++)sum+=Math.sqrt(i);document.getElementById('signal').textContent='Profile fixture work';return {sum}})()`)
		stopped := h.OK(t, "brw_profile", map[string]any{"action": "stop", "kind": kind})
		stored := capabilityObject(t, stopped.Data["artifact"])
		h.Check(t, "brw_profile", stopped.Data["running"] == false && stored["size_bytes"].(float64) > 0 && len(fmt.Sprint(stored["sha256"])) == 64, "profile was not stored as a bounded artifact handle: %s", stopped.Text)
		var data []byte
		for {
			part, _, more, err := service.Store().Read(fmt.Sprint(stored["id"]), int64(len(data)), artifact.MaxReadBytes)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, part...)
			if len(data) > 8<<20 {
				t.Fatal("fixture profile exceeded artifact budget")
			}
			if !more {
				break
			}
		}
		digest := sha256.Sum256(data)
		h.Check(t, "brw_profile", json.Valid(data) && float64(len(data)) == stored["size_bytes"] && hex.EncodeToString(digest[:]) == fmt.Sprint(stored["sha256"]), "stored %s capture differs from artifact metadata", kind)
	}
}

func featureReact(t *testing.T, h *featureHarness) {
	missing := h.OK(t, "brw_react", map[string]any{"action": "tree"})
	h.Check(t, "brw_react", missing.Data["present"] == false, "plain fixture falsely reported React: %s", missing.Text)
	capabilityEval(t, h, `(()=>{const component={type:function OwnedFixtureComponent(){},key:'owned',memoizedProps:{label:'public fixture',nested:{privateField:'never-serialize-nested'}},memoizedState:{memoizedState:1,next:null},return:null};const host={type:'input',return:component};component.child=host;document.body['__reactContainer$ownedFixture']={type:null,child:component};document.getElementById('note')['__reactFiber$ownedFixture']=host;return {installed:true}})()`)
	tree := h.OK(t, "brw_react", map[string]any{"action": "tree", "limit": 3})
	h.Check(t, "brw_react", tree.Data["present"] == true && strings.Contains(tree.Text, "OwnedFixtureComponent") && tree.Data["count"] == float64(3), "synthetic fiber tree differed: %s", tree.Text)
	inspect := h.OK(t, "brw_react", map[string]any{"action": "inspect", "target": "#note"})
	component := capabilityObject(t, inspect.Data["inspect"])
	props := capabilityObject(t, component["props"])
	h.Check(t, "brw_react", component["component"] == "OwnedFixtureComponent" && props["nested"] == "[object Object]" && !strings.Contains(inspect.Text, "never-serialize-nested"), "React inspection lost bounds: %s", inspect.Text)
}

func featureNetworkCapture(t *testing.T, h *featureHarness) {
	path := "/fixture/status/201?owned-capture=1"
	h.OK(t, "brw_network_capture", map[string]any{"filter": "owned-capture"})
	capabilityEval(t, h, fmt.Sprintf(`fetch(%q).then(async r=>({status:r.status,body:await r.text()}))`, path))
	duplicate := h.Call(t, "brw_network_capture", map[string]any{"filter": "owned-capture"})
	h.Check(t, "brw_network_capture", duplicate.IsError && duplicate.Data["code"] == "approval_consumed", "consumed approval could replay an identical action: %s", duplicate.Text)
	captured := capabilityArray(t, h.OK(t, "brw_network_capture", map[string]any{"filter": "owned-capture", "limit": 10}))
	h.Check(t, "brw_network_capture", len(captured) == 1 && captured[0]["status"] == float64(201) && captured[0]["completed"] == true && captured[0]["capture_id"] != "", "terminal capture differed: %v", captured)
	drained := capabilityArray(t, h.OK(t, "brw_network_capture", map[string]any{"filter": "owned-capture", "limit": 1}))
	h.Check(t, "brw_network_capture", len(drained) == 0, "terminal capture consumed more than once: %v", drained)
	requests := capabilityArray(t, h.OK(t, "brw_network_requests", map[string]any{"filter": "owned-capture"}))
	h.Check(t, "brw_network_requests", len(requests) >= 1 && strings.Contains(fmt.Sprint(requests[len(requests)-1]["url"]), path), "performance resource missing: %v", requests)
}

func featureReplayRequest(t *testing.T, h *featureHarness) {
	first := h.OK(t, "brw_replay_request", map[string]any{"url": "/download", "method": "GET", "max_bytes": 8})
	h.Check(t, "brw_replay_request", first.Data["status"] == float64(200) && first.Data["body_truncated"] == true && first.Data["next_offset"] == float64(8), "body window not explicit: %s", first.Text)
	whole := h.OK(t, "brw_replay_request", map[string]any{"url": "/download", "method": "GET"})
	data := []byte(fmt.Sprint(whole.Data["body"]))
	digest := sha256.Sum256(data)
	h.Check(t, "brw_replay_request", hex.EncodeToString(digest[:]) == h.State(t).DownloadSHA256, "replayed download body differs from oracle: %s", whole.Text)
	redirect := h.OK(t, "brw_replay_request", map[string]any{"url": "/fixture/redirect?to=%2Ffixture%2Fstatus%2F202"})
	h.Check(t, "brw_replay_request", redirect.Data["url"] == h.Fixture.URL()+"/fixture/status/202" && redirect.Data["status"] == float64(202), "replay concealed the committed redirect URL: %s", redirect.Text)
}

func featureRoute(t *testing.T, h *featureHarness) {
	url := h.Fixture.URL() + "/fixture/status/200?owned-route=1"
	h.OK(t, "brw_route", map[string]any{"action": "add", "pattern": url, "behaviour": "fulfill", "status": 203, "body": "owned route body", "content_type": "text/plain"})
	observed := capabilityEval(t, h, fmt.Sprintf(`fetch(%q).then(async r=>({status:r.status,body:await r.text()}))`, url))
	h.Check(t, "brw_route", observed["status"] == float64(203) && observed["body"] == "owned route body", "fulfill did not control the exchange: %v", observed)
	h.OK(t, "brw_route", map[string]any{"action": "clear"})
	h.OK(t, "brw_route", map[string]any{"action": "add", "pattern": url, "behaviour": "abort"})
	aborted := capabilityEval(t, h, fmt.Sprintf(`fetch(%q).then(()=>({failed:false}),()=>({failed:true}))`, url))
	h.Check(t, "brw_route", aborted["failed"] == true, "abort rule did not fail the request: %v", aborted)
	h.OK(t, "brw_route", map[string]any{"action": "clear", "pattern": url})
	restored := capabilityEval(t, h, fmt.Sprintf(`fetch(%q).then(r=>({status:r.status}))`, url))
	h.Check(t, "brw_route", restored["status"] == float64(200), "clear failed to restore network: %v", restored)
}

func featureStorage(t *testing.T, h *featureHarness) {
	for _, kind := range []string{"local", "session"} {
		h.OK(t, "brw_storage", map[string]any{"kind": kind, "action": "set", "key": "owned-feature", "value": kind + "-value"})
		read := h.OK(t, "brw_storage", map[string]any{"kind": kind, "action": "get", "key": "owned-feature"})
		h.Check(t, "brw_storage", read.Data["value"] == kind+"-value", "storage read differed: %s", read.Text)
		h.OK(t, "brw_storage", map[string]any{"kind": kind, "action": "remove", "key": "owned-feature"})
		removed := h.OK(t, "brw_storage", map[string]any{"kind": kind, "action": "get", "key": "owned-feature"})
		h.Check(t, "brw_storage", removed.Data["value"] == nil && removed.Data["key"] == "owned-feature", "removed storage retained a value: %s", removed.Text)
	}
}

func featureWindow(t *testing.T, h *featureHarness) {
	before := h.OK(t, "brw_window_bounds", nil)
	h.Check(t, "brw_window_bounds", before.Data["inner_width"].(float64) > 0 && before.Data["inner_height"].(float64) > 0, "window dimensions missing: %s", before.Text)
	observed := capabilityEval(t, h, `({width:innerWidth,height:innerHeight,dpr:devicePixelRatio})`)
	h.Check(t, "brw_window_bounds", before.Data["inner_width"] == observed["width"] && before.Data["inner_height"] == observed["height"] && before.Data["device_pixel_ratio"] == observed["dpr"], "bounds differ from actual page geometry: bounds=%s page=%v", before.Text, observed)
	resize := h.OK(t, "brw_window_resize", map[string]any{"width": 900, "height": 700, "state": "normal"})
	h.Check(t, "brw_window_resize", resize.Data["ok"] == true && (resize.Data["clamped"] == true || (resize.Data["width"] == float64(900) && resize.Data["height"] == float64(700))), "native resize differs without an explicit clamp: %s", resize.Text)
	smaller := h.OK(t, "brw_window_resize", map[string]any{"width": 640, "height": 480, "state": "normal"})
	h.Check(t, "brw_window_resize", smaller.Data["width"] != resize.Data["width"] || smaller.Data["height"] != resize.Data["height"], "native geometry did not change after a second size: first=%s second=%s", resize.Text, smaller.Text)
	if before.Data["outer_width"].(float64) > 0 && before.Data["outer_height"].(float64) > 0 {
		h.OK(t, "brw_window_resize", map[string]any{"width": before.Data["outer_width"], "height": before.Data["outer_height"], "state": "normal"})
	}
}

func featureTabGroups(t *testing.T, h *featureHarness) {
	for _, item := range []struct {
		name string
		args map[string]any
	}{
		{"brw_list_tab_groups", nil},
		{"brw_group_tabs", map[string]any{"tab_ids": []string{h.TabID}, "name": "Owned fixture group", "color": "blue"}},
		{"brw_ungroup_tabs", map[string]any{"tab_ids": []string{h.TabID}}},
	} {
		result := h.Call(t, item.name, item.args)
		h.Unsupported(t, item.name, result, "tab grouping is unavailable on any CDP transport")
	}
}

func featureNotifyUnavailable(t *testing.T, h *featureHarness) {
	removed := capabilityEval(t, h, `(()=>{Object.defineProperty(window,'Notification',{value:undefined,configurable:true});return {absent:typeof Notification==='undefined'}})()`)
	if removed["absent"] != true {
		t.Fatal("owned page still has a Notification constructor")
	}
	result := h.OK(t, "brw_notify", map[string]any{"kind": "done", "title": "Owned fixture", "message": "Synthetic unavailable-channel probe"})
	h.Check(t, "brw_notify", result.Data["ok"] == false && result.Data["delivery"] == "unavailable" && strings.Contains(fmt.Sprint(result.Data["note"]), "not available"), "notification falsely claimed delivery: %s", result.Text)
	h.status["brw_notify"] = "unavailable"
}
