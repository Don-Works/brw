package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestSnapshotBudgetPreservesLargestFittingPrefix(t *testing.T) {
	snap := sampleSnapshot()
	snap.Elements[0].Name = "Quoted \"snow 雪\""
	full, _ := json.Marshal(snap)
	for budget := 1; budget <= len(full)+1; budget++ {
		got := trimSnapshotToMaxBytes(snap, budget)
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if got.URL != snap.URL || got.Title != snap.Title || len(got.Elements) > len(snap.Elements) {
			t.Fatal("budget changed snapshot identity")
		}
		if len(data) > budget && len(got.Elements) != 0 {
			t.Fatalf("budget=%d response=%d", budget, len(data))
		}
		if len(got.Elements) < len(snap.Elements) {
			next := snap
			next.Elements = next.Elements[:len(got.Elements)+1]
			data, _ := json.Marshal(next)
			if len(data) <= budget {
				t.Fatalf("budget=%d discarded an element that fits", budget)
			}
		}
	}
}

func BenchmarkSnapshotBudget(b *testing.B) {
	snap := sampleSnapshot()
	snap.Elements = make([]snapshot.Element, 500)
	for i := range snap.Elements {
		snap.Elements[i] = snapshot.Element{Ref: "e1", Role: "button", Name: strings.Repeat("fixture", 16)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		trimSnapshotToMaxBytes(snap, 2048)
	}
}
