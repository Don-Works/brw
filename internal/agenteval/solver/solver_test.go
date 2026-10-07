package solver

import (
	"reflect"
	"testing"
)

func TestAgentExposesOnlyTheSemanticVerbs(t *testing.T) {
	allowed := map[string]bool{
		"Snapshot":  true,
		"Find":      true,
		"FindOne":   true,
		"Fill":      true,
		"Click":     true,
		"ClickText": true,
		"Select":    true,
		"WaitFor":   true,
		"Read":      true,
	}

	agent := reflect.TypeOf(&Agent{})
	seen := map[string]bool{}
	for index := range agent.NumMethod() {
		name := agent.Method(index).Name
		seen[name] = true
		if !allowed[name] {
			t.Errorf("Agent exposes %s; a solver acts through semantic verbs only, and a new one has to be shown not to be a way to run script in the page", name)
		}
	}
	for name := range allowed {
		if !seen[name] {
			t.Errorf("Agent no longer offers %s; a task written against it cannot act", name)
		}
	}

	value := reflect.TypeOf(Agent{})
	for index := range value.NumField() {
		if field := value.Field(index); field.IsExported() {
			t.Errorf("Agent.%s is exported; a task body could take the browser back out through it", field.Name)
		}
	}
}
