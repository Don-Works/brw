package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/usagelog"
)

const defaultTabLeaseTTL = 30 * time.Minute

// abandonedTabLeaseTTL is how long a lease outlives a call whose caller went
// away before it finished: the request's context was cancelled because the
// proxy dropped the connection, was killed, or relayed an MCP cancellation. A
// caller that is still alive renews the full TTL on its next call.
const abandonedTabLeaseTTL = 2 * time.Minute

type leaseContextKey struct{}

type tabLease struct {
	owner     string
	expiresAt time.Time
	updatedAt time.Time
	inFlight  int
	// groupID is the Chrome tab group the daemon placed this tab into when it
	// opened it for the owner (empty for claimed pre-existing tabs and grouping-
	// incapable transports). list_tabs compares it against the tab's live group
	// to flag drift — a human dragging the tab out of the agent's lane — as a
	// signal, never as enforcement.
	groupID string
	// opened is set when the daemon opened this tab for the owner rather than
	// the owner claiming a tab that already existed. A session release with
	// close_tabs closes only these.
	opened bool
}

// tabLeaseManager gives every shared-daemon browser session exclusive ownership
// of its working tabs. The bridge's per-tab mutex still serializes low-level
// RPCs; these longer-lived leases protect the multi-call agent workflow around
// those RPCs (snapshot -> reason -> action -> verify).
type tabLeaseManager struct {
	mu           sync.Mutex
	allocateMu   sync.Mutex
	ttl          time.Duration
	abandonedTTL time.Duration
	now          func() time.Time
	byTab        map[string]*tabLease
	defaultTabs  map[string]string
}

func newTabLeaseManager(ttl time.Duration) *tabLeaseManager {
	if ttl <= 0 {
		ttl = defaultTabLeaseTTL
	}
	return &tabLeaseManager{
		ttl: ttl, abandonedTTL: min(ttl, abandonedTabLeaseTTL), now: time.Now,
		byTab: make(map[string]*tabLease), defaultTabs: make(map[string]string),
	}
}

type tabLeaseConflictError struct {
	tabID     string
	expiresAt time.Time
}

func (e *tabLeaseConflictError) Error() string {
	return fmt.Sprintf("tab is leased by another browser session until %s; do not retry or focus it—call brw_open to get a new leased tab, or choose a tab marked available by brw_list_tabs", e.expiresAt.UTC().Format(time.RFC3339))
}

// maxInFlightHold is how long an operation that has not finished keeps its
// lease past the lease's own expiry. It covers the longest legitimate call, a
// recipe run, plus headroom. An in-flight count older than that is a call that
// will never release (its handler is wedged or its release was lost), and
// honouring it left a tab refusing every other session with an expiry already
// half an hour in the past.
const maxInFlightHold = recipe.DefaultMaxRunDuration + 5*time.Minute

// heldUntil is when lease stops being enforced: its expiry, extended while an
// operation that started less than maxInFlightHold ago is still running.
func heldUntil(lease *tabLease) time.Time {
	if lease.inFlight > 0 {
		if hold := lease.updatedAt.Add(maxInFlightHold); hold.After(lease.expiresAt) {
			return hold
		}
	}
	return lease.expiresAt
}

// sweepLocked drops every lease no longer held at now. Every path that
// enforces a lease sweeps first, so expiry is decided at enforcement time.
func (m *tabLeaseManager) sweepLocked(now time.Time) {
	for tabID, lease := range m.byTab {
		if !heldUntil(lease).After(now) {
			delete(m.byTab, tabID)
			if m.defaultTabs[lease.owner] == tabID {
				delete(m.defaultTabs, lease.owner)
			}
		}
	}
}

func (m *tabLeaseManager) acquire(owner, tabID string, makeDefault bool) (func(), error) {
	return m.acquireFor(context.Background(), owner, tabID, makeDefault)
}

