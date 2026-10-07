package mcp_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/testbed"
)

func init() {
	registerFeatureCase("page_navigation", []string{"brw_identity", "brw_open", "brw_list_tabs", "brw_focus_tab", "brw_close_tab", "brw_navigate", "brw_navigate_to", "brw_pushstate"}, featureNavigation)
	registerFeatureCase("page_reading", []string{"brw_read", "brw_read_data", "brw_snapshot", "brw_find", "brw_get", "brw_evaluate", "brw_observe"}, featureReading)
	registerFeatureCase("page_forms", []string{"brw_fill", "brw_type", "brw_select", "brw_click", "brw_click_text", "brw_focus", "brw_press"}, featureForms)
	registerFeatureCase("page_assertions", []string{"brw_assert", "brw_assert_hidden", "brw_assert_text", "brw_assert_value", "brw_assert_visible", "brw_batch", "brw_wait_for"}, featureAssertions)
}

func featureForms(t *testing.T, h *featureHarness) {
	note := h.Ref(t, "Fixture note")
	h.OK(t, "brw_fill", map[string]any{"ref": note, "text": "owned note"})
	h.Wait(t, func(s testbed.State) bool { return s.FormState.Note == "owned note" })
	h.Check(t, "brw_fill", featureGet(t, h, "value", note) == "owned note", "ordinary fill disagrees with oracle")
	h.OK(t, "brw_type", map[string]any{"ref": note, "text": "typed note"})
	h.Wait(t, func(s testbed.State) bool { return s.FormState.Note == "owned notetyped note" })
	h.Check(t, "brw_type", featureGet(t, h, "value", note) == "owned notetyped note", "type did not append to the exact field value")
	category := h.Ref(t, "Category")
	h.OK(t, "brw_select", map[string]any{"ref": category, "value": "Archive"})
	h.Check(t, "brw_select", featureGet(t, h, "value", category) == "Archive", "selection did not change exact control")
	h.OK(t, "brw_focus", map[string]any{"ref": note})
	h.Check(t, "brw_focus", featureGet(t, h, "focused", note) == true && h.Wait(t, func(s testbed.State) bool { return s.FocusName == "Fixture note" }).FocusName == "Fixture note", "focus disagrees with oracle")
	h.OK(t, "brw_press", map[string]any{"key": "Tab"})
	h.Check(t, "brw_press", featureGet(t, h, "focused", category) == true, "Tab did not advance focus to next field")
	stable := h.Ref(t, "Stable action")
	h.OK(t, "brw_click", map[string]any{"ref": stable})
	h.Wait(t, func(s testbed.State) bool { return s.ActionCounts["stable-action"] == 1 })
	h.Check(t, "brw_click", h.State(t).ActionCounts["stable-action"] == 1, "ref action was not recorded exactly once")
	h.Event(t, "hydrate")
	current := h.Ref(t, "Stable action")
	metadata := h.OK(t, "brw_get", map[string]any{"what": "attr", "target": current, "name": "data-epoch"})
	h.Check(t, "brw_click", metadata.Data["value"] == fmt.Sprint(h.State(t).DocumentEpoch) && h.State(t).ActionCounts["stable-action"] == 1, "hydrated target does not match current document epoch")
	h.OK(t, "brw_click_text", map[string]any{"text": "Stable action", "role": "button"})
	h.Wait(t, func(s testbed.State) bool { return s.ActionCounts["stable-action"] == 2 })
	h.Check(t, "brw_click_text", h.State(t).ActionCounts["stable-action"] == 2, "fresh semantic target was not clicked exactly once")
	h.RequireApprovals(t)
	password := h.Ref(t, "Fixture account password")
	card := h.Ref(t, "Fixture card number")
	before := h.State(t)
	h.Check(t, "brw_fill", !before.FormState.PasswordSupplied && !before.FormState.CardSupplied, "fixture begins with sensitive input")
	h.Approved(t, "brw_fill", map[string]any{"ref": password, "text": "synthetic-password-only"})
	h.Approved(t, "brw_fill", map[string]any{"ref": card, "text": "4242424242424242"})
	h.Wait(t, func(s testbed.State) bool { return s.FormState.PasswordSupplied && s.FormState.CardSupplied })
	for _, ref := range []string{password, card} {
		withheld := h.OK(t, "brw_get", map[string]any{"what": "value", "target": ref})
		h.Check(t, "brw_fill", withheld.Data["sensitive"] == true && withheld.Data["value"] == "", "typed sensitive read disclosed its value")
	}
	privateSnapshot := h.OK(t, "brw_snapshot", map[string]any{"mode": "all", "limit": 200})
	h.Check(t, "brw_fill", !strings.Contains(privateSnapshot.Text, "synthetic-password-only") && !strings.Contains(privateSnapshot.Text, "4242424242424242"), "snapshot disclosed synthetic credential data")
	payment := h.Ref(t, "Submit payment")
	denied := h.Call(t, "brw_click", map[string]any{"ref": payment})
	h.Check(t, "brw_click", denied.IsError && denied.Data["code"] == "approval_required" && !h.State(t).FormState.PaymentSubmitted, "sensitive form mutated before approval")
	h.approveResult(t, "brw_click", map[string]any{"ref": payment}, denied)
	h.Wait(t, func(s testbed.State) bool { return s.FormState.PaymentSubmitted })
	h.Check(t, "brw_click", h.State(t).FormState.PaymentSubmitted, "approved exact synthetic payment did not reach oracle")
}

