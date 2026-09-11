package artifact

import (
	"sort"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// HAR 1.2 (http://www.softwareishard.com/blog/har-12-spec/) is what DevTools,
// Charles and every HTTP analysis tool already import, so exporting it is how a
// captured session leaves brw and reaches a human or a bug report.
type harLog struct {
	Log harLogBody `json:"log"`
}

type harLogBody struct {
	Version string     `json:"version"`
	Creator harCreator `json:"creator"`
	Pages   []harPage  `json:"pages"`
	Entries []harEntry `json:"entries"`
	Comment string     `json:"comment,omitempty"`
}

type harCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type harPage struct {
	StartedDateTime string        `json:"startedDateTime"`
	ID              string        `json:"id"`
	Title           string        `json:"title"`
	PageTimings     harPageTiming `json:"pageTimings"`
}

type harPageTiming struct {
	OnContentLoad float64 `json:"onContentLoad"`
	OnLoad        float64 `json:"onLoad"`
}

type harEntry struct {
	StartedDateTime string      `json:"startedDateTime"`
	Time            float64     `json:"time"`
	Request         harRequest  `json:"request"`
	Response        harResponse `json:"response"`
	Cache           struct{}    `json:"cache"`
	Timings         harTimings  `json:"timings"`
	Comment         string      `json:"comment,omitempty"`
}

type harRequest struct {
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	HTTPVersion string      `json:"httpVersion"`
	Cookies     []struct{}  `json:"cookies"`
	Headers     []harHeader `json:"headers"`
	QueryString []harQuery  `json:"queryString"`
	PostData    *harPost    `json:"postData,omitempty"`
	HeadersSize int         `json:"headersSize"`
	BodySize    int         `json:"bodySize"`
}

type harResponse struct {
	Status      int         `json:"status"`
	StatusText  string      `json:"statusText"`
	HTTPVersion string      `json:"httpVersion"`
	Cookies     []struct{}  `json:"cookies"`
	Headers     []harHeader `json:"headers"`
	Content     harContent  `json:"content"`
	RedirectURL string      `json:"redirectURL"`
	HeadersSize int         `json:"headersSize"`
	BodySize    int         `json:"bodySize"`
}

type harHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harQuery struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harPost struct {
	MIMEType string `json:"mimeType"`
	Text     string `json:"text"`
}

type harContent struct {
	Size     int    `json:"size"`
	MIMEType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
}

type harTimings struct {
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}

// redactedHeaderNames are dropped from an exported HAR unless redaction is
// explicitly turned off.
//
// A HAR is meant to be shared — attached to a bug, sent to a vendor — and a raw
// one carries the session that produced it. Exporting credentials by default
// would turn a debugging aid into a credential-leak channel, so the default is
// redacted and un-redacting is a deliberate act.
var redactedHeaderNames = map[string]bool{
	"cookie":              true,
	"set-cookie":          true,
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"x-auth-token":        true,
	"x-csrf-token":        true,
	"x-xsrf-token":        true,
	"api-key":             true,
	"auth-token":          true,
}

const redactedPlaceholder = "[redacted by brw]"

func redactHeaders(headers map[string]string, redact bool) []harHeader {
	out := make([]harHeader, 0, len(headers))
	for name, value := range headers {
		if redact && redactedHeaderNames[strings.ToLower(name)] {
			value = redactedPlaceholder
		}
		out = append(out, harHeader{Name: name, Value: value})
	}
	// Stable order so two exports of the same capture compare equal.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func splitQuery(rawURL string) []harQuery {
	index := strings.Index(rawURL, "?")
	if index < 0 {
		return []harQuery{}
	}
	out := []harQuery{}
	for _, pair := range strings.Split(rawURL[index+1:], "&") {
		if pair == "" {
			continue
		}
		name, value, _ := strings.Cut(pair, "=")
		out = append(out, harQuery{Name: name, Value: value})
	}
	return out
}

// BuildHAR converts brw's captured requests into a HAR 1.2 log.
//
// startedAt values in a capture are page-relative milliseconds
// (performance.now), so they are rebased onto the capture's wall-clock start to
// produce the absolute timestamps HAR requires.
func BuildHAR(requests []snapshot.CapturedRequest, pageURL, pageTitle, version string, redact bool) harLog {
	base := time.Now().UTC()
	entries := make([]harEntry, 0, len(requests))
	for _, req := range requests {
		started := base.Add(time.Duration(req.StartedAt) * time.Millisecond)
		entry := harEntry{
			StartedDateTime: started.Format(time.RFC3339Nano),
			Time:            req.DurationMS,
			Request: harRequest{
				Method:      orDefault(req.Method, "GET"),
				URL:         req.URL,
				HTTPVersion: "HTTP/1.1",
				Cookies:     []struct{}{},
				Headers:     redactHeaders(req.RequestHeaders, redact),
				QueryString: splitQuery(req.URL),
				HeadersSize: -1,
				BodySize:    len(req.RequestBody),
			},
			Response: harResponse{
				Status:      req.Status,
				StatusText:  statusTextFor(req),
				HTTPVersion: "HTTP/1.1",
				Cookies:     []struct{}{},
				Headers:     []harHeader{},
				Content: harContent{
					Size:     len(req.ResponseSnippet),
					MIMEType: "application/octet-stream",
					Text:     req.ResponseSnippet,
				},
				RedirectURL: "",
				HeadersSize: -1,
				BodySize:    len(req.ResponseSnippet),
			},
			// brw captures one duration rather than the connect/send/wait/receive
			// breakdown, so the whole duration is reported as wait and the other
			// phases are marked unavailable (-1) rather than invented as 0.
			Timings: harTimings{Send: -1, Wait: req.DurationMS, Receive: -1},
		}
		if req.RequestBody != "" {
			body := req.RequestBody
			if redact {
				body = redactedPlaceholder
			}
			entry.Request.PostData = &harPost{MIMEType: "application/octet-stream", Text: body}
		}
		if !req.Completed {
			entry.Comment = "request had not completed when the capture was exported"
		}
		if req.Error != "" {
			entry.Comment = strings.TrimSpace(entry.Comment + " " + req.Error)
		}
		entries = append(entries, entry)
	}

	comment := "Exported by brw. Response bodies are capture snippets, not full bodies."
	if redact {
		comment += " Credential-bearing headers and request bodies are redacted; pass redaction=none to export them."
	} else {
		comment += " REDACTION DISABLED: this file may contain cookies, tokens and request bodies."
	}
	return harLog{Log: harLogBody{
		Version: "1.2",
		Creator: harCreator{Name: "brw", Version: version},
		Pages: []harPage{{
			StartedDateTime: base.Format(time.RFC3339Nano),
			ID:              "page_1",
			Title:           orDefault(pageTitle, pageURL),
			PageTimings:     harPageTiming{OnContentLoad: -1, OnLoad: -1},
		}},
		Entries: entries,
		Comment: comment,
	}}
}

func statusTextFor(req snapshot.CapturedRequest) string {
	if req.Error != "" {
		return "Error"
	}
	if req.Status == 0 {
		return ""
	}
	return ""
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// brwVersionForHAR labels the HAR's creator. It is set from the build's version
// at startup so an exported file names the brw that produced it.
var brwVersionForHAR = "dev"

// SetVersion records the running brw version for HAR creator metadata.
func SetVersion(version string) {
	if strings.TrimSpace(version) != "" {
		brwVersionForHAR = version
	}
}
