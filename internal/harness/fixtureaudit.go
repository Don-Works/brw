package harness

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// The audit parses a fixture rather than pattern-matching its text.
//
// The regex form it replaced had a second way past it for every shape it
// checked: an unquoted attribute value, an SVG <use href>, an xlink:href, an
// <object data>, a meta refresh, an <iframe srcdoc> carrying an escaped <img>,
// and a fetch() in an inline script all went through clean. A parser does not
// have a list of shapes to be incomplete about — it reports the elements, their
// attributes and their text, and the decision below is about which attributes a
// browser fetches.
//
// cssRefPattern and scriptURLPattern still scan text, because CSS and
// JavaScript are not HTML and there is nothing here that parses them.
var (
	cssRefPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?is)@import\s+(?:url\()?\s*["']([^"')]+)["']`),
		regexp.MustCompile(`(?is)\burl\(\s*["']?([^"')]+)["']?\s*\)`),
	}
	// An absolute URL anywhere in an executable script. A fixture has no reason
	// to carry one, and fetch("https://...") / import("https://...") are exactly
	// what an audit of what the page loads has to see.
	scriptURLPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"'` + "`" + `)\]}>]+`),
		regexp.MustCompile(`["'](//[A-Za-z0-9.\-]+(?::\d+)?/[^"']*)["']`),
	}
	metaRefreshURLPattern = regexp.MustCompile(`(?i)\burl\s*=\s*["']?([^"';,\s]+)`)
)

// nonFetchingLinkRels are the <link rel> values that declare a relationship
// without making the browser open anything.
//
// The audit is deliberately the other way round from the obvious one: it flags
// every <link href> EXCEPT these, rather than flagging a list of fetching rels.
// The fetching set is open — preconnect, modulepreload, dns-prefetch and
// whatever ships next — and a rel missing from a list of things to flag is a
// silent pass, which is the failure this check exists to prevent.
var nonFetchingLinkRels = map[string]bool{
	"canonical":  true,
	"alternate":  true,
	"author":     true,
	"license":    true,
	"next":       true,
	"prev":       true,
	"search":     true,
	"help":       true,
	"bookmark":   true,
	"me":         true,
	"nofollow":   true,
	"noopener":   true,
	"noreferrer": true,
	"tag":        true,
}

// fetchingAttrs are attributes whose value the browser resolves and loads,
// whatever element carries them. href is absent because it depends on the
// element: see refsFromAttr.
var fetchingAttrs = map[string]bool{
	"src":        true,
	"data-src":   true,
	"poster":     true,
	"action":     true,
	"formaction": true,
	"background": true,
}

// linkOnlyElements are the elements whose href the browser follows only when
// something clicks it. The fixtures carry illustrative links to the open web
// that no harness ever follows, so an <a href> is not a fetch.
var linkOnlyElements = map[string]bool{"a": true, "area": true}

// executableScriptTypes are the <script type> values whose body Chrome runs. A
// JSON-LD or application/json block is data the fixtures legitimately fill with
// schema.org URLs, and nothing fetches those.
var executableScriptTypes = map[string]bool{
	"":                       true,
	"module":                 true,
	"text/javascript":        true,
	"application/javascript": true,
	"text/ecmascript":        true,
	"application/ecmascript": true,
}

// srcdocDepth bounds the recursion through nested srcdoc documents, which are
// otherwise a way to make this walk forever.
const srcdocDepth = 4

// ExternalResourceRefs returns the references in a fixture that would make the
// browser fetch something off this machine.
//
// The harnesses claim to reach nothing but their own loopback fixture origin.
// That claim is about the pages as much as about the harness: one <img> at a CDN
// makes a "no network" run depend on somebody else's uptime, and it would do so
// silently, as slower numbers rather than as an error.
func ExternalResourceRefs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var external []string
	seen := map[string]bool{}
	consider := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		if IsExternalRef(ref) {
			external = append(external, ref)
		}
	}
	if err := scanDocument(string(data), srcdocDepth, consider); err != nil {
		return nil, err
	}
	return external, nil
}

