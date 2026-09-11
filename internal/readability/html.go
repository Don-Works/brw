package readability

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// resolveURL turns a page-relative href into an absolute one so links survive
// being read out of the page they came from. An href that will not parse is
// returned unchanged rather than dropped.
func resolveURL(base, href string) string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return href
	}
	ref, err := url.Parse(href)
	if err != nil {
		return href
	}
	return baseURL.ResolveReference(ref).String()
}

// FromHTML extracts a PageRead from raw HTML using the same shape the in-page
// reader produces, so a no-browser read pages, sections and renders exactly like
// a read taken through a tab.
//
// This is deliberately a structural extractor rather than a scoring one: it
// drops chrome (script/style/nav/header/footer/aside), prefers the most
// specific container that still holds the bulk of the prose, and keeps block
// boundaries as newlines. It carries no site-specific rules.
func FromHTML(pageURL string, htmlBytes []byte) (PageRead, error) {
	doc, err := html.Parse(strings.NewReader(string(htmlBytes)))
	if err != nil {
		return PageRead{}, err
	}
	read := PageRead{URL: pageURL}
	read.Title = strings.TrimSpace(textOf(findFirst(doc, atom.Title)))
	read.Metadata = metadataOf(doc)

	body := findFirst(doc, atom.Body)
	if body == nil {
		body = doc
	}
	main := pickMainContainer(body)

	var b strings.Builder
	renderBlock(main, &b)
	read.Main = collapseBlankRuns(b.String())
	read.Headings = headingsOf(main, read.Main)
	read.Links = linksOf(main, pageURL)
	return Normalize(read), nil
}

// FromMarkdown wraps already-markdown content in a PageRead. A server that
// honours Accept: text/markdown has done the extraction for us, and re-parsing
// its output as HTML would only damage it.
func FromMarkdown(pageURL, title, markdown string) PageRead {
	read := PageRead{URL: pageURL, Title: strings.TrimSpace(title), Main: collapseBlankRuns(markdown)}
	if read.Title == "" {
		read.Title = firstMarkdownHeading(markdown)
	}
	read.Headings = markdownHeadings(read.Main)
	return Normalize(read)
}

func firstMarkdownHeading(markdown string) string {
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			return strings.TrimSpace(strings.TrimLeft(trimmed, "# "))
		}
	}
	return ""
}

// markdownHeadings indexes ATX headings so a markdown read is section-addressable
// on the same code path as an HTML one.
func markdownHeadings(markdown string) []Heading {
	var headings []Heading
	offset := 0
	inFence := false
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(trimmed, "#") {
			level := 0
			for level < len(trimmed) && trimmed[level] == '#' {
				level++
			}
			if level <= 6 && level < len(trimmed) && trimmed[level] == ' ' {
				position := offset
				headings = append(headings, Heading{
					Level:  level,
					Text:   strings.TrimSpace(trimmed[level:]),
					Offset: &position,
				})
			}
		}
		offset += len([]rune(line)) + 1
	}
	return headings
}

// skippedContainers never carry the document's prose. Dropping them before
// extraction is what keeps a no-browser read from being mostly navigation.
var skippedContainers = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Nav: true, atom.Header: true, atom.Footer: true, atom.Aside: true,
	atom.Svg: true, atom.Iframe: true, atom.Form: true, atom.Button: true,
	atom.Select: true, atom.Option: true, atom.Textarea: true,
}

var blockElements = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Ul: true, atom.Ol: true, atom.Li: true, atom.Blockquote: true, atom.Pre: true,
	atom.Table: true, atom.Tr: true, atom.Br: true, atom.Hr: true, atom.Dl: true,
	atom.Dt: true, atom.Dd: true, atom.Figure: true, atom.Figcaption: true, atom.Main: true,
}

var headingLevels = map[atom.Atom]int{
	atom.H1: 1, atom.H2: 2, atom.H3: 3, atom.H4: 4, atom.H5: 5, atom.H6: 6,
}

func findFirst(n *html.Node, a atom.Atom) *html.Node {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if found := findFirst(child, a); found != nil {
			return found
		}
	}
	return nil
}

// pickMainContainer prefers an explicit <main> or <article>, then falls back to
// the deepest single element that still holds most of the body's text. Without
// the fallback, a page whose prose sits in an unlabelled <div> would be read
// together with every sidebar the body contains.
func pickMainContainer(body *html.Node) *html.Node {
	if m := findFirst(body, atom.Main); m != nil {
		return m
	}
	if a := findFirst(body, atom.Article); a != nil {
		return a
	}
	total := len(strings.TrimSpace(textOf(body)))
	if total == 0 {
		return body
	}
	best := body
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if child.Type != html.ElementNode || skippedContainers[child.DataAtom] {
				continue
			}
			// 60% keeps the container that holds the bulk of the prose while still
			// descending past wrappers that merely contain it.
			if len(strings.TrimSpace(textOf(child)))*100 >= total*60 {
				best = child
				walk(child)
				return
			}
		}
	}
	walk(body)
	return best
}

