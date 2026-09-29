package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func pageVerbs() []verb {
	return []verb{
		{
			name:    "goto",
			usage:   "<url>",
			summary: "navigate the current tab to a URL",
			method:  http.MethodPost,
			path:    "/api/page/navigate_to",
			build: func(_ *options, args []string) (request, error) {
				target, err := oneArg(args, "url")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"url": target}}, nil
			},
			render: renderAction,
		},
		historyVerb("back", "go back in this tab's history"),
		historyVerb("forward", "go forward in this tab's history"),
		historyVerb("reload", "reload the current tab"),
		refVerb("hover", "hover the element at a ref", "/api/page/hover"),
		refVerb("focus", "focus an element without clicking it", "/api/page/focus"),
		refVerb("commit", "commit a field the way Enter or leaving it does", "/api/page/commit"),
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
				return request{Body: map[string]any{"ref": ref, "click_count": 2}}, nil
			},
			render: renderAction,
		},
		{
			name:    "select",
			usage:   "@<ref> <value>",
			summary: "choose a dropdown option by value or visible label",
			method:  http.MethodPost,
			path:    "/api/page/select",
			build: func(_ *options, args []string) (request, error) {
				ref, value, err := refAndValue(args, "value")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"ref": ref, "value": value}}, nil
			},
			render: renderAction,
		},
		{
			name:    "scroll",
			usage:   "<up|down|left|right|@ref>",
			summary: "scroll the page one screen, or bring the element at a ref into view",
			method:  http.MethodPost,
			path:    "/api/page/scroll",
			build: func(_ *options, args []string) (request, error) {
				arg, err := oneArg(args, "direction or ref")
				if err != nil {
					return request{}, err
				}
				switch strings.ToLower(arg) {
				case "up", "down", "left", "right":
					return request{Body: map[string]any{"direction": strings.ToLower(arg)}}, nil
				}
				if !strings.HasPrefix(arg, "@") {
					return request{}, fmt.Errorf("%q is neither up, down, left, right nor a ref such as @e17", arg)
				}
				return request{Body: map[string]any{"ref": normalizeRef(arg)}}, nil
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
				ref, path, err := refAndValue(args, "path")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"ref": ref, "path": path}}, nil
			},
			render: renderAction,
		},
		{
			name:    "drag",
			usage:   "@<from> @<to>",
			summary: "drag the element at one ref onto another",
			method:  http.MethodPost,
			path:    "/api/page/drag",
			build: func(_ *options, args []string) (request, error) {
				if len(args) != 2 {
					return request{}, errors.New("needs a from ref and a to ref")
				}
				from, to := normalizeRef(args[0]), normalizeRef(args[1])
				if from == "" || to == "" {
					return request{}, errors.New("needs a from ref and a to ref")
				}
				return request{Body: map[string]any{
					"from": map[string]any{"ref": from},
					"to":   map[string]any{"ref": to},
				}}, nil
			},
			render: renderAction,
		},
		{
			name:    "clickxy",
			usage:   "<x> <y>",
			summary: "click at CSS pixel coordinates in the viewport",
			method:  http.MethodPost,
			path:    "/api/page/click_xy",
			build: func(_ *options, args []string) (request, error) {
				if len(args) != 2 {
					return request{}, errors.New("needs an x and a y")
				}
				x, err := strconv.ParseFloat(args[0], 64)
				if err != nil {
					return request{}, fmt.Errorf("x: %w", err)
				}
				y, err := strconv.ParseFloat(args[1], 64)
				if err != nil {
					return request{}, fmt.Errorf("y: %w", err)
				}
				return request{Body: map[string]any{"x": x, "y": y}}, nil
			},
			render: renderAction,
		},
		keyVerb("keydown", "hold a key down until keyup", "/api/page/key_down"),
		keyVerb("keyup", "release a held key", "/api/page/key_up"),
		assertVerb("assert visible", "", "wait until the element at a ref is visible", "/api/page/assert_visible"),
		assertVerb("assert hidden", "", "wait until the element at a ref is hidden", "/api/page/assert_hidden"),
		assertVerb("assert text", "text", "wait until the element at a ref contains this text", "/api/page/assert_text"),
		assertVerb("assert value", "value", "wait until the field at a ref holds this value", "/api/page/assert_value"),
		{
			name:    "pushstate",
			usage:   "<path>",
			summary: "change the URL through the History API without a reload",
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
			usage:   "<@ref|main>",
			summary: "scope later snapshot, find and get calls to one iframe, or back to main",
			method:  http.MethodPost,
			path:    "/api/page/frame",
			build: func(_ *options, args []string) (request, error) {
				target, err := oneArg(args, "frame ref or main")
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

func historyVerb(direction, summary string) verb {
	return verb{
		name:    direction,
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

// assertVerb is an assertion the daemon waits on, so --timeout is forwarded as
// its timeout_ms. field names the expected-value argument, or is empty.
func assertVerb(name, field, summary, path string) verb {
	usage := "@<ref>"
	if field != "" {
		usage += " <" + field + ">"
	}
	return verb{
		name:          name,
		usage:         usage,
		summary:       summary,
		method:        http.MethodPost,
		path:          path,
		serverTimeout: true,
		build: func(opts *options, args []string) (request, error) {
			var body map[string]any
			if field == "" {
				ref, err := refArg(args)
				if err != nil {
					return request{}, err
				}
				body = map[string]any{"ref": ref}
			} else {
				ref, value, err := refAndValue(args, field)
				if err != nil {
					return request{}, err
				}
				body = map[string]any{"ref": ref, field: value}
			}
			if opts.timeoutSet {
				body["timeout_ms"] = opts.timeout.Milliseconds()
			}
			return request{Body: body}, nil
		},
		render: renderAction,
	}
}

func refAndValue(args []string, name string) (string, string, error) {
	if len(args) != 2 {
		return "", "", fmt.Errorf("needs a ref and a %s", name)
	}
	ref := normalizeRef(args[0])
	if ref == "" {
		return "", "", errors.New("needs a ref, e.g. @e17")
	}
	return ref, args[1], nil
}
