package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
)

func envVerbs() []verb {
	return []verb{
		{
			name:    "locale",
			usage:   "[locale] [timezone]",
			summary: "override the tab's locale and timezone, or --clear to restore the host",
			method:  http.MethodPost,
			path:    "/api/page/locale",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.BoolVar(&opts.clear, "clear", false, "restore the host locale and timezone")
				fs.StringVar(&opts.device, "timezone", "", "IANA timezone, for example Europe/London")
			},
			build: func(opts *options, args []string) (request, error) {
				if opts.clear {
					if err := noArgs(args); err != nil {
						return request{}, err
					}
					return request{Body: map[string]any{"clear": true}}, nil
				}
				body := map[string]any{}
				if len(args) >= 1 {
					body["locale"] = args[0]
				}
				if len(args) >= 2 {
					body["timezone"] = args[1]
				}
				if opts.device != "" {
					body["timezone"] = opts.device
				}
				if len(args) > 2 {
					return request{}, fmt.Errorf("takes at most a locale and a timezone")
				}
				if len(body) == 0 {
					return request{}, fmt.Errorf("needs a locale, a timezone, or --clear")
				}
				return request{Body: body}, nil
			},
			render: renderJSON,
		},
		{
			name:    "init-script",
			usage:   "add|list|remove ...",
			summary: "install JS at document-start (and immediately); origin-scoped; never echoes source",
			method:  http.MethodPost,
			path:    "/api/page/init_script",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.domain, "origin", "", "only run on this origin")
			},
			build: func(opts *options, args []string) (request, error) {
				if len(args) == 0 {
					return request{}, fmt.Errorf("needs add, list, or remove")
				}
				action := args[0]
				rest := args[1:]
				body := map[string]any{"action": action}
				switch action {
				case "list":
					if err := noArgs(rest); err != nil {
						return request{}, err
					}
				case "remove":
					id, err := oneArg(rest, "script id")
					if err != nil {
						return request{}, err
					}
					body["id"] = id
				case "add":
					source, err := oneArg(rest, "javascript source")
					if err != nil {
						return request{}, err
					}
					body["source"] = source
					if opts.domain != "" {
						body["origin"] = opts.domain
					}
				default:
					return request{}, fmt.Errorf("unknown init-script action %q", action)
				}
				return request{Body: body}, nil
			},
			render: renderJSON,
		},
		{
			name:    "batch",
			usage:   "<json-or-file>",
			summary: "run many steps in one round trip (JSON array of {action,...} or a file containing one)",
			method:  http.MethodPost,
			path:    "/api/page/batch",
			build: func(_ *options, args []string) (request, error) {
				raw, err := oneArg(args, "json array or file path")
				if err != nil {
					return request{}, err
				}
				payload := strings.TrimSpace(raw)
				if !strings.HasPrefix(payload, "[") {
					data, err := os.ReadFile(payload)
					if err != nil {
						return request{}, fmt.Errorf("read batch file: %w", err)
					}
					payload = string(data)
				}
				var steps []browser.BatchStep
				if err := json.Unmarshal([]byte(payload), &steps); err != nil {
					return request{}, fmt.Errorf("batch steps: %w", err)
				}
				if len(steps) == 0 {
					return request{}, fmt.Errorf("batch needs at least one step")
				}
				return request{Body: map[string]any{"steps": steps}}, nil
			},
			render: renderJSON,
		},
	}
}
