package readability

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

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

// FromHTML extracts a PageRead from raw HTML using the same shape the in-page reader produces, so a no-browser read pages, sections and renders exactly like a read taken through a tab.
func FromHTML(pageURL string, htmlBytes []byte) (PageRead, error) {
	doc, err := html.Parse(bytes.NewReader(htmlBytes))
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

// FromMarkdown wraps already-markdown content in a PageRead.
func FromMarkdown(pageURL, title, markdown string) PageRead {
	read := PageRead{URL: pageURL, Title: strings.TrimSpace(title), Main: collapseBlankRuns(markdown)}
	read.Headings = markdownHeadings(read.Main)
	if read.Title == "" && len(read.Headings) > 0 {
		read.Title = read.Headings[0].Text
	}
	return Normalize(read)
}

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
		if n.DataAtom == atom.Pre {
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if child.Type != html.ElementNode || skippedContainers[child.DataAtom] {
				continue
			}

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

func renderBlock(n *html.Node, b *strings.Builder) {
	if n == nil {
		return
	}
	switch n.Type {
	case html.TextNode:
		text := normalizeInlineSpace(n.Data)
		if b.Len() == 0 || strings.HasSuffix(b.String(), "\n") || strings.HasSuffix(b.String(), " ") {
			text = strings.TrimLeft(text, " ")
		}
		if text != "" {
			b.WriteString(text)
		}
		return
	case html.ElementNode:
		if skippedContainers[n.DataAtom] {
			return
		}
		if n.DataAtom == atom.Pre {
			endBlock(b)
			text := textOf(n)
			longest, run := 0, 0
			for _, char := range text {
				if char == '`' {
					run++
					longest = max(longest, run)
				} else {
					run = 0
				}
			}
			fence := strings.Repeat("`", max(3, longest+1))
			b.WriteString(fence + "\n" + text)
			endBlock(b)
			b.WriteString(fence + "\n")
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

func normalizeInlineSpace(s string) string {
	return inlineSpace.ReplaceAllString(s, " ")
}

var inlineSpace = regexp.MustCompile(`[ \t\n\r\f\v]+`)

func collapseBlankRuns(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	fence := ""
	for _, line := range lines {
		marker := strings.TrimSpace(line)
		if fence != "" {
			out = append(out, line)
			if len(marker) >= len(fence) && strings.Trim(marker, fence[:1]) == "" {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(marker, "```") || strings.HasPrefix(marker, "~~~") {
			length := len(marker) - len(strings.TrimLeft(marker, marker[:1]))
			fence = marker[:length]
			blank = 0
			out = append(out, line)
			continue
		}
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
