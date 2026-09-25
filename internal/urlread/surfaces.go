package urlread

import (
	"bytes"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// AgentSurfaces lists the machine-facing alternatives a site offers to its human
// pages, gathered from the page's <head> and anchors, the response's Link
// header, and the concurrent discovery probes (/llms.txt, the page's .md
// variant, /.well-known/api-catalog, /.well-known/ai-catalog.json and
// /.well-known/ucp).
type AgentSurfaces struct {
	Markdown        []string `json:"markdown,omitempty"`
	LLMs            []string `json:"llms,omitempty"`
	APIDescriptions []string `json:"api_descriptions,omitempty"`
	APICatalog      string   `json:"api_catalog,omitempty"`
	MCP             []string `json:"mcp,omitempty"`
	A2AAgentCard    string   `json:"a2a_agent_card,omitempty"`
	UCP             string   `json:"ucp,omitempty"`
	// DeprecatedAIPlugin is a legacy ChatGPT-plugin manifest; it is reported
	// apart from api_descriptions because the format is retired.
	DeprecatedAIPlugin string `json:"deprecated_ai_plugin,omitempty"`
	// LLMsTxt is "present", "absent" or "unknown" (the probe timed out, was
	// refused by policy, or failed). Empty when no probe ran.
	LLMsTxt    string `json:"llms_txt,omitempty"`
	LLMsTxtURL string `json:"llms_txt_url,omitempty"`
}

func (s *AgentSurfaces) empty() bool {
	return s == nil || (len(s.Markdown) == 0 && len(s.LLMs) == 0 && len(s.APIDescriptions) == 0 &&
		s.APICatalog == "" && len(s.MCP) == 0 && s.A2AAgentCard == "" && s.UCP == "" &&
		s.DeprecatedAIPlugin == "" && s.LLMsTxt == "")
}

// Fallback hints: why a read that answered is not the page a person would see.
const (
	HintLoginWall    = "login_wall"
	HintJSShell      = "js_shell"
	HintChallenge    = "challenge"
	HintAuthRequired = "auth_required"
)

const (
	maxSurfaceLinks    = 5
	jsShellMaxTextChar = 200
)

var (
	mcpPathPattern     = regexp.MustCompile(`(?i)(^|/)(mcp|mcp/sse|sse)/?$`)
	openAPIPathPattern = regexp.MustCompile(`(?i)(openapi\.(json|ya?ml)|swagger\.json)$`)
	// documentPathPattern is a page for people: a link labelled "MCP" that
	// points at one is an article about MCP, not an endpoint.
	documentPathPattern = regexp.MustCompile(`(?i)\.(md|markdown|html?|txt|pdf)$`)
)

// surfaceLink is one hyperlink from any source, before classification.
type surfaceLink struct {
	href string
	rels []string
	typ  string
	text string
	// prose marks a link from a text body (llms.txt), where the path and the
	// link text are the only signals.
	prose bool
	// anchor marks an HTML <a>, where only well-known paths are meaningful.
	anchor bool
}

func (l surfaceLink) has(rel string) bool {
	for _, r := range l.rels {
		if r == rel {
			return true
		}
	}
	return false
}

func (s *AgentSurfaces) addLink(base *url.URL, l surfaceLink) {
	href := strings.TrimSpace(l.href)
	if href == "" {
		return
	}
	ref, err := url.Parse(href)
	if err != nil {
		return
	}
	abs := base.ResolveReference(ref)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return
	}
	resolved := abs.String()
	path := strings.ToLower(abs.Path)
	switch {
	case strings.HasSuffix(path, "/.well-known/ai-plugin.json"):
		setOnce(&s.DeprecatedAIPlugin, resolved)
	case strings.HasSuffix(path, "/.well-known/agent-card.json") || strings.HasSuffix(path, "/.well-known/agent.json"):
		setOnce(&s.A2AAgentCard, resolved)
	case l.has("api-catalog") || strings.HasSuffix(path, "/.well-known/api-catalog"):
		setOnce(&s.APICatalog, resolved)
	case l.anchor:
	case l.has("mcp") || l.typ == "application/mcp+json":
		s.MCP = appendCapped(s.MCP, resolved)
	case l.has("llms") || l.has("llms-txt") || l.typ == "text/llms" || l.typ == "text/llms+txt":
		s.LLMs = appendCapped(s.LLMs, resolved)
	case l.has("service-desc") || (l.has("alternate") && isOpenAPIType(l.typ)):
		s.APIDescriptions = appendCapped(s.APIDescriptions, resolved)
	case l.has("alternate") && (l.typ == "text/markdown" || l.typ == "text/x-markdown"):
		s.Markdown = appendCapped(s.Markdown, resolved)
	case l.prose && openAPIPathPattern.MatchString(path):
		s.APIDescriptions = appendCapped(s.APIDescriptions, resolved)
	case l.prose && (mcpPathPattern.MatchString(path) || strings.HasSuffix(path, "/server-card") ||
		(strings.Contains(l.text, "MCP") && !documentPathPattern.MatchString(path))):
		s.MCP = appendCapped(s.MCP, resolved)
	}
}

