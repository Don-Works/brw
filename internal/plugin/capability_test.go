package plugin

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// zeroWidthSpace is written as a rune rather than inline: an invisible
// character in a source literal is unreviewable, which is the same property
// that makes it worth testing the gate against.
var zeroWidthSpace = string(rune(0x200b))

// The capability gate is the whole security argument for having an extension
// point at all, so the attempts below are written as an attacker would make
// them: name the default you want widened, then try the variants that a
// trimming, case-folding or prefix-matching gate would let through.
func TestNoCapabilityCanWidenABrwSecurityDefault(t *testing.T) {
	for name, test := range map[string]struct {
		capability string
		wantErr    string
	}{
		"granted":                    {CapabilityCredentialRead, ""},
		"granted browser provider":   {CapabilityBrowserProvider, ""},
		"bulk cookie export":         {"cookies.export", "widen a brw security default"},
		"bulk storage export":        {"storage.export", "widen a brw security default"},
		"arbitrary command":          {"command.run", "widen a brw security default"},
		"chrome launch flags":        {"launch.mutate", "widen a brw security default"},
		"captcha egress":             {"captcha.solve", "widen a brw security default"},
		"navigation policy off":      {"navigation.policy.disable", "widen a brw security default"},
		"make brw a vault":           {"secret.store", "widen a brw security default"},
		"unknown name":               {"anything.at.all", "not a capability brw defines"},
		"browser provider suffixed":  {CapabilityBrowserProvider + "s", "not a capability brw defines"},
		"browser provider uppercase": {"Browser.Provider", "not a capability brw defines"},
		"browser provider spaced":    {CapabilityBrowserProvider + " ", "not a capability brw defines"},
		"trailing space":             {CapabilityCredentialRead + " ", "not a capability brw defines"},
		"leading space":              {" " + CapabilityCredentialRead, "not a capability brw defines"},
		"uppercase":                  {"Credential.Read", "not a capability brw defines"},
		"all caps":                   {"CREDENTIAL.READ", "not a capability brw defines"},
		"suffixed":                   {CapabilityCredentialRead + "onable", "not a capability brw defines"},
		"prefixed":                   {"x" + CapabilityCredentialRead, "not a capability brw defines"},
		"dotted child":               {CapabilityCredentialRead + ".all", "not a capability brw defines"},
		"traversal onto a refusal":   {CapabilityCredentialRead + "/../cookies.export", "not a capability brw defines"},
		"two names in one":           {CapabilityCredentialRead + "," + "cookies.export", "not a capability brw defines"},
		"newline separated":          {CapabilityCredentialRead + "\ncookies.export", "not a capability brw defines"},
		"nul separated":              {CapabilityCredentialRead + "\x00cookies.export", "not a capability brw defines"},
		"tab padded":                 {"\t" + CapabilityCredentialRead, "not a capability brw defines"},
		"empty":                      {"", "not a capability brw defines"},
		"unicode dot lookalike":      {"credential․read", "not a capability brw defines"},
		"zero width space":           {"credential.read" + zeroWidthSpace, "not a capability brw defines"},
		"fullwidth letters":          {"ｃredential.read", "not a capability brw defines"},
		"trailing carriage return":   {CapabilityCredentialRead + "\r", "not a capability brw defines"},
		"repeated with a separator":  {CapabilityCredentialRead + ";" + CapabilityCredentialRead, "not a capability brw defines"},
		"quoted":                     {`"` + CapabilityCredentialRead + `"`, "not a capability brw defines"},
		"glob":                       {"credential.*", "not a capability brw defines"},
		"wildcard over everything":   {"*", "not a capability brw defines"},
		"path shaped":                {"/" + CapabilityCredentialRead, "not a capability brw defines"},
		"url shaped":                 {"brw://" + CapabilityCredentialRead, "not a capability brw defines"},
		"credential write not read":  {"credential.write", "not a capability brw defines"},
		"credential list not read":   {"credential.list", "not a capability brw defines"},
		"browser provider shortened": {"browser", "not a capability brw defines"},
	} {
		t.Run(name, func(t *testing.T) {
			err := CheckCapability(test.capability)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckCapability(%q) = %v, want it granted", test.capability, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckCapability(%q) granted a capability brw must refuse", test.capability)
			}
			if !errors.Is(err, ErrCapabilityNotGrantable) {
				t.Fatalf("CheckCapability(%q) = %v, which is not the not-grantable class", test.capability, err)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("CheckCapability(%q) = %q, want a reason containing %q", test.capability, err, test.wantErr)
			}
		})
	}
}

// A lock, not a tautology: the grantable set is the entire security boundary,
// so widening it has to be a deliberate edit that fails this test first.
//
// browser.provider joined it when a backend started honouring it: a plugin can
// now decide WHICH browser brw drives. What that does not widen is what brw
// then does with the browser — the navigation policy, the containment
// boundary, the site-consent gate and the identity guard are unchanged code on
// a remote target, and internal/browser's RemoteUnavailable table is where the
// narrowing that comes with it is written down.
func TestGrantableSetIsExactlyTheTwoHonouredCapabilities(t *testing.T) {
	want := []string{CapabilityBrowserProvider, CapabilityCredentialRead}
	if got := GrantableCapabilities(); !slices.Equal(got, want) {
		t.Fatalf("grantable capabilities = %v, want %v; adding one changes what a plugin can reach, so say why in this test", got, want)
	}
}

// The three tables are the whole gate, and a name in two of them is a name
// whose second classification is unreachable: CheckCapability answers from the
// first that matches, so the reason in the other is text nobody will ever read.
// Enumerating them against CheckCapability is what makes that checkable rather
// than a convention.
//
// It also pins that every table entry produces the classification its table
// promises. "reserved" is empty today — browser.provider, the one entry it ever
// held, is granted now — so the loop over it proves nothing on its own; the
// grantable and refused loops are what fail if a name moves without its answer
// moving with it.
func TestEveryCapabilityTableEntryIsClassifiedExactlyOnce(t *testing.T) {
	seen := map[string]string{}
	claim := func(name, table string) {
		if previous, ok := seen[name]; ok {
			t.Errorf("capability %q is in both %s and %s; whichever CheckCapability answers from first makes the other's reason unreachable", name, previous, table)
		}
		seen[name] = table
	}
	for name := range grantable {
		claim(name, "grantable")
		if err := CheckCapability(name); err != nil {
			t.Errorf("grantable capability %q is refused: %v", name, err)
		}
	}
	for name := range reserved {
		claim(name, "reserved")
		err := CheckCapability(name)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("reserved capability %q = %v, want a refusal naming it as reserved", name, err)
		}
	}
	for name := range refused {
		claim(name, "refused")
		err := CheckCapability(name)
		if err == nil || !strings.Contains(err.Error(), "widen a brw security default") {
			t.Errorf("refused capability %q = %v, want a refusal naming the default it would widen", name, err)
		}
	}
	if len(seen) < len(grantable)+len(reserved)+len(refused) {
		t.Fatalf("the tables hold %d distinct names for %d entries", len(seen), len(grantable)+len(reserved)+len(refused))
	}
}
