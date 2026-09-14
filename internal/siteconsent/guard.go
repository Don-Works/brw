package siteconsent

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// NotGrantedError is the refusal for an origin the user has never answered for.
// It names the origin and the missing scope because the agent's only useful next
// move is to tell the user exactly what to grant.
type NotGrantedError struct {
	Origin  string
	Scope   Scope
	Expired bool
}

func (e *NotGrantedError) Error() string {
	if e.Expired {
		return fmt.Sprintf("site permission for %s (scope %s) has expired; brw is running non-interactively so it cannot ask. Re-grant it with: brwctl grants allow %s --scope %s", e.Origin, e.Scope, e.Origin, e.Scope)
	}
	return fmt.Sprintf("no site permission grant for %s (scope %s); brw is running non-interactively so it cannot ask. Grant it with: brwctl grants allow %s --scope %s", e.Origin, e.Scope, e.Origin, e.Scope)
}

// DeniedError is the refusal for an origin the user answered no to.
type DeniedError struct {
	Origin string
	Scope  Scope
	When   time.Time
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("site permission for %s (scope %s) was refused on %s; it stays refused until revoked with: brwctl grants revoke %s", e.Origin, e.Scope, e.When.UTC().Format(time.RFC3339), e.Origin)
}

// CategoryBlockedError is the refusal for an origin in a shipped blocklist
// category that carries no override.
type CategoryBlockedError struct {
	Origin   string
	Scope    Scope
	Category Category
	Source   string
}

func (e *CategoryBlockedError) Error() string {
	return fmt.Sprintf("%s is in brw's %s category (%s) and cannot be granted without an explicit override; re-run with: brwctl grants allow %s --scope %s --override-category %s. List source: %s",
		e.Origin, e.Category.Name, e.Category.Title, e.Origin, e.Scope, e.Category.Name, e.Source)
}

// AdminBlockedError is the refusal for an origin this machine's admin config
// blocks. There is no override: an operator list that a local prompt can talk
// its way past is not an operator list.
type AdminBlockedError struct {
	Origin string
	Scope  Scope
	Entry  string
}

func (e *AdminBlockedError) Error() string {
	return fmt.Sprintf("%s is blocked by this machine's brw admin config (entry %q); a local consent prompt cannot override an operator blocklist", e.Origin, e.Entry)
}

// ConfirmationRequiredError is the refusal for a high-risk action with nobody to
// confirm it. Non-interactive means refuse, never auto-approve: an unattended
// agent silently answering yes on the user's behalf is the failure this exists
// to prevent.
type ConfirmationRequiredError struct {
	Tool   string
	Origin string
	Risks  []Risk
}

func (e *ConfirmationRequiredError) Error() string {
	return fmt.Sprintf("%s on %s is a high-risk action (%s) and confirm-actions is on, but brw is running non-interactively so nobody can confirm it; refusing rather than approving it on the user's behalf", e.Tool, e.Origin, Summary(e.Risks))
}

// ConfirmationDeclinedError is the refusal for a high-risk action a person said
// no to.
type ConfirmationDeclinedError struct {
	Tool   string
	Origin string
	Risks  []Risk
}

func (e *ConfirmationDeclinedError) Error() string {
	return fmt.Sprintf("%s on %s was declined at the confirmation prompt (%s)", e.Tool, e.Origin, Summary(e.Risks))
}

// Prompter asks the user about an origin and returns their answer. A nil
// Prompter is the non-interactive case, which fails closed.
//
// AskSite is asked once per origin and scope; whatever it returns is recorded,
// so a refusal is not re-asked on the next action.
type Prompter interface {
	AskSite(origin string, scope Scope) (allow bool, err error)
	ConfirmAction(request ActionRequest, risks []Risk) (allow bool, err error)
}

// Guard answers "may brw do this here" from the store, the shipped category
// list and this machine's admin config.
type Guard struct {
	store      *Store
	admin      AdminConfig
	categories CategorySet
	source     string
	prompter   Prompter
	grantor    string
	now        func() time.Time
}

// NewGuard builds a guard over a store. A nil store disables consent entirely
// and every Authorize call passes: consent is opt-in, and a daemon started
// without a store must behave exactly as it did before this existed.
func NewGuard(store *Store, admin AdminConfig) (*Guard, error) {
	categories, err := ShippedCategories()
	if err != nil {
		return nil, err
	}
	source := categories.Source
	categories = categories.Extend(admin.CategoryDomains)
	return &Guard{
		store:      store,
		admin:      admin,
		categories: categories,
		source:     source,
		grantor:    "unknown",
		now:        time.Now,
	}, nil
}

// SetPrompter makes the guard interactive. Without one it is non-interactive and
// refuses anything not already granted.
func (g *Guard) SetPrompter(p Prompter) { g.prompter = p }

