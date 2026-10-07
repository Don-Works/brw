package mcp

import "sort"

// ToolNames is every tool in the unfiltered catalogue, sorted.
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
