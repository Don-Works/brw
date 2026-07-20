package mcp

import (
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

// TestEmulateDeviceSchemaAdvertisesEveryPreset stops the tool schema drifting
// from the presets brw actually registers. Models lean on the description text
// at least as much as the enum, and listing a subset there taught agents to
// guess unlisted model names — brw_emulate_device was the worst-performing
// tool in the usage ledger at a 31.6% error rate, almost all of it bad names.
func TestEmulateDeviceSchemaAdvertisesEveryPreset(t *testing.T) {
	var device map[string]any
	for _, tl := range tools() {
		if tl["name"] != "brw_emulate_device" {
			continue
		}
		schema, _ := tl["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		device, _ = props["device"].(map[string]any)
	}
	if device == nil {
		t.Fatal("brw_emulate_device has no device property")
	}

	// stringEnumSchema stores []string; tolerate []any in case that changes.
	enum := map[string]bool{}
	switch values := device["enum"].(type) {
	case []string:
		for _, v := range values {
			enum[v] = true
		}
	case []any:
		for _, v := range values {
			if s, ok := v.(string); ok {
				enum[s] = true
			}
		}
	default:
		t.Fatalf("device enum has unexpected type %T", device["enum"])
	}
	description, _ := device["description"].(string)

	for _, name := range browser.SupportedDevicePresets() {
		if !enum[name] {
			t.Errorf("device enum omits registered preset %q", name)
		}
		if !strings.Contains(description, name) {
			t.Errorf("device description omits registered preset %q", name)
		}
	}
}
