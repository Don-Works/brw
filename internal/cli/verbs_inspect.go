package cli

import (
	"encoding/json"
	"errors"
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
			usage:   "<what> [@ref] [attribute]",
			summary: "read one typed fact: url, title, text, value, attr, count, visible and more",
			method:  http.MethodGet,
			path:    "/api/page/get",
			build: func(_ *options, args []string) (request, error) {
				if len(args) == 0 || len(args) > 3 {
					return request{}, fmt.Errorf("needs what to read (%s), then an optional target and attribute name", strings.Join(snapshot.GetKindNames(), ", "))
				}
				values := url.Values{"what": {args[0]}}
				if len(args) > 1 {
					values.Set("target", normalizeRef(args[1]))
				}
				if len(args) > 2 {
					values.Set("name", args[2])
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
			summary: "print and drain the page's buffered console messages",
			method:  http.MethodGet,
			path:    "/api/page/console",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.BoolVar(&opts.onlyErrors, "errors", false, "only error-level messages")
				fs.StringVar(&opts.level, "level", "", "only this console level")
				fs.StringVar(&opts.pattern, "pattern", "", "regular expression the message must match")
				fs.IntVar(&opts.limit, "limit", 0, "at most this many messages")
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
			summary: "report what changed on the page since the last observation",
			method:  http.MethodGet,
			path:    "/api/page/observe",
			build: func(_ *options, args []string) (request, error) {
				return request{}, noArgs(args)
			},
			render: renderJSON,
		},
		{
			name:    "cookies",
			usage:   "[list [url] | set <name> <value> | delete <name>]",
			summary: "list, set or delete the tab's cookies at the CDP level (not on the extension bridge)",
			method:  http.MethodPost,
			path:    "/api/page/cookies",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.domain, "domain", "", "cookie domain for set or delete")
				fs.StringVar(&opts.cookiePath, "path", "", "cookie path for set or delete")
				fs.BoolVar(&opts.secure, "secure", false, "set the Secure attribute")
				fs.BoolVar(&opts.httpOnly, "http-only", false, "set the HttpOnly attribute")
				fs.StringVar(&opts.sameSite, "same-site", "", "SameSite: strict, lax or none")
			},
			build:  buildCookies,
			render: renderCookies,
		},
		{
			name:    "network",
			summary: "list the page's recent network requests",
			method:  http.MethodGet,
			path:    "/api/page/network_requests",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.filter, "filter", "", "only URLs containing this")
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
			summary: "install the in-page fetch/XHR recorder, or drain what it recorded",
			method:  http.MethodPost,
			path:    "/api/page/network_capture",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.filter, "filter", "", "only URLs containing this")
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
			summary: "read the clipboard of the machine the browser runs on",
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
			summary: "write text to the clipboard of the machine the browser runs on",
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
			usage:   "@<ref> [--out <file.png>]",
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
				return request{Query: url.Values{"ref": {ref}, "base64": {"1"}}}, nil
			},
			render: renderScreenshot,
		},
		{
			name:      "artifact capture",
			usage:     "<kind>",
			summary:   "store a capture of the page as an artifact handle (text, screenshot, pdf, har, …)",
			method:    http.MethodPost,
			path:      "/api/artifacts/capture",
			exactBody: true,
			build: func(_ *options, args []string) (request, error) {
				kind, err := oneArg(args, "kind")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"kind": kind}}, nil
			},
			render: renderJSON,
		},
		artifactIDVerb("artifact info", "print one artifact's metadata", "/api/artifacts/info"),
		artifactIDVerb("artifact delete", "delete one artifact", "/api/artifacts/delete"),
		{
			name:      "artifact search",
			usage:     "<artifact-id> <query>",
			summary:   "search inside a text artifact",
			method:    http.MethodPost,
			path:      "/api/artifacts/search",
			exactBody: true,
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.IntVar(&opts.limit, "limit", 0, "at most this many hits")
			},
			build: func(opts *options, args []string) (request, error) {
				if len(args) != 2 {
					return request{}, errors.New("needs an artifact id and a query")
				}
				body := map[string]any{"artifact_id": args[0], "query": args[1]}
				if opts.limit > 0 {
					body["limit"] = opts.limit
				}
				return request{Body: body}, nil
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
				fs.StringVar(&opts.origin, "origin", "", "only recipes for this origin")
				fs.IntVar(&opts.limit, "limit", 0, "at most this many matches")
			},
			build: func(opts *options, args []string) (request, error) {
				query, err := oneArg(args, "query")
				if err != nil {
					return request{}, err
				}
				body := map[string]any{"query": query}
				if opts.origin != "" {
					body["origin"] = opts.origin
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
			summary: "show a desktop notification to the person at the browser",
			method:  http.MethodPost,
			path:    "/api/page/notify",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.kind, "kind", "done", "needs_input, done or error")
			},
			build: func(opts *options, args []string) (request, error) {
				if len(args) != 2 {
					return request{}, errors.New("needs a title and a message")
				}
				return request{Body: map[string]any{"kind": opts.kind, "title": args[0], "message": args[1]}}, nil
			},
			render: renderAction,
		},
	}
}

func buildCookies(opts *options, args []string) (request, error) {
	action := "list"
	if len(args) > 0 {
		action, args = args[0], args[1:]
	}
	body := map[string]any{"action": action}
	switch action {
	case "list":
		target, err := atMostOneArg(args, "url")
		if err != nil {
			return request{}, err
		}
		if target != "" {
			body["url"] = target
		}
		return request{Body: body}, nil
	case "set":
		if len(args) != 2 {
			return request{}, errors.New("set needs a name and a value")
		}
		body["name"], body["value"] = args[0], args[1]
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
		return request{}, fmt.Errorf("unknown cookies action %q: want list, set or delete", action)
	}
	if opts.domain != "" {
		body["domain"] = opts.domain
	}
	if opts.cookiePath != "" {
		body["path"] = opts.cookiePath
	}
	return request{Body: body}, nil
}

func artifactIDVerb(name, summary, path string) verb {
	return verb{
		name:      name,
		usage:     "<artifact-id>",
		summary:   summary,
		method:    http.MethodPost,
		path:      path,
		exactBody: true,
		build: func(_ *options, args []string) (request, error) {
			id, err := oneArg(args, "artifact id")
			if err != nil {
				return request{}, err
			}
			return request{Body: map[string]any{"artifact_id": id}}, nil
		},
		render: renderJSON,
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
	_, err = fmt.Fprintf(w, "%s\n", encoded)
	return err
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

// renderCookies prints names, domains and paths. Values are only in --json.
func renderCookies(w io.Writer, _ *options, body []byte) error {
	var result browser.CookieResult
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	count := result.Count
	if count == 0 {
		count = len(result.Cookies)
	}
	line := fmt.Sprintf("%s  %d cookie(s)", result.Action, count)
	if result.URL != "" {
		line += "  " + result.URL
	}
	fmt.Fprintln(w, line)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range result.Cookies {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, c.Domain, c.Path)
	}
	return tw.Flush()
}
