package mcp_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/pagewatch"
	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/Don-Works/brw/internal/testbed"
)

func init() {
	registerFeatureCase("page_pointer", []string{"brw_key_down", "brw_key_up", "brw_mouse_down", "brw_mouse_up", "brw_drag", "brw_click_xy", "brw_hover", "brw_highlight", "brw_scroll"}, featurePointer)
	registerFeatureCase("page_frames", []string{"brw_frame"}, featureFrames)
	registerFeatureCase("page_dialogs", []string{"brw_dialog"}, featureDialogs)
	registerFeatureCase("page_events", []string{"brw_watch_page", "brw_page_watchers", "brw_page_events"}, featureEvents)
}

func featurePointer(t *testing.T, h *featureHarness) {
	hover := h.Ref(t, "Hover fixture target")
	h.OK(t, "brw_hover", map[string]any{"ref": hover})
	h.Wait(t, func(s testbed.State) bool { return s.ActionCounts["hover"] > 0 })
	h.Check(t, "brw_hover", h.State(t).ActionCounts["hover"] > 0, "hover did not reach owned target")
	marked := h.OK(t, "brw_highlight", map[string]any{"ref": hover, "label": "owned highlight", "duration_ms": 10000})
	h.Check(t, "brw_highlight", marked.Data["active"] == float64(1) && featureGet(t, h, "count", "#__brw_highlight_overlay") == float64(1), "highlight overlay was not installed")
	h.OK(t, "brw_highlight", map[string]any{"clear": true})
	h.Check(t, "brw_highlight", featureGet(t, h, "count", "#__brw_highlight_overlay") == float64(0), "highlight overlay did not clear")
	h.OK(t, "brw_scroll", map[string]any{"target": "#pointer-pad"})
	pad := featureBox(t, h, "#pointer-pad")
	h.Check(t, "brw_scroll", pad["y"] >= 0 && pad["y"] < 800, "target scroll left pointer fixture offscreen: %+v", pad)
	h.OK(t, "brw_evaluate", map[string]any{"expression": `(()=>{window.__featureInput=[];for(const type of ['keydown','keyup','mousedown','mouseup','mousemove'])document.addEventListener(type,e=>window.__featureInput.push({type,key:e.key||'',shift:e.shiftKey,trusted:e.isTrusted,x:e.clientX||0,y:e.clientY||0,target:e.target.id}),true);return {installed:true}})()`})
	down := h.OK(t, "brw_key_down", map[string]any{"key": "Shift"})
	held, _ := down.Data["held"].([]any)
	h.Check(t, "brw_key_down", len(held) == 1 && featureInput(t, h, "keydown", "Shift", true, ""), "held key did not dispatch trusted Shift keydown")
	x, y := pad["x"]+pad["width"]-10, pad["y"]+pad["height"]-10
	h.OK(t, "brw_mouse_down", map[string]any{"x": x, "y": y})
	h.Wait(t, func(s testbed.State) bool { return s.ActionCounts["pointer"] > 0 })
	h.Check(t, "brw_mouse_down", featureInput(t, h, "mousedown", "", true, "pointer-pad"), "mouse down lost held Shift or trusted event")
	h.OK(t, "brw_mouse_up", map[string]any{"x": x, "y": y})
	h.Check(t, "brw_mouse_up", featureInput(t, h, "mouseup", "", true, "pointer-pad"), "mouse up did not reach same pointer target")
	up := h.OK(t, "brw_key_up", map[string]any{"key": "all"})
	released, _ := up.Data["held"].([]any)
	h.Check(t, "brw_key_up", len(released) == 0 && featureInput(t, h, "keyup", "Shift", false, ""), "key up retained Shift or did not dispatch release")
	from := featureBox(t, h, "#drag-chip")
	to := featureBox(t, h, "#drop-zone")
	before := h.State(t).ActionCounts["pointer"]
	h.OK(t, "brw_drag", map[string]any{"from": map[string]any{"x": from["x"] + from["width"]/2, "y": from["y"] + from["height"]/2}, "to": map[string]any{"x": to["x"] + to["width"]/2, "y": to["y"] + to["height"]/2}, "steps": 12})
	h.Wait(t, func(s testbed.State) bool { return s.ActionCounts["pointer"] > before })
	h.Check(t, "brw_drag", h.State(t).ActionCounts["drag"] == 1 && featureGet(t, h, "text", "#drop-zone") == "Fixture chip dropped", "drag did not reach drop oracle: %+v", h.State(t).ActionCounts)
	h.OK(t, "brw_scroll", map[string]any{"target": "#board"})
	board := featureBox(t, h, "#board")
	state := h.State(t)
	var target testbed.VisualTarget
	for _, candidate := range state.VisualTargets {
		if candidate.Target {
			target = candidate
		}
	}
	h.OK(t, "brw_click_xy", map[string]any{"x": board["x"] + (float64(target.X)+float64(target.Width)/2)*board["width"]/320, "y": board["y"] + (float64(target.Y)+float64(target.Height)/2)*board["height"]/140})
	h.Wait(t, func(s testbed.State) bool { return s.VisualSelected == target.ID })
	h.Check(t, "brw_click_xy", target.ID == "K9" && h.State(t).VisualSelected == target.ID, "coordinate action selected visual distractor")
}

