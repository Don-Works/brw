package extensionbridge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

const crossOriginRef = "f0:e7"

type refVerbInvoker struct {
	capability reflect.Type
	invoke     func(context.Context, browser.Controller) error
}

var refVerbInvokers = map[string]refVerbInvoker{
	"Click": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.Click(ctx, crossOriginRef)
		return err
	}},
	"ClickButton": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.ClickButton(ctx, browser.ClickButtonOptions{MousePoint: browser.MousePoint{Ref: crossOriginRef}})
		return err
	}},
	"MouseDown": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.MouseDown(ctx, browser.MouseButtonOptions{MousePoint: browser.MousePoint{Ref: crossOriginRef}})
		return err
	}},
	"MouseUp": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.MouseUp(ctx, browser.MouseButtonOptions{MousePoint: browser.MousePoint{Ref: crossOriginRef}})
		return err
	}},
	"Drag": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.Drag(ctx, browser.DragOptions{
			From: browser.MousePoint{Ref: crossOriginRef},
			To:   browser.MousePoint{Ref: "e2"},
		})
		return err
	}},
	"Hover": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.Hover(ctx, crossOriginRef)
		return err
	}},
	"Type": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.Type(ctx, crossOriginRef, "hello")
		return err
	}},
	"Fill": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.Fill(ctx, snapshot.FillOptions{Ref: crossOriginRef, Text: "hello"})
		return err
	}},
	"UploadFile": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.UploadFile(ctx, snapshot.UploadOptions{ClickRef: crossOriginRef, Path: "/dev/null"})
		return err
	}},
	"Select": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.Select(ctx, crossOriginRef, "one")
		return err
	}},
	"ScreenshotAnnotated": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.ScreenshotAnnotated(ctx, browser.AnnotatedScreenshotOptions{Ref: crossOriginRef})
		return err
	}},
	"ScreenshotElement": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.ScreenshotElement(ctx, crossOriginRef)
		return err
	}},
	"AssertVisible": {invoke: func(ctx context.Context, c browser.Controller) error {
		return c.AssertVisible(ctx, crossOriginRef, time.Second)
	}},
	"AssertText": {invoke: func(ctx context.Context, c browser.Controller) error {
		return c.AssertText(ctx, crossOriginRef, "x", time.Second)
	}},
	"AssertValue": {invoke: func(ctx context.Context, c browser.Controller) error {
		return c.AssertValue(ctx, crossOriginRef, "x", time.Second)
	}},
	"AssertValueContains": {
		capability: reflect.TypeOf((*browser.ValueContainsAsserter)(nil)).Elem(),
		invoke: func(ctx context.Context, c browser.Controller) error {
			return c.(browser.ValueContainsAsserter).AssertValueContains(ctx, crossOriginRef, "x", time.Second)
		},
	},
	"AssertHidden": {invoke: func(ctx context.Context, c browser.Controller) error {
		return c.AssertHidden(ctx, crossOriginRef, time.Second)
	}},
	"CommitField": {invoke: func(ctx context.Context, c browser.Controller) error {
		return c.CommitField(ctx, crossOriginRef)
	}},
	"ExecutePlan": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.ExecutePlan(ctx, []browser.PlanStep{{Action: "click", Ref: crossOriginRef}})
		return err
	}},
	"ExecuteBatch": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := c.ExecuteBatch(ctx, []browser.BatchStep{{Action: "assert", AssertRef: crossOriginRef}})
		return err
	}},

	"WaitFor": {invoke: func(ctx context.Context, c browser.Controller) error {
		return c.WaitFor(ctx, "ref:"+crossOriginRef, time.Second)
	}},
	"WaitForOutcome": {
		capability: reflect.TypeOf((*browser.WaitObserver)(nil)).Elem(),
		invoke: func(ctx context.Context, c browser.Controller) error {
			_, err := c.(browser.WaitObserver).WaitForOutcome(ctx, "not_ref:"+crossOriginRef, time.Second)
			return err
		},
	},
	"Focus": {
		capability: reflect.TypeOf((*browser.ElementFocuser)(nil)).Elem(),
		invoke: func(ctx context.Context, c browser.Controller) error {
			_, err := c.(browser.ElementFocuser).Focus(ctx, crossOriginRef)
			return err
		},
	},
	"Assert": {invoke: func(ctx context.Context, c browser.Controller) error {
		_, err := browser.Assert(ctx, c, browser.AssertRequest{Assertion: "element_state", Ref: crossOriginRef, State: "visible"})
		return err
	}},

	"Touch": {
		capability: reflect.TypeOf((*browser.TouchController)(nil)).Elem(),
		invoke: func(ctx context.Context, c browser.Controller) error {
			_, err := c.(browser.TouchController).Touch(ctx, browser.TouchOptions{Action: "tap", Ref: crossOriginRef})
			return err
		},
	},
	"ScrollTo": {
		capability: reflect.TypeOf((*browser.ScrollToController)(nil)).Elem(),
		invoke: func(ctx context.Context, c browser.Controller) error {
			_, err := c.(browser.ScrollToController).ScrollTo(ctx, crossOriginRef)
			return err
		},
	},
	"React": {
		capability: reflect.TypeOf((*browser.ReactController)(nil)).Elem(),
		invoke: func(ctx context.Context, c browser.Controller) error {
			_, err := c.(browser.ReactController).React(ctx, browser.ReactOptions{Action: "inspect", Target: crossOriginRef})
			return err
		},
	},
	"Check": {
		capability: reflect.TypeOf((*browser.CheckController)(nil)).Elem(),
		invoke: func(ctx context.Context, c browser.Controller) error {
			want := true
			_, err := c.(browser.CheckController).Check(ctx, browser.CheckOptions{Ref: crossOriginRef, Checked: &want})
			return err
		},
	},
}

