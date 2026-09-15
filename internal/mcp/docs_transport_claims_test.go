package mcp

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
)

// transportExcludedClaims maps a phrase an agent reads as "this tool runs on
// every transport BUT one" to the transport it rules out. It is the other half
// of transportOnlyClaims: a tool that works on two of three lanes cannot be
// described with an "only" sentence without naming a lane it is not on, and
// three tools were mis-described as "direct-CDP only" for exactly that reason.
func transportExcludedClaims() map[string]string {
	return map[string]string{
		"not on the extension bridge":           brwidentity.TransportExtensionBridge,
		"not available on the extension bridge": brwidentity.TransportExtensionBridge,
		"not on the direct-cdp transport":       brwidentity.TransportDirectCDP,
		"not on a plugin-supplied browser":      brwidentity.TransportOffHostCDP,
		"not on a browser on another machine":   brwidentity.TransportOffHostCDP,
		"not on the chrome opt-in lane":         brwidentity.TransportChromeOptIn,
		"not on `--remote`":                     brwidentity.TransportRemoteCDP,
		"not on the remote-cdp transport":       brwidentity.TransportRemoteCDP,
	}
}

// advertisedTransports is the set of transports tools/list offers a tool on,
// read off the same table the filter uses.
func advertisedTransports(name string) []string {
	var advertised []string
	for _, transport := range brwidentity.Transports() {
		if !slices.Contains(transportUnsupported[name], transport) {
			advertised = append(advertised, transport)
		}
	}
	return advertised
}

// documentedTransportClaimFiles is every prose file an agent or an operator
// reads. The catalogue check that came with the third transport looked only at
// the descriptions inside tools/list, so "brw_open_incognito … Direct-CDP only"
// stayed in skills/brw/SKILL.md, in docs/agent-guide.md and in README.md while
// all three tools were advertised on remote-cdp — in a file edited by the very
// commit that fixed the descriptions.
func documentedTransportClaimFiles(t *testing.T) []string {
	t.Helper()
	root := moduleRoot(t)
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "testdata", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("no markdown found under %s, so this test checks nothing", root)
	}
	return files
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test file")
		}
		dir = parent
	}
}

// sentenceBoundary ends a sentence at a full stop followed by whitespace,
// tolerating the bold marker markdown puts between the two.
var sentenceBoundary = regexp.MustCompile(`\.(?:\*\*)?\s+`)

// docToolName matches a brw tool as the docs spell it, in backticks or bare.
var docToolName = regexp.MustCompile(`brw_[a-z0-9_]+`)

// documentedSentences splits a markdown file into the sentences a claim can
// live in, keeping the line each one started on so a failure names a place to
// go and fix.
type documentedSentence struct {
	line int
	text string
}

// unitStart reports a line that begins a new markdown unit: a list item, a
// table row or a heading. Anything else continues the unit it is in, because a
// bullet wrapped over four lines is one sentence to whoever reads it and the
// tools it is about are usually on the first of them while the transport claim
// is on the last.
func unitStart(trimmed string) bool {
	switch {
	case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "):
		return true
	case strings.HasPrefix(trimmed, "|"), strings.HasPrefix(trimmed, "#"):
		return true
	}
	return false
}

func documentedSentences(content string) []documentedSentence {
	var out []documentedSentence
	emit := func(line int, unit []string) {
		if len(unit) == 0 {
			return
		}
		for _, sentence := range sentenceBoundary.Split(strings.Join(unit, " "), -1) {
			if strings.TrimSpace(sentence) == "" {
				continue
			}
			out = append(out, documentedSentence{line: line, text: sentence})
		}
	}
	var unit []string
	unitLine := 0
	for index, raw := range strings.Split(content, "\n") {
		line := index + 1
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || unitStart(trimmed) {
			emit(unitLine, unit)
			unit = nil
			unitLine = 0
		}
		if trimmed == "" {
			continue
		}
		if unitLine == 0 {
			unitLine = line
		}
		unit = append(unit, trimmed)
	}
	emit(unitLine, unit)
	return out
}