func setOnce(field *string, value string) {
	if *field == "" {
		*field = value
	}
}

func isOpenAPIType(typ string) bool {
	return typ == "application/openapi+json" || typ == "application/openapi+yaml" ||
		strings.HasPrefix(typ, "application/vnd.oai.openapi")
}

func appendCapped(list []string, value string) []string {
	if len(list) >= maxSurfaceLinks {
		return list
	}
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

func mediaTypeOf(contentType string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
}

// parseLinkHeader reads RFC 8288 Link header values: <uri>; rel="a b"; type="t".
func parseLinkHeader(values []string) []surfaceLink {
	var links []surfaceLink
	for _, value := range values {
		for len(value) > 0 {
			open := strings.IndexByte(value, '<')
			if open < 0 {
				break
			}
			end := strings.IndexByte(value[open:], '>')
			if end < 0 {
				break
			}
			link := surfaceLink{href: value[open+1 : open+end]}
			rest := value[open+end+1:]
			next := strings.IndexByte(rest, '<')
			params := rest
			if next >= 0 {
				params = rest[:next]
				value = rest[next:]
			} else {
				value = ""
			}
			for _, param := range strings.Split(params, ";") {
				key, val, ok := strings.Cut(strings.TrimSpace(param), "=")
				if !ok {
					continue
				}
				val = strings.Trim(strings.TrimSpace(strings.TrimRight(strings.TrimSpace(val), ",")), `"`)
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "rel":
					link.rels = strings.Fields(strings.ToLower(val))
				case "type":
					link.typ = mediaTypeOf(val)
				}
			}
			links = append(links, link)
		}
	}
	return links
}