// acquireFor takes the lease for one call made under ctx. The returned release
// renews it for the full TTL, or for abandonedTTL when ctx was cancelled
// before the call finished.
func (m *tabLeaseManager) acquireFor(ctx context.Context, owner, tabID string, makeDefault bool) (func(), error) {
	owner = strings.TrimSpace(owner)
	tabID = strings.TrimSpace(tabID)
	if owner == "" || tabID == "" {
		return func() {}, nil
	}

	now := m.now()
	m.mu.Lock()
	m.sweepLocked(now)
	lease := m.byTab[tabID]
	if lease != nil && lease.owner != owner {
		expiresAt := heldUntil(lease)
		m.mu.Unlock()
		return nil, &tabLeaseConflictError{tabID: tabID, expiresAt: expiresAt}
	}
	if lease == nil {
		lease = &tabLease{owner: owner}
		m.byTab[tabID] = lease
	}
	lease.inFlight++
	lease.updatedAt = now
	lease.expiresAt = now.Add(m.ttl)
	if makeDefault {
		m.defaultTabs[owner] = tabID
	}
	m.mu.Unlock()
	return m.releaser(ctx, owner, tabID), nil
}

// releaser ends one in-flight use of owner's lease on tabID. Only the first
// call of the returned func counts.
func (m *tabLeaseManager) releaser(ctx context.Context, owner, tabID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			ttl := m.ttl
			if ctx.Err() != nil {
				ttl = m.abandonedTTL
			}
			now := m.now()
			m.mu.Lock()
			defer m.mu.Unlock()
			if current := m.byTab[tabID]; current != nil && current.owner == owner {
				if current.inFlight > 0 {
					current.inFlight--
				}
				current.updatedAt = now
				current.expiresAt = now.Add(ttl)
			}
		})
	}
}

func (m *tabLeaseManager) acquireDefault(owner string) (string, func(), bool) {
	return m.acquireDefaultFor(context.Background(), owner)
}

func (m *tabLeaseManager) acquireDefaultFor(ctx context.Context, owner string) (string, func(), bool) {
	now := m.now()
	m.mu.Lock()
	m.sweepLocked(now)
	tabID := m.defaultTabs[owner]
	lease := m.byTab[tabID]
	if tabID == "" || lease == nil || lease.owner != owner {
		delete(m.defaultTabs, owner)
		m.mu.Unlock()
		return "", nil, false
	}
	lease.inFlight++
	lease.updatedAt = now
	lease.expiresAt = now.Add(m.ttl)
	m.mu.Unlock()
	return tabID, m.releaser(ctx, owner, tabID), true
}

func (m *tabLeaseManager) resolveOrOpen(ctx context.Context, owner, explicitTabID string, open func(context.Context) (browser.OpenResult, error)) (string, func(), error) {
	if explicitTabID = strings.TrimSpace(explicitTabID); explicitTabID != "" {
		release, err := m.acquireFor(ctx, owner, explicitTabID, true)
		return explicitTabID, release, err
	}
	if tabID, release, ok := m.acquireDefaultFor(ctx, owner); ok {
		return tabID, release, nil
	}

	// Double-check under a separate allocation lock. This avoids two concurrent
	// first calls from the same session each opening a different scratch tab.
	m.allocateMu.Lock()
	defer m.allocateMu.Unlock()
	if tabID, release, ok := m.acquireDefaultFor(ctx, owner); ok {
		return tabID, release, nil
	}
	result, err := open(ctx)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(result.Tab.ID) == "" {
		return "", nil, fmt.Errorf("automatic working-tab allocation returned no tab id")
	}
	release, err := m.acquireFor(ctx, owner, result.Tab.ID, true)
	if err == nil {
		m.noteOpened(owner, result.Tab.ID)
		m.noteGroup(owner, result.Tab.ID, result.Tab.GroupID)
	}
	return result.Tab.ID, release, err
}

// noteOpened marks tabID as a tab the daemon opened for owner.
func (m *tabLeaseManager) noteOpened(owner, tabID string) {
	owner = strings.TrimSpace(owner)
	tabID = strings.TrimSpace(tabID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if lease := m.byTab[tabID]; lease != nil && lease.owner == owner {
		lease.opened = true
	}
}

// ownedTabs lists, sorted, the tabs owner holds a lease on and the subset the
// daemon opened for it.
func (m *tabLeaseManager) ownedTabs(owner string) (all, opened []string) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for tabID, lease := range m.byTab {
		if lease.owner != owner {
			continue
		}
		all = append(all, tabID)
		if lease.opened {
			opened = append(opened, tabID)
		}
	}
	sort.Strings(all)
	sort.Strings(opened)
	return all, opened
}

// releaseOwner drops every lease owner holds, including ones a call is still
// using, and forgets its default tab. It returns the released tab ids, sorted.
func (m *tabLeaseManager) releaseOwner(owner string) []string {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var released []string
	for tabID, lease := range m.byTab {
		if lease.owner == owner {
			delete(m.byTab, tabID)
			released = append(released, tabID)
		}
	}
	delete(m.defaultTabs, owner)
	sort.Strings(released)
	return released
}

