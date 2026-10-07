package siteconsent

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
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
type CategorySet struct {
	Version    string     `json:"version"`
	Source     string     `json:"source"`
	Update     string     `json:"update"`
	Coverage   string     `json:"coverage"`
	Categories []Category `json:"categories"`
}

var shippedCategories = sync.OnceValues(func() (CategorySet, error) {
	var shipped CategorySet
	err := json.Unmarshal(categoriesJSON, &shipped)
	if err == nil {
		err = shipped.validate()
	}
	return shipped, err
})

// ShippedCategories returns the list embedded in the binary.
func ShippedCategories() (CategorySet, error) {
	shipped, err := shippedCategories()
	return shipped.clone(), err
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

// Extend merges operator-supplied domains into the shipped categories.
func (c CategorySet) Extend(extra map[string][]string) CategorySet {
	if len(extra) == 0 {
		return c.clone()
	}
	out := c.clone()
	for _, name := range slices.Sorted(maps.Keys(extra)) {
		domains := extra[name]
		index := slices.IndexFunc(out.Categories, func(category Category) bool { return category.Name == name })
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

// CategoryOf returns the first category whose domain list covers the origin's host, and whether one did.
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
