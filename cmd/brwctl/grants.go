package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/siteconsent"
)

// grantsCommand is the operator-facing view of the consent store.
//
// It works on the FILE rather than through a running daemon, deliberately. A
// revocation has to be possible when the daemon is wedged, stopped, or is the
// very thing being taken away from an agent, and the store re-reads the file on
// its next decision, so a revocation typed here lands without a restart.
func grantsCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: brwctl grants <list|allow|revoke|revoke-all|ledger> [options]")
	}
	switch args[0] {
	case "list":
		return grantsList(args[1:], os.Stdout)
	case "allow":
		return grantsAllow(args[1:], os.Stdout)
	case "revoke":
		return grantsRevoke(args[1:], os.Stdout)
	case "revoke-all":
		return grantsRevokeAll(args[1:], os.Stdout)
	case "ledger":
		return grantsLedger(args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown grants command %q (want list, allow, revoke, revoke-all or ledger)", args[0])
	}
}

// grantsOptions are the flags every grants subcommand shares.
type grantsOptions struct {
	policy string
	json   bool
}

func (o *grantsOptions) register(fs *flag.FlagSet) {
	fs.StringVar(&o.policy, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path; the consent store lives beside it")
	fs.BoolVar(&o.json, "json", false, "print machine-readable JSON")
}

func (o *grantsOptions) guard() (*siteconsent.Guard, error) {
	dir, err := siteconsent.DirForPolicy(o.policy)
	if err != nil {
		return nil, err
	}
	store, err := siteconsent.OpenStore(siteconsent.StorePath(dir), siteconsent.KeyPath(dir))
	if err != nil {
		return nil, err
	}
	admin, err := siteconsent.LoadAdminConfig(siteconsent.DiscoverAdminConfigPath(siteconsent.StorePath(dir)))
	if err != nil {
		return nil, err
	}
	guard, err := siteconsent.NewGuard(store, admin)
	if err != nil {
		return nil, err
	}
	guard.SetGrantor(currentActor())
	return guard, nil
}

// currentActor names who a consent record is attributed to. The OS account is
// the only thing brw can actually observe; it is recorded rather than a
// friendlier label so the ledger says something verifiable.
func currentActor() string {
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		return u.Username
	}
	return "unknown"
}

type grantView struct {
	siteconsent.Grant
	Expired bool `json:"expired"`
}

func grantsList(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("grants list", flag.ContinueOnError)
	var opts grantsOptions
	opts.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("grants list takes no arguments")
	}
	guard, err := opts.guard()
	if err != nil {
		return err
	}
	store := guard.Store()
	now := time.Now()
	grants := store.List()
	views := make([]grantView, 0, len(grants))
	for _, grant := range grants {
		views = append(views, grantView{Grant: grant, Expired: grant.Expired(now)})
	}
	rejected := store.Rejected()
	if opts.json {
		return json.NewEncoder(out).Encode(map[string]any{
			"path": store.Path(), "grants": views, "rejected": rejected,
		})
	}
	if len(views) == 0 {
		fmt.Fprintln(out, "no site permission grants")
	}
	for _, view := range views {
		fields := []string{view.Origin, string(view.Scope), string(view.Decision),
			"by " + view.GrantedBy, "at " + view.GrantedAt.Format(time.RFC3339)}
		if !view.Expiry.IsZero() {
			fields = append(fields, "expires "+view.Expiry.Format(time.RFC3339))
		}
		if view.Expired {
			fields = append(fields, "EXPIRED")
		}
		if view.OverrideCategory != "" {
			fields = append(fields, "override:"+view.OverrideCategory)
		}
		fmt.Fprintln(out, strings.Join(fields, "  "))
	}
	for _, record := range rejected {
		fmt.Fprintf(out, "refused record %s %s: %s\n", record.Origin, record.Scope, record.Reason)
	}
	fmt.Fprintf(out, "store: %s\n", store.Path())
	return nil
}