// noteGroup records the tab group the daemon opened tabID into for owner, so
// annotate can flag the tab if it later leaves that group. No-op for tabs the
// daemon did not group (claimed tabs, grouping-incapable transports).
func (m *tabLeaseManager) noteGroup(owner, tabID, groupID string) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if lease := m.byTab[tabID]; lease != nil && lease.owner == owner {
		lease.groupID = groupID
	}
}

// claim reserves a tab without adding an in-flight operation. It is used to
// preflight multi-tab batch/plan/group operations; the normal 30-minute renewal
// window comfortably covers the operation after the atomic ownership check.
func (m *tabLeaseManager) claim(owner, tabID string, makeDefault bool) error {
	release, err := m.acquire(owner, tabID, makeDefault)
	if err != nil {
		return err
	}
	release()
	return nil
}

// claimAll atomically checks and reserves a set of tabs. No partial claims are
// left behind when one member is already owned by another session.
func (m *tabLeaseManager) claimAll(owner string, tabIDs []string) ([]string, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, nil
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(now)
	unique := make(map[string]struct{}, len(tabIDs))
	for _, tabID := range tabIDs {
		tabID = strings.TrimSpace(tabID)
		if tabID == "" {
			continue
		}
		unique[tabID] = struct{}{}
		if lease := m.byTab[tabID]; lease != nil && lease.owner != owner {
			return nil, &tabLeaseConflictError{tabID: tabID, expiresAt: heldUntil(lease)}
		}
	}
	var newlyClaimed []string
	for tabID := range unique {
		lease := m.byTab[tabID]
		if lease == nil {
			lease = &tabLease{owner: owner}
			m.byTab[tabID] = lease
			newlyClaimed = append(newlyClaimed, tabID)
		}
		lease.updatedAt = now
		lease.expiresAt = now.Add(m.ttl)
	}
	return newlyClaimed, nil
}

func (m *tabLeaseManager) bind(owner, tabID string, makeDefault bool) error {
	return m.claim(owner, tabID, makeDefault)
}

func (m *tabLeaseManager) release(owner, tabID string) {
	owner = strings.TrimSpace(owner)
	tabID = strings.TrimSpace(tabID)
	if owner == "" || tabID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lease := m.byTab[tabID]
	if lease == nil || lease.owner != owner {
		return
	}
	delete(m.byTab, tabID)
	if m.defaultTabs[owner] != tabID {
		return
	}
	delete(m.defaultTabs, owner)
	var newest string
	var newestAt time.Time
	for candidate, other := range m.byTab {
		if other.owner == owner && (newest == "" || other.updatedAt.After(newestAt)) {
			newest = candidate
			newestAt = other.updatedAt
		}
	}
	if newest != "" {
		m.defaultTabs[owner] = newest
	}
}

func (m *tabLeaseManager) annotate(owner string, tabs []browser.Tab) []browser.Tab {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(now)
	out := make([]browser.Tab, len(tabs))
	copy(out, tabs)
	for i := range out {
		lease := m.byTab[out[i].ID]
		switch {
		case lease == nil:
			out[i].Lease = &browser.TabLeaseInfo{Status: "available"}
		case owner != "" && lease.owner == owner:
			info := &browser.TabLeaseInfo{Status: "mine", Mine: true, ExpiresAt: heldUntil(lease).UTC().Format(time.RFC3339)}
			// Drift: the daemon grouped this tab into the agent's lane, but it is
			// no longer there (a human dragged it out, or Chrome rearranged the
			// strip). Ownership is unchanged — leases enforce that — so this is a
			// courtesy signal the agent may act on with brw_group_tabs.
			if lease.groupID != "" && out[i].GroupID != lease.groupID {
				info.GroupDrift = true
				info.ExpectedGroupID = lease.groupID
			}
			out[i].Lease = info
		default:
			out[i].Lease = &browser.TabLeaseInfo{Status: "leased", ExpiresAt: heldUntil(lease).UTC().Format(time.RFC3339)}
		}
	}
	return out
}

