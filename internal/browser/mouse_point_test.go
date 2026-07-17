package browser

import (
	"strings"
	"testing"
)

func TestMousePointValidate(t *testing.T) {
	if err := (MousePoint{Ref: "e1"}).Validate("drag from"); err != nil {
		t.Fatalf("ref point should be valid: %v", err)
	}
	x, y := 1.0, 2.0
	if err := (MousePoint{X: &x, Y: &y}).Validate("drag to"); err != nil {
		t.Fatalf("xy point should be valid: %v", err)
	}
	err := (MousePoint{}).Validate("drag from")
	if err == nil {
		t.Fatal("empty point should be invalid")
	}
	if !strings.Contains(err.Error(), "from:{ref:") && !strings.Contains(err.Error(), "brw_drag({from:") {
		t.Fatalf("error should show nested from/to example: %v", err)
	}
}

func TestDragOptionsValidate(t *testing.T) {
	err := (DragOptions{}).Validate()
	if err == nil {
		t.Fatal("empty drag options should be invalid")
	}
	if !strings.Contains(err.Error(), "drag from") {
		t.Fatalf("expected drag from error, got %v", err)
	}
	opts := DragOptions{From: MousePoint{Ref: "e3"}, To: MousePoint{Ref: "e4"}}
	if err := opts.Validate(); err != nil {
		t.Fatalf("valid nested drag: %v", err)
	}
}
