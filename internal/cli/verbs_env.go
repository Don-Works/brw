package cli

import (
	"encoding/json"
	"errors"
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
			summary: "override the tab's locale and time zone, or --clear to restore the browser's",
			method:  http.MethodPost,
			path:    "/api/page/locale",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.BoolVar(&opts.clear, "clear", false, "remove the override")
				fs.StringVar(&opts.timezone, "timezone", "", "IANA time zone, for example Europe/London")
			},
			build: func(opts *options, args []string) (request, error) {
				if opts.clear {
					if len(args) != 0 || opts.timezone != "" {
						return request{}, errors.New("--clear takes no locale or time zone")
					}
					return request{Body: map[string]any{"clear": true}}, nil
				}
				if len(args) > 2 {
					return request{}, errors.New("takes at most a locale and a time zone")
				}
				body := map[string]any{}
				if len(args) > 0 {
					body["locale"] = args[0]
				}
				if len(args) > 1 {
					body["timezone"] = args[1]
				}
				if opts.timezone != "" {
					if len(args) > 1 {
						return request{}, errors.New("give the time zone once, as an argument or as --timezone")
					}
					body["timezone"] = opts.timezone
				}
				if len(body) == 0 {
					return request{}, errors.New("needs a locale, a time zone, or --clear")
				}
				return request{Body: body}, nil
			},
			render: renderJSON,
		},
		{
			name:    "init-script",
			usage:   "<add <js> | list | remove <id> | clear>",
			summary: "run JavaScript before every document the tab loads; list never prints the source",
			method:  http.MethodPost,
			path:    "/api/page/init_script",
			build: func(_ *options, args []string) (request, error) {
				if len(args) == 0 {
					return request{}, errors.New("needs add, list, remove or clear")
				}
				action, rest := args[0], args[1:]
				body := map[string]any{"action": action}
				switch action {
				case "list", "clear":
					if err := noArgs(rest); err != nil {
						return request{}, err
					}
				case "add":
					source, err := oneArg(rest, "JavaScript source")
					if err != nil {
						return request{}, err
					}
					body["source"] = source
				case "remove":
					id, err := oneArg(rest, "script id")
					if err != nil {
						return request{}, err
					}
					body["id"] = id
				default:
					return request{}, fmt.Errorf("unknown init-script action %q: want add, list, remove or clear", action)
				}
				return request{Body: body}, nil
			},
			render: renderJSON,
		},
		{
			name:    "batch",
			usage:   "<json-array|file>",
			summary: "run several steps in one round trip: a JSON array of {action,...} or a file holding one",
			method:  http.MethodPost,
			path:    "/api/page/batch",
			build: func(_ *options, args []string) (request, error) {
				raw, err := oneArg(args, "JSON array or file")
				if err != nil {
					return request{}, err
				}
				payload := []byte(strings.TrimSpace(raw))
				if len(payload) == 0 || payload[0] != '[' {
					if payload, err = os.ReadFile(raw); err != nil {
						return request{}, fmt.Errorf("read batch file: %w", err)
					}
				}
				var steps []browser.BatchStep
				if err := json.Unmarshal(payload, &steps); err != nil {
					return request{}, fmt.Errorf("batch steps: %w", err)
				}
				if len(steps) == 0 {
					return request{}, errors.New("batch needs at least one step")
				}
				return request{Body: map[string]any{"steps": json.RawMessage(payload)}}, nil
			},
			render: renderJSON,
		},
	}
}
