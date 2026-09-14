package siteconsent

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

//go:embed categories.json
var categoriesJSON []byte

// Category is one shipped blocklist category.
type Category struct {
	Name    string   `json:"name"`
	Title   string   `json:"title"`
	Why     string   `json:"why"`
	Domains []string `json:"domains"`
}

// CategorySet is the shipped blocklist plus whatever an operator added.
//
// Source and Update carry the list's provenance into every surface that shows a
// refusal, so a user told "this origin is in the financial-services category"
// can find out who decided that and how to change it without reading the source.
type CategorySet struct {
	Version    string     `json:"version"`
	Source     string     `json:"source"`
	Update     string     `json:"update"`
	Coverage   string     `json:"coverage"`
	Categories []Category `json:"categories"`
}

var (
	shippedOnce sync.Once
	shipped     CategorySet
	shippedErr  error
)

// ShippedCategories returns the list embedded in the binary.
func ShippedCategories() (CategorySet, error) {
	shippedOnce.Do(func() {
		shippedErr = json.Unmarshal(categoriesJSON, &shipped)
		if shippedErr == nil {
			shippedErr = shipped.validate()
		}
	})
	return shipped.clone(), shippedErr
}

func (c CategorySet) clone() CategorySet {
	out := c
	out.Categories = make([]Category, len(c.Categories))
	for i, category := range c.Categories {
		out.Categories[i] = category
		out.Categories[i].Domains = append([]string(nil), category.Domains...)
	}
	return out
}

func (c CategorySet) validate() error {
	if strings.TrimSpace(c.Version) == "" {
		return fmt.Errorf("category list has no version")
	}
	if strings.TrimSpace(c.Source) == "" || strings.TrimSpace(c.Update) == "" {
		return fmt.Errorf("category list must document its source and its update path")
	}
	seen := map[string]bool{}
	for _, category := range c.Categories {
		if strings.TrimSpace(category.Name) == "" {
			return fmt.Errorf("category list has an unnamed category")
		}
		if seen[category.Name] {
			return fmt.Errorf("category %q appears twice", category.Name)
		}
		seen[category.Name] = true
		if len(category.Domains) == 0 {
			return fmt.Errorf("category %q lists no domains, so it can never match", category.Name)
		}
		for _, domain := range category.Domains {
			if strings.TrimSpace(domain) == "" || strings.Contains(domain, "/") || strings.Contains(domain, ":") {
				return fmt.Errorf("category %q lists %q, which is not a bare domain", category.Name, domain)
			}
		}
	}
	return nil
}

// Extend merges operator-supplied domains into the shipped categories. A name
// that is not already a shipped category becomes a new one, so a managed machine
// can carry categories brw never heard of.
func (c CategorySet) Extend(extra map[string][]string) CategorySet {
	if len(extra) == 0 {
		return c.clone()
	}
	out := c.clone()
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		domains := extra[name]
		index := -1
		for i := range out.Categories {
			if out.Categories[i].Name == name {
				index = i
				break
			}
		}
		if index < 0 {
			out.Categories = append(out.Categories, Category{
				Name:    name,
				Title:   name,
				Why:     "Added by this machine's brw admin config.",
				Domains: append([]string(nil), domains...),
			})
			continue
		}
		out.Categories[index].Domains = append(out.Categories[index].Domains, domains...)
	}
	return out
}

// CategoryOf returns the first category whose domain list covers the origin's
// host, and whether one did. Subdomains count: a grant for
// "https://secure.example-bank.test" is the same decision as one for the
// apex, and a category list that only matched apexes would be trivially
// side-stepped.
func (c CategorySet) CategoryOf(origin string) (Category, bool) {
	host := HostOfOrigin(origin)
	if host == "" {
		return Category{}, false
	}
	for _, category := range c.Categories {
		for _, domain := range category.Domains {
			if hostMatches(host, domain) {
				return category, true
			}
		}
	}
	return Category{}, false
}