// SetGrantor records who a recorded consent is attributed to.
func (g *Guard) SetGrantor(name string) {
	if strings.TrimSpace(name) != "" {
		g.grantor = strings.TrimSpace(name)
	}
}

// SetClock replaces the guard's clock, for tests that need an expiry to have
// passed without waiting for it.
func (g *Guard) SetClock(now func() time.Time) {
	if now != nil {
		g.now = now
	}
}

// Store exposes the underlying store for the listing and revocation surfaces.
func (g *Guard) Store() *Store { return g.store }

// Categories exposes the effective category list (shipped plus admin additions).
func (g *Guard) Categories() CategorySet { return g.categories }

// Enabled reports whether this guard gates anything.
func (g *Guard) Enabled() bool { return g != nil && g.store != nil }

// Interactive reports whether there is anyone to ask.
func (g *Guard) Interactive() bool { return g != nil && g.prompter != nil }

// ConfirmActions reports whether the high-risk confirmation gate is on.
func (g *Guard) ConfirmActions() bool { return g != nil && g.admin.ConfirmActions }

// Authorize decides whether brw may act on rawURL at the given scope.
//
// Order is load-bearing. The admin blocklist is checked before anything a local
// prompt or a stored grant could say, so an operator decision cannot be
// overridden from the machine it constrains. The shipped category blocklist is
// checked before an ordinary grant so a blocklisted origin cannot be granted by
// the normal path at all - only by a record that carries the matching override.
func (g *Guard) Authorize(rawURL string, scope Scope) error {
	if !g.Enabled() {
		return nil
	}
	origin, err := CanonicalOrigin(rawURL)
	if err != nil {
		return err
	}
	if origin == "" {
		// about:blank, a data: URL or a relative reference: no site to consent to.
		return nil
	}
	host := HostOfOrigin(origin)
	if entry, blocked := g.admin.blocks(host); blocked {
		return &AdminBlockedError{Origin: origin, Scope: scope, Entry: entry}
	}
	if g.admin.allows(host) {
		return nil
	}
	now := g.now()
	category, inCategory := g.categories.CategoryOf(origin)

	grant, found := g.store.Lookup(origin, scope, now)
	if found && grant.Decision == DecisionDeny {
		return &DeniedError{Origin: origin, Scope: scope, When: grant.GrantedAt}
	}
	if found && grant.Decision == DecisionAllow {
		if !inCategory || grant.OverrideCategory == category.Name {
			return nil
		}
		// A grant exists but does not carry the override this category needs.
		// That happens when a domain enters a category after it was granted, and
		// the safe answer is the category one.
		return &CategoryBlockedError{Origin: origin, Scope: scope, Category: category, Source: g.source}
	}

	if inCategory {
		// An interactive prompt cannot mint an override: the override is an
		// explicit flag on an explicit command, so that crossing a category
		// boundary is a thing the user typed and not a thing they clicked
		// through while an agent waited.
		return &CategoryBlockedError{Origin: origin, Scope: scope, Category: category, Source: g.source}
	}

	expired := g.store.hasExpired(origin, scope, now)
	if g.prompter == nil {
		return &NotGrantedError{Origin: origin, Scope: scope, Expired: expired}
	}
	allow, err := g.prompter.AskSite(origin, scope)
	if err != nil {
		return fmt.Errorf("ask for site permission on %s: %w", origin, err)
	}
	decision := DecisionAllow
	if !allow {
		decision = DecisionDeny
	}
	recorded, err := g.store.Record(Grant{
		Origin:    origin,
		Scope:     scope,
		Decision:  decision,
		GrantedAt: now,
		GrantedBy: g.grantor,
		Expiry:    g.admin.expiryFrom(now),
	})
	if err != nil {
		return fmt.Errorf("record site permission for %s: %w", origin, err)
	}
	_ = g.store.AppendLedger(LedgerEntry{
		At: now, Event: "prompt", Origin: origin, Scope: scope,
		Decision: decision, Actor: g.grantor,
	})
	if !allow {
		return &DeniedError{Origin: origin, Scope: scope, When: recorded.GrantedAt}
	}
	return nil
}

// hasExpired reports whether an expired record exists for origin+scope, so the
// refusal can say "expired" rather than "never granted". Those are different
// facts for the person reading the message.
func (s *Store) hasExpired(origin string, want Scope, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	for _, grant := range s.grants {
		if grant.Origin == origin && grant.Scope.covers(want) && grant.Expired(now) {
			return true
		}
	}
	return false
}

// GrantOptions is one explicit, operator-initiated grant.
type GrantOptions struct {
	Origin string
	Scope  Scope
	// OverrideCategory must name the category the origin is in for a
	// blocklisted origin to be granted at all.
	OverrideCategory string
	TTL              time.Duration
	Actor            string
	Note             string
}

