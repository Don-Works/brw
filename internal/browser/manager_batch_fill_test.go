package browser

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestBatchFillHonoursValueAlias(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tab := openHTMLInManager(t, m, ctx, `<input aria-label="Amount" value="original">`)
	ctx = WithTabID(ctx, tab)
	snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{})
	if err != nil || len(snap.Elements) != 1 {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	ref := snap.Elements[0].Ref
	for _, tc := range []struct{ text, value, want string }{
		{"", "41", "41"},
		{"42", "ignored", "42"},
		{"", "", ""},
	} {
		result, err := m.ExecuteBatch(ctx, []BatchStep{{Action: "fill", Ref: ref, Text: tc.text, Value: tc.value}})
		if err != nil || !result.OK {
			t.Fatalf("batch=%+v err=%v", result, err)
		}
		if err := m.AssertValue(ctx, ref, tc.want, time.Second); err != nil {
			t.Fatalf("text=%q value=%q want=%q: %v", tc.text, tc.value, tc.want, err)
		}
	}
}

func TestBatchAssertValueAcceptsEmptyAndStopsOnMismatch(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tab := openHTMLInManager(t, m, ctx, `<input aria-label="Name" value="original">`)
	ctx = WithTabID(ctx, tab)
	snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{})
	if err != nil || len(snap.Elements) != 1 {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	ref := snap.Elements[0].Ref
	result, err := m.ExecuteBatch(ctx, []BatchStep{
		{Action: "fill", Ref: ref, Text: ""},
		{Action: "assert_value", Ref: ref, Value: "", TimeoutMS: 100},
		{Action: "fill", Ref: ref, Text: "not empty"},
		{Action: "assert_value", Ref: ref, Value: "", TimeoutMS: 100},
		{Action: "fill", Ref: ref, Text: "must not run"},
	})
	if err != nil || result.OK || len(result.Steps) != 4 || !result.Steps[1].OK || result.Steps[3].OK {
		t.Fatalf("batch=%+v err=%v", result, err)
	}
	if err := m.AssertValue(ctx, ref, "not empty", time.Second); err != nil {
		t.Fatal(err)
	}
	result, err = m.ExecuteBatch(ctx, []BatchStep{{Action: "assert_value", Value: "", TimeoutMS: 100}})
	if err != nil || result.OK || !strings.Contains(result.Error, "requires ref") {
		t.Fatalf("missing ref batch=%+v err=%v", result, err)
	}
}
