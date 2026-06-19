package store

import (
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestRefStoreObserveAndGet(t *testing.T) {
	st := New()
	st.Observe("tab-1", []snapshot.Element{{Ref: "e1", Role: "button", Name: "Continue", Tag: "button"}})

	got, ok := st.Get("tab-1", "e1")
	if !ok {
		t.Fatal("expected ref to be stored")
	}
	if got.Role != "button" || got.Name != "Continue" {
		t.Fatalf("stored ref mismatch: %#v", got)
	}
}

func TestRefStoreDropTab(t *testing.T) {
	st := New()
	st.Observe("tab-1", []snapshot.Element{{Ref: "e1"}})
	st.SetActive("tab-1")
	st.DropTab("tab-1")

	if _, ok := st.Get("tab-1", "e1"); ok {
		t.Fatal("expected tab refs to be dropped")
	}
	if st.Active() != "" {
		t.Fatalf("expected active tab to clear, got %q", st.Active())
	}
}
