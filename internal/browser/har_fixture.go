package browser

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// HARHeader is one recorded response header.
//
// A list rather than a map because HAR 1.2 stores headers as a list precisely
// so a response can carry two Link, Vary or Www-Authenticate headers, and
// collapsing those into a map would replay only the last one.
type HARHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HAREntry is one recorded exchange from a HAR log, reduced to what answering a
// live request from it needs. The transports never see the HAR document itself:
// the surface that owns the artifact store decodes it and hands the entries
// down, so internal/browser stays free of any dependency on artifact storage.
type HAREntry struct {
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	RequestBody string      `json:"request_body,omitempty"`
	Status      int         `json:"status"`
	ContentType string      `json:"content_type,omitempty"`
	Headers     []HARHeader `json:"headers,omitempty"`
	Body        string      `json:"body,omitempty"`
	// Truncated marks a response body the recording clipped rather than stored
	// whole. brw's in-page capture keeps the first 2 KiB of each response, so a
	// larger one replays as a prefix: valid bytes, but not a valid document. The
	// flag is what turns that from a parse error in the page into a number the
	// agent is told at install time.
	Truncated bool `json:"truncated,omitempty"`
}

// HARRedactedPlaceholder is what an exported HAR carries where a credential
// header or a request body was. Declared here, next to the replay that has to
// recognise it, because a body-keyed replay of a redacted capture can never
// match anything and has to be refused rather than left to miss every request.
const HARRedactedPlaceholder = "[redacted by brw]"

// Match keys a replay can be keyed on. They are explicit rather than inferred
// because the right key set is a property of the fixture, not of brw: a page
// that POSTs a different body to one endpoint per action needs "body" to tell
// the recordings apart, and a fixture that does not needs it left out or every
// replay after the first request misses.
const (
	HARMatchMethod = "method"
	HARMatchURL    = "url"
	HARMatchBody   = "body"
)

// What a request matching the replay's pattern does when the HAR has no entry
// for it.
const (
	// HARMissPassthrough sends the request to the real network.
	HARMissPassthrough = "passthrough"
	// HARMissFail refuses it, so the page under test cannot reach anything the
	// recording did not contain.
	HARMissFail = "fail"
)

// maxRouteMisses bounds the recorded miss list so a page looping on a failing
// request cannot grow it without limit.
const maxRouteMisses = 50

// RouteMiss is one request the HAR had no entry for.
type RouteMiss struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// RouteFixture describes the HAR behind a replay rule. It carries counts and
// misses, never bodies: brw_route's reply is a description of the fixture, and
// echoing recorded response bodies back would put the whole capture into an
// agent's context on every list.
type RouteFixture struct {
	ArtifactID string   `json:"har_artifact_id"`
	Match      []string `json:"match"`
	OnMiss     string   `json:"on_miss"`
	Entries    int      `json:"entries"`
	Served     int      `json:"served"`
	Missed     int      `json:"missed"`
	// Truncated counts entries whose recorded response is a clipped snippet, and
	// ServedTruncated how many of those were actually handed to the page. A page
	// parsing a clipped JSON body fails with a syntax error that points at the
	// page rather than at the fixture, so both numbers are reported.
	Truncated       int `json:"truncated_entries,omitempty"`
	ServedTruncated int `json:"served_truncated,omitempty"`
	// NotReplayable counts requests inside the pattern that a brw HAR cannot
	// hold at all — the document, a script, a stylesheet, an image — and that
	// were sent to the network instead. See harReplayableResourceType.
	NotReplayable int         `json:"not_replayable,omitempty"`
	Misses        []RouteMiss `json:"misses,omitempty"`
}

// harFixture answers requests from a recorded HAR.
//
// Entries are consumed in recorded order: the first entry that matches and has
// not been used yet wins, so a HAR that recorded one URL twice with different
// answers replays them in sequence rather than repeating the first forever.
// Once every matching entry is used, the first one answers again — a page that
// polls more often than the recording did gets the recorded answer rather than
// a miss, which would otherwise turn on_miss:"fail" into a flaky test.
//
// Lookups go through an index built once at construction rather than a scan of
// every entry: find runs inside the interception handler with the request held
// open and the fixture lock serialising every other paused request on the same
// route, so a six-figure HAR would otherwise make each request wait on a walk
// of the whole recording.
type harFixture struct {
	artifactID string
	match      []string
	onMiss     string

	mu    sync.Mutex
	index map[string][]int
	// cursor is the next position in a key's entry list to serve. It is what
	// consumes recordings in order; once it reaches the end the key's first
	// entry answers every later request.
	cursor          map[string]int
	entries         []HAREntry
	truncated       int
	served          int
	servedTruncated int
	missed          int
	notReplayable   int
	misses          []RouteMiss
}