func featureBox(t *testing.T, h *featureHarness, target string) map[string]float64 {
	t.Helper()
	value := featureGet(t, h, "box", target)
	raw, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s has no observed box", target)
	}
	box := map[string]float64{}
	for _, key := range []string{"x", "y", "width", "height"} {
		box[key], ok = raw[key].(float64)
		if !ok {
			t.Fatalf("%s box missing %s", target, key)
		}
	}
	return box
}

func featureInput(t *testing.T, h *featureHarness, kind, key string, shift bool, target string) bool {
	t.Helper()
	result := h.OK(t, "brw_evaluate", map[string]any{"expression": fmt.Sprintf(`({events:window.__featureInput,phase:%d})`, h.called["brw_evaluate"])})
	rows, _ := result.Data["events"].([]any)
	for _, row := range rows {
		if event, ok := row.(map[string]any); ok && event["type"] == kind && event["key"] == key && event["shift"] == shift && event["trusted"] == true && (target == "" || event["target"] == target) {
			return true
		}
	}
	return false
}

func featureFrames(t *testing.T, h *featureHarness) {
	h.OK(t, "brw_navigate_to", map[string]any{"url": h.Fixture.URL() + "/#frames"})
	h.OK(t, "brw_wait_for", map[string]any{"condition": "selector:#frame-action", "timeout_ms": 2000})
	same := h.OK(t, "brw_frame", map[string]any{"target": "#same-frame"})
	h.Check(t, "brw_frame", same.Data["accessible"] == true && same.Data["scope"] == "#same-frame", "same-origin frame was not selected")
	child := h.Ref(t, "Frame action")
	h.OK(t, "brw_click", map[string]any{"ref": child})
	h.Wait(t, func(s testbed.State) bool { return s.ActionCounts["frame-click"] == 1 })
	h.OK(t, "brw_frame", map[string]any{"target": "main"})
	source := h.OK(t, "brw_get", map[string]any{"what": "attr", "target": "#cross-frame", "name": "src"})
	h.Check(t, "brw_frame", strings.HasPrefix(fmt.Sprint(source.Data["value"]), h.Fixture.FrameURL()+"/frame?"), "cross-frame source is not the owned child origin")
	ref := ""
	h.Wait(t, func(testbed.State) bool {
		snap := h.OK(t, "brw_snapshot", map[string]any{"mode": "all", "limit": 200, "include_frames": true})
		elements, _ := snap.Data["elements"].([]any)
		for _, value := range elements {
			if e, ok := value.(map[string]any); ok && e["name"] == "Frame action" {
				candidate, _ := e["ref"].(string)
				if strings.Contains(candidate, ":") {
					ref = candidate
				}
			}
		}
		return ref != ""
	})
	cross := h.OK(t, "brw_frame", map[string]any{"target": ref})
	h.Check(t, "brw_frame", cross.Data["kind"] == "cross_origin" && cross.Data["accessible"] == false && cross.Data["switched"] == false, "cross-origin frame scope pretended to be readable through parent DOM")
	if _, err := h.Consent.Revoke(h.Fixture.FrameURL(), siteconsent.ScopeAct, "owned-fixture-test"); err != nil {
		t.Fatal(err)
	}
	denied := h.Call(t, "brw_click", map[string]any{"ref": ref})
	h.Check(t, "brw_frame", denied.IsError && strings.Contains(denied.Text, h.Fixture.FrameURL()) && strings.Contains(denied.Text, "act") && h.State(t).ActionCounts["frame-click"] == 1, "child Read-only consent authorized a child action")
	if _, err := h.Consent.Allow(siteconsent.GrantOptions{Origin: h.Fixture.FrameURL(), Scope: siteconsent.ScopeAct, Actor: "owned-fixture-test"}); err != nil {
		t.Fatal(err)
	}
	h.OK(t, "brw_click", map[string]any{"ref": ref})
	h.Wait(t, func(s testbed.State) bool { return s.ActionCounts["frame-click"] == 2 })
	h.Check(t, "brw_frame", h.State(t).ActionCounts["frame-click"] == 2, "child Act grant did not allow exactly one action")
}

