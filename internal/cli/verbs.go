package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/readability"
)

// verb is one CLI action bound to exactly one route the daemon already serves.
// method+path are the whole binding: build shapes only the query string and
// body, so no verb can invent a surface internal/http does not have. The route
// test walks this table against the daemon's own mux.
type verb struct {
	name    string // may be two words, e.g. "artifact read"
	usage   string
	summary string
	method  string
	path    string
	flags   func(fs *flag.FlagSet, opts *options)
	build   func(opts *options, args []string) (request, error)
	render  func(w io.Writer, opts *options, body []byte) error

	// exactBody marks a route whose handler decodes a fixed schema with
	// DisallowUnknownFields. Nothing may be added to the request the verb built
	// — a tab_id folded in from --tab turns the call into a 400.
	exactBody bool
	// serverTimeout marks a verb that forwards --timeout to the daemon, which
	// then enforces it. Only those need the client deadline to sit beyond it.
	serverTimeout bool
	// unsupported reads a capability gap out of a 200 answer, for the routes
	// that report one with a flag rather than an error. Without it a script
	// branching on the exit code reads "this transport cannot" as success.
	unsupported func(body []byte) (string, bool)
}

func verbs() []verb {
	table := []verb{
		{
			name:    "open",
			usage:   "<url>",
			summary: "open a URL in a new tab",
			method:  http.MethodPost,
			path:    "/api/browser/open",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.group, "group", "", "put the new tab in this tab group")
			},
			build: func(opts *options, args []string) (request, error) {
				target, err := oneArg(args, "url")
				if err != nil {
					return request{}, err
				}
				body := map[string]any{"url": target}
				if opts.group != "" {
					body["group"] = opts.group
				}
				return request{Body: body}, nil
			},
			render: renderOpen,
		},
		{
			name:    "find",
			usage:   "[query]",
			summary: "find interactive elements and print their refs",
			method:  http.MethodGet,
			path:    "/api/page/find",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.text, "text", "", "match elements containing this text")
				fs.StringVar(&opts.role, "role", "", "match this accessibility role (button, link, textbox…)")
				fs.IntVar(&opts.limit, "limit", 0, "return at most this many elements")
				fs.BoolVar(&opts.viewportOnly, "viewport-only", false, "only elements currently in the viewport")
			},
			build: func(opts *options, args []string) (request, error) {
				query, err := atMostOneArg(args, "query")
				if err != nil {
					return request{}, err
				}
				values := url.Values{}
				setString(values, "query", query)
				setString(values, "text", opts.text)
				setString(values, "role", opts.role)
				setInt(values, "limit", opts.limit)
				setBool(values, "viewport_only", opts.viewportOnly)
				return request{Query: values}, nil
			},
			render: renderElementList,
		},
		{
			name:    "click",
			usage:   "@<ref>",
			summary: "click the element at a ref from find or snapshot",
			method:  http.MethodPost,
			path:    "/api/page/click",
			build: func(_ *options, args []string) (request, error) {
				ref, err := refArg(args)
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"ref": ref}}, nil
			},
			render: renderAction,
		},
		{
			name:    "fill",
			usage:   "@<ref> <text>",
			summary: "fill a field, replacing what it holds",
			method:  http.MethodPost,
			path:    "/api/page/fill",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.BoolVar(&opts.appendText, "append", false, "append to the field instead of replacing it")
			},
			build: func(opts *options, args []string) (request, error) {
				if len(args) != 2 {
					return request{}, errors.New("needs a ref and the text to fill")
				}
				return request{Body: map[string]any{
					"ref":     normalizeRef(args[0]),
					"text":    args[1],
					"replace": !opts.appendText,
				}}, nil
			},
			render: renderAction,
		},
		{
			name:    "type",
			usage:   "@<ref> <text>",
			summary: "type text into a field keystroke by keystroke",
			method:  http.MethodPost,
			path:    "/api/page/type",
			build: func(_ *options, args []string) (request, error) {
				if len(args) != 2 {
					return request{}, errors.New("needs a ref and the text to type")
				}
				return request{Body: map[string]any{"ref": normalizeRef(args[0]), "text": args[1]}}, nil
			},
			render: renderAction,
		},
		{
			name:    "press",
			usage:   "<key>",
			summary: "press a key (Enter, Tab, Escape, ArrowDown…)",
			method:  http.MethodPost,
			path:    "/api/page/press",
			build: func(_ *options, args []string) (request, error) {
				key, err := oneArg(args, "key")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"key": key}}, nil
			},
			render: renderAction,
		},
		{
			name:    "read",
			usage:   "",
			summary: "read the page as text",
			method:  http.MethodGet,
			path:    "/api/page/read",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.IntVar(&opts.maxChars, "max-chars", 0, "return at most this many characters of the body")
				fs.Int64Var(&opts.offset, "offset", 0, "start the body at this character offset")
				fs.StringVar(&opts.section, "section", "", "read only the section under this heading")
			},
			build: func(opts *options, args []string) (request, error) {
				if err := noArgs(args); err != nil {
					return request{}, err
				}
				values := url.Values{}
				// The route treats any bound parameter as "bound this read" and
				// its absence as the unbounded contract, so send them only when
				// asked for: an unflagged `brw read` returns the whole page. Once
				// one bound is sent the others have to say -1 explicitly, or
				// --offset alone would silently cap the prose at the route's
				// 20000-char default and the lists at theirs.
				if opts.maxChars > 0 || opts.offset > 0 {
					maxChars := opts.maxChars
					if maxChars <= 0 {
						maxChars = readability.UnboundedReadChars
					}
					values.Set("max_chars", strconv.Itoa(maxChars))
					values.Set("offset", strconv.FormatInt(opts.offset, 10))
					values.Set("max_links", strconv.Itoa(readability.UnboundedReadChars))
					values.Set("max_headings", strconv.Itoa(readability.UnboundedReadChars))
				}
				setString(values, "section", opts.section)
				return request{Query: values}, nil
			},
			render: renderRead,
		},
		{
			name:    "snapshot",
			usage:   "[query]",
			summary: "print the page's semantic element refs",
			method:  http.MethodGet,
			path:    "/api/page/snapshot",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.mode, "mode", "", "snapshot mode (frontier, full, forms…)")
				fs.StringVar(&opts.role, "role", "", "only elements with this accessibility role")
				fs.IntVar(&opts.limit, "limit", 0, "return at most this many elements")
				fs.BoolVar(&opts.viewportOnly, "viewport-only", false, "only elements currently in the viewport")
			},
			build: func(opts *options, args []string) (request, error) {
				query, err := atMostOneArg(args, "query")
				if err != nil {
					return request{}, err
				}
				values := url.Values{}
				setString(values, "query", query)
				setString(values, "mode", opts.mode)
				setString(values, "role", opts.role)
				setInt(values, "limit", opts.limit)
				setBool(values, "viewport_only", opts.viewportOnly)
				return request{Query: values}, nil
			},
			render: renderElementList,
		},
		{
			name:    "screenshot",
			usage:   "[--out <file.png>]",
			summary: "capture the viewport as a PNG",
			method:  http.MethodGet,
			path:    "/api/visual/screenshot",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.out, "out", "screenshot.png", "write the PNG here")
			},
			build: func(_ *options, args []string) (request, error) {
				if err := noArgs(args); err != nil {
					return request{}, err
				}
				// base64=1 keeps the response JSON, which is what --json has to
				// print and what the controller can decode; the raw route hands
				// back image bytes with no envelope at all.
				return request{Query: url.Values{"base64": []string{"1"}}}, nil
			},
			render: renderScreenshot,
		},
		{
			name:          "wait",
			usage:         "<condition>",
			summary:       "wait for load, idle, a URL/title/text substring, a ref, a selector, a dialog or a download",
			method:        http.MethodPost,
			path:          "/api/page/wait_for",
			serverTimeout: true,
			build: func(opts *options, args []string) (request, error) {
				condition, err := oneArg(args, "condition")
				if err != nil {
					return request{}, err
				}
				body := map[string]any{"condition": condition}
				// Absent timeout_ms means the daemon's own default, which is the
				// right answer unless the caller said how long they will wait.
				if opts.timeoutSet {
					body["timeout_ms"] = opts.timeout.Milliseconds()
				}
				return request{Body: body}, nil
			},
			render: renderAction,
		},
		{
			name:    "tabs",
			summary: "list the daemon's open tabs",
			method:  http.MethodGet,
			path:    "/api/browser/tabs",
			build: func(_ *options, args []string) (request, error) {
				return request{}, noArgs(args)
			},
			render: renderTabs,
		},
		{
			name:        "downloads",
			summary:     "list downloads this session captured",
			method:      http.MethodGet,
			path:        "/api/page/downloads",
			unsupported: downloadsUnsupported,
			build: func(_ *options, args []string) (request, error) {
				return request{}, noArgs(args)
			},
			render: renderDownloads,
		},
		{
			name:      "artifact read",
			usage:     "<artifact-id>",
			summary:   "read a captured artifact's text",
			method:    http.MethodPost,
			path:      "/api/artifacts/read",
			exactBody: true,
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.Int64Var(&opts.offset, "offset", 0, "start reading at this byte offset")
				fs.IntVar(&opts.maxBytes, "max-bytes", 0, "read at most this many bytes")
			},
			build: func(opts *options, args []string) (request, error) {
				id, err := oneArg(args, "artifact-id")
				if err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{
					"artifact_id": id,
					"offset":      opts.offset,
					"max_bytes":   opts.maxBytes,
				}}, nil
			},
			render: renderArtifactChunk,
		},
		{
			name:    "health",
			summary: "report the daemon's identity and transport",
			method:  http.MethodGet,
			path:    "/health",
			build: func(_ *options, args []string) (request, error) {
				return request{}, noArgs(args)
			},
			render: renderHealth,
		},
		{
			name:    "grants",
			summary: "list the site permission grants this profile holds",
			method:  http.MethodGet,
			path:    "/api/consent/grants",
			build: func(_ *options, args []string) (request, error) {
				return request{}, noArgs(args)
			},
			render: renderGrants,
		},
		{
			name:      "grants revoke",
			usage:     "<origin>|--all",
			summary:   "revoke a site permission grant; takes effect on the next action",
			method:    http.MethodPost,
			path:      "/api/consent/revoke",
			exactBody: true,
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.BoolVar(&opts.clear, "all", false, "revoke every grant this profile holds")
				fs.StringVar(&opts.scope, "scope", "", "revoke only this scope (read or act); default revokes both")
			},
			build: func(opts *options, args []string) (request, error) {
				body := map[string]any{}
				if opts.clear {
					if len(args) != 0 {
						return request{}, errors.New("--all revokes everything, so it takes no origin")
					}
					body["all"] = true
					return request{Body: body}, nil
				}
				origin, err := oneArg(args, "origin")
				if err != nil {
					return request{}, err
				}
				body["origin"] = origin
				if opts.scope != "" {
					body["scope"] = opts.scope
				}
				return request{Body: body}, nil
			},
			render: renderRevoke,
		},
	}
	// The developer observations are defined beside their renderers in
	// verbs_devtools.go; the completion scripts and the route test read this
	// table, so they are covered the same way every verb above is.
	return append(table, devtoolsVerbs()...)
}

