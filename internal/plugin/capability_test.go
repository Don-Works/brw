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
		"bulk cookie export":         {"cookies.export", "widen a brw security default"},
		"bulk storage export":        {"storage.export", "widen a brw security default"},
		"arbitrary command":          {"command.run", "widen a brw security default"},
		"chrome launch flags":        {"launch.mutate", "widen a brw security default"},
		"captcha egress":             {"captcha.solve", "widen a brw security default"},
		"navigation policy off":      {"navigation.policy.disable", "widen a brw security default"},
		"make brw a vault":           {"secret.store", "widen a brw security default"},
		"reserved browser provider":  {CapabilityBrowserProvider, "reserved"},
		"unknown name":               {"anything.at.all", "not a capability brw defines"},
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
func TestGrantableSetIsExactlyCredentialRead(t *testing.T) {
	if got := GrantableCapabilities(); !slices.Equal(got, []string{CapabilityCredentialRead}) {
		t.Fatalf("grantable capabilities = %v; adding one changes what a plugin can reach, so say why in this test", got)
	}
}

// The reserved name has an interface shape and no grant. Advertising it as
// available would be a claim with nothing behind it, so the refusal is the
// feature and the message has to tell an operator which of the two it is.
func TestReservedBrowserProviderIsRefusedRatherThanSilentlyIgnored(t *testing.T) {
	err := CheckCapability(CapabilityBrowserProvider)
	if err == nil {
		t.Fatal("browser.provider was granted; nothing honours it")
	}
	if !strings.Contains(err.Error(), "reserved") || strings.Contains(err.Error(), "widen") {
		t.Fatalf("browser.provider refusal = %q; it is reserved, not a widening", err)
	}
}
