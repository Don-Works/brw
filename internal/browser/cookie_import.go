package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// maxImportedCookies bounds one import so a pasted multi-megabyte dump cannot
// make brw issue thousands of CDP commands in one call.
const maxImportedCookies = 200

// ImportedCookie is one cookie parsed from a cURL command, a Cookie header, or a
// JSON array.
type ImportedCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"http_only,omitempty"`
	SameSite string  `json:"same_site,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
}

// ParseCookieImport reads cookies from the three shapes a caller actually has:
// a JSON array of {name,value,...}, a "Copy as cURL" command, or a bare Cookie
// header. domain, when set, is applied to every parsed cookie and overrides any
// domain in the input, which is what scoping an import to one site means.
func ParseCookieImport(input, domain string) ([]ImportedCookie, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return nil, errors.New("import needs curl: a cURL command, a Cookie header, or a JSON array of cookies")
	}
	defaultDomain := strings.TrimSpace(domain)

	var cookies []ImportedCookie
	var err error
	switch {
	case strings.HasPrefix(raw, "[") || strings.HasPrefix(raw, "{"):
		cookies, err = parseCookieJSON(raw)
	default:
		header := extractCookieHeader(raw)
		if header == "" {
			return nil, errors.New("no Cookie header found in the import: pass a cURL command containing -H 'Cookie: ...'/--cookie/-b, a bare Cookie header, or a JSON array")
		}
		cookies = parseCookieHeader(header)
	}
	if err != nil {
		return nil, err
	}
	if len(cookies) == 0 {
		return nil, errors.New("the import contained no cookies")
	}
	if len(cookies) > maxImportedCookies {
		return nil, fmt.Errorf("the import carries %d cookies, over the %d limit", len(cookies), maxImportedCookies)
	}
	for i := range cookies {
		cookies[i].Name = strings.TrimSpace(cookies[i].Name)
		if cookies[i].Name == "" {
			return nil, errors.New("the import contained a cookie with an empty name")
		}
		if defaultDomain != "" {
			cookies[i].Domain = defaultDomain
		}
	}
	return cookies, nil
}

// parseCookieJSON accepts an array of cookie objects, or one object.
func parseCookieJSON(raw string) ([]ImportedCookie, error) {
	var array []ImportedCookie
	if err := json.Unmarshal([]byte(raw), &array); err == nil {
		return array, nil
	}
	var one ImportedCookie
	if err := json.Unmarshal([]byte(raw), &one); err != nil {
		return nil, fmt.Errorf("the JSON import is neither a cookie object nor an array of them: %w", err)
	}
	return []ImportedCookie{one}, nil
}

// extractCookieHeader pulls the Cookie header out of a cURL command, or returns
// the input unchanged when it already looks like a bare Cookie header.
func extractCookieHeader(raw string) string {
	lower := strings.ToLower(raw)
	if !strings.Contains(lower, "curl ") && !strings.Contains(raw, "-H") &&
		!strings.Contains(lower, "--cookie") && !strings.Contains(lower, "-b ") {
		// A bare "a=b; c=d" header.
		if strings.Contains(raw, "=") && !strings.ContainsAny(raw, "\n") {
			return raw
		}
		return ""
	}
	// Join continuation lines so a multi-line cURL command parses as one.
	joined := strings.ReplaceAll(raw, "\\\n", " ")
	joined = strings.ReplaceAll(joined, "\r\n", "\n")
	fields := splitShellFields(joined)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "-H" || f == "--header":
			if i+1 < len(fields) {
				if v, ok := headerValue(fields[i+1]); ok {
					return v
				}
				i++
			}
		case f == "-b" || f == "--cookie":
			if i+1 < len(fields) {
				// -b may name a FILE, in which case there is no inline header.
				if strings.Contains(fields[i+1], "=") {
					return fields[i+1]
				}
				i++
			}
		case strings.HasPrefix(f, "-H") && len(f) > 2:
			if v, ok := headerValue(strings.TrimPrefix(f, "-H")); ok {
				return v
			}
		case strings.HasPrefix(f, "--cookie="):
			return strings.TrimPrefix(f, "--cookie=")
		case strings.HasPrefix(f, "--header="):
			if v, ok := headerValue(strings.TrimPrefix(f, "--header=")); ok {
				return v
			}
		}
	}
	return ""
}

func headerValue(header string) (string, bool) {
	header = strings.TrimSpace(header)
	colon := strings.Index(header, ":")
	if colon < 0 {
		return "", false
	}
	if !strings.EqualFold(strings.TrimSpace(header[:colon]), "cookie") {
		return "", false
	}
	return strings.TrimSpace(header[colon+1:]), true
}

// splitShellFields splits on whitespace and strips simple single/double quotes,
// which is enough for a pasted cURL command without pulling in a shell parser.
func splitShellFields(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// parseCookieHeader splits a "a=b; c=d" Cookie header.
func parseCookieHeader(header string) []ImportedCookie {
	var out []ImportedCookie
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq <= 0 {
			continue
		}
		out = append(out, ImportedCookie{
			Name:  strings.TrimSpace(part[:eq]),
			Value: strings.TrimSpace(part[eq+1:]),
		})
	}
	return out
}
