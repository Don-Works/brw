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
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// A receipt answers one question a restarted daemon cannot otherwise answer:
// did the write this recipe was in the middle of actually reach the other side?
//
// It answers it only partly, and the part it cannot answer is the important
// one. A receipt records that brw dispatched something. brw writes it on brw's
// side of the network, so a process killed a millisecond after the request left
// leaves exactly the same receipt whether the remote side committed the
// transaction or dropped it. That is why a recipe carrying a write must also
// carry a step that reads the state back — see RequireWriteVerification — and
// why an in-flight receipt is grounds to stop, never grounds to retry.

const (
	// ReceiptInFlight means the write was dispatched and its declared
	// postcondition has not been seen. It is the state a crash leaves behind.
	ReceiptInFlight = "in_flight"
	// ReceiptCommitted means the declared postcondition passed after dispatch.
	ReceiptCommitted = "committed"
)

// receiptKeyDomain separates these hashes from every other sha256 in brw, so a
// digest computed for another purpose can never collide into a receipt key.
const receiptKeyDomain = "brw.recipe.receipt.v1"

// Receipt is the provider-side record of one external write.
//
// It lives with the provider, not in the daemon's memory or its cache
// directory, because the whole point is to survive the daemon. A receipt in a
// process that died with the request in flight is a receipt that was never
// written.
type Receipt struct {
	Key           string `json:"key"`
	RecipeID      string `json:"recipe_id"`
	RecipeVersion string `json:"recipe_version"`
	RecipeDigest  string `json:"recipe_digest"`
	StepID        string `json:"step_id"`
	Origin        string `json:"origin"`
	Status        string `json:"status"`
	// SiteNonceDigest is a digest of the site's own per-submission token, when
	// the step declared one. The comparison a rerun makes needs only equality,
	// and the receipt is stored by a third party, so the live token itself never
	// leaves the machine.
	SiteNonceDigest string    `json:"site_nonce_digest,omitempty"`
	DispatchedAt    time.Time `json:"dispatched_at"`
	CommittedAt     time.Time `json:"committed_at,omitempty"`
	// Evidence names what was observed to pass. It is the postcondition's
	// description, never page content: a receipt is a control record and lives
	// with a third party.
	Evidence string `json:"evidence,omitempty"`
}

// Receipts is the provider-side write ledger. Implementations are expected to
// be durable and shared; an in-memory one is a test double, not a deployment.
type Receipts interface {
	// Lookup reports the receipt held for key, if any.
	Lookup(context.Context, string) (Receipt, bool, error)
	// Begin records an in-flight receipt BEFORE the write is dispatched and
	// reports whether THIS call created it. An existing record is returned
	// unchanged rather than overwritten — the existing record is the evidence,
	// and losing it is how a duplicate write happens — and the created flag is
	// what lets the caller tell "nobody had dispatched this" from "somebody had,
	// between our lookup and our begin".
	Begin(context.Context, Receipt) (Receipt, bool, error)
	// Commit records completion evidence. It is called only after the declared
	// postcondition has passed.
	Commit(context.Context, string, string) (Receipt, error)
}

// ReceiptKey derives the idempotency key for one external write step.
//
// The key is sha256 over a domain tag and then, in this order: the recipe's
// content digest, the step id, the exact origin, and the normalized runtime
// inputs. The digest covers the id, the version and every step, so a changed
// recipe is a different write; the step id is separate because two write steps
// in one recipe are two writes and must not share one receipt.
//
// The normalization rules are written down because a key that cannot be
// reproduced after a restart is worse than no key at all — it produces a
// receipt nobody can find, which reads exactly like a write that never
// happened:
//
//   - Only inputs the recipe DECLARES take part. The runner refuses an
//     undeclared input before any step runs, so letting one into the key would
//     let a rejected request change it.
//   - An input the caller did not supply is encoded as absent, which is a
//     different key from supplying it empty. "No value" and "the empty value"
//     are different submissions.
//   - Names are sorted by byte order. Map iteration order is not an order.
//   - CRLF and lone CR become LF. The same typed paragraph comes back from a
//     textarea with whichever line ending the browser preferred, and that must
//     not mint a second key for one submission.
//   - Nothing else is touched: no trimming, no case folding, no Unicode
//     normalization. Leading space in a reference number is part of what the
//     remote side will store, so it is part of the key.
//   - Every field is written length-prefixed, so no field's bytes can be read
//     as part of the next one.
//   - A value that is not valid UTF-8 is refused rather than repaired, because
//     repair is a rule the next implementation would have to guess.
//
// The key depends on nothing else: no clock, no host, no session, no attempt
// counter. Two runs of the same pinned recipe against the same origin with the
// same inputs produce the same key on any machine, at any time.
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

	names := make([]string, 0, len(value.Inputs))
	for name := range value.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
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

// hashSiteNonce reduces the page's live submission token to a digest.
//
// Equality is the whole of what a rerun asks of it, and the receipt is held by
// the private provider, so shipping the token itself would put a live page
// credential in somebody else's store for no gain. The domain tag separates
// these digests from receipt keys computed over the same helper.
func hashSiteNonce(nonce string) string {
	if nonce == "" {
		return ""
	}
	sum := sha256.New()
	writeKeyField(sum, receiptKeyDomain+".nonce")
	writeKeyField(sum, nonce)
	return hex.EncodeToString(sum.Sum(nil))
}

// writeKeyField length-prefixes one field so the concatenation is unambiguous.
func writeKeyField(sum hash.Hash, field string) {
	var length [binary.MaxVarintLen64]byte
	sum.Write(length[:binary.PutUvarint(length[:], uint64(len(field)))])
	sum.Write([]byte(field))
}

func normalizeReceiptInput(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}

// normalizeReceiptOrigin renders the origin in exactly one way, so a write
// dispatched to https://Example.test:443 and one dispatched to
// https://example.test share a receipt rather than duplicating.
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
		// An IPv6 literal loses its brackets through Hostname().
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

// MemoryReceipts is an in-process Receipts for tests and for a single-process
// tool run. It is deliberately not the daemon's default: memory does not
// survive the restart the whole mechanism exists for, so a daemon configured
// with this would report every interrupted write as one that never started.
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
