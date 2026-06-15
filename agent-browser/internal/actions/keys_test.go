package actions

import (
	"testing"
)

func TestDescribeKey_Enter(t *testing.T) {
	d := DescribeKey("Enter")
	if d.Key != "Enter" || d.Code != "Enter" || d.Text != "\r" {
		t.Fatalf("unexpected Enter descriptor: %+v", d)
	}
}

func TestDescribeKey_Tab(t *testing.T) {
	d := DescribeKey("Tab")
	if d.Key != "Tab" || d.Code != "Tab" {
		t.Fatalf("unexpected Tab descriptor: %+v", d)
	}
}

func TestDescribeKey_Escape(t *testing.T) {
	d := DescribeKey("Escape")
	if d.Key != "Escape" || d.WindowsVirtualKeyCode != 27 {
		t.Fatalf("unexpected Escape descriptor: %+v", d)
	}
}

func TestDescribeKey_ArrowDown(t *testing.T) {
	d := DescribeKey("ArrowDown")
	if d.Key != "ArrowDown" || d.WindowsVirtualKeyCode != 40 {
		t.Fatalf("unexpected ArrowDown descriptor: %+v", d)
	}
}

func TestDescribeKey_Space(t *testing.T) {
	d := DescribeKey("Space")
	if d.Key != " " || d.Code != "Space" || d.Text != " " {
		t.Fatalf("unexpected Space descriptor: %+v", d)
	}
}

func TestDescribeKey_SingleChar(t *testing.T) {
	d := DescribeKey("a")
	if d.Key != "a" || d.Code != "KeyA" || d.Text != "a" {
		t.Fatalf("unexpected 'a' descriptor: %+v", d)
	}
}

func TestDescribeKey_Digit(t *testing.T) {
	d := DescribeKey("5")
	if d.Key != "5" || d.Code != "Digit5" || d.Text != "5" {
		t.Fatalf("unexpected '5' descriptor: %+v", d)
	}
}

func TestDescribeKey_Empty(t *testing.T) {
	d := DescribeKey("")
	if d.Key != "" {
		t.Fatalf("expected empty descriptor for empty input, got %+v", d)
	}
}

func TestDescribeKey_CtrlA(t *testing.T) {
	d := DescribeKey("Ctrl+a")
	if d.Key != "a" || d.Modifiers != 2 {
		t.Fatalf("unexpected Ctrl+a descriptor: %+v", d)
	}
	if d.Text != "" {
		t.Fatalf("expected empty text for modified key, got %q", d.Text)
	}
}

func TestDescribeKey_MetaShiftS(t *testing.T) {
	d := DescribeKey("Meta+Shift+s")
	if d.Key != "s" || d.Modifiers != 12 { // 4 (meta) + 8 (shift)
		t.Fatalf("unexpected Meta+Shift+s descriptor: %+v", d)
	}
}

func TestDescribeKey_AltOption(t *testing.T) {
	d := DescribeKey("Alt+Enter")
	if d.Key != "Enter" || d.Modifiers != 1 {
		t.Fatalf("unexpected Alt+Enter descriptor: %+v", d)
	}
	d2 := DescribeKey("Option+Enter")
	if d2.Key != "Enter" || d2.Modifiers != 1 {
		t.Fatalf("unexpected Option+Enter descriptor: %+v", d2)
	}
}

func TestDescribeKey_CaseInsensitive(t *testing.T) {
	d1 := DescribeKey("ENTER")
	d2 := DescribeKey("enter")
	if d1.Key != d2.Key || d1.Code != d2.Code {
		t.Fatalf("expected case-insensitive match: %+v vs %+v", d1, d2)
	}
}

func TestDescribeKey_Delete(t *testing.T) {
	d := DescribeKey("Delete")
	if d.Key != "Delete" || d.WindowsVirtualKeyCode != 46 {
		t.Fatalf("unexpected Delete descriptor: %+v", d)
	}
}

func TestDescribeKey_Backspace(t *testing.T) {
	d := DescribeKey("Backspace")
	if d.Key != "Backspace" || d.WindowsVirtualKeyCode != 8 {
		t.Fatalf("unexpected Backspace descriptor: %+v", d)
	}
}
