package extensionbridge

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// crossOriginRef is the shape of a ref that names an element inside a
// cross-origin iframe: a frame index and a ref minted in that frame's document.
const crossOriginRef = "f0:e7"

// refVerbInvoker calls one ref-taking Controller method with a cross-origin ref
// in every position that method accepts one, and returns the error.
//
// capability, when set, is the optional interface the verb is reached through
// rather than through Controller itself. It is DATA rather than a type assertion
// inside invoke, because an invoker that answered a failed assertion with nil
// returned "no error" for a call it never made, and the test read that as the
// verb having ACCEPTED a cross-origin ref — the opposite diagnosis, pointing at a
// guard that is fine rather than at a capability that has gone. The assertion is
// made by the test, against this field, so a transport that no longer implements
// the interface is named as such unless transportsWithoutCapability says in
// writing that it never did.
type refVerbInvoker struct {
	capability reflect.Type
	invoke     func(context.Context, browser.Controller) error
}

// refVerbInvokers is the table of ref-taking verbs.
//
// The table is the point. Guarding "click and fill" is not a guard; the property
// is "this ref belongs to another document", and every verb that resolves a ref
// has to say so. TestControllerRefMethodsAreAllInvokable checks this table
// against browser.ControllerRefMethods, which is itself checked against the
// Controller interface — so a new ref-taking verb fails here until someone
// decides what it does with a cross-origin ref.
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
	// A wait names its ref inside the condition string. It is the case a guard
	// written against parameters called "ref" walks straight past, and the one
	// where the un-guarded answer is worst: the wait cannot ever come true, so it
	// burns its whole timeout and then reports the page never got there.
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
}

// capabilityInterfaces are the optional transport capabilities alongside
// Controller. They are enumerated too: brw_focus and brw_wait reach the
// transport through these, so a classification that covered only Controller
// would leave two ref-taking verbs outside the table.
var capabilityInterfaces = []reflect.Type{
	reflect.TypeOf((*browser.WaitObserver)(nil)).Elem(),
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
	// AssertValueContains was reached through an ANONYMOUS interface in the recipe
	// runner, so it belonged to no type this list could enumerate and stayed
	// unguarded on both transports while its four siblings were covered. Naming
	// the interface is what puts it back inside the property.
	reflect.TypeOf((*browser.ValueContainsAsserter)(nil)).Elem(),
}

// transportsWithoutCapability records the (transport, verb) pairs where the
// transport genuinely does not implement the optional interface that verb is
// reached through, so there is no call to guard.
//
// It is empty, and that is the assertion: both first-party transports implement
// all three optional interfaces the ref table names. A transport that drops one
// has to be written down here, which is what separates "this verb cannot be
// reached on this transport" from "this verb is reachable and ungated" — two
// states the invoker used to report identically.
var transportsWithoutCapability = map[string]map[string]bool{}

// routesInsteadOfRefusing names the (transport, method) pairs that REACH into a
// cross-origin frame rather than refusing. Direct CDP attaches a session to the
// frame's own target for a click, so Click there must not return the capability
// error — TestManagerClicksARefInsideACrossOriginFrame (internal/browser) is
// what proves that click actually lands in the frame.
var routesInsteadOfRefusing = map[string]map[string]bool{
	"direct-cdp": {"Click": true},
}

// TestControllerRefMethodsAreAllInvokable ties the invoker table to the
// classification, so neither can quietly fall behind the Controller interface.
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

// TestControllerMethodsAreClassifiedForCrossOriginRefs enumerates the Controller
// interface itself. Every method has to be classified as taking element refs or
// not, so adding a verb forces the decision rather than inheriting whichever
// behaviour the implementation happens to have.
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

// TestRefVerbsRefuseCrossOriginRefsByName is the capability guard, run against
// both first-party transports.
//
// Before it, a ref inside a cross-origin iframe reached the top document's
// resolver, which cannot see that document: the verb came back with "ref not
// found — the page likely changed; re-run brw_snapshot", advice that can never
// help because re-snapshotting mints the same ref. Worse, clickRef answered a
// resolve failure by falling through to its own shallow finder, so a failure
// could look like an action. The refusal has to be recognisable
// (errors.Is on the sentinel), not just differently worded.
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
				// Cancelled: nothing here should get far enough to need a browser, and
				// a verb that skipped the guard fails on I/O with a different error
				// instead of hanging.
				cancel()
				err := verb.invoke(ctx, controller)
				if err == nil {
					t.Fatalf("%s.%s accepted a ref inside a cross-origin iframe", transport, name)
				}
				if !errors.Is(err, snapshot.ErrCrossOriginFrameUnsupported) {
					t.Fatalf("%s.%s refused a cross-origin ref with %v, which callers cannot recognise as the capability gap", transport, name, err)
				}
				if !containsRef(err.Error()) {
					t.Fatalf("%s.%s refusal does not name the ref: %v", transport, name, err)
				}
			})
		}
	}
}

func containsRef(message string) bool {
	for i := 0; i+len(crossOriginRef) <= len(message); i++ {
		if message[i:i+len(crossOriginRef)] == crossOriginRef {
			return true
		}
	}
	return false
}
