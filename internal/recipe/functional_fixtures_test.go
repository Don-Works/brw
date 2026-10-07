package recipe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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

	const wantRecipes = 3
	if len(matches) != wantRecipes {
		t.Fatalf("scripts/test-functional.sh writes %d recipes, this guard expected %d; add the new one to the count so it is covered too", len(matches), wantRecipes)
	}

	for _, match := range matches {
		name, body := match[1], match[2]
		t.Run(name, func(t *testing.T) {

			expanded := strings.ReplaceAll(body, "$fixture_port", "17391")
			expanded = strings.ReplaceAll(expanded, `\$`, "$")

			var value Recipe
			if err := json.Unmarshal([]byte(expanded), &value); err != nil {
				t.Fatalf("%s.json is not a Recipe: %v", name, err)
			}
			if err := Validate(value); err != nil {
				t.Fatalf("%s.json fails Validate, so /api/recipes/run answers 400: %v", name, err)
			}

			if err := RequireWriteVerification(value); err != nil {
				t.Fatalf("%s.json fails RequireWriteVerification, so /api/recipes/run answers 400: %v", name, err)
			}
		})
	}
}