// A transport claim in the docs is the same promise a tool description makes,
// read by the same agent, and wrong in the same way. The catalogue is the only
// thing that decides which transports a tool is offered on, so a sentence that
// says otherwise is checked against it here rather than left to whoever last
// edited the table to remember.
//
// The convention this enforces: a transport claim names the tools it is about,
// in its own sentence. A claim with no tool in it is a claim nothing can check,
// and that is how "**Direct-CDP transport only.**" sat two lines above
// `brw_cookies` in docs/agent-guide.md while brw_cookies was advertised on
// remote-cdp.
func TestNoDocumentedTransportClaimContradictsTheToolCatalogue(t *testing.T) {
	registered := map[string]bool{}
	for _, tool := range tools() {
		name, _ := tool["name"].(string)
		registered[name] = true
	}
	only := transportOnlyClaims()
	excluded := transportExcludedClaims()

	for _, path := range documentedTransportClaimFiles(t) {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		relative := relativeToModule(t, path)
		for _, sentence := range documentedSentences(string(content)) {
			lowered := strings.ToLower(sentence.text)
			var claims []string
			keep := map[string]bool{}
			for _, transport := range brwidentity.Transports() {
				keep[transport] = true
			}
			for phrase, claimed := range only {
				if strings.Contains(lowered, phrase) {
					claims = append(claims, phrase)
					for transport := range keep {
						if transport != claimed {
							keep[transport] = false
						}
					}
				}
			}
			for phrase, ruled := range excluded {
				if strings.Contains(lowered, phrase) {
					claims = append(claims, phrase)
					keep[ruled] = false
				}
			}
			if len(claims) == 0 {
				continue
			}
			var wanted []string
			for _, transport := range brwidentity.Transports() {
				if keep[transport] {
					wanted = append(wanted, transport)
				}
			}
			var named []string
			for _, candidate := range docToolName.FindAllString(sentence.text, -1) {
				if registered[candidate] && !slices.Contains(named, candidate) {
					named = append(named, candidate)
				}
			}
			if len(named) == 0 {
				t.Errorf("%s:%d says %q and names no brw tool, so nothing checks it; put the tool names in the same sentence as the claim", relative, sentence.line, claims)
				continue
			}
			for _, name := range named {
				advertised := advertisedTransports(name)
				if !slices.Equal(advertised, wanted) {
					t.Errorf("%s:%d says %v about %s, which reads as %v; tools/list advertises it on %v", relative, sentence.line, claims, name, wanted, advertised)
				}
			}
		}
	}
}

// transportEnumeration matches a documented list of transport names, the shape
// an agent reads as "these are the lanes there are".
var transportEnumeration = regexp.MustCompile(
	"(?:`(?:" + transportAlternation + ")`\\s*(?:,|\\||or)\\s*)+`(?:" + transportAlternation + ")`")

// transportAlternation is every transport name, for the pattern above. Built
// from the closed set rather than spelled out, so a transport added to
// brwidentity is one the enumeration check starts looking for instead of one it
// silently stops noticing.
var transportAlternation = strings.Join(brwidentity.Transports(), "|")

// A docs enumeration of the transports has to be the whole closed set. A
// two-value one is not merely incomplete: it tells an agent that the value it
// just read from brw_identity cannot happen, so whatever it does about
// remote-cdp is whatever it does about an impossible answer.
func TestDocumentedTransportEnumerationsAreComplete(t *testing.T) {
	for _, path := range documentedTransportClaimFiles(t) {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		relative := relativeToModule(t, path)
		for _, match := range transportEnumeration.FindAllString(string(content), -1) {
			for _, transport := range brwidentity.Transports() {
				if !strings.Contains(match, "`"+transport+"`") {
					t.Errorf("%s enumerates the transports as %q, leaving out %q", relative, match, transport)
				}
			}
		}
	}
}

// "both transports" was written when there were two. The tool descriptions are
// already checked for it; the docs say it to the same agent.
func TestNoDocSaysThereAreTwoTransports(t *testing.T) {
	for _, path := range documentedTransportClaimFiles(t) {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		relative := relativeToModule(t, path)
		lowered := strings.ToLower(string(content))
		for _, stale := range []string{"both transports", "either transport", "the two transports"} {
			if strings.Contains(lowered, stale) {
				t.Errorf("%s says %q; brw has %d transports", relative, stale, len(brwidentity.Transports()))
			}
		}
	}
}

func relativeToModule(t *testing.T, path string) string {
	t.Helper()
	relative, err := filepath.Rel(moduleRoot(t), path)
	if err != nil {
		return path
	}
	return relative
}