// Allow records an explicit grant, which is what brwctl and the options page
// call. A blocklisted origin without a matching override is refused here too:
// the override flag is checked against the ACTUAL category, so passing the name
// of some other category grants nothing.
func (g *Guard) Allow(opts GrantOptions) (Grant, error) {
	if !g.Enabled() {
		return Grant{}, errors.New("site consent is not enabled on this daemon")
	}
	origin, err := CanonicalOrigin(opts.Origin)
	if err != nil {
		return Grant{}, err
	}
	if origin == "" {
		return Grant{}, fmt.Errorf("%q has no network origin to grant", opts.Origin)
	}
	host := HostOfOrigin(origin)
	if entry, blocked := g.admin.blocks(host); blocked {
		return Grant{}, &AdminBlockedError{Origin: origin, Scope: opts.Scope, Entry: entry}
	}
	now := g.now()
	category, inCategory := g.categories.CategoryOf(origin)
	if inCategory && opts.OverrideCategory != category.Name {
		return Grant{}, &CategoryBlockedError{Origin: origin, Scope: opts.Scope, Category: category, Source: g.source}
	}
	override := ""
	if inCategory {
		override = category.Name
	}
	actor := opts.Actor
	if strings.TrimSpace(actor) == "" {
		actor = g.grantor
	}
	var expiry time.Time
	if opts.TTL > 0 {
		expiry = now.Add(opts.TTL)
	} else {
		expiry = g.admin.expiryFrom(now)
	}
	grant, err := g.store.Record(Grant{
		Origin:           origin,
		Scope:            opts.Scope,
		Decision:         DecisionAllow,
		GrantedAt:        now,
		GrantedBy:        actor,
		Expiry:           expiry,
		OverrideCategory: override,
		Note:             opts.Note,
	})
	if err != nil {
		return Grant{}, err
	}
	event := "allow"
	if override != "" {
		event = "category-override"
	}
	if err := g.store.AppendLedger(LedgerEntry{
		At: now, Event: event, Origin: origin, Scope: opts.Scope,
		Decision: DecisionAllow, Actor: actor, OverrideCategory: override,
		Reason: opts.Note,
	}); err != nil {
		return Grant{}, fmt.Errorf("write consent ledger: %w", err)
	}
	return grant, nil
}

// Revoke drops the records for one origin and records the revocation.
func (g *Guard) Revoke(origin string, scope Scope, actor string) (int, error) {
	if !g.Enabled() {
		return 0, errors.New("site consent is not enabled on this daemon")
	}
	removed, err := g.store.Revoke(origin, scope)
	if err != nil || removed == 0 {
		return removed, err
	}
	canonical, _ := CanonicalOrigin(origin)
	_ = g.store.AppendLedger(LedgerEntry{
		At: g.now(), Event: "revoke", Origin: canonical, Scope: scope, Actor: actorOr(actor, g.grantor),
	})
	return removed, nil
}

// RevokeAll drops every record and records that it happened.
func (g *Guard) RevokeAll(actor string) (int, error) {
	if !g.Enabled() {
		return 0, errors.New("site consent is not enabled on this daemon")
	}
	removed, err := g.store.RevokeAll()
	if err != nil || removed == 0 {
		return removed, err
	}
	_ = g.store.AppendLedger(LedgerEntry{
		At: g.now(), Event: "revoke-all", Actor: actorOr(actor, g.grantor),
		Reason: fmt.Sprintf("%d records", removed),
	})
	return removed, nil
}

// CheckAction applies the high-risk confirmation gate. It is separate from
// Authorize because the two answer different questions: Authorize asks whether
// brw may touch this site at all, CheckAction asks whether THIS action needs a
// person to say yes first.
func (g *Guard) CheckAction(request ActionRequest) error {
	if !g.Enabled() || !g.admin.ConfirmActions {
		return nil
	}
	origin, err := CanonicalOrigin(request.Origin)
	if err != nil {
		return err
	}
	request.Origin = origin
	risks := Classify(request, g.categories)
	if len(risks) == 0 {
		return nil
	}
	if g.prompter == nil {
		return &ConfirmationRequiredError{Tool: request.Tool, Origin: origin, Risks: risks}
	}
	allow, err := g.prompter.ConfirmAction(request, risks)
	if err != nil {
		return fmt.Errorf("confirm %s on %s: %w", request.Tool, origin, err)
	}
	if !allow {
		return &ConfirmationDeclinedError{Tool: request.Tool, Origin: origin, Risks: risks}
	}
	_ = g.store.AppendLedger(LedgerEntry{
		At: g.now(), Event: "confirm-action", Origin: origin, Actor: g.grantor,
		Reason: request.Tool + ": " + Summary(risks),
	})
	return nil
}

func actorOr(actor, fallback string) string {
	if strings.TrimSpace(actor) != "" {
		return strings.TrimSpace(actor)
	}
	return fallback
}
