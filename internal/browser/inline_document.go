package browser

import (
	"errors"
	"strconv"
	"strings"

	"github.com/chromedp/cdproto/fetch"
)

// inlineDocumentTextTypes are media types Chrome downloads although the body is
// text an agent can read. Measured against headless Chromium 152: text/csv,
// application/x-ndjson, application/yaml and application/octet-stream abort the
// navigation with net::ERR_ABORTED and start a download, while application/json,
// application/xml, application/*+json, application/*+xml, text/yaml and
// text/markdown render inline. Unknown application/* types stay untouched
// because a binary body rendered as text is worse than a download.
var inlineDocumentTextTypes = map[string]bool{
	"text/csv":                  true,
	"text/tab-separated-values": true,
	"application/csv":           true,
	"application/x-ndjson":      true,
	"application/ndjson":        true,
	"application/jsonl":         true,
	"application/x-jsonlines":   true,
	"application/yaml":          true,
	"application/x-yaml":        true,
	"application/toml":          true,
}

// InlineDocumentBodyLimit caps the body brw will re-serve to change a media
// type. Chrome derives the download decision from the media type it parsed when
// the headers arrived, so a Content-Type rewrite only takes effect through
// Fetch.fulfillRequest, which carries the whole body through the daemon or the
// extension worker. Dropping Content-Disposition needs no body and has no cap.
const InlineDocumentBodyLimit = 8 << 20

// InlineDocumentRewrite is what a download-shaped main-document response needs
// to render as a page.
type InlineDocumentRewrite struct {
	// Headers is the response header set to continue or fulfil with.
	Headers []*fetch.HeaderEntry
	// Changed is false when the response already renders inline.
	Changed bool
	// NeedsBody is true when the media type changed, which only
	// Fetch.fulfillRequest can apply; a dropped disposition continues in place.
	NeedsBody bool
}

// InlineDocumentHeaders decides how a main-document response should be
// rewritten so Chrome renders it instead of saving it. Two things stop a text
// body from committing as a document: a Content-Disposition of attachment,
// which Google's JSON endpoints send on every response, and a media type Chrome
// has no viewer for. The first is dropped; the second becomes text/plain with
// the original charset kept. Every other header passes through, except that a
// re-served body loses Content-Encoding and Content-Length, which described the
// wire form Fetch.getResponseBody has already decoded.
func InlineDocumentHeaders(headers []*fetch.HeaderEntry) InlineDocumentRewrite {
	rewrite := InlineDocumentRewrite{Headers: make([]*fetch.HeaderEntry, 0, len(headers))}
	for _, header := range headers {
		if header == nil {
			continue
		}
		switch strings.ToLower(header.Name) {
		case "content-disposition":
			if dispositionIsAttachment(header.Value) {
				rewrite.Changed = true
				continue
			}
		case "content-type":
			if rewritten, ok := inlineContentType(header.Value); ok {
				rewrite.Headers = append(rewrite.Headers, &fetch.HeaderEntry{Name: header.Name, Value: rewritten})
				rewrite.Changed = true
				rewrite.NeedsBody = true
				continue
			}
		}
		rewrite.Headers = append(rewrite.Headers, header)
	}
	if rewrite.NeedsBody {
		kept := rewrite.Headers[:0]
		for _, header := range rewrite.Headers {
			switch strings.ToLower(header.Name) {
			case "content-encoding", "content-length", "transfer-encoding":
				continue
			}
			kept = append(kept, header)
		}
		rewrite.Headers = kept
	}
	return rewrite
}

// InlineDocumentBodyWithinLimit reports whether a declared Content-Length
// allows the body to be re-served. An undeclared length is allowed through.
func InlineDocumentBodyWithinLimit(headers []*fetch.HeaderEntry) bool {
	for _, header := range headers {
		if header == nil || !strings.EqualFold(header.Name, "content-length") {
			continue
		}
		length, err := strconv.ParseInt(strings.TrimSpace(header.Value), 10, 64)
		return err != nil || length <= InlineDocumentBodyLimit
	}
	return true
}

func dispositionIsAttachment(value string) bool {
	kind, _, _ := strings.Cut(value, ";")
	return strings.EqualFold(strings.TrimSpace(kind), "attachment")
}

// inlineContentType maps a downloaded text type to text/plain, keeping the
// parameters (charset) the server sent.
func inlineContentType(value string) (string, bool) {
	mediaType, params, _ := strings.Cut(value, ";")
	if !inlineDocumentTextTypes[strings.ToLower(strings.TrimSpace(mediaType))] {
		return "", false
	}
	rewritten := "text/plain"
	if strings.TrimSpace(params) != "" {
		rewritten += ";" + params
	}
	return rewritten, true
}

// NavigationAbortedError names what net::ERR_ABORTED on a brw-driven top-level
// navigation almost always means: the response was a download, so no document
// replaced the page. Chrome reports the same code for a navigation superseded by
// another, which the message allows for.
func NavigationAbortedError(verb string) error {
	return errors.New(verb + ": the browser aborted the navigation (net::ERR_ABORTED). The destination was served as a download rather than a page, or another navigation replaced it; brw renders text downloads (JSON, XML, CSV) inline, so a binary attachment is the usual cause. Check brw_downloads for the saved file")
}

// IsNavigationAbortedError reports whether a navigation failure was net::ERR_ABORTED.
func IsNavigationAbortedError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "net::ERR_ABORTED")
}
