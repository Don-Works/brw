package urlread

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	llmsProbeMaxBytes    = 64 << 10
	variantProbeMaxBytes = 8 << 10
	catalogProbeMaxBytes = 64 << 10
	presenceProbeMaxByte = 4 << 10
)

var (
	llmsProbeTimeout = 2 * time.Second
	skipDiscovery    = false
)

const (
	llmsPresent = "present"
	llmsAbsent  = "absent"
	llmsUnknown = "unknown"
)

type probeOutcome struct {
	state string
	body  []byte
	url   string
}

type probeSpec struct {
	url      string
	accept   string
	maxBytes int64
	accepts  func(mediaType string) bool
}

// probe fetches one discovery resource concurrently with the main read. It
// never fails the read: every problem reports "unknown".
type probe struct {
	url  string
	done chan probeOutcome
}

func startProbe(ctx context.Context, spec probeSpec, opts Options, userAgent string, timeout time.Duration) *probe {
	p := &probe{url: spec.url, done: make(chan probeOutcome, 1)}
	go func() {
		p.done <- runProbe(ctx, spec, opts, userAgent, timeout)
	}()
	return p
}

func (p *probe) wait(deadline <-chan time.Time) probeOutcome {
	if p == nil {
		return probeOutcome{}
	}
	select {
	case outcome := <-p.done:
		return outcome
	case <-deadline:
		return probeOutcome{state: llmsUnknown, url: p.url}
	}
}

func runProbe(ctx context.Context, spec probeSpec, opts Options, userAgent string, timeout time.Duration) probeOutcome {
	out := probeOutcome{state: llmsUnknown, url: spec.url}
	if opts.PolicyCheck != nil {
		if err := opts.PolicyCheck(spec.url); err != nil {
			return out
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{
		Transport:     guardedTransport(),
		CheckRedirect: redirectCheck(opts),
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.url, nil)
	if err != nil {
		return out
	}
	request.Header.Set("Accept", spec.accept)
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return out
	}
	defer response.Body.Close()
	out.url = response.Request.URL.String()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
			out.state = llmsAbsent
		}
		return out
	}
	if !spec.accepts(mediaTypeOf(response.Header.Get("Content-Type"))) {
		out.state = llmsAbsent
		return out
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, spec.maxBytes))
	if err != nil && !errors.Is(err, io.EOF) {
		return out
	}
	if looksLikeHTML(body) || len(bytes.TrimSpace(body)) == 0 {
		out.state = llmsAbsent
		return out
	}
	out.state = llmsPresent
	out.body = body
	return out
}

func acceptsText(mediaType string) bool {
	switch mediaType {
	case "", "text/plain", "text/markdown", "text/x-markdown":
		return true
	}
	return false
}

func acceptsJSON(mediaType string) bool {
	return mediaType == "" || mediaType == "application/json" || mediaType == "text/plain" ||
		strings.HasSuffix(mediaType, "+json")
}

func acceptsNonHTML(mediaType string) bool {
	return mediaType != "text/html" && mediaType != "application/xhtml+xml"
}

func looksLikeHTML(body []byte) bool {
	head := bytes.ToLower(bytes.TrimSpace(body))
	if len(head) > 512 {
		head = head[:512]
	}
	return bytes.HasPrefix(head, []byte("<!doctype html")) || bytes.HasPrefix(head, []byte("<html")) ||
		bytes.Contains(head, []byte("<head")) || bytes.Contains(head, []byte("<body"))
}

func redirectCheck(opts Options) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("redirect to unsupported scheme %q refused", req.URL.Scheme)
		}
		if opts.PolicyCheck != nil {
			return opts.PolicyCheck(req.URL.String())
		}
		return nil
	}
}

// markdownVariantURL is the conventional .md rendering of a page: /docs/ ->
// /docs/index.html.md, /a.html -> /a.html.md, /a -> /a.md. Empty when the path
// is already a text resource.
func markdownVariantURL(target *url.URL) string {
	path := target.Path
	if path == "" {
		path = "/"
	}
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".txt") {
		return ""
	}
	if strings.HasSuffix(path, "/") {
		path += "index.html.md"
	} else {
		path += ".md"
	}
	variant := *target
	variant.Path = path
	variant.RawPath = ""
	variant.RawQuery = ""
	variant.ForceQuery = false
	variant.Fragment = ""
	return variant.String()
}

// discovery is the batch of probes run alongside one read. All of them start
// before the main request and share one deadline, so the batch adds at most
// the probe timeout to a read.
type discovery struct {
	deadline   <-chan time.Time
	llms       *probe
	variant    *probe
	apiCatalog *probe
	aiCatalog  *probe
	ucp        *probe
}

func startDiscovery(ctx context.Context, target *url.URL, opts Options, userAgent string) *discovery {
	timeout := llmsProbeTimeout
	d := &discovery{deadline: time.After(timeout + 100*time.Millisecond)}
	root := func(path string) string { return target.ResolveReference(&url.URL{Path: path}).String() }
	start := func(spec probeSpec) *probe { return startProbe(ctx, spec, opts, userAgent, timeout) }
	d.llms = start(probeSpec{url: root("/llms.txt"), accept: "text/markdown, text/plain;q=0.9", maxBytes: llmsProbeMaxBytes, accepts: acceptsText})
	if variant := markdownVariantURL(target); variant != "" {
		d.variant = start(probeSpec{url: variant, accept: "text/markdown, text/plain;q=0.9", maxBytes: variantProbeMaxBytes, accepts: acceptsText})
	}
	d.apiCatalog = start(probeSpec{url: root("/.well-known/api-catalog"), accept: "application/linkset+json, application/json;q=0.9", maxBytes: catalogProbeMaxBytes, accepts: acceptsJSON})
	d.aiCatalog = start(probeSpec{url: root("/.well-known/ai-catalog.json"), accept: "application/json", maxBytes: catalogProbeMaxBytes, accepts: acceptsJSON})
	d.ucp = start(probeSpec{url: root("/.well-known/ucp"), accept: "application/json, */*;q=0.5", maxBytes: presenceProbeMaxByte, accepts: acceptsNonHTML})
	return d
}

