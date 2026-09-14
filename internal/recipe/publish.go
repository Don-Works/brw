package recipe

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Publishing goes to the private provider and nowhere else.
//
// brw is public and owns the recipe ABI; an operator's recipes are theirs and
// describe what their business does on which sites. A compiled draft is a
// recipe that has not been reviewed yet, so it is the same class of thing. It
// is handed to the provider over the write API, or written to a file the
// operator names outside every Git checkout. There is no third option, and in
// particular no default path inside this repository.

// Draft is a compiled recipe together with the material that justifies it.
type Draft struct {
	Recipe  Recipe           `json:"recipe"`
	Review  string           `json:"review"`
	Targets []CompiledTarget `json:"targets,omitempty"`
	// Source names what the draft was compiled from, for a reviewer deciding
	// how much to trust it.
	Source string `json:"source"`
}

// PublishedDraft is the provider's answer: the identity it now holds.
type PublishedDraft struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	// ReviewURL is where a human reads the draft, when the provider offers one.
	ReviewURL string `json:"review_url,omitempty"`
}

// DraftWriter is the private provider's write side. It is separate from
// Provider because reading recipes and creating them are different privileges:
// a daemon that runs recipes has no business publishing them.
type DraftWriter interface {
	PublishDraft(context.Context, Draft) (PublishedDraft, error)
}

// NewDraft renders a compile result as the document the provider receives.
func NewDraft(result CompileResult, source string) Draft {
	if strings.TrimSpace(source) == "" {
		source = "brw_trace"
	}
	return Draft{Recipe: result.Recipe, Review: result.Review, Targets: result.Targets, Source: source}
}

// ValidateDraft checks a draft before it leaves the machine. Publishing an
// invalid recipe would put a document nothing can execute in front of a
// reviewer, who would approve it on the strength of the review body.
func ValidateDraft(draft Draft) error {
	var problems []error
	if err := Validate(draft.Recipe); err != nil {
		problems = append(problems, err)
	}
	if err := RequireWriteVerification(draft.Recipe); err != nil {
		problems = append(problems, err)
	}
	if strings.TrimSpace(draft.Review) == "" {
		problems = append(problems, errors.New("a draft must carry the review body a human reads"))
	}
	if len(draft.Review) > 1<<20 {
		problems = append(problems, errors.New("draft review body exceeds 1 MiB"))
	}
	if strings.TrimSpace(draft.Source) == "" {
		problems = append(problems, errors.New("a draft must name what it was compiled from"))
	}
	return errors.Join(problems...)
}

// PublishDraft hands a compiled draft to the private provider.
func (p *HTTPProvider) PublishDraft(ctx context.Context, draft Draft) (PublishedDraft, error) {
	if err := ValidateDraft(draft); err != nil {
		return PublishedDraft{}, err
	}
	digest, err := Digest(draft.Recipe)
	if err != nil {
		return PublishedDraft{}, err
	}
	var out struct {
		Draft PublishedDraft `json:"draft"`
	}
	if err := p.post(ctx, "/v1/recipes/drafts", draft, &out); err != nil {
		return PublishedDraft{}, err
	}
	// The provider is authenticated, not trusted. A reply naming a different
	// recipe would send the operator to review something else entirely.
	if out.Draft.ID != draft.Recipe.ID || out.Draft.Version != draft.Recipe.Version || out.Draft.Digest != digest {
		return PublishedDraft{}, errors.New("recipe provider acknowledged a draft that does not match the one published")
	}
	if url := strings.TrimSpace(out.Draft.ReviewURL); url != "" {
		if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://127.0.0.1") && !strings.HasPrefix(url, "http://localhost") {
			return PublishedDraft{}, errors.New("recipe provider returned a review URL that is not HTTPS")
		}
	}
	return out.Draft, nil
}

// Lookup reads the provider's receipt for key.
func (p *HTTPProvider) Lookup(ctx context.Context, key string) (Receipt, bool, error) {
	if err := validateReceiptKey(key); err != nil {
		return Receipt{}, false, err
	}
	var out struct {
		Found   bool    `json:"found"`
		Receipt Receipt `json:"receipt"`
	}
	if err := p.post(ctx, "/v1/receipts/lookup", map[string]string{"key": key}, &out); err != nil {
		return Receipt{}, false, err
	}
	if !out.Found {
		return Receipt{}, false, nil
	}
	if err := checkReceiptReply(out.Receipt, key); err != nil {
		return Receipt{}, false, err
	}
	return out.Receipt, true, nil
}

// Begin records an in-flight receipt with the provider before dispatch.
func (p *HTTPProvider) Begin(ctx context.Context, receipt Receipt) (Receipt, error) {
	receipt.Status = ReceiptInFlight
	if err := validateReceipt(receipt); err != nil {
		return Receipt{}, err
	}
	var out struct {
		Receipt Receipt `json:"receipt"`
	}
	if err := p.post(ctx, "/v1/receipts/begin", receipt, &out); err != nil {
		return Receipt{}, err
	}
	if err := checkReceiptReply(out.Receipt, receipt.Key); err != nil {
		return Receipt{}, err
	}
	if out.Receipt.StepID != receipt.StepID || out.Receipt.RecipeID != receipt.RecipeID {
		return Receipt{}, errors.New("recipe provider acknowledged a receipt describing a different write")
	}
	return out.Receipt, nil
}

// Commit records completion evidence against an existing receipt.
func (p *HTTPProvider) Commit(ctx context.Context, key, evidence string) (Receipt, error) {
	if err := validateReceiptKey(key); err != nil {
		return Receipt{}, err
	}
	if len(evidence) > 2000 {
		return Receipt{}, errors.New("receipt evidence is too long")
	}
	var out struct {
		Receipt Receipt `json:"receipt"`
	}
	if err := p.post(ctx, "/v1/receipts/commit", map[string]string{"key": key, "evidence": evidence}, &out); err != nil {
		return Receipt{}, err
	}
	if err := checkReceiptReply(out.Receipt, key); err != nil {
		return Receipt{}, err
	}
	if out.Receipt.Status != ReceiptCommitted {
		return Receipt{}, fmt.Errorf("recipe provider acknowledged a commit but reports status %q", out.Receipt.Status)
	}
	return out.Receipt, nil
}

func checkReceiptReply(receipt Receipt, key string) error {
	if receipt.Key != key {
		return errors.New("recipe provider returned a receipt for a different key")
	}
	return validateReceipt(receipt)
}

func validateReceiptKey(key string) error {
	if len(key) != 64 {
		return errors.New("receipt key must be a sha256 digest")
	}
	for index := 0; index < len(key); index++ {
		character := key[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return errors.New("receipt key must be a lowercase hex sha256 digest")
		}
	}
	return nil
}