func featureAssertions(t *testing.T, h *featureHarness) {
	note := h.Ref(t, "Fixture note")
	h.OK(t, "brw_fill", map[string]any{"ref": note, "text": "assertion fixture"})
	h.Wait(t, func(s testbed.State) bool { return s.FormState.Note == "assertion fixture" })
	h.OK(t, "brw_assert_value", map[string]any{"ref": note, "value": "assertion fixture", "timeout_ms": 100})
	badValue := h.Call(t, "brw_assert_value", map[string]any{"ref": note, "value": "incorrect", "timeout_ms": 30})
	h.Check(t, "brw_assert_value", badValue.IsError && featureGet(t, h, "value", note) == "assertion fixture", "assertion accepted an incorrect ordinary value")
	stable := h.Ref(t, "Stable action")
	h.OK(t, "brw_assert_text", map[string]any{"ref": stable, "text": "Stable action", "timeout_ms": 100})
	badText := h.Call(t, "brw_assert_text", map[string]any{"ref": stable, "text": "wrong target text", "timeout_ms": 30})
	h.Check(t, "brw_assert_text", badText.IsError, "text assertion accepted a wrong field")
	h.OK(t, "brw_assert_visible", map[string]any{"ref": stable, "timeout_ms": 100})
	h.Check(t, "brw_assert_visible", featureGet(t, h, "visible", stable) == true, "visible assertion disagrees with typed state")
	h.Event(t, "overlay")
	dismiss := h.Ref(t, "Dismiss overlay")
	h.Event(t, "overlay")
	h.OK(t, "brw_assert_hidden", map[string]any{"ref": dismiss, "timeout_ms": 100})
	h.Check(t, "brw_assert_hidden", featureGet(t, h, "hidden", dismiss) == true, "hidden assertion disagrees with closed overlay")
	assertion := h.OK(t, "brw_assert", map[string]any{"assertion": "element_count", "selector": "#report table tbody tr", "count": 4})
	wrong := h.Call(t, "brw_assert", map[string]any{"assertion": "element_count", "selector": "#report table tbody tr", "count": 3})
	h.Check(t, "brw_assert", assertion.Data["ok"] == true && wrong.IsError, "count assertion accepted wrong report source shape")
	batch := h.OK(t, "brw_batch", map[string]any{"steps": []any{map[string]any{"action": "assert_value", "ref": note, "value": "assertion fixture", "timeout_ms": 100}, map[string]any{"action": "fill", "ref": note, "text": "batched fixture"}, map[string]any{"action": "assert_value", "ref": note, "value": "batched fixture", "timeout_ms": 100}}})
	h.Wait(t, func(s testbed.State) bool { return s.FormState.Note == "batched fixture" })
	h.Check(t, "brw_batch", batch.Data["ok"] == true && batch.Data["steps_completed"] == float64(3) && featureGet(t, h, "value", note) == "batched fixture", "batch did not execute and assert all three steps: %s", batch.Text)
	state := h.Event(t, "mutation")
	wait := h.OK(t, "brw_wait_for", map[string]any{"condition": "text:" + state.MutationLabel, "timeout_ms": 500})
	missing := h.Call(t, "brw_wait_for", map[string]any{"condition": "selector:#missing-owned-fixture", "timeout_ms": 30})
	h.Check(t, "brw_wait_for", wait.Data["ok"] == true && missing.IsError && featureGet(t, h, "text", "#signal") == state.MutationLabel, "wait accepted absent element or wrong mutation")
}