func grantsAllow(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("grants allow", flag.ContinueOnError)
	var opts grantsOptions
	opts.register(fs)
	var scope, override, note string
	var ttl time.Duration
	fs.StringVar(&scope, "scope", string(siteconsent.ScopeRead), "read (observe the site) or act (change things on it)")
	fs.StringVar(&override, "override-category", "", "cross a shipped blocklist category by naming it; the override is written to the consent ledger with who and when")
	fs.StringVar(&note, "note", "", "why this grant was made; recorded in the ledger")
	fs.DurationVar(&ttl, "ttl", 0, "expire the grant after this long; 0 uses the admin config default")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("grants allow takes exactly one origin, for example https://example.com")
	}
	parsedScope, err := siteconsent.ParseScope(scope)
	if err != nil {
		return err
	}
	guard, err := opts.guard()
	if err != nil {
		return err
	}
	grant, err := guard.Allow(siteconsent.GrantOptions{
		Origin:           fs.Arg(0),
		Scope:            parsedScope,
		OverrideCategory: override,
		TTL:              ttl,
		Actor:            currentActor(),
		Note:             note,
	})
	if err != nil {
		return err
	}
	if opts.json {
		return json.NewEncoder(out).Encode(grant)
	}
	fmt.Fprintf(out, "granted %s scope %s to %s\n", grant.Origin, grant.Scope, grant.GrantedBy)
	if grant.OverrideCategory != "" {
		fmt.Fprintf(out, "category override %q recorded in %s\n", grant.OverrideCategory, siteconsent.LedgerPathFor(guard.Store().Path()))
	}
	return nil
}

func grantsRevoke(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("grants revoke", flag.ContinueOnError)
	var opts grantsOptions
	opts.register(fs)
	var scope string
	fs.StringVar(&scope, "scope", "", "revoke only this scope (read or act); default revokes both")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("grants revoke takes exactly one origin")
	}
	var parsedScope siteconsent.Scope
	if scope != "" {
		parsed, err := siteconsent.ParseScope(scope)
		if err != nil {
			return err
		}
		parsedScope = parsed
	}
	guard, err := opts.guard()
	if err != nil {
		return err
	}
	removed, err := guard.Revoke(fs.Arg(0), parsedScope, currentActor())
	if err != nil {
		return err
	}
	if opts.json {
		return json.NewEncoder(out).Encode(map[string]any{"ok": true, "removed": removed})
	}
	fmt.Fprintf(out, "revoked %d grant(s) for %s\n", removed, fs.Arg(0))
	return nil
}

func grantsRevokeAll(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("grants revoke-all", flag.ContinueOnError)
	var opts grantsOptions
	opts.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("grants revoke-all takes no arguments")
	}
	guard, err := opts.guard()
	if err != nil {
		return err
	}
	removed, err := guard.RevokeAll(currentActor())
	if err != nil {
		return err
	}
	if opts.json {
		return json.NewEncoder(out).Encode(map[string]any{"ok": true, "removed": removed})
	}
	fmt.Fprintf(out, "revoked %d grant(s)\n", removed)
	return nil
}

func grantsLedger(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("grants ledger", flag.ContinueOnError)
	var opts grantsOptions
	opts.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("grants ledger takes no arguments")
	}
	guard, err := opts.guard()
	if err != nil {
		return err
	}
	entries, err := guard.Store().Ledger()
	if err != nil {
		return err
	}
	if opts.json {
		return json.NewEncoder(out).Encode(entries)
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "no consent events recorded")
	}
	for _, entry := range entries {
		fields := []string{entry.At.Format(time.RFC3339), entry.Event, entry.Actor}
		if entry.Origin != "" {
			fields = append(fields, entry.Origin)
		}
		if entry.Scope != "" {
			fields = append(fields, string(entry.Scope))
		}
		if entry.OverrideCategory != "" {
			fields = append(fields, "override:"+entry.OverrideCategory)
		}
		if entry.Reason != "" {
			fields = append(fields, entry.Reason)
		}
		fmt.Fprintln(out, strings.Join(fields, "  "))
	}
	return nil
}
