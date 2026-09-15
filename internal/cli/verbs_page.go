package cli

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// pageVerbs are the everyday page actions the HTTP API already serves. They
// exist so a shell can drive the same surface an MCP client does, without
// inventing a second protocol.
func pageVerbs() []verb {
	return []verb{
		navVerb("goto", "<url>", "navigate the current tab to a URL", "/api/page/navigate_to", func(arg string) map[string]any {
			return map[string]any{"url": arg}
		}, "url"),
		navDir("back", "go back in this tab's history", "back"),
		navDir("forward", "go forward in this tab's history", "forward"),
		navDir("reload", "reload the current tab", "reload"),
		refVerb("hover", "hover the element at a ref", "/api/page/hover"),
		{
			name:    "select",
			usage:   "@<ref> <value>",
			summary: "choose a dropdown option by value or visible label",
			method:  http.MethodPost,
			path:    "/api/page/select",
			build: func(_ *options, args []string) (request, error) {
				ref, value, err := twoArgs(args, "ref", "value")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"ref": normalizeRef(ref), "value": value}}, nil
			},
			render: renderAction,
		},
		{
			name:    "scroll",
			usage:   "<up|down|left|right>",
			summary: "scroll the page",
			method:  http.MethodPost,
			path:    "/api/page/scroll",
			build: func(_ *options, args []string) (request, error) {
				direction, err := oneArg(args, "direction")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"direction": direction}}, nil
			},
			render: renderAction,
		},
		{
			name:    "dblclick",
			usage:   "@<ref>",
			summary: "double-click the element at a ref",
			method:  http.MethodPost,
			path:    "/api/page/click",
			build: func(_ *options, args []string) (request, error) {
				ref, err := refArg(args)
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"click_count": 2, "ref": ref}}, nil
			},
			render: renderAction,
		},
		{
			name:    "upload",
			usage:   "@<ref> <path>",
			summary: "set a file input from a path on the browser host",
			method:  http.MethodPost,
			path:    "/api/page/upload_file",
			build: func(_ *options, args []string) (request, error) {
				ref, path, err := twoArgs(args, "ref", "path")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"path": path, "ref": normalizeRef(ref)}}, nil
			},
			render: renderAction,
		},
		{
			name:    "drag",
			usage:   "@<from> @<to>",
			summary: "drag from one ref onto another",
			method:  http.MethodPost,
			path:    "/api/page/drag",
			build: func(_ *options, args []string) (request, error) {
				from, to, err := twoArgs(args, "from ref", "to ref")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{
					"from": map[string]any{"ref": normalizeRef(from)},
					"to":   map[string]any{"ref": normalizeRef(to)},
				}}, nil
			},
			render: renderAction,
		},
		keyVerb("keydown", "hold a key down (release with keyup)", "/api/page/key_down"),
		keyVerb("keyup", "release a held key", "/api/page/key_up"),
		refVerb("commit", "commit the focused field (Enter on the control)", "/api/page/commit"),
		refVerb("focus", "focus an element without clicking it", "/api/page/focus"),
		{
			name:    "clickxy",
			usage:   "<x> <y>",
			summary: "click at CSS pixel coordinates in the viewport",
			method:  http.MethodPost,
			path:    "/api/page/click_xy",
			build: func(_ *options, args []string) (request, error) {
				xs, ys, err := twoArgs(args, "x", "y")
				if err != nil {
					return request{}, err
				}
				x, err := strconv.ParseFloat(xs, 64)
				if err != nil {
					return request{}, fmt.Errorf("x: %w", err)
				}
				y, err := strconv.ParseFloat(ys, 64)
				if err != nil {
					return request{}, fmt.Errorf("y: %w", err)
				}
				return request{Body: map[string]any{"x": x, "y": y}}, nil
			},
			render: renderAction,
		},
		{
			name:          "assert visible",
			usage:         "@<ref>",
			summary:       "wait until the ref is visible",
			method:        http.MethodPost,
			path:          "/api/page/assert_visible",
			serverTimeout: true,
			build:         assertRefBody,
			render:        renderAction,
		},
		{
			name:          "assert hidden",
			usage:         "@<ref>",
			summary:       "wait until the ref is hidden",
			method:        http.MethodPost,
			path:          "/api/page/assert_hidden",
			serverTimeout: true,
			build:         assertRefBody,
			render:        renderAction,
		},
		{
			name:          "assert text",
			usage:         "@<ref> <text>",
			summary:       "wait until the ref contains this text",
			method:        http.MethodPost,
			path:          "/api/page/assert_text",
			serverTimeout: true,
			build: func(opts *options, args []string) (request, error) {
				ref, text, err := twoArgs(args, "ref", "text")
				if err != nil {
					return request{}, err
				}
				body := map[string]any{"ref": normalizeRef(ref), "text": text}
				if opts.timeoutSet {
					body["timeout_ms"] = opts.timeout.Milliseconds()
				}
				return request{Body: body}, nil
			},
			render: renderAction,
		},
		{
			name:          "assert value",
			usage:         "@<ref> <value>",
			summary:       "wait until the field holds this value",
			method:        http.MethodPost,
			path:          "/api/page/assert_value",
			serverTimeout: true,
			build: func(opts *options, args []string) (request, error) {
				ref, value, err := twoArgs(args, "ref", "value")
				if err != nil {
					return request{}, err
				}
				body := map[string]any{"ref": normalizeRef(ref), "value": value}
				if opts.timeoutSet {
					body["timeout_ms"] = opts.timeout.Milliseconds()
				}
				return request{Body: body}, nil
			},
			render: renderAction,
		},
		{
			name:    "pushstate",
			usage:   "<path>",
			summary: "change the URL through the History API with no reload",
			method:  http.MethodPost,
			path:    "/api/page/pushstate",
			build: func(_ *options, args []string) (request, error) {
				target, err := oneArg(args, "path")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"url": target}}, nil
			},
			render: renderAction,
		},
		{
			name:    "frame",
			usage:   "<ref|main>",
			summary: "scope later snapshot/find/get calls to one iframe, or back to main",
			method:  http.MethodPost,
			path:    "/api/page/frame",
			build: func(_ *options, args []string) (request, error) {
				target, err := oneArg(args, "frame")
				if err != nil {
					return request{}, err
				}
				if strings.EqualFold(target, "main") {
					return request{Body: map[string]any{"target": "main"}}, nil
				}
				return request{Body: map[string]any{"target": normalizeRef(target)}}, nil
			},
			render: renderAction,
		},
	}
}

