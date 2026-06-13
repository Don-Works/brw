package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/chromedp"
)

var interactiveAXRoles = map[string]bool{
	"button": true, "checkbox": true, "combobox": true, "link": true,
	"listbox": true, "menuitem": true, "radio": true, "searchbox": true,
	"slider": true, "spinbutton": true, "switch": true, "tab": true,
	"textbox": true, "treeitem": true,
}

func EnrichAccessibility(ctx context.Context, snap *PageSnapshot) {
	var nodes []*accessibility.Node
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		nodes, err = accessibility.GetFullAXTree().Do(ctx)
		return err
	}))
	if err != nil {
		snap.Accessibility = AccessibilitySummary{Available: false, Error: err.Error()}
		return
	}

	summary := AccessibilitySummary{
		Available: true,
		NodeCount: len(nodes),
		Roles:     map[string]int{},
	}
	seen := map[string]bool{}
	for _, node := range nodes {
		if node == nil || node.Ignored {
			continue
		}
		role := axValue(node.Role)
		name := axValue(node.Name)
		if role == "" {
			continue
		}
		summary.Roles[role]++
		if interactiveAXRoles[role] {
			summary.InteractiveNodeCount++
			seen[role+"|"+normalize(name)] = true
		}
	}
	for i := range snap.Elements {
		key := snap.Elements[i].Role + "|" + normalize(snap.Elements[i].Name)
		if seen[key] {
			snap.Elements[i].Source = appendSource(snap.Elements[i].Source, "ax")
		}
	}
	snap.Accessibility = summary
}

func axValue(v *accessibility.Value) string {
	if v == nil || v.Value == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(v.Value), &s); err == nil {
		return s
	}
	var anyValue any
	if err := json.Unmarshal([]byte(v.Value), &anyValue); err == nil {
		return fmt.Sprint(anyValue)
	}
	return strings.Trim(string(v.Value), `"`)
}

func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func appendSource(src []string, add string) []string {
	for _, item := range src {
		if item == add {
			return src
		}
	}
	return append(src, add)
}
