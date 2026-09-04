package recipe

import (
	"context"
	"fmt"
	"testing"
)

// This benchmark contains synthetic metadata only. It exercises the in-process
// candidate index without placing a real recipe corpus in the source tree.
func BenchmarkCatalogSearch100K(b *testing.B) {
	const count = 100_000
	catalog := &Catalog{
		entries: make([]catalogEntry, 0, count),
		idf:     map[string]float64{"invoice": 2, "billing": 2, "common": 1},
	}
	for index := 0; index < count; index++ {
		terms := map[string]float64{"common": 1}
		doc := "common synthetic automation"
		if index == 0 {
			terms["invoice"] = 1
			terms["billing"] = 1
			doc = "download invoice from billing page"
		}
		catalog.entries = append(catalog.entries, catalogEntry{
			recipe: Recipe{
				ID: fmt.Sprintf("synthetic.catalog.recipe-%06d", index), Version: "1.0.0",
				Name: "Synthetic", Description: "Synthetic benchmark metadata",
				Origins: []string{"https://synthetic.example.test"}, Risk: "read_only",
			},
			digest: fmt.Sprintf("%064x", index+1), doc: doc, terms: terms,
		})
	}
	catalog.buildIndexes()
	b.ReportMetric(count, "recipes")
	b.Run("rare-intent", func(b *testing.B) {
		for index := 0; index < b.N; index++ {
			matches, err := catalog.Search(context.Background(), "download billing invoice", "", 10)
			if err != nil || len(matches) != 1 || matches[0].ID != "synthetic.catalog.recipe-000000" {
				b.Fatalf("matches=%+v err=%v", matches, err)
			}
		}
	})
	b.Run("rare-intent-linear-control", func(b *testing.B) {
		query := termFrequency(tokenize("download billing invoice"))
		for iteration := 0; iteration < b.N; iteration++ {
			found := 0
			for _, entry := range catalog.entries {
				if lexicalScore(query, entry.terms, catalog.idf) > 0 {
					found++
				}
			}
			if found != 1 {
				b.Fatalf("linear control found %d entries", found)
			}
		}
	})
	b.Run("common-intent-top-50", func(b *testing.B) {
		for index := 0; index < b.N; index++ {
			matches, err := catalog.Search(context.Background(), "common", "", 50)
			if err != nil || len(matches) != 50 {
				b.Fatalf("matches=%d err=%v", len(matches), err)
			}
		}
	})
}
