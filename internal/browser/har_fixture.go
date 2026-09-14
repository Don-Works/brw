package browser

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// HAREntry is one recorded exchange from a HAR log, reduced to what answering a
// live request from it needs. The transports never see the HAR document itself:
// the surface that owns the artifact store decodes it and hands the entries
// down, so internal/browser stays free of any dependency on artifact storage.
type HAREntry struct {
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	RequestBody string            `json:"request_body,omitempty"`
	Status      int               `json:"status"`
	ContentType string            `json:"content_type,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        string            `json:"body,omitempty"`
}

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
	// HARMissFail refuses it, which is what makes a fixture deterministic: the
	// page under test cannot reach anything the recording did not contain.
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
	ArtifactID string      `json:"har_artifact_id"`
	Match      []string    `json:"match"`
	OnMiss     string      `json:"on_miss"`
	Entries    int         `json:"entries"`
	Served     int         `json:"served"`
	Missed     int         `json:"missed"`
	Misses     []RouteMiss `json:"misses,omitempty"`
}

// harFixture answers requests from a recorded HAR.
//
// Entries are consumed in recorded order: the first entry that matches and has
// not been used yet wins, so a HAR that recorded one URL twice with different
// answers replays them in sequence rather than repeating the first forever.
// Once every matching entry is used, the first one answers again — a page that
// polls more often than the recording did gets the recorded answer rather than
// a miss, which would otherwise turn on_miss:"fail" into a flaky test.
type harFixture struct {
	artifactID string
	match      []string
	onMiss     string

	mu      sync.Mutex
	entries []HAREntry
	used    []bool
	served  int
	missed  int
	misses  []RouteMiss
}

func newHARFixture(artifactID string, entries []HAREntry, match []string, onMiss string) *harFixture {
	return &harFixture{
		artifactID: artifactID,
		match:      match,
		onMiss:     onMiss,
		entries:    entries,
		used:       make([]bool, len(entries)),
	}
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

// find returns the recorded answer for one request.
func (f *harFixture) find(method, url, body string) (HAREntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	first := -1
	for i := range f.entries {
		if !f.matchesLocked(f.entries[i], method, url, body) {
			continue
		}
		if first < 0 {
			first = i
		}
		if !f.used[i] {
			f.used[i] = true
			f.served++
			return f.entries[i], true
		}
	}
	if first < 0 {
		return HAREntry{}, false
	}
	f.served++
	return f.entries[first], true
}

func (f *harFixture) matchesLocked(entry HAREntry, method, url, body string) bool {
	for _, key := range f.match {
		switch key {
		case HARMatchMethod:
			if !strings.EqualFold(harMethod(entry.Method), harMethod(method)) {
				return false
			}
		case HARMatchURL:
			if entry.URL != url {
				return false
			}
		case HARMatchBody:
			if entry.RequestBody != body {
				return false
			}
		}
	}
	return true
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

// view snapshots the fixture for a reply. The stored fixture is mutated from
// the interception handler's goroutine, so a listing has to copy under the lock
// rather than hand out the live value.
func (f *harFixture) view() RouteFixture {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := RouteFixture{
		ArtifactID: f.artifactID,
		Match:      append([]string(nil), f.match...),
		OnMiss:     f.onMiss,
		Entries:    len(f.entries),
		Served:     f.served,
		Missed:     f.missed,
	}
	if len(f.misses) > 0 {
		out.Misses = append([]RouteMiss(nil), f.misses...)
	}
	return out
}

func harMethod(method string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return "GET"
	}
	return method
}