// apply folds the probe outcomes into s. markdownVariant is false when the main
// response was already markdown, so a .md twin adds nothing.
func (d *discovery) apply(s *AgentSurfaces, markdownVariant bool) {
	llms := d.llms.wait(d.deadline)
	s.LLMsTxt = llms.state
	s.LLMsTxtURL = d.llms.url
	if llms.state == llmsPresent {
		base := parseOr(llms.url, d.llms.url)
		for _, link := range proseLinks(llms.body) {
			s.addLink(base, link)
		}
	}
	if d.variant != nil {
		if out := d.variant.wait(d.deadline); out.state == llmsPresent && markdownVariant {
			s.Markdown = appendCapped(s.Markdown, d.variant.url)
		}
	}
	if out := d.apiCatalog.wait(d.deadline); out.state == llmsPresent {
		if hrefs, ok := linksetHrefs(out.body); ok {
			setOnce(&s.APICatalog, d.apiCatalog.url)
			base := parseOr(out.url, d.apiCatalog.url)
			for _, h := range hrefs {
				s.addLink(base, h)
			}
		}
	}
	if out := d.aiCatalog.wait(d.deadline); out.state == llmsPresent {
		base := parseOr(out.url, d.aiCatalog.url)
		for _, href := range aiCatalogMCP(out.body) {
			s.addLink(base, surfaceLink{href: href, rels: []string{"mcp"}})
		}
	}
	if out := d.ucp.wait(d.deadline); out.state == llmsPresent {
		setOnce(&s.UCP, d.ucp.url)
	}
}

func parseOr(primary, fallback string) *url.URL {
	if u, err := url.Parse(primary); err == nil && u.Scheme != "" {
		return u
	}
	u, _ := url.Parse(fallback)
	return u
}

// linksetHrefs reads an RFC 9727 API catalog (an RFC 9264 linkset) and returns
// the hrefs of its service-desc, service-doc and item links.
func linksetHrefs(body []byte) ([]surfaceLink, bool) {
	var doc struct {
		Linkset []map[string]json.RawMessage `json:"linkset"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Linkset == nil {
		return nil, false
	}
	var links []surfaceLink
	for _, entry := range doc.Linkset {
		// service-desc names a machine-readable API description. An item is
		// classified by its path like a prose link, so an OpenAPI file or a
		// retired plugin manifest is recognised and the API's own base URL is
		// not. service-doc is documentation for people and is not reported.
		for _, rel := range []string{"service-desc", "item"} {
			var targets []struct {
				Href string `json:"href"`
				Type string `json:"type"`
			}
			raw, ok := entry[rel]
			if !ok || json.Unmarshal(raw, &targets) != nil {
				continue
			}
			for _, t := range targets {
				if strings.TrimSpace(t.Href) == "" {
					continue
				}
				if rel == "service-desc" {
					links = append(links, surfaceLink{href: t.Href, rels: []string{"service-desc"}, typ: t.Type})
				} else {
					links = append(links, surfaceLink{href: t.Href, typ: t.Type, prose: true})
				}
			}
		}
	}
	return links, true
}

var mcpURLKeys = map[string]bool{"url": true, "endpoint": true, "href": true, "uri": true, "server_url": true, "serverurl": true, "mcp_url": true, "remote": true}

// aiCatalogMCP pulls MCP server URLs out of an ai-catalog.json document. The
// format is young, so it reads the document structurally: a URL-valued field
// counts when its object or an enclosing key says "mcp", or when the URL path
// itself is an MCP endpoint. A trailing /server-card is dropped so the MCP URL
// is what gets reported.
func aiCatalogMCP(body []byte) []string {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil
	}
	var out []string
	var walk func(v any, mcp bool)
	walk = func(v any, mcp bool) {
		switch node := v.(type) {
		case map[string]any:
			here := mcp
			for _, k := range []string{"type", "protocol", "kind", "transport"} {
				if s, ok := node[k].(string); ok && strings.Contains(strings.ToLower(s), "mcp") {
					here = true
				}
			}
			for k, child := range node {
				key := strings.ToLower(k)
				if s, ok := child.(string); ok {
					if (here || strings.Contains(key, "mcp")) && mcpURLKeys[key] || looksLikeMCPURL(s) {
						out = appendCapped(out, strings.TrimSuffix(s, "/server-card"))
					}
					continue
				}
				walk(child, here || strings.Contains(key, "mcp"))
			}
		case []any:
			for _, child := range node {
				walk(child, mcp)
			}
		case string:
			if mcp || looksLikeMCPURL(node) {
				out = appendCapped(out, strings.TrimSuffix(node, "/server-card"))
			}
		}
	}
	walk(doc, false)
	filtered := out[:0]
	for _, u := range out {
		if parsed, err := url.Parse(u); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			filtered = append(filtered, u)
		}
	}
	return filtered
}

func looksLikeMCPURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return mcpPathPattern.MatchString(u.Path) || strings.HasSuffix(u.Path, "/server-card")
}