func navDir(name, summary, direction string) verb {
	return verb{
		name:    name,
		summary: summary,
		method:  http.MethodPost,
		path:    "/api/page/navigate",
		build: func(_ *options, args []string) (request, error) {
			if err := noArgs(args); err != nil {
				return request{}, err
			}
			return request{Body: map[string]any{"direction": direction}}, nil
		},
		render: renderAction,
	}
}

func navVerb(name, usage, summary, path string, body func(string) map[string]any, argName string) verb {
	return verb{
		name:    name,
		usage:   usage,
		summary: summary,
		method:  http.MethodPost,
		path:    path,
		build: func(_ *options, args []string) (request, error) {
			value, err := oneArg(args, argName)
			if err != nil {
				return request{}, err
			}
			return request{Body: body(value)}, nil
		},
		render: renderAction,
	}
}

func refVerb(name, summary, path string) verb {
	return verb{
		name:    name,
		usage:   "@<ref>",
		summary: summary,
		method:  http.MethodPost,
		path:    path,
		build: func(_ *options, args []string) (request, error) {
			ref, err := refArg(args)
			if err != nil {
				return request{}, err
			}
			return request{Body: map[string]any{"ref": ref}}, nil
		},
		render: renderAction,
	}
}

func keyVerb(name, summary, path string) verb {
	return verb{
		name:    name,
		usage:   "<key>",
		summary: summary,
		method:  http.MethodPost,
		path:    path,
		build: func(_ *options, args []string) (request, error) {
			key, err := oneArg(args, "key")
			if err != nil {
				return request{}, err
			}
			return request{Body: map[string]any{"key": key}}, nil
		},
		render: renderAction,
	}
}

func assertRefBody(opts *options, args []string) (request, error) {
	ref, err := refArg(args)
	if err != nil {
		return request{}, err
	}
	body := map[string]any{"ref": ref}
	if opts.timeoutSet {
		body["timeout_ms"] = opts.timeout.Milliseconds()
	}
	return request{Body: body}, nil
}
