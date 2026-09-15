package mcp

import (
	"context"
	"encoding/json"
	"os"

	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

// crossOriginRef is the shape of a ref that names an element inside a
// cross-origin iframe: a frame index and a ref minted in that frame's document.
const crossOriginRef = "f0:e7"

// refParameterNames are the schema properties that carry an element ref. They are
// listed rather than pattern-matched so a count like max_refs cannot be mistaken
// for one, and so a new spelling has to be added deliberately.
var refParameterNames = map[string]bool{
	"ref":       true,
	"refs":      true,
	"target":    true,
	"click_ref": true,
}

// TestEveryToolTakingARefIsClassifiedForCrossOriginRefs walks the real catalogue
// and requires each tool advertising a ref parameter to say what it does with a
// ref inside a cross-origin iframe.
//
// The Controller-level enumeration cannot see these: brw_get, brw_highlight and
// brw_artifact_capture hand their ref to Evaluate, to the devtools observer and
// to the artifact service, none of which is a ref-taking Controller method. So
// the tool schema is the second place the property is anchored, and a new tool
// with a ref parameter fails here until someone decides.
func TestEveryToolTakingARefIsClassifiedForCrossOriginRefs(t *testing.T) {
	seen := map[string]bool{}
	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		schema, _ := tl["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		var refParams []string
		for key, spec := range properties {
			if !refParameterNames[strings.ToLower(key)] {
				continue
			}
			// max_rules-style integers are not refs; neither is a boolean.
			if kind, _ := spec.(map[string]any)["type"].(string); kind != "string" && kind != "array" {
				continue
			}
			refParams = append(refParams, key)
		}
		if len(refParams) == 0 {
			continue
		}
		seen[name] = true
		entry, classified := refTakingTools[name]
		if !classified {
			t.Errorf("%s advertises %v but is unclassified: say what it does with a ref inside a cross-origin iframe, so such a ref cannot reach it unchecked", name, refParams)
			continue
		}
		if strings.TrimSpace(entry.Reason) == "" {
			t.Errorf("%s is classified with no reason; the reason is what a reviewer argues with", name)
		}
	}
	for name := range refTakingTools {
		if !seen[name] {
			t.Errorf("refTakingTools names %s, which advertises no ref parameter any more", name)
		}
	}
}

// TestToolsGuardedHereRefuseCrossOriginRefsByName is the half the Controller
// enumeration cannot reach: these three refuse in callTool or not at all.
//
// The fake controller would happily run each of them, so a refusal here is the
// guard and nothing else. It has to be recognisable (errors.Is on the sentinel),
// not just differently worded — a caller that cannot tell the capability gap from
// a stale ref re-snapshots forever.
func TestToolsGuardedHereRefuseCrossOriginRefsByName(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"brw_get", map[string]any{"what": "value", "target": crossOriginRef}},
		{"brw_get", map[string]any{"what": "count", "target": crossOriginRef}},
		{"brw_highlight", map[string]any{"ref": crossOriginRef}},
		{"brw_highlight", map[string]any{"refs": []string{"e1", crossOriginRef}}},
		{"brw_artifact_capture", map[string]any{"kind": "screenshot", "ref": crossOriginRef}},
	}
	server := New(fakeController{})
	for _, tc := range cases {
		t.Run(tc.tool+"/"+firstKey(tc.args), func(t *testing.T) {
			payload, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}
			result, rpcErr := server.callTool(context.Background(), tc.tool, payload)
			text := rpcErrText(rpcErr) + resultText(result)
			if !strings.Contains(text, crossOriginRef) {
				t.Fatalf("%s did not name the ref it refused: %s", tc.tool, text)
			}
			if !strings.Contains(text, snapshot.ErrCrossOriginFrameUnsupported.Error()) {
				t.Fatalf("%s refused with %q, which callers cannot recognise as the capability gap", tc.tool, text)
			}
		})
	}
}

// staleFrameRefShape is the ref shape brw no longer mints. The element half of
// an f<i>: ref is a ref from the FRAME's own walk, so it carries the walker's
// collision suffixes (e6_edit_2) and is not always e<j>.
const staleFrameRefShape = "f<i>:e<j>"

// TestNothingAdvertisesTheOldFrameRefShape keeps the tool surface and the skill
// telling an agent the same thing about a ref it will actually be handed.
//
// A tool's prose and its parameter schema drifted apart once already: the
// description was rewritten to f<i>:<ref> and brw_frame's target schema kept
// f<i>:e<j>, which is the line an agent reads when it builds the argument. The
// catalogue and the skill are walked rather than listed so a third copy cannot
// appear somewhere nobody thought to check.
func TestNothingAdvertisesTheOldFrameRefShape(t *testing.T) {
	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		description, _ := tl["description"].(string)
		if strings.Contains(description, staleFrameRefShape) {
			t.Errorf("%s describes frame refs as %s; the element half is a ref from the frame's own walk, which carries the walker's suffixes", name, staleFrameRefShape)
		}
		schema, _ := tl["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		for key, spec := range properties {
			text, _ := spec.(map[string]any)["description"].(string)
			if strings.Contains(text, staleFrameRefShape) {
				t.Errorf("%s's %s schema describes frame refs as %s, which is the line an agent reads when it builds the argument", name, key, staleFrameRefShape)
			}
		}
	}
	skill, err := os.ReadFile("../../skills/brw/SKILL.md")
	if err != nil {
		t.Fatalf("read skills/brw/SKILL.md: %v", err)
	}
	if strings.Contains(string(skill), staleFrameRefShape) {
		t.Errorf("skills/brw/SKILL.md describes frame refs as %s", staleFrameRefShape)
	}
}

// TestSequenceToolsSayTheyRefuseACrossOriginRef closes a gap between what brw
// tells an agent and what it does.
//
// brw_snapshot's description tells an agent that an f<i>:<ref> can be passed to
// brw_click and the click lands in that frame. ExecutePlan and ExecuteBatch then
// refuse such a ref for the whole call — correctly, since a step does not route
// into the frame — but nothing an agent reads said so, so it batched the click it
// had just been told worked and got a refusal.
func TestSequenceToolsSayTheyRefuseACrossOriginRef(t *testing.T) {
	skill, err := os.ReadFile("../../skills/brw/SKILL.md")
	if err != nil {
		t.Fatalf("read skills/brw/SKILL.md: %v", err)
	}
	sources := map[string]string{"skills/brw/SKILL.md": string(skill)}
	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		if name != "brw_plan" && name != "brw_batch" {
			continue
		}
		description, _ := tl["description"].(string)
		sources[name] = description
	}
	if len(sources) != 3 {
		t.Fatalf("expected brw_plan and brw_batch in the catalogue; found %d sources", len(sources))
	}
	for where, text := range sources {
		if !strings.Contains(text, "cross-origin iframe") {
			t.Errorf("%s never says a ref inside a cross-origin iframe is refused by brw_plan and brw_batch, so an agent batches the click brw_snapshot told it would work", where)
		}
	}
}

func firstKey(args map[string]any) string {
	for _, candidate := range []string{"what", "ref", "refs", "target"} {
		if value, ok := args[candidate]; ok {
			if text, isText := value.(string); isText {
				return candidate + "=" + text
			}
			return candidate
		}
	}
	return "args"
}
