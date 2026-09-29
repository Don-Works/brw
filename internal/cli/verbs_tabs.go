package cli

import (
	"errors"
	"flag"
	"net/http"
)

func tabVerbs() []verb {
	return []verb{
		tabIDVerb("tab close", "close a tab", "/api/browser/close"),
		tabIDVerb("tab focus", "make a tab the default target of later verbs (does not raise the window)", "/api/browser/focus"),
		{
			name:    "tab groups",
			summary: "list Chrome tab groups (extension bridge only)",
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
			summary: "put tabs into a Chrome tab group (extension bridge only)",
			method:  http.MethodPost,
			path:    "/api/browser/group_tabs",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.label, "name", "", "group title")
				fs.StringVar(&opts.color, "color", "", "group colour")
			},
			build: func(opts *options, args []string) (request, error) {
				if len(args) == 0 {
					return request{}, errors.New("needs at least one tab id")
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
			summary: "take tabs out of their Chrome tab group (extension bridge only)",
			method:  http.MethodPost,
			path:    "/api/browser/ungroup_tabs",
			build: func(_ *options, args []string) (request, error) {
				if len(args) == 0 {
					return request{}, errors.New("needs at least one tab id")
				}
				return request{Body: map[string]any{"tab_ids": args}}, nil
			},
			render: renderAction,
		},
		{
			name:    "incognito",
			usage:   "<url>",
			summary: "open a URL in a fresh isolated browser context (not on the extension bridge)",
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
			summary: "dispose of an incognito context and every tab in it",
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
			summary: "emulate a named device or a viewport size, or --clear to stop",
			method:  http.MethodPost,
			path:    "/api/browser/emulate_device",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.BoolVar(&opts.clear, "clear", false, "stop emulating")
				fs.IntVar(&opts.width, "width", 0, "viewport width in CSS pixels")
				fs.IntVar(&opts.height, "height", 0, "viewport height in CSS pixels")
			},
			build: func(opts *options, args []string) (request, error) {
				device, err := atMostOneArg(args, "device name")
				if err != nil {
					return request{}, err
				}
				if opts.clear {
					if device != "" || opts.width > 0 || opts.height > 0 {
						return request{}, errors.New("--clear takes no device or size")
					}
					return request{Body: map[string]any{"clear": true}}, nil
				}
				body := map[string]any{}
				if device != "" {
					body["device"] = device
				}
				if opts.width > 0 {
					body["width"] = opts.width
				}
				if opts.height > 0 {
					body["height"] = opts.height
				}
				if len(body) == 0 {
					return request{}, errors.New("needs a device name, --width and --height, or --clear")
				}
				return request{Body: body}, nil
			},
			render: renderJSON,
		},
	}
}

// tabIDVerb sends the tab as "id": the generic path folds --tab into tab_id,
// which the handler prefers, so the positional must not share that field.
func tabIDVerb(name, summary, path string) verb {
	return verb{
		name:    name,
		usage:   "<tab-id>",
		summary: summary,
		method:  http.MethodPost,
		path:    path,
		build: func(_ *options, args []string) (request, error) {
			id, err := oneArg(args, "tab id")
			if err != nil {
				return request{}, err
			}
			return request{Body: map[string]any{"id": id}}, nil
		},
		render: renderAction,
	}
}
