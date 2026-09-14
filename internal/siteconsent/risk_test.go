package siteconsent

import (
	"reflect"
	"testing"
)

func TestClassify(t *testing.T) {
	set, err := ShippedCategories()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		request ActionRequest
		want    []RiskClass
	}{
		{
			name:    "plain navigation button",
			request: ActionRequest{Tool: "brw_click", Origin: "https://example.test", Label: "Next page"},
			want:    nil,
		},
		{
			name:    "checkout button",
			request: ActionRequest{Tool: "brw_click", Origin: "https://example.test", Label: "Proceed to checkout"},
			want:    []RiskClass{RiskPurchase},
		},
		{
			name:    "publish button",
			request: ActionRequest{Tool: "brw_click", Origin: "https://example.test", Label: "Publish post"},
			want:    []RiskClass{RiskPublish},
		},
		{
			name:    "clicked text rather than a label",
			request: ActionRequest{Tool: "brw_click_text", Origin: "https://example.test", Text: "Place order"},
			want:    []RiskClass{RiskPurchase},
		},
		{
			name:    "card number field",
			request: ActionRequest{Tool: "brw_fill", Origin: "https://example.test", Label: "Continue", Fields: []string{"Email", "Card number"}},
			want:    []RiskClass{RiskPersonalData},
		},
		{
			name:    "national insurance field",
			request: ActionRequest{Tool: "brw_fill", Origin: "https://example.test", Fields: []string{"National Insurance number"}},
			want:    []RiskClass{RiskPersonalData},
		},
		{
			name:    "ordinary fields are not personal data",
			request: ActionRequest{Tool: "brw_fill", Origin: "https://example.test", Fields: []string{"Search", "Quantity"}},
			want:    nil,
		},
		{
			name:    "blocklisted origin makes any action high risk",
			request: ActionRequest{Tool: "brw_click", Origin: "https://thepiratebay.org", Label: "Next page"},
			want:    []RiskClass{RiskBlocklistedCategory},
		},
		{
			name:    "a purchase on a blocklisted origin reports both",
			request: ActionRequest{Tool: "brw_click", Origin: "https://paypal.com", Label: "Send money"},
			want:    []RiskClass{RiskBlocklistedCategory, RiskPurchase},
		},
		{
			name:    "payload is not a payment",
			request: ActionRequest{Tool: "brw_click", Origin: "https://example.test", Label: "Download payload"},
			want:    nil,
		},
		{
			name:    "an opaque ref with no label carries no words to classify",
			request: ActionRequest{Tool: "brw_click", Origin: "https://example.test"},
			want:    nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []RiskClass
			for _, risk := range Classify(c.request, set) {
				got = append(got, risk.Class)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Classify(%+v) = %v, want %v", c.request, got, c.want)
			}
		})
	}
}

func TestSummaryNamesEachClassAndItsEvidence(t *testing.T) {
	risks := []Risk{{Class: RiskPurchase, Evidence: "checkout"}, {Class: RiskPersonalData, Evidence: "card number"}}
	if got := Summary(risks); got != "purchase (checkout), personal-data (card number)" {
		t.Fatalf("Summary = %q", got)
	}
	if got := Summary(nil); got != "" {
		t.Fatalf("Summary(nil) = %q", got)
	}
}