var (
	markdownLinkPattern = regexp.MustCompile(`\[([^\]]*)\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	bareURLPattern      = regexp.MustCompile(`https?://[^\s<>()\[\]"']+`)
)

// proseLinks reads markdown links and bare URLs out of a text body.
func proseLinks(body []byte) []surfaceLink {
	var links []surfaceLink
	text := string(body)
	for _, m := range markdownLinkPattern.FindAllStringSubmatch(text, -1) {
		links = append(links, surfaceLink{href: m[2], text: m[1], prose: true})
	}
	stripped := markdownLinkPattern.ReplaceAllString(text, " ")
	for _, raw := range bareURLPattern.FindAllString(stripped, -1) {
		links = append(links, surfaceLink{href: strings.TrimRight(raw, ".,;:"), prose: true})
	}
	return links
}

// htmlSignals is everything one tokenizer pass over an HTML body collects.
type htmlSignals struct {
	title         string
	surfaces      AgentSurfaces
	passwordInput bool
	scripts       int
	emptyMount    bool
	noscriptJS    bool
}

var mountIDs = map[string]bool{"root": true, "app": true, "__next": true, "__nuxt": true, "svelte": true, "q-app": true}

func scanHTML(base *url.URL, body []byte) htmlSignals {
	var sig htmlSignals
	z := html.NewTokenizer(bytes.NewReader(body))
	var (
		inTitle, inNoscript bool
		titleText           strings.Builder
		noscriptText        strings.Builder
		openMount           bool
	)
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			sig.title = strings.TrimSpace(titleText.String())
			if strings.Contains(strings.ToLower(noscriptText.String()), "javascript") {
				sig.noscriptJS = true
			}
			return sig
		case html.TextToken:
			text := z.Text()
			if inTitle {
				titleText.Write(text)
			}
			if inNoscript {
				noscriptText.Write(text)
			}
			if openMount && len(bytes.TrimSpace(text)) > 0 {
				openMount = false
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "title":
				inTitle = false
			case "noscript":
				inNoscript = false
			case "div":
				if openMount {
					sig.emptyMount = true
					openMount = false
				}
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			attrs := map[string]string{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				attrs[string(k)] = string(v)
			}
			tag := string(name)
			if openMount && tag != "div" {
				openMount = false
			}
			switch tag {
			case "title":
				inTitle = tt == html.StartTagToken
			case "noscript":
				inNoscript = tt == html.StartTagToken
			case "script":
				sig.scripts++
			case "input":
				if strings.EqualFold(strings.TrimSpace(attrs["type"]), "password") {
					sig.passwordInput = true
				}
			case "div":
				openMount = tt == html.StartTagToken && mountIDs[strings.TrimSpace(attrs["id"])]
				if tt == html.SelfClosingTagToken && mountIDs[strings.TrimSpace(attrs["id"])] {
					sig.emptyMount = true
				}
			case "link":
				sig.surfaces.addLink(base, surfaceLink{
					href: attrs["href"],
					rels: strings.Fields(strings.ToLower(attrs["rel"])),
					typ:  mediaTypeOf(attrs["type"]),
				})
			case "a":
				sig.surfaces.addLink(base, surfaceLink{href: attrs["href"], anchor: true})
			}
		}
	}
}

var (
	loginPathPattern  = regexp.MustCompile(`(?i)(^|[/_.-])(login|signin|sign-in|log-in|auth|sso|session)([/_.-]|$)`)
	loginTitlePattern = regexp.MustCompile(`(?i)\b(log ?in|sign ?in)\b`)
	challengeTitles   = []string{"just a moment", "attention required! | cloudflare"}
)

func isChallenge(header http.Header, body []byte, title string) bool {
	if strings.EqualFold(strings.TrimSpace(header.Get("cf-mitigated")), "challenge") {
		return true
	}
	if bytes.Contains(body, []byte("/cdn-cgi/challenge-platform")) {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(title))
	for _, t := range challengeTitles {
		if strings.HasPrefix(lower, t) {
			return true
		}
	}
	return false
}

// successHint classifies a 2xx HTML response whose prose is not the page.
func successHint(finalURL *url.URL, sig htmlSignals, mainChars int) string {
	if sig.passwordInput {
		path := ""
		if finalURL != nil {
			path = finalURL.Path
		}
		if loginPathPattern.MatchString(path) || loginTitlePattern.MatchString(sig.title) {
			return HintLoginWall
		}
	}
	if mainChars < jsShellMaxTextChar && sig.scripts > 0 && (sig.emptyMount || sig.noscriptJS) {
		return HintJSShell
	}
	return ""
}

func hintAdvice(hint string) string {
	switch hint {
	case HintAuthRequired:
		return "auth_required: needs a signed-in session; use brw_open + brw_read"
	case HintChallenge:
		return "challenge: bot check; use brw_open in a real profile"
	case HintLoginWall:
		return "login_wall: the server answered with a login page; use brw_open + brw_read in a signed-in profile"
	case HintJSShell:
		return "js_shell: the page renders in JavaScript; use brw_open + brw_read"
	}
	return ""
}
