package siteconsent

import (
	"sort"
	"strings"
)

// RiskClass names one kind of high-risk action.
type RiskClass string

const (
	// RiskPublish covers making something visible to other people.
	RiskPublish RiskClass = "publish"
	// RiskPurchase covers spending money or committing to spend it.
	RiskPurchase RiskClass = "purchase"
	// RiskPersonalData covers handing over identity, payment or credential details that are not recoverable once sent.
	RiskPersonalData RiskClass = "personal-data"
	// RiskBlocklistedCategory covers any action at all on an origin in a shipped blocklist category.
	RiskBlocklistedCategory RiskClass = "blocklisted-category"
	// RiskConsequential covers a WebMCP page tool the page itself declares consequential or destructive.
	RiskConsequential RiskClass = "consequential"
)

type riskRule struct {
	Class   RiskClass
	Signals []string
}

var riskRules = []riskRule{
	{
		Class: RiskPublish,
		Signals: []string{
			"publish", "post ", "post comment", "submit post", "go live",
			"tweet", "share publicly", "make public", "send message", "send email",
			"send reply", "reply all", "comment", "broadcast", "merge pull request",
		},
	},
	{
		Class: RiskPurchase,
		Signals: []string{
			"buy", "purchase", "pay now", "pay ", "checkout", "check out",
			"place order", "confirm order", "complete order", "subscribe",
			"start subscription", "donate", "send money", "transfer funds",
			"confirm payment", "add to order", "bid", "book now", "reserve now",
		},
	},
}

var personalDataSignals = []string{
	"account number", "bank account", "card number", "cardnumber", "credit card",
	"cvc", "cvv", "date of birth", "dateofbirth", "driver licence",
	"driver's license", "iban", "national insurance", "nationalinsurance",
	"passport", "routing number", "security code", "social security",
	"sort code", "sortcode", "ssn", "tax id", "taxid", "tax file number",
}

// ActionRequest is what the consent layer knows about one action before it runs.
type ActionRequest struct {
	Tool   string
	Origin string
	Label  string
	Text   string
	Fields []string
	// Consequential is set when the target declares its own effects need a person's agreement, as a WebMCP tool's consequentialHint does.
	Consequential bool
}

// Risk is one classification hit, with the evidence that produced it.
type Risk struct {
	Class    RiskClass `json:"class"`
	Evidence string    `json:"evidence"`
	Detail   string    `json:"detail,omitempty"`
}

// Classify returns the risk classes an action falls into, plus the evidence.
func Classify(request ActionRequest, categories CategorySet) []Risk {
	var risks []Risk
	haystack := strings.ToLower(strings.Join([]string{request.Label, request.Text}, " "))

	padded := " " + strings.Join(strings.Fields(haystack), " ") + " "
	for _, rule := range riskRules {
		for _, signal := range rule.Signals {
			if strings.Contains(padded, signal) {
				risks = append(risks, Risk{Class: rule.Class, Evidence: strings.TrimSpace(signal), Detail: strings.TrimSpace(haystack)})
				break
			}
		}
	}

	for _, field := range request.Fields {
		lowered := strings.ToLower(strings.Join(strings.Fields(field), " "))
		matched := ""
		for _, signal := range personalDataSignals {
			if strings.Contains(lowered, signal) {
				matched = signal
				break
			}
		}
		if matched != "" {
			risks = append(risks, Risk{Class: RiskPersonalData, Evidence: matched, Detail: strings.TrimSpace(lowered)})
			break
		}
	}
	if request.Consequential {
		risks = append(risks, Risk{Class: RiskConsequential, Evidence: "declared consequential by the page", Detail: strings.TrimSpace(request.Label)})
	}
	if category, blocked := categories.CategoryOf(request.Origin); blocked {
		risks = append(risks, Risk{Class: RiskBlocklistedCategory, Evidence: category.Name, Detail: category.Title})
	}
	sort.SliceStable(risks, func(i, j int) bool { return risks[i].Class < risks[j].Class })
	return risks
}

// Summary renders risks for an error message or a prompt.
func Summary(risks []Risk) string {
	if len(risks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(risks))
	for _, risk := range risks {
		parts = append(parts, string(risk.Class)+" ("+risk.Evidence+")")
	}
	return strings.Join(parts, ", ")
}
