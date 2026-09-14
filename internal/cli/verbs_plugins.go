package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/Don-Works/brw/internal/plugin"
)

// Plugins get shell verbs and no MCP tools. Loading one is an operator decision
// made against a filesystem, and revoking one is an operator decision made in a
// hurry — neither belongs in the surface an agent drives. `brw plugins` is how
// a human answers "what did this daemon grant?" without reading the manifests.

func pluginVerbs() []verb {
	return []verb{
		{
			name:    "plugins",
			summary: "list the plugins this daemon loaded and what each holds",
			method:  http.MethodGet,
			path:    "/api/plugins",
			build: func(_ *options, args []string) (request, error) {
				return request{}, noArgs(args)
			},
			render: renderPlugins,
		},
		{
			name:      "plugin revoke",
			usage:     "<plugin-id>",
			summary:   "drop a plugin's capabilities until the daemon restarts",
			method:    http.MethodPost,
			path:      "/api/plugins/revoke",
			exactBody: true,
			build: func(_ *options, args []string) (request, error) {
				id, err := oneArg(args, "plugin id")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"id": strings.TrimSpace(id)}}, nil
			},
			render: renderPluginRevoke,
		},
	}
}

type pluginListResult struct {
	Plugins   []plugin.Status `json:"plugins"`
	Grantable []string        `json:"grantable_capabilities"`
}

type pluginRevokeResult struct {
	Revoked string          `json:"revoked"`
	Plugins []plugin.Status `json:"plugins"`
}

func renderPlugins(w io.Writer, _ *options, body []byte) error {
	var result pluginListResult
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	if len(result.Plugins) == 0 {
		fmt.Fprintf(w, "no plugins loaded (grantable capabilities: %s)\n", strings.Join(result.Grantable, ", "))
		return nil
	}
	writePluginTable(w, result.Plugins)
	fmt.Fprintf(w, "grantable capabilities: %s\n", strings.Join(result.Grantable, ", "))
	return nil
}

func renderPluginRevoke(w io.Writer, _ *options, body []byte) error {
	var result pluginRevokeResult
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	fmt.Fprintf(w, "revoked %s; it holds nothing until brwd restarts\n", result.Revoked)
	writePluginTable(w, result.Plugins)
	return nil
}

func writePluginTable(w io.Writer, plugins []plugin.Status) {
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tVERSION\tSTATE\tCAPABILITIES")
	for _, status := range plugins {
		state := "granted"
		if status.Revoked {
			state = "revoked"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", status.ID, status.Version, state, strings.Join(status.Capabilities, ","))
	}
	_ = table.Flush()
}
