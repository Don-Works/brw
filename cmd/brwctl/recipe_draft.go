package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Don-Works/brw/internal/recipe"
)

func recipeDraft(args []string) error {
	fs := flag.NewFlagSet("recipe draft", flag.ContinueOnError)
	var fromTrace, out, id, version, name, description, origins, plan string
	var publish bool
	fs.StringVar(&fromTrace, "from-trace", "", "brw_trace JSON path, or - for stdin")
	fs.StringVar(&out, "out", "", "draft output path; a split flow writes <out> and <out> with -send before the extension")
	fs.StringVar(&id, "id", "", "dotted recipe id, e.g. google.chat.search-conversations")
	fs.StringVar(&version, "version", "1.0.0", "semantic version")
	fs.StringVar(&name, "name", "", "human-readable name")
	fs.StringVar(&description, "description", "", "what this recipe does")
	fs.StringVar(&origins, "origins", "", "comma-separated exact origins; inferred from the trace when omitted")
	fs.StringVar(&plan, "plan", "", "compile plan JSON: identity, origins, risk and the steps that commit external writes; switches --from-trace to the compiler, which needs a scoped trace carrying the observation around each action")
	fs.BoolVar(&publish, "publish", false, "publish the compiled draft through the private provider's write API (compile mode only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fromTrace == "" || fs.NArg() != 0 {
		return errors.New("recipe draft requires --from-trace")
	}
	if plan != "" {

		return recipeCompile(fromTrace, plan, out, publish)
	}
	if publish {
		return errors.New("--publish needs --plan: a skeleton draft still carries TODO markers and is not publishable")
	}
	if out == "" {
		return errors.New("recipe draft requires --out")
	}

	data, err := readRecipeSource(fromTrace, false)
	if err != nil {
		return err
	}
	actions, err := decodeTrace(data)
	if err != nil {
		return err
	}

	drafts, err := recipe.DraftFromTrace(actions, recipe.DraftOptions{
		ID: id, Version: version, Name: name, Description: description,
		Origins: splitCommaList(origins),
	})
	if err != nil {
		return err
	}

	paths := draftPaths(out, len(drafts))
	for i, draft := range drafts {
		body, err := json.MarshalIndent(draft, "", "  ")
		if err != nil {
			return err
		}
		body = append(body, '\n')
		if err := os.WriteFile(paths[i], body, 0o600); err != nil {
			return err
		}
	}

	todos := 0
	for _, p := range paths {
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		todos += strings.Count(string(body), recipe.TodoMarker)
	}
	fmt.Printf("wrote %d draft(s): %s\n", len(paths), strings.Join(paths, ", "))
	fmt.Printf("%d TODO marker(s) remain — resolve every one, then run `brwctl recipe validate --file <path>`.\n", todos)
	if len(paths) > 1 {
		fmt.Println("This trace contained a send-shaped action, so it was split: review and install the prepare draft and the send draft separately.")
	}
	return nil
}

func decodeTrace(data []byte) ([]recipe.TraceAction, error) {
	var direct []recipe.TraceAction
	if err := json.Unmarshal(data, &direct); err == nil && len(direct) > 0 {
		return direct, nil
	}
	var wrapped struct {
		Entries []recipe.TraceAction `json:"entries"`
		Trace   []recipe.TraceAction `json:"trace"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, fmt.Errorf("parse trace JSON: %w", err)
	}
	if len(wrapped.Entries) > 0 {
		return wrapped.Entries, nil
	}
	if len(wrapped.Trace) > 0 {
		return wrapped.Trace, nil
	}
	return nil, errors.New("trace JSON contained no entries")
}

func draftPaths(out string, count int) []string {
	if count < 2 {
		return []string{out}
	}
	ext := filepath.Ext(out)
	stem := strings.TrimSuffix(out, ext)
	return []string{stem + "-prepare" + ext, stem + "-send" + ext}
}

func splitCommaList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
