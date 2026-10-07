package harness

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

var (
	cssRefPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?is)@import\s+(?:url\()?\s*["']([^"')]+)["']`),
		regexp.MustCompile(`(?is)\burl\(\s*["']?([^"')]+)["']?\s*\)`),
	}

	scriptURLPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"'` + "`" + `)\]}>]+`),
		regexp.MustCompile(`["'](//[A-Za-z0-9.\-]+(?::\d+)?/[^"']*)["']`),
	}
	metaRefreshURLPattern = regexp.MustCompile(`(?i)\burl\s*=\s*["']?([^"';,\s]+)`)
)

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

var fetchingAttrs = map[string]bool{
	"src":        true,
	"data-src":   true,
	"poster":     true,
	"action":     true,
	"formaction": true,
	"background": true,
}

var linkOnlyElements = map[string]bool{"a": true, "area": true}

var executableScriptTypes = map[string]bool{
	"":                       true,
	"module":                 true,
	"text/javascript":        true,
	"application/javascript": true,
	"text/ecmascript":        true,
	"application/ecmascript": true,
}

const srcdocDepth = 4

// ExternalResourceRefs returns the references in a fixture that would make the browser fetch something off this machine.
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

func srcsetCandidates(value string) []string {
	var refs []string
	for _, candidate := range strings.Split(value, ",") {
		if fields := strings.Fields(candidate); len(fields) > 0 {
			refs = append(refs, fields[0])
		}
	}
	return refs
}

// IsExternalRef reports whether a reference names a host other than this machine.
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

// AuditFixtures checks several fixtures at once and returns one error naming every offending reference.
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