func featureDialogs(t *testing.T, h *featureHarness) {
	h.OK(t, "brw_dialog", map[string]any{"action": "expect", "response": "accept", "prompt_text": "owned answer", "count": 3})
	for _, label := range []string{"Show fixture alert", "Show fixture confirm", "Show fixture prompt"} {
		h.OK(t, "brw_click_text", map[string]any{"text": label, "role": "button"})
	}
	h.Wait(t, func(s testbed.State) bool { return s.ActionCounts["dialog"] == 3 })
	status := h.OK(t, "brw_dialog", map[string]any{"action": "status"})
	rows, _ := status.Data["dialogs"].([]any)
	types := map[string]bool{}
	for _, value := range rows {
		if record, ok := value.(map[string]any); ok {
			kind, _ := record["type"].(string)
			types[kind] = record["accepted"] == true && record["decided_by"] == "armed" && strings.HasPrefix(fmt.Sprint(record["url"]), h.Fixture.URL())
		}
	}
	h.Check(t, "brw_dialog", status.Data["count"] == float64(3) && types["alert"] && types["confirm"] && types["prompt"], "native dialog expectations did not answer all three owned dialogs: %s", status.Text)
	empty := h.OK(t, "brw_dialog", map[string]any{"action": "status", "peek": true})
	h.Check(t, "brw_dialog", empty.Data["count"] == float64(0), "consumed dialog ledger repeated records")
}

func featureEvents(t *testing.T, h *featureHarness) {
	initial := h.State(t)
	h.Event(t, "disconnect")
	reconnected := h.Wait(t, func(s testbed.State) bool {
		return s.SSEConnectionCount > initial.SSEConnectionCount && s.WSConnectionCount > initial.WSConnectionCount && s.AppliedCursor == s.Cursor && s.AcknowledgedCursor == s.Cursor
	})
	if reconnected.DisconnectCount != 1 || reconnected.Cursor != 1 {
		t.Fatal("stream replay duplicated disconnect or logical cursor")
	}
	virtual := h.Event(t, "virtualize")
	text := fmt.Sprint(featureGet(t, h, "text", "#virtual-list"))
	lines := []string{}
	for _, id := range virtual.VisibleItemIDs {
		lines = append(lines, "Inventory row "+id)
	}
	if strings.Join(strings.Fields(text), " ") != strings.Join(lines, " ") {
		t.Fatalf("visible virtual rows differ from exact oracle window: %q", text)
	}
	service, err := pagewatch.New(h.Ctx, h.Manager, filepath.Join(t.TempDir(), "watchers"), nil, h.Consent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	h.Server.SetPageWatchAPI(service)
	watch := h.OK(t, "brw_watch_page", map[string]any{"id": "owned-signal", "url": h.Fixture.URL() + "/?frames=none", "selector": "#signal", "mode": "text", "interval_ms": 1000})
	h.Check(t, "brw_watch_page", watch.Data["id"] == "owned-signal" && watch.Data["url"] == h.Fixture.URL()+"/?frames=none", "watcher did not pin exact owned URL/selector")
	h.Wait(t, func(testbed.State) bool {
		listed := h.OK(t, "brw_page_watchers", nil)
		rows, _ := listed.Data["watchers"].([]any)
		if len(rows) != 1 {
			return false
		}
		row, _ := rows[0].(map[string]any)
		return row["last_sample_at"] != nil
	})
	state := h.Event(t, "mutation")
	var event featureResult
	h.Wait(t, func(testbed.State) bool {
		event = h.OK(t, "brw_page_events", map[string]any{"watcher_id": "owned-signal", "since_seq": 0})
		rows, _ := event.Data["events"].([]any)
		return len(rows) > 0
	})
	rows, _ := event.Data["events"].([]any)
	first, _ := rows[0].(map[string]any)
	h.Check(t, "brw_page_events", first["kind"] == "changed" && first["digest"] != "" && !strings.Contains(event.Text, state.MutationLabel), "page event disclosed sampled private text or missed mutation: %s", event.Text)
	cursor := event.Data["latest_seq"]
	replay := h.OK(t, "brw_page_events", map[string]any{"watcher_id": "owned-signal", "since_seq": 0})
	left, _ := json.Marshal(event.Data["events"])
	right, _ := json.Marshal(replay.Data["events"])
	h.Check(t, "brw_page_events", string(left) == string(right), "read-only event replay changed or acknowledged its cursor")
	after := h.OK(t, "brw_page_events", map[string]any{"watcher_id": "owned-signal", "since_seq": cursor})
	remaining, _ := after.Data["events"].([]any)
	h.Check(t, "brw_page_events", len(remaining) == 0 && after.Data["latest_seq"] == cursor, "exclusive cursor repeated an acknowledged event")
	paused := h.OK(t, "brw_page_watchers", map[string]any{"action": "pause", "id": "owned-signal"})
	h.Check(t, "brw_page_watchers", strings.Contains(paused.Text, `"enabled":false`), "pause did not disable watcher")
	h.OK(t, "brw_page_watchers", map[string]any{"action": "resume", "id": "owned-signal"})
	h.OK(t, "brw_page_watchers", map[string]any{"action": "remove", "id": "owned-signal"})
	listed := h.OK(t, "brw_page_watchers", nil)
	watchers, _ := listed.Data["watchers"].([]any)
	h.Check(t, "brw_page_watchers", len(watchers) == 0 && !strings.Contains(listed.Text, "owned-signal"), "removed watcher retained registration")
}
