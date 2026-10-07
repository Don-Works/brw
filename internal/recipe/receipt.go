package recipe

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// ReceiptInFlight means the write was dispatched and its declared postcondition has not been seen.
	ReceiptInFlight = "in_flight"
	// ReceiptCommitted means the declared postcondition passed after dispatch.
	ReceiptCommitted = "committed"
)

const receiptKeyDomain = "brw.recipe.receipt.v1"

// Receipt is the provider-side record of one external write.
type Receipt struct {
	Key           string `json:"key"`
	RecipeID      string `json:"recipe_id"`
	RecipeVersion string `json:"recipe_version"`
	RecipeDigest  string `json:"recipe_digest"`
	StepID        string `json:"step_id"`
	Origin        string `json:"origin"`
	Status        string `json:"status"`
	// SiteNonceDigest is a digest of the site's own per-submission token, when the step declared one.
	SiteNonceDigest string    `json:"site_nonce_digest,omitempty"`
	DispatchedAt    time.Time `json:"dispatched_at"`
	CommittedAt     time.Time `json:"committed_at,omitempty"`
	// Evidence names what was observed to pass.
	Evidence string `json:"evidence,omitempty"`
}

// Receipts is the provider-side write ledger.
type Receipts interface {
	// Lookup reports the receipt held for key, if any.
	Lookup(context.Context, string) (Receipt, bool, error)
	// Begin records an in-flight receipt BEFORE the write is dispatched and reports whether THIS call created it.
	Begin(context.Context, Receipt) (Receipt, bool, error)
	// Commit records completion evidence.
	Commit(context.Context, string, string) (Receipt, error)
}

// ReceiptKey derives the idempotency key for one external write step.
func ReceiptKey(value Recipe, step Step, origin string, inputs map[string]string) (string, error) {
	digest, err := Digest(value)
	if err != nil {
		return "", err
	}
	exactOrigin, err := normalizeReceiptOrigin(origin)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(step.ID) == "" {
		return "", errors.New("receipt key needs a step id")
	}
	sum := sha256.New()
	writeKeyField(sum, receiptKeyDomain)
	writeKeyField(sum, digest)
	writeKeyField(sum, step.ID)
	writeKeyField(sum, exactOrigin)

	names := sortedInputNames(value.Inputs)
	writeKeyField(sum, fmt.Sprintf("inputs:%d", len(names)))
	for _, name := range names {
		writeKeyField(sum, name)
		supplied, ok := inputs[name]
		if !ok {
			if value.Inputs[name].Required {
				return "", fmt.Errorf("receipt key needs required input %q", name)
			}
			writeKeyField(sum, "absent")
			continue
		}
		if !utf8.ValidString(supplied) {
			return "", fmt.Errorf("input %q is not valid UTF-8, so its receipt key would not be reproducible", name)
		}
		writeKeyField(sum, "present")
		writeKeyField(sum, normalizeReceiptInput(supplied))
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func hashSiteNonce(nonce string) string {
	if nonce == "" {
		return ""
	}
	sum := sha256.New()
	writeKeyField(sum, receiptKeyDomain+".nonce")
	writeKeyField(sum, nonce)
	return hex.EncodeToString(sum.Sum(nil))
}

func writeKeyField(sum hash.Hash, field string) {
	var length [binary.MaxVarintLen64]byte
	sum.Write(length[:binary.PutUvarint(length[:], uint64(len(field)))])
	sum.Write([]byte(field))
}

func normalizeReceiptInput(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}

func normalizeReceiptOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("receipt key needs an exact origin, got %q", raw)
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", fmt.Errorf("receipt key needs an exact origin with no path, query or credentials, got %q", raw)
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	exact := scheme + "://" + host
	if port != "" {
		exact += ":" + port
	}
	if strings.Contains(host, ":") {

		exact = scheme + "://[" + host + "]"
		if port != "" {
			exact += ":" + port
		}
	}
	if err := validateOrigin(exact); err != nil {
		return "", err
	}
	return exact, nil
}

// MemoryReceipts is an in-process Receipts for tests and for a single-process tool run.
type MemoryReceipts struct {
	mu      sync.Mutex
	records map[string]Receipt
}

func NewMemoryReceipts() *MemoryReceipts {
	return &MemoryReceipts{records: map[string]Receipt{}}
}

func (m *MemoryReceipts) Lookup(_ context.Context, key string) (Receipt, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[key]
	return record, ok, nil
}

func (m *MemoryReceipts) Begin(_ context.Context, receipt Receipt) (Receipt, bool, error) {
	if err := validateReceipt(receipt); err != nil {
		return Receipt{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.records[receipt.Key]; ok {
		return existing, false, nil
	}
	receipt.Status = ReceiptInFlight
	if receipt.DispatchedAt.IsZero() {
		receipt.DispatchedAt = time.Now().UTC()
	}
	m.records[receipt.Key] = receipt
	return receipt, true, nil
}

func (m *MemoryReceipts) Commit(_ context.Context, key, evidence string) (Receipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[key]
	if !ok {
		return Receipt{}, fmt.Errorf("no receipt for key %s", key)
	}
	record.Status = ReceiptCommitted
	record.CommittedAt = time.Now().UTC()
	record.Evidence = evidence
	m.records[key] = record
	return record, nil
}

func validateReceipt(receipt Receipt) error {
	if strings.TrimSpace(receipt.Key) == "" {
		return errors.New("receipt needs a key")
	}
	if strings.TrimSpace(receipt.StepID) == "" {
		return errors.New("receipt needs a step id")
	}
	if !recipeIDPattern.MatchString(receipt.RecipeID) || !versionPattern.MatchString(receipt.RecipeVersion) {
		return errors.New("receipt needs a pinned recipe identity")
	}
	if _, err := hex.DecodeString(receipt.RecipeDigest); err != nil || len(receipt.RecipeDigest) != 2*sha256.Size {
		return errors.New("receipt needs the pinned recipe digest")
	}
	if err := validateOrigin(receipt.Origin); err != nil {
		return err
	}
	if receipt.Status != "" && receipt.Status != ReceiptInFlight && receipt.Status != ReceiptCommitted {
		return fmt.Errorf("unsupported receipt status %q", receipt.Status)
	}
	return nil
}