// ownsTab reports whether owner currently holds the lease on tabID. Used to keep
// one session's recorded actions out of another session's trace.
func (m *tabLeaseManager) ownsTab(owner, tabID string) bool {
	if owner == "" || tabID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(m.now())
	lease := m.byTab[tabID]
	return lease != nil && lease.owner == owner
}

func (m *tabLeaseManager) stats() map[string]any {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(now)
	owners := make(map[string]struct{})
	inFlight := 0
	for _, lease := range m.byTab {
		owners[lease.owner] = struct{}{}
		inFlight += lease.inFlight
	}
	return map[string]any{
		"active_tabs": len(m.byTab), "owners": len(owners),
		"in_flight": inFlight, "ttl_seconds": int64(m.ttl / time.Second),
	}
}

func requestLeaseOwner(r *http.Request) string {
	if owner := usagelog.SafeID(r.Header.Get(usagelog.HeaderOwnerID)); owner != "" {
		return owner
	}
	return usagelog.SafeID(r.Header.Get(usagelog.HeaderSessionID))
}

func leaseOwner(ctx context.Context) string {
	owner, _ := ctx.Value(leaseContextKey{}).(string)
	return owner
}

func leaseScopedPath(path string) bool {
	if path == "/api/artifacts/capture" || path == "/api/recipes/run" {
		return true
	}
	if strings.HasPrefix(path, "/api/visual/") || path == "/api/browser/emulate_device" {
		return true
	}
	if !strings.HasPrefix(path, "/api/page/") {
		return false
	}
	switch path {
	case "/api/page/trace", "/api/page/clear_trace":
		return false
	default:
		return true
	}
}

func requestTabID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if tabID := strings.TrimSpace(r.URL.Query().Get("tab_id")); tabID != "" {
		return tabID, true
	}
	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return "", true
	}
	body := r.Body
	data, err := io.ReadAll(io.LimitReader(body, maxRequestBodyBytes+1))
	_ = body.Close()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return "", false
	}
	if len(data) > maxRequestBodyBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body too large"})
		return "", false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	var envelope struct {
		TabID string `json:"tab_id"`
	}
	// Leave malformed JSON to the endpoint's normal decoder so its established
	// error shape and validation behaviour remain unchanged.
	_ = json.Unmarshal(data, &envelope)
	return strings.TrimSpace(envelope.TabID), true
}

func (s *Server) leaseMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := requestLeaseOwner(r)
		ctx := context.WithValue(r.Context(), leaseContextKey{}, owner)
		ctx = context.WithValue(ctx, agentNameContextKey{}, requestAgentName(r))
		r = r.WithContext(ctx)
		if owner == "" || !leaseScopedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		explicitTabID, ok := requestTabID(w, r)
		if !ok {
			return
		}
		tabID, release, err := s.leases.resolveOrOpen(r.Context(), owner, explicitTabID, func(ctx context.Context) (browser.OpenResult, error) {
			// The session's isolated working tab opens straight into its per-agent
			// tab group, so each concurrent agent gets a named lane in the strip.
			result, err := s.openInOwnerGroup(ctx, "about:blank", owner)
			if err == nil && ctx.Err() != nil {
				s.closeAbandonedTab(ctx, result.Tab.ID)
				return browser.OpenResult{}, ctx.Err()
			}
			return result, err
		})
		if err != nil {
			writeLeaseError(w, err)
			return
		}
		defer release()
		ctx = browser.WithImplicitTabID(r.Context(), tabID)
		if explicitTabID != "" {
			ctx = browser.WithTabID(r.Context(), tabID)
		}
		capture := &leaseStatusResponseWriter{ResponseWriter: w}
		next.ServeHTTP(capture, r.WithContext(ctx))
		if capture.status >= http.StatusBadRequest && capture.Header().Get(usagelog.HeaderErrorClass) == "tab_lost" {
			// A human may close an agent tab outside brw. Drop the stale default so
			// the session's next operation allocates a new isolated working tab.
			s.leases.release(owner, tabID)
		}
	})
}

type leaseStatusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *leaseStatusResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *leaseStatusResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func writeLeaseError(w http.ResponseWriter, err error) {
	if _, ok := err.(*tabLeaseConflictError); !ok {
		writeError(w, err)
		return
	}
	w.Header().Set(usagelog.HeaderErrorClass, "tab_contended")
	w.Header().Set(usagelog.HeaderErrorFingerprint, usagelog.Fingerprint(err.Error()))
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": err.Error(), "code": "tab_contended", "retryable": false,
	})
}
