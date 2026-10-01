package browser

import (
	"context"
	"errors"
	"fmt"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestHeadlessRefClickHasOneTrustedBusinessEffect(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tab := openHTMLInManager(t, m, ctx, `<main>Owned click fixture</main>`)
	ctx = WithTabID(ctx, tab)
	for _, touch := range []bool{false, true} {
		if _, err := m.EmulateDevice(ctx, DeviceEmulationOptions{Width: 1280, Height: 1000, DeviceScaleFactor: 1, Mobile: &touch, Touch: &touch}); err != nil {
			t.Fatal(err)
		}
		for round := 0; round < 3; round++ {
			if _, err := m.Evaluate(ctx, `document.body.innerHTML='<dialog open><button>Confirm</button></dialog><output id="state">Pending</output>';window.events=[];document.querySelector('button').addEventListener('click',e=>{events.push(e.isTrusted);if(e.isTrusted){document.querySelector('#state').textContent='1 member added';document.querySelector('dialog').remove()}})`); err != nil {
				t.Fatal(err)
			}
			snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{Role: "button", Limit: 10})
			if err != nil || len(snap.Elements) != 1 {
				t.Fatalf("button snapshot=%+v err=%v", snap, err)
			}
			started := time.Now()
			var result ActionResult
			if round == 0 {
				result, err = m.Click(ctx, snap.Elements[0].Ref)
			} else if round == 1 {
				if _, err := m.Evaluate(ctx, `document.querySelector('button').removeAttribute('data-brw-ref');Object.defineProperty(crypto,'randomUUID',{value:undefined,configurable:true})`); err != nil {
					t.Fatal(err)
				}
				result, err = m.ClickText(ctx, snapshot.ClickTextOptions{Text: "Confirm"})
			} else {
				point, pointErr := m.Evaluate(ctx, `(()=>{var r=document.querySelector('button').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2}})()`)
				if pointErr != nil {
					t.Fatal(pointErr)
				}
				p := point.(map[string]any)
				_, err = m.ClickXY(ctx, p["x"].(float64), p["y"].(float64))
				result.Message = "dispatched coordinate click"
			}
			if err != nil {
				t.Fatal(err)
			}
			state, err := m.Evaluate(ctx, `events.length===1 && events[0]===true && document.querySelector('#state').textContent==='1 member added' && !document.querySelector('dialog')`)
			if err != nil || state != true {
				t.Fatalf("headless effect=%v err=%v", state, err)
			}
			t.Logf("touch=%t round=%d verified_click_ms=%.3f message=%s", touch, round, float64(time.Since(started).Microseconds())/1000, result.Message)
		}
	}
}

type trustedTapExecutor struct {
	events       []input.TouchType
	modifiers    []input.Modifier
	cancel       context.CancelFunc
	failAt       input.TouchType
	cleanupFails bool
}

func (e *trustedTapExecutor) Execute(ctx context.Context, method string, params, _ any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if method != "Input.dispatchTouchEvent" {
		return fmt.Errorf("unexpected command %s", method)
	}
	p := params.(*input.DispatchTouchEventParams)
	e.events = append(e.events, p.Type)
	e.modifiers = append(e.modifiers, p.Modifiers)
	if p.Type == input.TouchStart && e.cancel != nil {
		e.cancel()
	}
	if p.Type == e.failAt || p.Type == input.TouchCancel && e.cleanupFails {
		return errors.New("input acknowledgement failed")
	}
	return nil
}

func TestTrustedTapCancelsInterruptedInput(t *testing.T) {
	for _, name := range []string{"success", "start_failure", "end_failure", "cancel_after_start", "cleanup_failure", "already_cancelled"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			executor := &trustedTapExecutor{}
			switch name {
			case "start_failure":
				executor.failAt = input.TouchStart
			case "end_failure":
				executor.failAt = input.TouchEnd
			case "cancel_after_start":
				executor.cancel = cancel
			case "cleanup_failure":
				executor.failAt = input.TouchEnd
				executor.cleanupFails = true
			case "already_cancelled":
				cancel()
			}
			err := dispatchTrustedTap(cdp.WithExecutor(ctx, executor), 30, 40, input.ModifierShift)
			want := map[string]string{"success": "[touchStart touchEnd]", "start_failure": "[touchStart touchCancel]", "end_failure": "[touchStart touchEnd touchCancel]", "cancel_after_start": "[touchStart touchCancel]", "cleanup_failure": "[touchStart touchEnd touchCancel]", "already_cancelled": "[]"}[name]
			if fmt.Sprint(executor.events) != want || (err == nil) != (name == "success") {
				t.Fatalf("events=%v want=%s err=%v", executor.events, want, err)
			}
			for _, modifiers := range executor.modifiers {
				if modifiers != input.ModifierShift {
					t.Fatalf("lost modifiers %d", modifiers)
				}
			}
			if name == "cleanup_failure" && !strings.Contains(err.Error(), "touch cancellation unconfirmed") {
				t.Fatal(err)
			}
			if name == "cancel_after_start" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
