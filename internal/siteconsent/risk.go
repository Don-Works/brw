package siteconsent

import (
	"sort"
	"strings"
)

// RiskClass names one kind of high-risk action. The set is closed and the rules
// below are the only thing that decides membership: the classification is a
// table, not a condition scattered through the call sites, so it can be read,
// reviewed and tested in one place.
type RiskClass string

const (
	// RiskPublish covers making something visible to other people.
	RiskPublish RiskClass = "publish"
	// RiskPurchase covers spending money or committing to spend it.
	RiskPurchase RiskClass = "purchase"
	// RiskPersonalData covers handing over identity, payment or credential
	// details that are not recoverable once sent.
	RiskPersonalData RiskClass = "personal-data"
	// RiskBlocklistedCategory covers any action at all on an origin in a shipped
	// blocklist category. It is not about what the action says it does.
	RiskBlocklistedCategory RiskClass = "blocklisted-category"
)

// riskRule matches an action's own words. Signals are lowercase substrings
// matched against the action's target label and its typed text.
//
// Substring matching over an accessible name is deliberately blunt. The cost of
// a false positive is one confirmation prompt; the cost of a false negative is a
// purchase the user did not ask for. Where the two trade off, this over-matches.
type riskRule struct {
	Class   RiskClass
	Signals []string
}

// riskRules is the classification. Adding a class means adding a row here.
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

// personalDataSignals match the FIELDS an action fills rather than the button it
// presses. A form carrying any of these is personal data going somewhere, and
// the submit button that sends it is frequently labelled nothing more alarming
// than "Continue".
var personalDataSignals = []string{
	"account number", "bank account", "card number", "cardnumber", "credit card",
	"cvc", "cvv", "date of birth", "dateofbirth", "driver licence",
	"driver's license", "iban", "national insurance", "nationalinsurance",
	"passport", "routing number", "security code", "social security",
	"sort code", "sortcode", "ssn", "tax id", "taxid", "tax file number",
}

// ActionRequest is what the consent layer knows about one action before it runs.
//
// Label is the human-readable target: the accessible name of the element the
// agent addressed, or the text it asked brw to click. Fields are the field
// labels an action writes to. Both come from the agent's own arguments or from
// the snapshot brw already returned to it, never from a fresh page query - a
// confirmation gate that cost a round trip per click would be turned off.
type ActionRequest struct {
	Tool   string
	Origin string
	Label  string
	Text   string
	Fields []string
}

// Risk is one classification hit, with the evidence that produced it.
type Risk struct {
	Class    RiskClass `json:"class"`
	Evidence string    `json:"evidence"`
	Detail   string    `json:"detail,omitempty"`
}

// Classify returns the risk classes an action falls into, plus the evidence.
//
// It reads only what the caller passed. An action addressed by an opaque ref
// whose label brw never saw carries no words to match, so only origin-level
// rules (the category one) can fire for it. That is a real limit of the
// classification, not a gap this pretends to cover.
func Classify(request ActionRequest, categories CategorySet) []Risk {
	var risks []Risk
	haystack := strings.ToLower(strings.Join([]string{request.Label, request.Text}, " "))
	// A trailing space lets a signal like "pay " match "pay now" and "pay in
	// full" without also matching "payload" or "payment method" labels that are
	// only describing a field.
	padded := " " + strings.Join(strings.Fields(haystack), " ") + " "
	for _, rule := range riskRules {
		for _, signal := range rule.Signals {
			if strings.Contains(padded, signal) {
				risks = append(risks, Risk{Class: rule.Class, Evidence: strings.TrimSpace(signal), Detail: strings.TrimSpace(haystack)})
				break
			}
		}
	}
	// One personal-data hit is enough: the answer is "this form carries personal
	// data", and listing every matching field would put the field names a user
	// is about to type into an error string.
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