// downloadsUnsupported reads the flag the downloads route sets when the active
// transport cannot observe downloads. It answers 200 with an empty list, which
// is indistinguishable from "no downloads" to anything but this flag.
func downloadsUnsupported(body []byte) (string, bool) {
	var result browser.DownloadsResult
	if err := json.Unmarshal(body, &result); err != nil || result.Supported {
		return "", false
	}
	if note := strings.TrimSpace(result.Note); note != "" {
		return note, true
	}
	return "this transport cannot observe downloads", true
}

// normalizeRef accepts a ref in the shape brw prints it (@e17) as well as the
// bare form the HTTP API takes, so a pasted line of snapshot output works.
func normalizeRef(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(value), "@")
}

func refArg(args []string) (string, error) {
	value, err := oneArg(args, "ref")
	if err != nil {
		return "", err
	}
	ref := normalizeRef(value)
	if ref == "" {
		return "", errors.New("needs a ref, e.g. @e17")
	}
	return ref, nil
}

func oneArg(args []string, name string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("needs exactly one %s", name)
	}
	return args[0], nil
}

func atMostOneArg(args []string, name string) (string, error) {
	switch len(args) {
	case 0:
		return "", nil
	case 1:
		return args[0], nil
	default:
		return "", fmt.Errorf("takes at most one %s", name)
	}
}

func noArgs(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("takes no arguments, got %d", len(args))
	}
	return nil
}

func setString(values url.Values, name, value string) {
	if strings.TrimSpace(value) != "" {
		values.Set(name, value)
	}
}

func setInt(values url.Values, name string, value int) {
	if value > 0 {
		values.Set(name, strconv.Itoa(value))
	}
}

func setBool(values url.Values, name string, value bool) {
	if value {
		values.Set(name, "true")
	}
}
