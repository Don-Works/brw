package extensionbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func TestBridgeRefClickDoesNotRetryAmbiguousInput(t *testing.T) {
	for _, failure := range []string{"resolve", "target", "input", "detached", "none"} {
		t.Run(failure, func(t *testing.T) {
			b := New("", time.Second, "")
			stub := serveRPCStub(t, b, func(kind string, call int) (map[string]any, bool, string) {
				if kind == "get_tab_input_state" {
					return map[string]any{"active": true, "windowFocused": false}, true, ""
				}
				if kind != "cdp" {
					return map[string]any{}, true, ""
				}
				if (failure == "input" || failure == "detached") && call == 4 {
					if failure == "detached" {
						return nil, false, "detached while handling command"
					}
					return nil, false, "input acknowledgement failed"
				}
				value := map[string]any{"ok": true, "ref": "e1", "x": 30, "y": 40, "viewport_x": 30, "viewport_y": 40, "width": 20, "height": 20}
				if failure == "resolve" && call == 0 {
					value = map[string]any{"ok": false, "reason": "no_key"}
				}
				if failure == "target" && call == 1 {
					value = map[string]any{"ok": false, "error": "click target not hit-testable"}
				}
				return map[string]any{"result": map[string]any{"value": value}}, true, ""
			})
			defer stub.stop()
			err := b.clickRef(browser.WithTabID(context.Background(), "42"), "e1")
			want := map[string]int{"resolve": 1, "target": 2, "input": 5, "detached": 5, "none": 6}[failure]
			if stub.count("cdp") != want || (err == nil) != (failure == "none") {
				t.Fatalf("failure=%s calls=%d want=%d err=%v", failure, stub.count("cdp"), want, err)
			}
			if failure == "target" && !strings.Contains(err.Error(), "not hit-testable") {
				t.Fatal(err)
			}
		})
	}
}

func TestBridgeRefClickInactiveTargetSendsNoInput(t *testing.T) {
	for _, state := range []map[string]any{{"active": false, "windowFocused": true}, {"active": false, "windowFocused": false}} {
		b := New("", time.Second, "")
		stub := serveRPCStub(t, b, func(kind string, call int) (map[string]any, bool, string) { return state, true, "" })
		err := b.clickRef(browser.WithTabID(context.Background(), "42"), "e1")
		if err == nil || !strings.Contains(err.Error(), "brw_focus_tab") || stub.count("cdp") != 0 {
			t.Fatalf("state=%v err=%v input=%d", state, err, stub.count("cdp"))
		}
		stub.stop()
	}
}

func TestBridgeTrustedClickRequiresPinnedTab(t *testing.T) {
	b := New("", time.Second, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := b.clickRef(ctx, "e1")
	if err == nil || !strings.Contains(err.Error(), "explicit tab_id") {
		t.Fatalf("unpinned target err=%v", err)
	}
}

func TestBridgeTouchInputDoesNotReplayAmbiguousAcknowledgements(t *testing.T) {
	for _, typ := range []string{"touchStart", "touchEnd", "touchMove", "touchCancel"} {
		t.Run(typ, func(t *testing.T) {
			b := New("", time.Second, "")
			stub := serveRPCStub(t, b, func(kind string, call int) (map[string]any, bool, string) {
				return nil, false, "detached while handling command"
			})
			defer stub.stop()
			_, err := b.cdp(browser.WithTabID(context.Background(), "42"), "", "Input.dispatchTouchEvent", map[string]any{"type": typ, "touchPoints": []map[string]any{}})
			if err == nil || stub.count("cdp") != 1 {
				t.Fatalf("type=%s calls=%d err=%v", typ, stub.count("cdp"), err)
			}
		})
	}
}

func TestBridgeTouchClickCancelsInterruptedInput(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(fmt.Sprint(cleanupFails), func(t *testing.T) {
			b := New("", time.Second, "")
			b.emulationStates["42"] = bridgeDeviceEmulationState{Config: browser.DeviceEmulationConfig{Touch: true}}
			ctx, cancel := context.WithCancel(browser.WithTabID(context.Background(), "42"))
			defer cancel()
			stub := serveRPCStub(t, b, func(kind string, call int) (map[string]any, bool, string) {
				if kind == "get_tab_input_state" {
					return map[string]any{"active": true}, true, ""
				}
				if kind == "cdp" && call == 3 {
					cancel()
				}
				if kind == "cdp" && call == 4 && cleanupFails {
					return nil, false, "detached while cancelling"
				}
				value := map[string]any{"ok": true, "ref": "e1", "x": 30, "y": 40, "viewport_x": 30, "viewport_y": 40, "width": 20, "height": 20}
				return map[string]any{"result": map[string]any{"value": value}}, true, ""
			})
			defer stub.stop()
			err := b.clickRef(ctx, "e1")
			if !errors.Is(err, context.Canceled) || stub.count("cdp") != 5 {
				t.Fatalf("calls=%d err=%v", stub.count("cdp"), err)
			}
			if cleanupFails && !strings.Contains(err.Error(), "touch cancellation unconfirmed") {
				t.Fatal(err)
			}
		})
	}
}
