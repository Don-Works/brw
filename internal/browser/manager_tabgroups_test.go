package browser

import (
	"context"
	"errors"
	"testing"
)

// TestDirectCDPTabGroupingReturnsExplicitError pins the fix for the silent
// no-op bug: the direct-CDP Manager previously returned nil from GroupTabs,
// UngroupTabs, and OpenInGroup (reporting success while no Chrome tab group was
// ever created). CDP exposes no tab-grouping primitive, so these methods must
// now fail loudly with ErrTabGroupingUnsupported instead of fabricating
// success.
func TestDirectCDPTabGroupingReturnsExplicitError(t *testing.T) {
	m := &Manager{}
	ctx := context.Background()

	if err := m.GroupTabs(ctx, []string{"1", "2"}, TabGroupOptions{Name: "shopping", Color: "blue"}); !errors.Is(err, ErrTabGroupingUnsupported) {
		t.Fatalf("GroupTabs: want ErrTabGroupingUnsupported, got %v", err)
	}

	if err := m.UngroupTabs(ctx, []string{"1", "2"}); !errors.Is(err, ErrTabGroupingUnsupported) {
		t.Fatalf("UngroupTabs: want ErrTabGroupingUnsupported, got %v", err)
	}

	res, err := m.OpenInGroup(ctx, "https://example.com", TabGroupOptions{Name: "shopping"})
	if !errors.Is(err, ErrTabGroupingUnsupported) {
		t.Fatalf("OpenInGroup: want ErrTabGroupingUnsupported, got %v", err)
	}
	if res.Tab.ID != "" {
		t.Fatalf("OpenInGroup: expected zero OpenResult on unsupported error, got tab id %q", res.Tab.ID)
	}

	if _, err := m.ListTabGroups(ctx); !errors.Is(err, ErrTabGroupingUnsupported) {
		t.Fatalf("ListTabGroups: want ErrTabGroupingUnsupported, got %v", err)
	}
}
