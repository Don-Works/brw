package snapshot

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/revitt/agent-browser/internal/readability"
)

func structuredTestContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(allocCtx)
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 30*time.Second)
	return timeoutCtx, func() {
		timeoutCancel()
		cancel()
		allocCancel()
	}
}

func TestEvaluateStructuredFromFixture(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	fixture := filepath.Join(filepath.Dir(file), "..", "..", "tests", "fixtures", "structured-product.html")
	abs, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatal(err)
	}
	pageURL := (&url.URL{Scheme: "file", Path: abs}).String()

	ctx, cancel := structuredTestContext(t)
	defer cancel()

	var data StructuredData
	if err := chromedp.Run(ctx,
		chromedp.Navigate(pageURL),
		chromedp.Evaluate(StructuredDataScript, &data),
	); err != nil {
		t.Fatal(err)
	}

	if data.Source != "next_data" {
		t.Fatalf("source = %q, want next_data", data.Source)
	}
	if data.Name != "Trail Runner Pro Next" {
		t.Fatalf("name = %q", data.Name)
	}
	if data.Price != "99.95" {
		t.Fatalf("price = %q", data.Price)
	}
	if data.Currency != "USD" {
		t.Fatalf("currency = %q", data.Currency)
	}
	if data.Availability != "InStock" {
		t.Fatalf("availability = %q", data.Availability)
	}
	if data.Rating != "4.8" {
		t.Fatalf("rating = %q", data.Rating)
	}
	if data.ReviewCount != "512" {
		t.Fatalf("reviewCount = %q", data.ReviewCount)
	}
	if data.Brand != "NextBrand" {
		t.Fatalf("brand = %q", data.Brand)
	}
	if len(data.Images) != 2 {
		t.Fatalf("images = %#v", data.Images)
	}
}

func TestEvaluateStructuredJSONLDFallback(t *testing.T) {
	html := `<!DOCTYPE html><html><head>
<script type="application/ld+json">{"@type":"Product","name":"Solo Shoe","offers":{"price":"12.00","priceCurrency":"GBP"}}</script>
</head><body><h1>Solo</h1></body></html>`

	ctx, cancel := structuredTestContext(t)
	defer cancel()

	var data StructuredData
	if err := chromedp.Run(ctx,
		chromedp.Navigate("data:text/html,"+url.PathEscape(html)),
		chromedp.Evaluate(StructuredDataScript, &data),
	); err != nil {
		t.Fatal(err)
	}
	if data.Source != "json_ld" {
		t.Fatalf("source = %q, want json_ld", data.Source)
	}
	if data.Name != "Solo Shoe" || data.Price != "12.00" || data.Currency != "GBP" {
		t.Fatalf("unexpected data: %+v", data)
	}
}

func TestEvaluateStructuredProductGroupRatingBackfill(t *testing.T) {
	// Real-world pattern (e.g. Decathlon): the aggregateRating lives on the
	// ProductGroup parent while price/name live on the specific variant Product.
	// The picked Product must inherit the group's rating instead of dropping it.
	html := `<!DOCTYPE html><html><head>
<script type="application/ld+json">[
{"@type":"ProductGroup","name":"Trail Shorts","aggregateRating":{"@type":"AggregateRating","ratingValue":"4.81","reviewCount":"13194"}},
{"@type":"Product","name":"Trail Shorts - Black","offers":{"price":"12.99","priceCurrency":"GBP","availability":"https://schema.org/InStock"}}
]</script>
</head><body><h1>Trail</h1></body></html>`

	ctx, cancel := structuredTestContext(t)
	defer cancel()

	var data StructuredData
	if err := chromedp.Run(ctx,
		chromedp.Navigate("data:text/html,"+url.PathEscape(html)),
		chromedp.Evaluate(StructuredDataScript, &data),
	); err != nil {
		t.Fatal(err)
	}
	if data.Source != "json_ld" {
		t.Fatalf("source = %q, want json_ld", data.Source)
	}
	if data.Name != "Trail Shorts - Black" {
		t.Fatalf("name = %q, want the variant Product", data.Name)
	}
	if data.Price != "12.99" || data.Currency != "GBP" {
		t.Fatalf("price/currency = %q/%q", data.Price, data.Currency)
	}
	if data.Rating != "4.81" {
		t.Fatalf("rating = %q, want 4.81 backfilled from ProductGroup", data.Rating)
	}
	if data.ReviewCount != "13194" {
		t.Fatalf("reviewCount = %q, want 13194 backfilled from ProductGroup", data.ReviewCount)
	}
}

func TestStructuredOutputSmallerThanSnapshotPlusRead(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	fixture := filepath.Join(filepath.Dir(file), "..", "..", "tests", "fixtures", "structured-product.html")
	abs, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	pageURL := (&url.URL{Scheme: "file", Path: abs}).String()

	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate(pageURL)); err != nil {
		t.Fatal(err)
	}

	var data StructuredData
	if err := chromedp.Run(ctx, chromedp.Evaluate(StructuredDataScript, &data)); err != nil {
		t.Fatal(err)
	}
	snap, err := EvaluateWithOptions(ctx, SnapshotOptions{Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	read, err := readability.Evaluate(ctx)
	if err != nil {
		t.Fatal(err)
	}

	structuredBytes, _ := json.Marshal(data)
	snapshotBytes, _ := json.Marshal(snap)
	readBytes, _ := json.Marshal(read)
	combined := len(snapshotBytes) + len(readBytes)
	if len(structuredBytes) >= combined {
		t.Fatalf("structured=%d not smaller than snapshot+read=%d", len(structuredBytes), combined)
	}
	t.Logf("structured=%dB snapshot=%dB read=%dB snapshot+read=%dB reduction=%.0f%%",
		len(structuredBytes), len(snapshotBytes), len(readBytes), combined,
		100*(1-float64(len(structuredBytes))/float64(combined)))
}