func featureNavigation(t *testing.T, h *featureHarness) {
	identity := h.OK(t, "brw_identity", nil)
	profile, _ := identity.Data["identity"].(map[string]any)
	h.Check(t, "brw_identity", identity.Data["connected"] == true && profile["profile"] == "owned-fixture" && profile["headless"] == true, "wrong isolated identity: %s", identity.Text)
	first := h.TabID
	opened := h.OK(t, "brw_open", map[string]any{"url": h.Fixture.URL() + "/?frames=none&navigation=second"})
	tab, _ := opened.Data["tab"].(map[string]any)
	second, _ := tab["id"].(string)
	h.Check(t, "brw_open", second != "" && second != first && tab["url"] == h.Fixture.URL()+"/?frames=none&navigation=second", "tab did not open exact owned destination: %s", opened.Text)
	listed := h.OK(t, "brw_list_tabs", nil)
	var tabs []map[string]any
	if err := json.Unmarshal([]byte(listed.Text), &tabs); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, tab := range tabs {
		if id, ok := tab["id"].(string); ok {
			found[id] = true
		}
	}
	h.Check(t, "brw_list_tabs", found[first] && found[second], "owned tabs missing: %s", listed.Text)
	h.OK(t, "brw_focus_tab", map[string]any{"id": first})
	active, err := h.Manager.ActiveTabID(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.Check(t, "brw_focus_tab", active == first, "focus did not pin first tab")
	h.OK(t, "brw_close_tab", map[string]any{"id": second})
	h.Wait(t, func(testbed.State) bool {
		remaining, err := h.Manager.ListTabs(h.Ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, tab := range remaining {
			if tab.ID == second {
				return false
			}
		}
		return true
	})
	h.Check(t, "brw_close_tab", true, "closed tab remains present")
	h.TabID = first
	destination := h.Fixture.URL() + "/?frames=none&navigation=changed"
	nav := h.OK(t, "brw_navigate_to", map[string]any{"url": destination})
	h.Check(t, "brw_navigate_to", nav.Data["url"] == destination && featureGet(t, h, "url", "") == destination, "destination not committed: %s", nav.Text)
	back := h.OK(t, "brw_navigate", map[string]any{"direction": "back"})
	h.Check(t, "brw_navigate", back.Data["url"] == h.Fixture.URL()+"/?frames=none" && featureGet(t, h, "url", "") == h.Fixture.URL()+"/?frames=none", "history back returned wrong document: %s", back.Text)
	state := h.State(t)
	push := h.OK(t, "brw_pushstate", map[string]any{"url": h.Fixture.URL() + "/?frames=none&navigation=spa", "state": `{"fixture":true}`})
	h.Check(t, "brw_pushstate", push.Data["url"] == h.Fixture.URL()+"/?frames=none&navigation=spa" && h.State(t).RunID == state.RunID, "same-document history failed: %s", push.Text)
}

func featureReading(t *testing.T, h *featureHarness) {
	state := h.State(t)
	read := h.OK(t, "brw_read", map[string]any{"max_chars": 20000})
	main, _ := read.Data["main"].(string)
	verified := fmt.Sprintf("Fictional observer %s visited station %s. The verified water measurement is %s litres.", state.Reading.Facts["observer"], state.Reading.Facts["site_code"], state.Reading.Facts["water_litres"])
	h.Check(t, "brw_read", strings.Contains(main, verified) && strings.Contains(main, state.Reading.RequiredPhrases[0]) && !strings.Contains(main, "Wrong Person"), "read did not preserve the corrected report source and fields")
	h.Check(t, "brw_read", featureHasMeasurementTable(read.Text, state), "read table did not bind oracle fields to verified columns")
	metadata, _ := json.Marshal(map[string]any{"@context": "https://schema.org", "@type": "Product", "name": "Verified station " + state.Reading.Facts["site_code"], "offers": map[string]any{"price": state.Reading.Facts["water_litres"], "priceCurrency": "LITRE"}})
	h.OK(t, "brw_evaluate", map[string]any{"expression": `(()=>{const s=document.createElement('script');s.type='application/ld+json';s.textContent=JSON.stringify(` + string(metadata) + `);document.head.append(s);return {installed:true}})()`})
	structured := h.OK(t, "brw_read_data", nil)
	h.Check(t, "brw_read_data", structured.Data["source"] == "json_ld" && structured.Data["name"] == "Verified station "+state.Reading.Facts["site_code"] && structured.Data["price"] == state.Reading.Facts["water_litres"] && structured.Data["currency"] == "LITRE", "structured metadata lost exact source/field bindings: %s", structured.Text)
	snap := h.OK(t, "brw_snapshot", map[string]any{"mode": "all", "limit": 200, "include_frames": true, "include_ax": true})
	elements, _ := snap.Data["elements"].([]any)
	h.Check(t, "brw_snapshot", featureElement(elements, "Stable action") != "" && featureElement(elements, "Shadow action") != "" && !strings.Contains(snap.Text, "Wrong Person"), "snapshot omitted reachable semantic controls")
	find := h.OK(t, "brw_find", map[string]any{"query": "Fixture note", "role": "textbox", "limit": 20})
	found, _ := find.Data["elements"].([]any)
	ref := featureElement(found, "Fixture note")
	h.Check(t, "brw_find", ref != "", "semantic textbox search returned no exact-name target: %s", find.Text)
	value := h.OK(t, "brw_get", map[string]any{"what": "value", "target": ref})
	h.Check(t, "brw_get", value.Data["value"] == "" && value.Data["sensitive"] != true, "ordinary empty input was not distinguished from withholding: %s", value.Text)
	eval := h.OK(t, "brw_evaluate", map[string]any{"expression": `({title:document.title,station:document.querySelector('#report table tbody tr').cells[1].textContent,source:document.querySelector('#report').getAttribute('aria-label')})`})
	h.Check(t, "brw_evaluate", eval.Data["station"] == state.Reading.Facts["site_code"] && eval.Data["source"] == "Verified field report", "evaluation answered wrong source or field: %s", eval.Text)
	before := h.OK(t, "brw_observe", nil)
	h.Event(t, "mutation")
	after := h.OK(t, "brw_observe", nil)
	h.Check(t, "brw_observe", before.Data["url"] == after.Data["url"] && before.Text != after.Text, "observation did not distinguish mutation")
}

func featureGet(t *testing.T, h *featureHarness, what, target string) any {
	t.Helper()
	args := map[string]any{"what": what}
	if target != "" {
		args["target"] = target
	}
	return h.OK(t, "brw_get", args).Data["value"]
}

func featureElement(elements []any, name string) string {
	for _, value := range elements {
		if e, ok := value.(map[string]any); ok && e["name"] == name {
			if ref, ok := e["ref"].(string); ok {
				return ref
			}
		}
	}
	return ""
}

func featureHasMeasurementTable(text string, state testbed.State) bool {
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(value any) bool {
		switch v := value.(type) {
		case map[string]any:
			rows, ok := v["rows"].([]any)
			if ok && v["caption"] == "Verified measurements" {
				pairs := map[string]string{}
				for _, row := range rows {
					if cells, ok := row.([]any); ok && len(cells) == 2 {
						left, _ := cells[0].(string)
						right, _ := cells[1].(string)
						pairs[left] = right
					}
				}
				if pairs["Station"] == state.Reading.Facts["site_code"] && pairs["Observer"] == state.Reading.Facts["observer"] && pairs["Water litres"] == state.Reading.Facts["water_litres"] {
					return true
				}
			}
			for _, child := range v {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range v {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(decoded)
}
