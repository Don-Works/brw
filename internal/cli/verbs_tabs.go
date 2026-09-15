package cli

import (
	"flag"
	"fmt"
	"net/http"
)

func tabVerbs() []verb {
	return []verb{
		{
			name:    "tab close",
			usage:   "<tab-id>",
			summary: "close a tab this session opened",
			method:  http.MethodPost,
			path:    "/api/browser/close",
			build: func(_ *options, args []string) (request, error) {
				id, err := oneArg(args, "tab id")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"tab_id": id}}, nil
			},
			render: renderAction,
		},
		{
			name:    "tab focus",
			usage:   "<tab-id>",
			summary: "make this tab the session's default target (does not raise the OS window)",
			method:  http.MethodPost,
			path:    "/api/browser/focus",
			build: func(_ *options, args []string) (request, error) {
				id, err := oneArg(args, "tab id")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"tab_id": id}}, nil
			},
			render: renderAction,
		},
		{
			name:    "tab groups",
			summary: "list Chrome tab groups",
			method:  http.MethodGet,
			path:    "/api/browser/tab_groups",
			build: func(_ *options, args []string) (request, error) {
				return request{}, noArgs(args)
			},
			render: renderJSON,
		},
		{
			name:    "group",
			usage:   "<tab-id>...",
			summary: "put tabs into a Chrome tab group",
			method:  http.MethodPost,
			path:    "/api/browser/group_tabs",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.label, "name", "", "group title")
				fs.StringVar(&opts.color, "color", "", "group colour")
			},
			build: func(opts *options, args []string) (request, error) {
				if len(args) == 0 {
					return request{}, fmt.Errorf("needs at least one tab id")
				}
				body := map[string]any{"tab_ids": args}
				if opts.label != "" {
					body["name"] = opts.label
				}
				if opts.color != "" {
					body["color"] = opts.color
				}
				return request{Body: body}, nil
			},
			render: renderAction,
		},
		{
			name:    "ungroup",
			usage:   "<tab-id>...",
			summary: "remove tabs from their Chrome tab group",
			method:  http.MethodPost,
			path:    "/api/browser/ungroup_tabs",
			build: func(_ *options, args []string) (request, error) {
				if len(args) == 0 {
					return request{}, fmt.Errorf("needs at least one tab id")
				}
				return request{Body: map[string]any{"tab_ids": args}}, nil
			},
			render: renderAction,
		},
		{
			name:    "incognito",
			usage:   "<url>",
			summary: "open a URL in a fresh isolated context (direct-CDP only)",
			method:  http.MethodPost,
			path:    "/api/browser/open_incognito",
			build: func(_ *options, args []string) (request, error) {
				target, err := oneArg(args, "url")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"url": target}}, nil
			},
			render: renderOpen,
		},
		{
			name:    "close-context",
			usage:   "<context-id>",
			summary: "dispose an incognito context and every tab in it",
			method:  http.MethodPost,
			path:    "/api/browser/close_context",
			build: func(_ *options, args []string) (request, error) {
				id, err := oneArg(args, "context id")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"context_id": id}}, nil
			},
			render: renderAction,
		},
		{
			name:    "emulate",
			usage:   "[device]",
			summary: "apply DevTools device emulation, or --clear to drop it",
			method:  http.MethodPost,
			path:    "/api/browser/emulate_device",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.BoolVar(&opts.clear, "clear", false, "clear device emulation")
				fs.IntVar(&opts.width, "width", 0, "layout viewport width")
				fs.IntVar(&opts.height, "height", 0, "layout viewport height")
			},
			build: func(opts *options, args []string) (request, error) {
				if opts.clear {
					if err := noArgs(args); err != nil {
						return request{}, err
					}
					return request{Body: map[string]any{"clear": true}}, nil
				}
				body := map[string]any{}
				if len(args) == 1 {
					body["device"] = args[0]
				} else if len(args) != 0 {
					return request{}, fmt.Errorf("takes at most one device name")
				}
				if opts.width > 0 {
					body["width"] = opts.width
				}
				if opts.height > 0 {
					body["height"] = opts.height
				}
				if len(body) == 0 {
					return request{}, fmt.Errorf("needs a device name, --width/--height, or --clear")
				}
				return request{Body: body}, nil
			},
			render: renderJSON,
		},
	}
}
