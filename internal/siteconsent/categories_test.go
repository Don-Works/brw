package siteconsent

import (
	"strings"
	"testing"
)

// TestCategoryDataIsWellFormed is the gate the shipped list's update path names:
// a pull request that adds a domain has to keep the file loadable and keep the
// provenance fields filled in, or this fails.
func TestCategoryDataIsWellFormed(t *testing.T) {
	set, err := ShippedCategories()
	if err != nil {
		t.Fatalf("shipped category list does not load: %v", err)
	}
	for _, field := range []struct{ name, value string }{
		{"version", set.Version},
		{"source", set.Source},
		{"update", set.Update},
		{"coverage", set.Coverage},
	} {
		if strings.TrimSpace(field.value) == "" {
			t.Errorf("shipped category list has no %s; a blocklist with no documented %s cannot be argued with", field.name, field.name)
		}
	}
	want := map[string]bool{"financial-services": true, "adult": true, "pirated": true}
	for _, category := range set.Categories {
		delete(want, category.Name)
		if strings.TrimSpace(category.Why) == "" {
			t.Errorf("category %q does not say why it is on the list", category.Name)
		}
	}
	if len(want) != 0 {
		t.Errorf("shipped categories are missing %v", want)
	}
}

func TestCategoryOf(t *testing.T) {
	set, err := ShippedCategories()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		origin string
		want   string
	}{
		{name: "apex financial host", origin: "https://paypal.com", want: "financial-services"},
		{name: "subdomain of a listed host", origin: "https://secure.paypal.com", want: "financial-services"},
		{name: "adult host", origin: "https://pornhub.com", want: "adult"},
		{name: "pirated host", origin: "https://thepiratebay.org", want: "pirated"},
		{name: "unlisted host", origin: "https://example.test", want: ""},
		{name: "a host merely containing a listed name is not listed", origin: "https://paypal.com.example.test", want: ""},
		{name: "no origin", origin: "", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			category, found := set.CategoryOf(c.origin)
			if c.want == "" {
				if found {
					t.Fatalf("CategoryOf(%q) matched %q", c.origin, category.Name)
				}
				return
			}
			if !found || category.Name != c.want {
				t.Fatalf("CategoryOf(%q) = %q/%v, want %q", c.origin, category.Name, found, c.want)
			}
		})
	}
}

func TestExtendAddsAndCreatesCategories(t *testing.T) {
	set, err := ShippedCategories()
	if err != nil {
		t.Fatal(err)
	}
	extended := set.Extend(map[string][]string{
		"financial-services": {"treasury.example.test"},
		"internal-tooling":   {"admin.example.test"},
	})
	if category, found := extended.CategoryOf("https://treasury.example.test"); !found || category.Name != "financial-services" {
		t.Fatalf("an added domain did not join the existing category: %q/%v", category.Name, found)
	}
	if category, found := extended.CategoryOf("https://admin.example.test"); !found || category.Name != "internal-tooling" {
		t.Fatalf("a new category was not created: %q/%v", category.Name, found)
	}
	// Extend must not mutate the shipped list in place; a second reader of
	// ShippedCategories would otherwise inherit one machine's admin config.
	if _, found := set.CategoryOf("https://treasury.example.test"); found {
		t.Fatal("Extend mutated the shipped category list")
	}
}
