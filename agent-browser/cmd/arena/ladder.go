package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Task is one rung on the difficulty ladder. A task is a natural-language goal
// plus a cheap success oracle evaluated against the agent's final answer.
type Task struct {
	ID        string   `json:"id"`
	Tier      int      `json:"tier"`
	Name      string   `json:"name"`
	Prompt    string   `json:"prompt"`
	ExpectAny []string `json:"expect_any,omitempty"`
	ExpectAll []string `json:"expect_all,omitempty"`
	Skills    []string `json:"skills,omitempty"`
}

// Ladder is the ordered set of tasks loaded from ladder.json.
type Ladder struct {
	Version int    `json:"version"`
	Notes   string `json:"notes,omitempty"`
	Tasks   []Task `json:"tasks"`
}

func loadLadder(path string) (Ladder, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Ladder{}, err
	}
	var l Ladder
	if err := json.Unmarshal(data, &l); err != nil {
		return Ladder{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if l.Version != 1 {
		return Ladder{}, fmt.Errorf("unsupported ladder version %d", l.Version)
	}
	if len(l.Tasks) == 0 {
		return Ladder{}, fmt.Errorf("ladder %s has no tasks", path)
	}
	return l, nil
}

// pass reports whether the agent's final answer satisfies the task oracle.
// expect_all entries must all be present; expect_any requires at least one.
// Matching is case-insensitive substring.
func (t Task) pass(answer string) bool {
	low := strings.ToLower(answer)
	for _, want := range t.ExpectAll {
		if !strings.Contains(low, strings.ToLower(want)) {
			return false
		}
	}
	if len(t.ExpectAny) > 0 {
		for _, want := range t.ExpectAny {
			if strings.Contains(low, strings.ToLower(want)) {
				return true
			}
		}
		return false
	}
	// No expect_any given: success is defined purely by expect_all (or trivially true).
	return true
}
