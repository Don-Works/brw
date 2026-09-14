package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestShippedSuitesNameStepsTheRunnerImplements decodes every scenario the way
// the runner does and requires each step to name exactly one implemented step.
//
// loadSuite is deliberately lenient, so a mistyped step key decodes to an empty
// step and only fails after the daemon, a real browser and the fixture server
// have started — several minutes into a run, as "empty or unknown step".
func TestShippedSuitesNameStepsTheRunnerImplements(t *testing.T) {
	suites := []string{"core.json", "decathlon.json"}
	for _, name := range suites {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "tests", "scenarios", name)
			suite, err := loadSuite(path)
			if err != nil {
				t.Fatalf("load %s: %v", name, err)
			}
			if len(suite.Scenarios) == 0 {
				t.Fatalf("%s declares no scenarios", name)
			}
			raw := decodeRawScenarios(t, path)
			if len(raw) != len(suite.Scenarios) {
				t.Fatalf("%s: %d raw scenarios, %d decoded", name, len(raw), len(suite.Scenarios))
			}
			for index, scenario := range suite.Scenarios {
				if scenario.ID == "" {
					t.Fatalf("%s scenario %d has no id", name, index)
				}
				for stepIndex, action := range scenario.Actions {
					checkStep(t, scenario.ID, stepIndex, action, raw[index].Actions[stepIndex])
				}
			}
		})
	}
}

type rawScenario struct {
	Actions []json.RawMessage `json:"actions"`
}

func decodeRawScenarios(t *testing.T, path string) []rawScenario {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Scenarios []rawScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file.Scenarios
}

// checkStep proves the decoded step carries exactly one action, and that the
// raw object contained no key the step type does not know.
func checkStep(t *testing.T, scenarioID string, index int, decoded step, raw json.RawMessage) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var strict step
	if err := decoder.Decode(&strict); err != nil {
		t.Errorf("%s step %d: %v", scenarioID, index, err)
		return
	}
	value := reflect.ValueOf(decoded)
	named := 0
	for field := 0; field < value.NumField(); field++ {
		if value.Field(field).Kind() == reflect.Pointer && !value.Field(field).IsNil() {
			named++
		}
	}
	if named != 1 {
		t.Errorf("%s step %d names %d actions, want exactly 1: %s", scenarioID, index, named, raw)
	}
}
