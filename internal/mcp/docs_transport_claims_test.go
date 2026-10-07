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

func advertisedTransports(name string) []string {
	var advertised []string
	for _, transport := range brwidentity.Transports() {
		if !slices.Contains(transportUnsupported[name], transport) {
			advertised = append(advertised, transport)
		}
	}
	return advertised
}

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

var sentenceBoundary = regexp.MustCompile(`\.(?:\*\*)?\s+`)

var docToolName = regexp.MustCompile(`brw_[a-z0-9_]+`)

type documentedSentence struct {
	line int
	text string
}

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

var transportEnumeration = regexp.MustCompile(
	"(?:`(?:" + transportAlternation + ")`\\s*(?:,|\\||or)\\s*)+`(?:" + transportAlternation + ")`")

var transportAlternation = strings.Join(brwidentity.Transports(), "|")

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
