package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

const crossOriginRef = "f0:e7"

var refParameterNames = map[string]bool{
	"ref":       true,
	"refs":      true,
	"target":    true,
	"click_ref": true,
}

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

const staleFrameRefShape = "f<i>:e<j>"

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

var sequenceTools = []string{"brw_plan", "brw_batch"}

func routingToolName(t *testing.T) string {
	t.Helper()
	var names []string
	for name, entry := range refTakingTools {
		if entry.Disposition == refToolRoutesIntoTheFrame {
			names = append(names, name)
		}
	}
	if len(names) != 1 {
		t.Fatalf("expected exactly one tool classified as routing into a cross-origin frame, found %v", names)
	}
	return names[0]
}

func TestSequenceToolsRefuseACrossOriginRefAndSayTheyDo(t *testing.T) {

	server := New(&browser.Manager{})
	step := map[string]any{"action": "click", "ref": crossOriginRef}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tool := range sequenceTools {
		payload, err := json.Marshal(map[string]any{"steps": []map[string]any{step}})
		if err != nil {
			t.Fatalf("marshal %s args: %v", tool, err)
		}
		text := callSequenceTool(t, server, ctx, tool, payload)
		if !strings.Contains(text, snapshot.ErrCrossOriginFrameUnsupported.Error()) {
			t.Fatalf("%s answered %q, which is not the cross-origin capability refusal the descriptions promise", tool, text)
		}
		if !strings.Contains(text, crossOriginRef) {
			t.Fatalf("%s refused without naming the ref it refused: %s", tool, text)
		}
	}

	routing := routingToolName(t)
	skill, err := os.ReadFile("../../skills/brw/SKILL.md")
	if err != nil {
		t.Fatalf("read skills/brw/SKILL.md: %v", err)
	}
	sources := map[string]string{"skills/brw/SKILL.md": string(skill)}
	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		for _, want := range sequenceTools {
			if name == want {
				description, _ := tl["description"].(string)
				sources[name] = description
			}
		}
	}
	if len(sources) != len(sequenceTools)+1 {
		t.Fatalf("expected %v in the catalogue; found %d sources", sequenceTools, len(sources))
	}
	for where, text := range sources {
		if !saysSuchARefIsRefused(text, routing) {
			t.Errorf("%s never says, where it mentions a cross-origin iframe, that %v refuse such a ref and that %s is what reaches the frame — so an agent batches the click brw_snapshot told it would work", where, sequenceTools, routing)
		}
	}
}

func callSequenceTool(t *testing.T, server *Server, ctx context.Context, tool string, payload []byte) (text string) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			text = fmt.Sprintf("ran past the guard and crashed on the browser it does not have: %v", recovered)
		}
	}()
	result, rpcErr := server.callTool(ctx, tool, payload)
	return rpcErrText(rpcErr) + resultText(result)
}

const refusalPassage = 260

func saysSuchARefIsRefused(text, routing string) bool {
	const phrase = "cross-origin iframe"
	for at := 0; ; {
		i := strings.Index(text[at:], phrase)
		if i < 0 {
			return false
		}
		start := at + i
		end := start + refusalPassage
		if end > len(text) {
			end = len(text)
		}
		passage := text[start:end]
		if strings.Contains(passage, "refuse") && strings.Contains(passage, routing) {
			return true
		}
		at = start + len(phrase)
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

type refToolDisposition int

const (
	refToolGuardedAtTransport refToolDisposition = iota

	refToolGuardedHere

	refToolRoutesIntoTheFrame

	refToolNamesTheFrame
)

var refTakingTools = map[string]struct {
	Disposition refToolDisposition
	Reason      string
}{
	"brw_click":              {refToolRoutesIntoTheFrame, "direct CDP attaches a session to the frame's own target and clicks there; the extension bridge refuses by name"},
	"brw_hover":              {refToolGuardedAtTransport, "Hover resolves against the top document"},
	"brw_type":               {refToolGuardedAtTransport, "Type resolves against the top document"},
	"brw_fill":               {refToolGuardedAtTransport, "Fill resolves against the top document"},
	"brw_select":             {refToolGuardedAtTransport, "Select resolves against the top document"},
	"brw_focus":              {refToolGuardedAtTransport, "Focus resolves against the top document"},
	"brw_commit":             {refToolGuardedAtTransport, "CommitField resolves against the top document"},
	"brw_mouse_down":         {refToolGuardedAtTransport, "MouseDown resolves against the top document"},
	"brw_mouse_up":           {refToolGuardedAtTransport, "MouseUp resolves against the top document"},
	"brw_upload_file":        {refToolGuardedAtTransport, "UploadFile resolves both its ref and its click_ref against the top document"},
	"brw_screenshot_save":    {refToolGuardedAtTransport, "SaveScreenshot guards cross-origin refs before any capture or disk write"},
	"brw_screenshot":         {refToolGuardedAtTransport, "a ref makes this an annotated crop, which goes through ScreenshotAnnotated"},
	"brw_screenshot_element": {refToolGuardedAtTransport, "ScreenshotElement resolves against the top document"},
	"brw_assert":             {refToolGuardedAtTransport, "Assert resolves against the top document"},
	"brw_assert_visible":     {refToolGuardedAtTransport, "AssertVisible resolves against the top document"},
	"brw_assert_hidden":      {refToolGuardedAtTransport, "AssertHidden resolves against the top document"},
	"brw_assert_text":        {refToolGuardedAtTransport, "AssertText resolves against the top document"},
	"brw_assert_value":       {refToolGuardedAtTransport, "AssertValue resolves against the top document"},
	"brw_frame":              {refToolNamesTheFrame, "the target IS a frame ref: the element half is dropped and the frame's box is reported"},
	"brw_get":                {refToolGuardedHere, "the target reaches the transport as JavaScript through Evaluate, which is ref-free"},
	"brw_highlight":          {refToolGuardedHere, "the overlay is drawn in the top document by the devtools observer, not through a Controller ref method"},
	"brw_artifact_capture":   {refToolGuardedHere, "the ref reaches the artifact service, which captures from the top document"},

	"brw_touch":  {refToolGuardedAtTransport, "Touch resolves both its ref and its to_ref against the top document"},
	"brw_scroll": {refToolGuardedAtTransport, "Scrolling to a target resolves the ref or selector against the top document"},
	"brw_react":  {refToolGuardedAtTransport, "React inspection resolves its target against the top document"},
	"brw_check":  {refToolGuardedAtTransport, "Check resolves its ref against the top document and refuses a cross-origin one by name"},
}
