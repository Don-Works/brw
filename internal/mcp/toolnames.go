package mcp

import "sort"

// ToolNames is every tool in the unfiltered catalogue, sorted.
//
// It exists so code outside this package can check a tool name it prints
// against the catalogue itself rather than against a copy of it. A harness that
// labels its measurements with tool names has no other way to notice a rename.
func ToolNames() []string {
	catalogue := tools()
	names := make([]string, 0, len(catalogue))
	for _, tool := range catalogue {
		if name, ok := tool["name"].(string); ok && name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