var capabilityInterfaces = []reflect.Type{
	reflect.TypeOf((*browser.WaitObserver)(nil)).Elem(),
	reflect.TypeOf((*browser.EnvironmentController)(nil)).Elem(),
	reflect.TypeOf((*browser.DialogController)(nil)).Elem(),
	reflect.TypeOf((*browser.RouteController)(nil)).Elem(),
	reflect.TypeOf((*browser.RouteReplayer)(nil)).Elem(),
	reflect.TypeOf((*browser.ClipboardController)(nil)).Elem(),
	reflect.TypeOf((*browser.KeyHoldController)(nil)).Elem(),
	reflect.TypeOf((*browser.HistoryController)(nil)).Elem(),
	reflect.TypeOf((*browser.SessionStateController)(nil)).Elem(),
	reflect.TypeOf((*browser.ElementFocuser)(nil)).Elem(),
	reflect.TypeOf((*browser.WindowReader)(nil)).Elem(),
	reflect.TypeOf((*browser.ActiveTabReporter)(nil)).Elem(),
	reflect.TypeOf((*browser.DocumentIdentityProvider)(nil)).Elem(),
	reflect.TypeOf((*browser.Asserter)(nil)).Elem(),

	reflect.TypeOf((*browser.ValueContainsAsserter)(nil)).Elem(),

	reflect.TypeOf((*browser.TouchController)(nil)).Elem(),
	reflect.TypeOf((*browser.InitScriptController)(nil)).Elem(),
	reflect.TypeOf((*browser.ProfilerController)(nil)).Elem(),
	reflect.TypeOf((*browser.ReactController)(nil)).Elem(),
	reflect.TypeOf((*browser.ScrollToController)(nil)).Elem(),
	reflect.TypeOf((*browser.CheckController)(nil)).Elem(),
}

var transportsWithoutCapability = map[string]map[string]bool{

	"extension-bridge": {"Touch": true, "InitScript": true, "Profile": true},
}