func newHARFixture(artifactID string, entries []HAREntry, match []string, onMiss string) *harFixture {
	f := &harFixture{
		artifactID: artifactID,
		match:      match,
		onMiss:     onMiss,
		entries:    entries,
		index:      make(map[string][]int, len(entries)),
		cursor:     make(map[string]int, len(entries)),
	}
	for i := range entries {
		if entries[i].Truncated {
			f.truncated++
		}
		key := f.key(entries[i].Method, entries[i].URL, entries[i].RequestBody)
		f.index[key] = append(f.index[key], i)
	}
	return f
}

// NormalizeHARMatch validates the requested match keys, defaulting to method
// and URL.
func NormalizeHARMatch(keys []string) ([]string, error) {
	out := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		switch key {
		case HARMatchMethod, HARMatchURL, HARMatchBody:
		default:
			return nil, fmt.Errorf("unknown match key %q: use method, url or body", key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	if len(out) == 0 {
		return []string{HARMatchMethod, HARMatchURL}, nil
	}
	return out, nil
}

// NormalizeHARMiss validates on_miss, defaulting to passthrough so a replay
// added to a live page does not silently cut off everything it did not record.
func NormalizeHARMiss(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return HARMissPassthrough, nil
	case HARMissPassthrough:
		return HARMissPassthrough, nil
	case HARMissFail:
		return HARMissFail, nil
	default:
		return "", fmt.Errorf("unknown on_miss %q: use passthrough or fail", value)
	}
}

// key renders the match keys of one request (or one recorded entry) into the
// index key they share. Both sides go through it, so an entry is findable by
// exactly the requests that would have matched it field by field.
func (f *harFixture) key(method, url, body string) string {
	var out strings.Builder
	for _, key := range f.match {
		var field string
		switch key {
		case HARMatchMethod:
			field = harMethod(method)
		case HARMatchURL:
			field = url
		case HARMatchBody:
			field = body
		}
		// Length-prefixed rather than delimited: a decoded request body can
		// contain any byte, and any separator it could also contain would let two
		// different field splits share one key and answer each other's requests.
		fmt.Fprintf(&out, "%d:%s", len(field), field)
	}
	return out.String()
}

// find returns the recorded answer for one request.
func (f *harFixture) find(method, url, body string) (HAREntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := f.key(method, url, body)
	candidates := f.index[key]
	if len(candidates) == 0 {
		return HAREntry{}, false
	}
	if pos := f.cursor[key]; pos < len(candidates) {
		f.cursor[key] = pos + 1
		return f.serveLocked(candidates[pos]), true
	}
	return f.serveLocked(candidates[0]), true
}

func (f *harFixture) serveLocked(i int) HAREntry {
	f.served++
	if f.entries[i].Truncated {
		f.servedTruncated++
	}
	return f.entries[i]
}

// recordMiss books an unmatched request and returns the reason, which is also
// what the caller reports as the failure. Naming the method, the URL and the
// keys that were compared is the difference between "the fixture is incomplete"
// and a page that mysteriously half-loads.
func (f *harFixture) recordMiss(method, url string) RouteMiss {
	f.mu.Lock()
	defer f.mu.Unlock()
	miss := RouteMiss{
		Method: harMethod(method),
		URL:    clipDialogText(url),
		At:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	miss.Reason = fmt.Sprintf("no HAR entry matches %s %s on [%s]; the fixture holds %d entries",
		miss.Method, miss.URL, strings.Join(f.match, " "), len(f.entries))
	f.missed++
	f.misses = append(f.misses, miss)
	if len(f.misses) > maxRouteMisses {
		f.misses = f.misses[len(f.misses)-maxRouteMisses:]
	}
	return miss
}

// recordNotReplayable books a request the recording could never have held, which
// went to the network instead of through the fixture.
func (f *harFixture) recordNotReplayable() {
	f.mu.Lock()
	f.notReplayable++
	f.mu.Unlock()
}

// view snapshots the fixture for a reply. The stored fixture is mutated from
// the interception handler's goroutine, so a listing has to copy under the lock
// rather than hand out the live value.
func (f *harFixture) view() RouteFixture {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := RouteFixture{
		ArtifactID:      f.artifactID,
		Match:           append([]string(nil), f.match...),
		OnMiss:          f.onMiss,
		Entries:         len(f.entries),
		Served:          f.served,
		Missed:          f.missed,
		Truncated:       f.truncated,
		ServedTruncated: f.servedTruncated,
		NotReplayable:   f.notReplayable,
	}
	if len(f.misses) > 0 {
		out.Misses = append([]RouteMiss(nil), f.misses...)
	}
	return out
}

// missSummary reports the fixture's miss count and the most recent reasons, for
// the observation that has to explain a half-loaded page.
func (f *harFixture) missSummary(limit int) (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missed == 0 || limit <= 0 {
		return f.missed, nil
	}
	reasons := make([]string, 0, limit)
	for i := len(f.misses) - 1; i >= 0 && len(reasons) < limit; i-- {
		reasons = append(reasons, f.misses[i].Reason)
	}
	return f.missed, reasons
}

func harMethod(method string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return "GET"
	}
	return method
}
