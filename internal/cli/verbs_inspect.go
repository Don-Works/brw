package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

func inspectVerbs() []verb {
	return []verb{
		{
			name:    "get",
			usage:   "<what> [target] [name]",
			summary: "read one typed fact (url, title, text, value, attr, count, visible, …)",
			method:  http.MethodGet,
			path:    "/api/page/get",
			build: func(_ *options, args []string) (request, error) {
				if len(args) == 0 {
					return request{}, fmt.Errorf("needs what to read: one of %s", strings.Join(snapshot.GetKindNames(), ", "))
				}
				values := url.Values{}
				values.Set("what", args[0])
				if len(args) > 1 {
					values.Set("target", normalizeRef(args[1]))
				}
				if len(args) > 2 {
					values.Set("name", args[2])
				}
				if len(args) > 3 {
					return request{}, fmt.Errorf("get takes what, an optional target, and an optional attribute name")
				}
				return request{Query: values}, nil
			},
			render: renderJSON,
		},
		{
			name:    "eval",
			usage:   "<js>",
			summary: "run JavaScript in the page and print the JSON result",
			method:  http.MethodPost,
			path:    "/api/page/evaluate",
			build: func(_ *options, args []string) (request, error) {
				expr, err := oneArg(args, "expression")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"expression": expr}}, nil
			},
			render: renderJSON,
		},
		{
			name:    "console",
			summary: "print buffered page console messages",
			method:  http.MethodGet,
			path:    "/api/page/console",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.BoolVar(&opts.onlyErrors, "errors", false, "only error-severity messages")
				fs.StringVar(&opts.level, "level", "", "only this console level")
				fs.StringVar(&opts.pattern, "pattern", "", "regexp against the message text")
				fs.IntVar(&opts.limit, "limit", 0, "return at most this many messages")
			},
			build: func(opts *options, args []string) (request, error) {
				if err := noArgs(args); err != nil {
					return request{}, err
				}
				values := url.Values{}
				setBool(values, "only_errors", opts.onlyErrors)
				setString(values, "level", opts.level)
				setString(values, "pattern", opts.pattern)
				setInt(values, "limit", opts.limit)
				return request{Query: values}, nil
			},
			render: renderConsole,
		},
		{
			name:    "observe",
			summary: "print a cheap page-change observation (url, title, focus, diffs)",
			method:  http.MethodGet,
			path:    "/api/page/observe",
			build: func(_ *options, args []string) (request, error) {
				return request{}, noArgs(args)
			},
			render: renderJSON,
		},
		{
			name:    "cookies",
			usage:   "[list|set|delete]",
			summary: "list, set or delete cookies (direct-CDP only)",
			method:  http.MethodPost,
			path:    "/api/page/cookies",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.domain, "domain", "", "cookie domain")
				fs.StringVar(&opts.cookiePath, "path", "", "cookie path")
				fs.BoolVar(&opts.secure, "secure", false, "Secure attribute")
				fs.BoolVar(&opts.httpOnly, "http-only", false, "HttpOnly attribute")
				fs.StringVar(&opts.sameSite, "same-site", "", "SameSite attribute")
			},
			build: func(opts *options, args []string) (request, error) {
				action := "list"
				if len(args) > 0 {
					action = args[0]
					args = args[1:]
				}
				body := map[string]any{"action": action}
				switch action {
				case "list":
					if err := noArgs(args); err != nil {
						return request{}, err
					}
				case "set":
					name, value, err := twoArgs(args, "name", "value")
					if err != nil {
						return request{}, err
					}
					body["name"] = name
					body["value"] = value
					if opts.domain != "" {
						body["domain"] = opts.domain
					}
					if opts.cookiePath != "" {
						body["path"] = opts.cookiePath
					}
					if opts.secure {
						body["secure"] = true
					}
					if opts.httpOnly {
						body["http_only"] = true
					}
					if opts.sameSite != "" {
						body["same_site"] = opts.sameSite
					}
				case "delete":
					name, err := oneArg(args, "cookie name")
					if err != nil {
						return request{}, err
					}
					body["name"] = name
				default:
					return request{}, fmt.Errorf("unknown cookies action %q (list, set, delete)", action)
				}
				return request{Body: body}, nil
			},
			render: renderCookies,
		},
		{
			name:    "network",
			summary: "list recent network requests",
			method:  http.MethodGet,
			path:    "/api/page/network_requests",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.filter, "filter", "", "substring filter")
			},
			build: func(opts *options, args []string) (request, error) {
				if err := noArgs(args); err != nil {
					return request{}, err
				}
				values := url.Values{}
				setString(values, "filter", opts.filter)
				return request{Query: values}, nil
			},
			render: renderJSON,
		},
		{
			name:    "network capture",
			summary: "install or drain the in-page fetch/XHR interceptor",
			method:  http.MethodPost,
			path:    "/api/page/network_capture",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.filter, "filter", "", "substring filter")
			},
			build: func(opts *options, args []string) (request, error) {
				if err := noArgs(args); err != nil {
					return request{}, err
				}
				body := map[string]any{}
				if opts.filter != "" {
					body["filter"] = opts.filter
				}
				return request{Body: body}, nil
			},
			render: renderJSON,
		},
		{
			name:    "clipboard read",
			summary: "read the system clipboard (direct-CDP only)",
			method:  http.MethodPost,
			path:    "/api/page/clipboard",
			build: func(_ *options, args []string) (request, error) {
				if err := noArgs(args); err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"action": "read"}}, nil
			},
			render: renderJSON,
		},
		{
			name:    "clipboard write",
			usage:   "<text>",
			summary: "write text to the system clipboard (direct-CDP only)",
			method:  http.MethodPost,
			path:    "/api/page/clipboard",
			build: func(_ *options, args []string) (request, error) {
				text, err := oneArg(args, "text")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"action": "write", "text": text}}, nil
			},
			render: renderJSON,
		},
		{
			name:    "screenshot element",
			usage:   "@<ref> [--out file.png]",
			summary: "capture one element as a PNG",
			method:  http.MethodGet,
			path:    "/api/visual/screenshot_element",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.out, "out", "element.png", "write the PNG here")
			},
			build: func(_ *options, args []string) (request, error) {
				ref, err := refArg(args)
				if err != nil {
					return request{}, err
				}
				return request{Query: url.Values{"ref": []string{ref}, "base64": []string{"1"}}}, nil
			},
			render: renderScreenshot,
		},
		{
			name:    "artifact capture",
			usage:   "<kind>",
			summary: "store a page capture as an artifact handle (text, screenshot, pdf, download, video, har, semantic_json)",
			method:  http.MethodPost,
			path:    "/api/artifacts/capture",
			build: func(_ *options, args []string) (request, error) {
				kind, err := oneArg(args, "kind")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"kind": kind}}, nil
			},
			render: renderJSON,
		},
		{
			name:      "artifact info",
			usage:     "<artifact-id>",
			summary:   "print metadata for one artifact",
			method:    http.MethodPost,
			path:      "/api/artifacts/info",
			exactBody: true,
			build: func(_ *options, args []string) (request, error) {
				id, err := oneArg(args, "artifact-id")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"artifact_id": id}}, nil
			},
			render: renderJSON,
		},
		{
			name:      "artifact search",
			usage:     "<artifact-id> <query>",
			summary:   "search inside a text artifact",
			method:    http.MethodPost,
			path:      "/api/artifacts/search",
			exactBody: true,
			build: func(opts *options, args []string) (request, error) {
				id, query, err := twoArgs(args, "artifact-id", "query")
				if err != nil {
					return request{}, err
				}
				body := map[string]any{"artifact_id": id, "query": query}
				if opts.limit > 0 {
					body["limit"] = opts.limit
				}
				return request{Body: body}, nil
			},
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.IntVar(&opts.limit, "limit", 0, "return at most this many hits")
			},
			render: renderJSON,
		},
		{
			name:      "artifact delete",
			usage:     "<artifact-id>",
			summary:   "delete an artifact",
			method:    http.MethodPost,
			path:      "/api/artifacts/delete",
			exactBody: true,
			build: func(_ *options, args []string) (request, error) {
				id, err := oneArg(args, "artifact-id")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"artifact_id": id}}, nil
			},
			render: renderJSON,
		},
		{
			name:      "recipe search",
			usage:     "<query>",
			summary:   "search private recipes by intent (metadata only)",
			method:    http.MethodPost,
			path:      "/api/recipes/search",
			exactBody: true,
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.domain, "origin", "", "restrict to this origin")
				fs.IntVar(&opts.limit, "limit", 0, "return at most this many matches")
			},
			build: func(opts *options, args []string) (request, error) {
				query, err := oneArg(args, "query")
				if err != nil {
					return request{}, err
				}
				body := map[string]any{"query": query}
				if opts.domain != "" {
					body["origin"] = opts.domain
				}
				if opts.limit > 0 {
					body["limit"] = opts.limit
				}
				return request{Body: body}, nil
			},
			render: renderJSON,
		},
		{
			name:    "notify",
			usage:   "<title> <message>",
			summary: "show a desktop notification (needs_input, done, or error)",
			method:  http.MethodPost,
			path:    "/api/page/notify",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.kind, "kind", "done", "needs_input, done, or error")
			},
			build: func(opts *options, args []string) (request, error) {
				title, message, err := twoArgs(args, "title", "message")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"kind": opts.kind, "message": message, "title": title}}, nil
			},
			render: renderAction,
		},
	}
}

func renderJSON(w io.Writer, _ *options, body []byte) error {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s\n", encoded)
	return nil
}

func renderConsole(w io.Writer, _ *options, body []byte) error {
	var messages []browser.ConsoleMessage
	if err := json.Unmarshal(body, &messages); err != nil {
		return err
	}
	if len(messages) == 0 {
		fmt.Fprintln(w, "no console messages")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, msg := range messages {
		fmt.Fprintf(tw, "%s\t%s\n", msg.Level, msg.Text)
	}
	return tw.Flush()
}

func renderCookies(w io.Writer, _ *options, body []byte) error {
	var result browser.CookieResult
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s  %d cookie(s)", result.Action, result.Count)
	if result.URL != "" {
		fmt.Fprintf(w, "  %s", result.URL)
	}
	fmt.Fprintln(w)
	if len(result.Cookies) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, cookie := range result.Cookies {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", cookie.Name, cookie.Domain, cookie.Path)
	}
	return tw.Flush()
}