var routesInsteadOfRefusing = map[string]map[string]bool{
	"direct-cdp": {"Click": true},
}

func TestControllerRefMethodsAreAllInvokable(t *testing.T) {
	for name := range browser.ControllerRefMethods {
		if _, ok := refVerbInvokers[name]; !ok {
			t.Errorf("%s takes an element ref but no invoker exercises its cross-origin refusal", name)
		}
	}
	enumerated := map[reflect.Type]bool{}
	for _, iface := range capabilityInterfaces {
		enumerated[iface] = true
	}
	for name, verb := range refVerbInvokers {
		if _, ok := browser.ControllerRefMethods[name]; !ok {
			t.Errorf("invoker for %s names a method that is no longer classified as ref-taking", name)
		}
		if verb.capability == nil {
			continue
		}
		if !enumerated[verb.capability] {
			t.Errorf("%s is reached through %s, which capabilityInterfaces does not list, so TestControllerMethodsAreClassifiedForCrossOriginRefs never walks it", name, verb.capability)
		}
	}
}

func TestControllerMethodsAreClassifiedForCrossOriginRefs(t *testing.T) {
	seen := map[string]bool{}
	ifaces := append([]reflect.Type{reflect.TypeOf((*browser.Controller)(nil)).Elem()}, capabilityInterfaces...)
	for _, iface := range ifaces {
		for i := 0; i < iface.NumMethod(); i++ {
			name := iface.Method(i).Name
			seen[name] = true
			_, takesRef := browser.ControllerRefMethods[name]
			refFree := browser.ControllerRefFreeMethods[name]
			switch {
			case takesRef && refFree:
				t.Errorf("%s.%s is classified both as ref-taking and as ref-free", iface.Name(), name)
			case !takesRef && !refFree:
				t.Errorf("%s.%s is unclassified: say whether it can be handed an element ref, so a ref inside a cross-origin iframe cannot reach it unchecked", iface.Name(), name)
			}
		}
	}
	for name := range browser.ControllerRefMethods {
		if !seen[name] {
			t.Errorf("ControllerRefMethods names %s, which is not a method of Controller or any capability interface", name)
		}
	}
	for name := range browser.ControllerRefFreeMethods {
		if !seen[name] {
			t.Errorf("ControllerRefFreeMethods names %s, which is not a method of Controller or any capability interface", name)
		}
	}
}

func TestRefVerbsRefuseCrossOriginRefsByName(t *testing.T) {
	transports := map[string]browser.Controller{
		"extension-bridge": New("", time.Second, ""),
		"direct-cdp":       &browser.Manager{},
	}
	for transport, controller := range transports {
		for name, verb := range refVerbInvokers {
			if routesInsteadOfRefusing[transport][name] {
				continue
			}
			t.Run(transport+"/"+name, func(t *testing.T) {
				if verb.capability != nil && !reflect.TypeOf(controller).Implements(verb.capability) {
					if transportsWithoutCapability[transport][name] {
						t.Skipf("%s does not implement %s, so %s cannot be reached on it at all", transport, verb.capability, name)
					}
					t.Fatalf("%s no longer implements %s, so %s reaches it by some other path and this subtest would otherwise have asserted nothing; if the capability was dropped on purpose, record it in transportsWithoutCapability", transport, verb.capability, name)
				}
				ctx, cancel := context.WithCancel(context.Background())

				cancel()
				err := verb.invoke(ctx, controller)
				if err == nil {
					t.Fatalf("%s.%s accepted a ref inside a cross-origin iframe", transport, name)
				}
				if !errors.Is(err, snapshot.ErrCrossOriginFrameUnsupported) {
					t.Fatalf("%s.%s refused a cross-origin ref with %v, which callers cannot recognise as the capability gap", transport, name, err)
				}
				if !strings.Contains(err.Error(), crossOriginRef) {
					t.Fatalf("%s.%s refusal does not name the ref: %v", transport, name, err)
				}
			})
		}
	}
}
