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
			name:    "a page tool the page declares consequential",
			request: ActionRequest{Tool: "brw_call_page_tool", Origin: "https://example.test", Label: "archive thread", Consequential: true},
			want:    []RiskClass{RiskConsequential},
		},
		{
			name:    "a page tool is classified by its words too",
			request: ActionRequest{Tool: "brw_call_page_tool", Origin: "https://example.test", Label: "place order Place the order in the cart", Consequential: true},
			want:    []RiskClass{RiskConsequential, RiskPurchase},
		},
		{
			name:    "a read-only page tool with plain words is not high risk",
			request: ActionRequest{Tool: "brw_call_page_tool", Origin: "https://example.test", Label: "search products"},
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
