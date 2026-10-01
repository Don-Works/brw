package browser

import (
	"fmt"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestSelectFrontierElementsPrioritizesActiveTask(t *testing.T) {
	elements := []snapshot.Element{
		{Ref: "background", Signals: []string{"focused", "has-popup"}},
		{Ref: "add", Signals: []string{"task-scope"}},
	}
	result := SelectFrontierElements(elements, "background", 1)
	if len(result) != 1 || result[0].Ref != "add" {
		t.Fatalf("dialog action missing: %+v", result)
	}
}

func TestSelectFrontierKeepsFocusedDialogControlWithinLimit(t *testing.T) {
	elements := []snapshot.Element{{Ref: "background", Signals: []string{"focused"}}}
	for i := 0; i < 20; i++ {
		elements = append(elements, snapshot.Element{Ref: fmt.Sprint(i), Signals: []string{"task-scope"}})
	}
	result := SelectFrontierElements(elements, "19", 12)
	if len(result) != 12 || result[0].Ref != "19" {
		t.Fatalf("focused dialog control lost: %+v", result)
	}
}

func TestSelectFrontierElements_FocusedFirst(t *testing.T) {
	elements := []snapshot.Element{
		{Ref: "e1", Role: "button", Name: "A", Visible: true, InViewport: true},
		{Ref: "e2", Role: "textbox", Name: "B", Visible: true, InViewport: true, Signals: []string{"focused"}},
		{Ref: "e3", Role: "link", Name: "C", Visible: true, InViewport: true},
	}
	result := SelectFrontierElements(elements, "", 10)
	if len(result) == 0 || result[0].Ref != "e2" {
		t.Fatalf("expected focused element first, got %v", result)
	}
}

func TestSelectFrontierElements_FocusRef(t *testing.T) {
	elements := []snapshot.Element{
		{Ref: "e1", Role: "button", Name: "A", Visible: true, InViewport: true},
		{Ref: "e2", Role: "textbox", Name: "B", Visible: true, InViewport: true},
	}
	result := SelectFrontierElements(elements, "e2", 10)
	if len(result) == 0 || result[0].Ref != "e2" {
		t.Fatalf("expected focus ref first, got %v", result)
	}
}

func TestSelectFrontierElements_SignalPriority(t *testing.T) {
	elements := []snapshot.Element{
		{Ref: "e1", Role: "button", Name: "A", Visible: true, InViewport: true},
		{Ref: "e2", Role: "alert", Name: "B", Visible: true, InViewport: true, Signals: []string{"invalid"}},
	}
	result := SelectFrontierElements(elements, "", 10)
	if len(result) == 0 || result[0].Ref != "e2" {
		t.Fatalf("expected invalid signal element first, got %v", result)
	}
}

func TestSelectFrontierElements_ViewportFallback(t *testing.T) {
	elements := []snapshot.Element{
		{Ref: "e1", Role: "button", Name: "A", Visible: true, InViewport: true},
		{Ref: "e2", Role: "link", Name: "B", Visible: true, InViewport: false},
	}
	result := SelectFrontierElements(elements, "", 10)
	if len(result) != 1 || result[0].Ref != "e1" {
		t.Fatalf("expected viewport fallback to e1, got %v", result)
	}
}

func TestSelectFrontierElements_Limit(t *testing.T) {
	elements := []snapshot.Element{
		{Ref: "e1", Role: "button", Name: "A", Visible: true, InViewport: true, Signals: []string{"focused"}},
		{Ref: "e2", Role: "button", Name: "B", Visible: true, InViewport: true, Signals: []string{"focused"}},
		{Ref: "e3", Role: "button", Name: "C", Visible: true, InViewport: true, Signals: []string{"focused"}},
	}
	result := SelectFrontierElements(elements, "", 2)
	if len(result) != 2 {
		t.Fatalf("expected limit 2, got %d", len(result))
	}
}

func TestSelectFrontierElements_EmptyInput(t *testing.T) {
	result := SelectFrontierElements(nil, "", 10)
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestSelectFrontierElements_ZeroLimit(t *testing.T) {
	elements := []snapshot.Element{{Ref: "e1", Role: "button"}}
	result := SelectFrontierElements(elements, "", 0)
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestSelectFrontierElements_DeduplicateRefs(t *testing.T) {
	elements := []snapshot.Element{
		{Ref: "e1", Role: "button", Name: "A", Visible: true, InViewport: true, Signals: []string{"focused", "invalid"}},
	}
	result := SelectFrontierElements(elements, "", 10)
	count := 0
	for _, el := range result {
		if el.Ref == "e1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected ref e1 once, got %d times", count)
	}
}