// scanDocument parses one document and reports every reference in it that the
// browser would load.
func scanDocument(text string, depth int, consider func(string)) error {
	doc, err := html.Parse(strings.NewReader(text))
	if err != nil {
		return err
	}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		switch node.Type {
		case html.ElementNode:
			scanElement(node, depth, consider)
		case html.TextNode:
			scanText(node, consider)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return nil
}

func scanElement(node *html.Node, depth int, consider func(string)) {
	tag := strings.ToLower(node.Data)
	rel, httpEquiv := "", ""
	for _, attr := range node.Attr {
		switch attrName(attr) {
		case "rel":
			rel = strings.ToLower(strings.TrimSpace(attr.Val))
		case "http-equiv":
			httpEquiv = strings.ToLower(strings.TrimSpace(attr.Val))
		}
	}
	for _, attr := range node.Attr {
		name := attrName(attr)
		if name == "srcdoc" {
			// The parser has already unescaped the value, so what is in hand is
			// the nested document itself. Past the recursion limit it is scanned
			// bluntly for absolute URLs instead of being walked, because running
			// out of depth must not be a way to pass.
			if depth > 0 {
				_ = scanDocument(attr.Val, depth-1, consider)
			} else {
				for _, ref := range absoluteURLs(unescapeFully(attr.Val)) {
					consider(ref)
				}
			}
			continue
		}
		for _, ref := range refsFromAttr(tag, name, attr.Val, rel, httpEquiv) {
			consider(ref)
		}
	}
}

// attrName is the attribute's name as it was written, so a namespaced
// xlink:href is told apart from a plain href.
func attrName(attr html.Attribute) string {
	name := strings.ToLower(attr.Key)
	if attr.Namespace != "" {
		return strings.ToLower(attr.Namespace) + ":" + name
	}
	return name
}

func refsFromAttr(tag, name, value, rel, httpEquiv string) []string {
	switch {
	case fetchingAttrs[name]:
		return []string{value}
	case name == "srcset":
		return srcsetCandidates(value)
	case name == "data":
		// <object data> loads a document or plugin resource; the attribute means
		// nothing on anything else.
		if tag == "object" {
			return []string{value}
		}
	case name == "xlink:href":
		return []string{value}
	case name == "href":
		switch {
		case linkOnlyElements[tag]:
			return nil
		case tag == "link" && nonFetchingLinkRels[rel]:
			return nil
		}
		// Everything else's href is loaded without a click: <link>, <base>,
		// which redirects every relative reference on the page, and the SVG
		// <use>, <image> and <filter> family.
		return []string{value}
	case name == "style":
		return cssRefs(value)
	case name == "content":
		if tag == "meta" && httpEquiv == "refresh" {
			if match := metaRefreshURLPattern.FindStringSubmatch(value); match != nil {
				return []string{match[1]}
			}
		}
	}
	return nil
}

func scanText(node *html.Node, consider func(string)) {
	if node.Parent == nil || node.Parent.Type != html.ElementNode {
		return
	}
	switch strings.ToLower(node.Parent.Data) {
	case "style":
		for _, ref := range cssRefs(node.Data) {
			consider(ref)
		}
	case "script":
		if !executableScript(node.Parent) {
			return
		}
		for _, ref := range absoluteURLs(node.Data) {
			consider(ref)
		}
	}
}

// unescapeFully strips the entity layers a nested srcdoc accumulates, so the
// blunt scan below sees a URL rather than one run into an "&quot;". Bounded
// because unescaping is what adds the next layer, and a crafted value could
// otherwise be made to peel forever.
func unescapeFully(text string) string {
	for range 2 * srcdocDepth {
		unescaped := html.UnescapeString(text)
		if unescaped == text {
			break
		}
		text = unescaped
	}
	return text
}

// absoluteURLs pulls every absolute reference out of text that is not markup.
func absoluteURLs(text string) []string {
	var refs []string
	for _, pattern := range scriptURLPatterns {
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			refs = append(refs, match[len(match)-1])
		}
	}
	return refs
}

func executableScript(node *html.Node) bool {
	for _, attr := range node.Attr {
		if attrName(attr) == "type" {
			return executableScriptTypes[strings.ToLower(strings.TrimSpace(attr.Val))]
		}
	}
	return true
}

func cssRefs(text string) []string {
	var refs []string
	for _, pattern := range cssRefPatterns {
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			refs = append(refs, match[1])
		}
	}
	return refs
}

// srcsetCandidates splits a srcset into its URLs. Flagging the whole attribute
// instead would make a perfectly local "a.png 1x, b.png 2x" unparseable and so
// external, which is a false alarm the audit cannot afford if anyone is to keep
// running it.
func srcsetCandidates(value string) []string {
	var refs []string
	for _, candidate := range strings.Split(value, ",") {
		if fields := strings.Fields(candidate); len(fields) > 0 {
			refs = append(refs, fields[0])
		}
	}
	return refs
}

// IsExternalRef reports whether a reference names a host other than this
// machine. A relative or fragment reference resolves against the fixture origin
// and is not external; a scheme-relative "//host/..." is, which is the form a
// string check for "http" misses.
func IsExternalRef(ref string) bool {
	trimmed := strings.TrimSpace(ref)
	switch {
	case trimmed == "", strings.HasPrefix(trimmed, "#"), strings.HasPrefix(trimmed, "data:"),
		strings.HasPrefix(trimmed, "blob:"), strings.HasPrefix(trimmed, "about:"):
		return false
	case strings.HasPrefix(trimmed, "//"):
		trimmed = "https:" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		// An unparseable reference is not something this can clear, and a fixture
		// has no reason to carry one.
		return true
	}
	if parsed.Scheme == "" && parsed.Host == "" {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "ws", "wss", "":
	default:
		return false
	}
	return !isLoopbackHost(parsed.Hostname())
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

// AuditFixtures checks several fixtures at once and returns one error naming
// every offending reference.
func AuditFixtures(paths []string) error {
	var problems []string
	for _, path := range paths {
		refs, err := ExternalResourceRefs(path)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			problems = append(problems, fmt.Sprintf("%s loads %s", path, ref))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("fixtures reach off this machine: %s", strings.Join(problems, "; "))
}
