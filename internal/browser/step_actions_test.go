package browser

import (
	"sort"
	"testing"

	"github.com/Don-Works/brw/internal/stepscan"
)

// planAndBatchStepActions is every step verb this backend's plan and batch
// runners implement, read out of the runners themselves so a verb added to one
// of them cannot quietly escape the tables that classify steps.
func planAndBatchStepActions(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, function := range []string{"executePlanStep", "executeBatchStep"} {
		labels, err := stepscan.SwitchCases("manager.go", function, "Action")
		if err != nil {
			t.Fatal(err)
		}
		for _, label := range labels {
			seen[label] = true
		}
	}
	out := make([]string, 0, len(seen))
	for action := range seen {
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}