func textOf(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
			return
		}
		if node.Type == html.ElementNode && skippedContainers[node.DataAtom] {
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return b.String()
}

// renderBlock flattens an element tree to prose, emitting a newline at block
// boundaries so paragraph structure survives into Main.
func renderBlock(n *html.Node, b *strings.Builder) {
	if n == nil {
		return
	}
	switch n.Type {
	case html.TextNode:
		text := normalizeInlineSpace(n.Data)
		if text != "" {
			b.WriteString(text)
		}
		return
	case html.ElementNode:
		if skippedContainers[n.DataAtom] {
			return
		}
		if level, isHeading := headingLevels[n.DataAtom]; isHeading {
			endBlock(b)
			b.WriteString(strings.Repeat("#", level) + " " + strings.TrimSpace(normalizeInlineSpace(textOf(n))))
			endBlock(b)
			return
		}
		if n.DataAtom == atom.Li {
			endBlock(b)
			b.WriteString("- ")
		} else if blockElements[n.DataAtom] {
			endBlock(b)
		}
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		renderBlock(child, b)
	}
	if n.Type == html.ElementNode && blockElements[n.DataAtom] {
		endBlock(b)
	}
}

func endBlock(b *strings.Builder) {
	s := b.String()
	if s == "" || strings.HasSuffix(s, "\n") {
		return
	}
	b.WriteString("\n")
}

// normalizeInlineSpace collapses runs of whitespace to a single space, which is
// how a browser renders inline text and what keeps indented source HTML from
// arriving as ragged prose.
func normalizeInlineSpace(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v'
	}), " ")
}

func collapseBlankRuns(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if strings.TrimSpace(trimmed) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, trimmed)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// headingsOf indexes headings by their character offset within the extracted
// prose, which is what makes a section addressable by name.
func headingsOf(root *html.Node, main string) []Heading {
	var headings []Heading
	searchFrom := 0
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if skippedContainers[n.DataAtom] {
				return
			}
			if level, ok := headingLevels[n.DataAtom]; ok {
				text := strings.TrimSpace(normalizeInlineSpace(textOf(n)))
				if text != "" {
					heading := Heading{Level: level, Text: text, ID: attr(n, "id")}
					marker := strings.Repeat("#", level) + " " + text
					if index := strings.Index(main[min(searchFrom, len(main)):], marker); index >= 0 {
						position := len([]rune(main[:min(searchFrom, len(main))+index]))
						heading.Offset = &position
						searchFrom = min(searchFrom, len(main)) + index + len(marker)
					} else {
						outside := -1
						heading.Offset = &outside
					}
					headings = append(headings, heading)
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return headings
}

func linksOf(root *html.Node, base string) []Link {
	var links []Link
	seen := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if skippedContainers[n.DataAtom] {
				return
			}
			if n.DataAtom == atom.A {
				href := strings.TrimSpace(attr(n, "href"))
				text := strings.TrimSpace(normalizeInlineSpace(textOf(n)))
				if href != "" && text != "" && !strings.HasPrefix(href, "javascript:") {
					resolved := resolveURL(base, href)
					if key := text + "\x00" + resolved; !seen[key] {
						seen[key] = true
						links = append(links, Link{Text: text, Href: resolved})
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return links
}

func metadataOf(doc *html.Node) Metadata {
	meta := Metadata{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Html && meta.Lang == "" {
			meta.Lang = strings.TrimSpace(attr(n, "lang"))
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Link && meta.Canonical == "" {
			if strings.EqualFold(strings.TrimSpace(attr(n, "rel")), "canonical") {
				meta.Canonical = strings.TrimSpace(attr(n, "href"))
			}
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Meta {
			name := strings.ToLower(attr(n, "name"))
			property := strings.ToLower(attr(n, "property"))
			content := strings.TrimSpace(attr(n, "content"))
			if content != "" {
				if name == "description" && meta.Description == "" {
					meta.Description = content
				}
				if strings.HasPrefix(property, "og:") {
					if meta.OpenGraph == nil {
						meta.OpenGraph = map[string]string{}
					}
					meta.OpenGraph[strings.TrimPrefix(property, "og:")] = content
					if property == "og:description" && meta.Description == "" {
						meta.Description = content
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return meta
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}
