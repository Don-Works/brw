package browser

import (
	"context"
	"testing"
	"time"
)

func TestManagerPressArrowPerformsNativeNumberInputDefault(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, `data:text/html,<input id="n" type="number" value="42">`)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	if _, err := m.Evaluate(tabCtx, `document.querySelector('#n').focus(); true`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Press(tabCtx, "ArrowUp"); err != nil {
		t.Fatal(err)
	}
	value, err := m.Evaluate(tabCtx, `document.querySelector('#n').value`)
	if err != nil {
		t.Fatal(err)
	}
	if value != "43" {
		t.Fatalf("number input value after ArrowUp = %#v, want 43", value)
	}
}
