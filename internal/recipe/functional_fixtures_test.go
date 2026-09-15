package recipe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The functional suite writes its recipes as heredocs inside the shell script,
// so nothing in `go test` parses them. When RequireWriteVerification landed,
// all three went from valid to refused and the only thing that noticed was a
// CI job that runs a real browser for four minutes first, then reports
// `curl: (22) ... error: 400` with no recipe id and no reason.
//
// This reads the same recipes the script writes and puts them through the same
// validation the runner does, so that drift fails in milliseconds with the
// recipe named.
var functionalRecipeHeredoc = regexp.MustCompile(`(?s)cat >"\$recipe_root/([a-zA-Z0-9_.-]+)\.json" <<EOF\n(.*?)\nEOF\n`)

func TestFunctionalSuiteRecipesSatisfyTheRunner(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "test-functional.sh")
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	matches := functionalRecipeHeredoc.FindAllStringSubmatch(string(script), -1)
	if len(matches) == 0 {
		t.Fatal("found no recipe heredocs in scripts/test-functional.sh; this guard is matching the wrong shape and would pass on anything")
	}

	// The script names three. Pinning the count is what makes a fourth recipe
	// visible here rather than silently unchecked.
	const wantRecipes = 3
	if len(matches) != wantRecipes {
		t.Fatalf("scripts/test-functional.sh writes %d recipes, this guard expected %d; add the new one to the count so it is covered too", len(matches), wantRecipes)
	}

	for _, match := range matches {
		name, body := match[1], match[2]
		t.Run(name, func(t *testing.T) {
			// The heredoc is unquoted, so the shell expands $fixture_port and the
			// author escapes every \${input:...} to keep it literal. Undo both in
			// the same order the shell would.
			expanded := strings.ReplaceAll(body, "$fixture_port", "17391")
			expanded = strings.ReplaceAll(expanded, `\$`, "$")

			var value Recipe
			if err := json.Unmarshal([]byte(expanded), &value); err != nil {
				t.Fatalf("%s.json is not a Recipe: %v", name, err)
			}
			if err := Validate(value); err != nil {
				t.Fatalf("%s.json fails Validate, so /api/recipes/run answers 400: %v", name, err)
			}
			// Runner.Run checks this separately from Validate, so a recipe can be
			// parseable and still be refused at execution — which is exactly how
			// this got past everything except the functional job.
			if err := RequireWriteVerification(value); err != nil {
				t.Fatalf("%s.json fails RequireWriteVerification, so /api/recipes/run answers 400: %v", name, err)
			}
		})
	}
}
