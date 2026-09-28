package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// SetDefaultGroup configures the tab-group title brw_open lands in when the
// caller passes no group of its own (empty disables default grouping). Call
// before serving.
func (b *Bridge) SetDefaultGroup(name string) { b.defaultGroup = strings.TrimSpace(name) }

// SetRaiseWindowOnFocus configures whether focus_tab raises the Chrome window to
// the OS foreground. The daemon sets this to false so automation never steals
// the user's focus. Call before serving.
func (b *Bridge) SetRaiseWindowOnFocus(v bool) { b.raiseWindowOnFocus = v }

// SetFollowFocus selects how a no-tab_id action resolves its target tab. Pass
// false (the daemon default) for isolation — brw works on tabs it owns and opens
// a fresh background tab rather than touching the user's focused tab. Pass true
// for the legacy behavior where brw follows the user's manually-focused tab. Call
// before serving. See the followFocus field for the full contract.
func (b *Bridge) SetFollowFocus(v bool) { b.followFocus = v }

// openTabParams stamps the foreground/background intent onto an open_tab request.
// In isolation (followFocus=false) brw opens its tab in the BACKGROUND
// (active:false) so it never switches the tab the user is looking at; the
// extension still pins the new tab as brw's working target and the daemon
// resolves it by id. In follow-focus mode the tab opens active so the user's view
// follows the agent — the legacy behavior. An extension that predates the active
// flag treats a missing/true value as active, so this is backward compatible.
func (b *Bridge) openTabParams(params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["active"] = b.followFocus
	// The extension creates the tab blank, arms the inline-document rewrite and
	// only then navigates, so an attachment-flagged text response is the tab's
	// first document rather than an empty tab plus a file.
	params["inlineDocument"] = true
	if b.webmcp {
		params["webmcp"] = snapshot.WebMCPInstallScript
	}
	return params
}

func (b *Bridge) Open(ctx context.Context, url string) (browser.OpenResult, error) {
	// Corral the agent's tabs into one labelled group by default so they don't
	// scatter loose across the user's window. An explicit group from the caller
	// (OpenInGroup) always wins; this only fills the no-group case.
	if group := b.defaultGroup; group != "" {
		return b.OpenInGroup(ctx, url, browser.TabGroupOptions{Name: group})
	}
	start := time.Now()
	var err error
	url, err = b.prepareNavigationURL(url)
	if err != nil {
		return browser.OpenResult{}, err
	}
	raw, err := b.call(ctx, "open_tab", b.openTabParams(map[string]any{"url": url}))
	if err != nil {
		return browser.OpenResult{}, err
	}
	var tab extTab
	if err := json.Unmarshal(raw, &tab); err != nil {
		return browser.OpenResult{}, err
	}
	out := tab.toBrowserTab()
	if out.ID == "" {
		return browser.OpenResult{}, errors.New("open_tab returned no tab id")
	}
	// The tab exists from here on, so every exit records what became of it.
	// Earlier failures create no tab and are not recorded: an entry without a
	// tab id would be an unscoped URL.
	recordOpen := func(finalURL string, err error) {
		if finalURL == "" {
			finalURL = url
		}
		b.recordObservation(out.ID, browser.TraceActionOpen, finalURL, start, err)
	}
	b.setActiveTabID(out.ID)
	// open_tab creates and navigates in one extension call, so containment is
	// armed immediately AFTER rather than before the first document. Fetch
	// interception covers that document's subresources from here; only scripts
	// that already ran in it can hold pristine WebSocket/RTC references.
	b.ensureContainment(ctx, out.ID)
	b.noteOpenedWebMCP(ctx, out.ID, tab.WebMCPArmed)
	ready := b.waitOpenReady(ctx, url, out.ID)
	b.disarmInlineDocument(out.ID)
	// Re-read the tab after commit so the agent gets a real url/title instead of
	// the empty fields chrome.tabs.create often returns mid-navigation.
	out = b.refreshOpenedTab(ctx, out, url)
	result := b.openResult(ctx, out, url, ready)
	// In isolation we resolve no-tab_id actions by the owned id (b.active), so the
	// opened tab need not be foregrounded — keeping it in the background means the
	// open never disturbs the tab the user is on. Only follow-focus mode, where
	// later actions chase the live foreground tab, must make the new tab current.
	if b.followFocus {
		if err := b.ensureForegroundTab(ctx, out.ID); err != nil {
			recordOpen(out.URL, err)
			return result, err
		}
	}
	if err := b.verifyOpenedTabURL(ctx, out.ID); err != nil {
		recordOpen(out.URL, err)
		return browser.OpenResult{}, err
	}
	recordOpen(out.URL, result.NavigationErr())
	return result, nil
}

// waitOpenReady blocks until the freshly opened tab is usable, matching the
// direct-CDP Open() contract so an immediate brw_evaluate / brw_read on
// the new tab doesn't race the transient about:blank Chrome reports before the
// real navigation commits. Returns whether readiness was confirmed; a wait
// timeout is not fatal (the tab still exists), it just reports ready=false. The
// wait targets the specific new tab id (not the resolved active tab) because
// open_tab creates the tab inactive, so the focused tab is still the old one.
func (b *Bridge) waitOpenReady(ctx context.Context, url, tabID string) bool {
	if tabID == "" {
		return false
	}
	waitCtx := browser.WithTabID(ctx, tabID)
	if url == "about:blank" {
		return b.WaitFor(waitCtx, "ready", 5*time.Second) == nil
	}
	return b.WaitFor(waitCtx, "committed", 10*time.Second) == nil
}

func (b *Bridge) ListTabs(ctx context.Context) ([]browser.Tab, error) {
	raw, err := b.call(ctx, "list_tabs", nil)
	if err != nil {
		return nil, err
	}
	var tabs []extTab
	if err := json.Unmarshal(raw, &tabs); err != nil {
		return nil, err
	}
	out := make([]browser.Tab, 0, len(tabs))
	activeID := ""
	fallbackActiveID := ""
	hasFocusedWindow := false
	for _, tab := range tabs {
		outTab := tab.toBrowserTab()
		out = append(out, outTab)
		if outTab.WindowFocused {
			hasFocusedWindow = true
		}
		if outTab.Active && outTab.WindowFocused {
			activeID = outTab.ID
		}
		if fallbackActiveID == "" && outTab.Active {
			fallbackActiveID = outTab.ID
		}
	}
	if activeID == "" && !hasFocusedWindow && b.activeTabID() == "" {
		activeID = fallbackActiveID
	}
	// Only let a list refresh repoint the cached active id in follow-focus mode.
	// In isolation that cache is brw's OWNED tab; the user's focused tab (what
	// activeID reflects here) must not overwrite it, or the next no-tab_id action
	// would land on the user's tab.
	if activeID != "" && b.followFocus {
		b.setActiveTabID(activeID)
	}
	return out, nil
}

func (b *Bridge) ListTabGroups(ctx context.Context) ([]browser.TabGroup, error) {
	raw, err := b.call(ctx, "list_tab_groups", nil)
	if err != nil {
		return nil, err
	}
	var groups []extTabGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, err
	}
	out := make([]browser.TabGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, group.toBrowserTabGroup())
	}
	return out, nil
}

func (b *Bridge) FocusTab(ctx context.Context, id string) error {
	tabID, err := requireTabID(id)
	if err != nil {
		return err
	}
	start := time.Now()
	raw, err := b.call(ctx, "focus_tab", map[string]any{"tabId": tabID, "raiseWindow": b.raiseWindowOnFocus})
	if err != nil {
		b.recordObservation(strings.TrimSpace(id), browser.TraceActionFocusTab, "", start, err)
		return err
	}
	var tab extTab
	if err := json.Unmarshal(raw, &tab); err == nil && tab.ID != 0 {
		focused := strconv.Itoa(tab.ID)
		b.setActiveTabID(focused)
		b.recordObservation(focused, browser.TraceActionFocusTab, "", start, nil)
		return nil
	}
	// Unmarshal failed or returned zero ID; fall through to use the original
	// id. The focus_tab call succeeded, so the focus did happen — we just
	// cannot confirm the tab metadata.
	if strings.TrimSpace(id) != "" {
		b.setActiveTabID(id)
	}
	b.recordObservation(strings.TrimSpace(id), browser.TraceActionFocusTab, "", start, nil)
	return nil
}

func (b *Bridge) CloseTab(ctx context.Context, id string) error {
	tabID, err := requireTabID(id)
	if err != nil {
		return err
	}
	tabKey := strings.TrimSpace(id)
	start := time.Now()
	_, err = b.call(ctx, "close_tab", map[string]any{"tabId": tabID})
	if err != nil && b.tabGoneAfterFailedClose(ctx, tabID) {
		err = nil
	}
	// Recorded before the tab state is dropped: the entry is scoped by the
	// tab's lease, and forgetting the tab first leaves the close unattributable.
	b.recordObservation(tabKey, browser.TraceActionCloseTab, "", start, err)
	if err == nil {
		b.invalidateTabState(tabKey)
	}
	return err
}

// closeTabVerifyWindow bounds how long CloseTab keeps checking the tab list
// after the extension reported a failed close.
const (
	closeTabVerifyWindow   = 3 * time.Second
	closeTabVerifyInterval = 250 * time.Millisecond
)

// tabGoneAfterFailedClose reports whether tabID has left the browser even
// though close_tab returned an error. The extension's error means removal was
// not observed inside its budget, not that the tab survived: Chrome can finish
// an accepted close after the budget, and a tab that was already gone fails
// the close with "No tab with id". The close's post-condition is that the tab
// no longer exists, so that is what decides the outcome.
func (b *Bridge) tabGoneAfterFailedClose(ctx context.Context, tabID int) bool {
	deadline := time.Now().Add(closeTabVerifyWindow)
	for {
		if ctx.Err() != nil {
			return false
		}
		present, err := b.tabListed(ctx, tabID)
		if err != nil {
			return false
		}
		if !present {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		timer := time.NewTimer(closeTabVerifyInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// tabListed asks the extension whether tabID is still in the browser. It reads
// the raw list rather than going through ListTabs, which also refreshes the
// cached active tab.
func (b *Bridge) tabListed(ctx context.Context, tabID int) (bool, error) {
	raw, err := b.call(ctx, "list_tabs", nil)
	if err != nil {
		return false, err
	}
	var tabs []struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &tabs); err != nil {
		return false, err
	}
	for _, tab := range tabs {
		if tab.ID == tabID {
			return true, nil
		}
	}
	return false, nil
}

func (b *Bridge) GroupTabs(ctx context.Context, tabIDs []string, opts browser.TabGroupOptions) error {
	ids := make([]int, 0, len(tabIDs))
	for _, id := range tabIDs {
		ids = append(ids, parseTabID(id))
	}
	params := map[string]any{
		"tabIds": ids,
		"name":   opts.Name,
		"color":  opts.Color,
	}
	if opts.GroupID != "" {
		groupID, err := requireGroupID(opts.GroupID)
		if err != nil {
			return err
		}
		params["groupId"] = groupID
	}
	_, err := b.call(ctx, "group_tabs", params)
	return err
}

func (b *Bridge) UngroupTabs(ctx context.Context, tabIDs []string) error {
	ids := make([]int, 0, len(tabIDs))
	for _, id := range tabIDs {
		ids = append(ids, parseTabID(id))
	}
	_, err := b.call(ctx, "ungroup_tabs", map[string]any{"tabIds": ids})
	return err
}

func (b *Bridge) OpenInGroup(ctx context.Context, url string, opts browser.TabGroupOptions) (browser.OpenResult, error) {
	start := time.Now()
	var err error
	url, err = b.prepareNavigationURL(url)
	if err != nil {
		return browser.OpenResult{}, err
	}
	params := map[string]any{"url": url}
	if opts.GroupID != "" {
		groupID, err := requireGroupID(opts.GroupID)
		if err != nil {
			return browser.OpenResult{}, err
		}
		params["groupId"] = groupID
	}
	if opts.Name != "" {
		params["groupName"] = opts.Name
	}
	if opts.Color != "" {
		params["groupColor"] = opts.Color
	}
	raw, err := b.call(ctx, "open_tab", b.openTabParams(params))
	if err != nil {
		return browser.OpenResult{}, err
	}
	var tab extTab
	if err := json.Unmarshal(raw, &tab); err != nil {
		return browser.OpenResult{}, err
	}
	out := tab.toBrowserTab()
	if out.ID == "" {
		return browser.OpenResult{}, errors.New("open_tab returned no tab id")
	}
	recordOpen := func(finalURL string, err error) {
		if finalURL == "" {
			finalURL = url
		}
		b.recordObservation(out.ID, browser.TraceActionOpen, finalURL, start, err)
	}
	b.setActiveTabID(out.ID)
	b.ensureContainment(ctx, out.ID)
	b.noteOpenedWebMCP(ctx, out.ID, tab.WebMCPArmed)
	ready := b.waitOpenReady(ctx, url, out.ID)
	b.disarmInlineDocument(out.ID)
	// Same rehydrate as Open — agents need url/title on the open observation.
	out = b.refreshOpenedTab(ctx, out, url)
	b.recordTabGroupDegradation(out.GroupWarning)
	result := b.openResult(ctx, out, url, ready)
	// See Open: in isolation the owned id drives resolution, so we leave the tab in
	// the background and never steal the user's current tab; only follow-focus mode
	// foregrounds it.
	if b.followFocus {
		if err := b.ensureForegroundTab(ctx, out.ID); err != nil {
			recordOpen(out.URL, err)
			return result, err
		}
	}
	if err := b.verifyOpenedTabURL(ctx, out.ID); err != nil {
		recordOpen(out.URL, err)
		return browser.OpenResult{}, err
	}
	recordOpen(out.URL, result.NavigationErr())
	return result, nil
}

// refreshOpenedTab re-lists tabs after open readiness so the returned Tab carries
// the committed URL/title (chrome.tabs.create often reports empty strings). Falls
// back to the requested URL when the live tab record is still blank.
func (b *Bridge) refreshOpenedTab(ctx context.Context, tab browser.Tab, requestedURL string) browser.Tab {
	tabs, err := b.ListTabs(ctx)
	if err == nil {
		for _, live := range tabs {
			if live.ID != tab.ID {
				continue
			}
			// Prefer live url/title; keep group metadata from either side.
			if live.URL != "" {
				tab.URL = live.URL
			}
			if live.Title != "" {
				tab.Title = live.Title
			}
			if live.GroupID != "" {
				tab.GroupID = live.GroupID
			}
			if live.GroupTitle != "" {
				tab.GroupTitle = live.GroupTitle
			}
			if live.GroupColor != "" {
				tab.GroupColor = live.GroupColor
			}
			tab.GroupCollapsed = live.GroupCollapsed
			tab.Active = live.Active
			tab.Highlighted = live.Highlighted
			tab.WindowFocused = live.WindowFocused
			if live.WindowID != 0 {
				tab.WindowID = live.WindowID
			}
			if live.WindowType != "" {
				tab.WindowType = live.WindowType
			}
			break
		}
	}
	if tab.URL == "" && requestedURL != "" && requestedURL != "about:blank" {
		tab.URL = requestedURL
	}
	return tab
}

type extTab struct {
	ID             int    `json:"id"`
	URL            string `json:"url"`
	Title          string `json:"title"`
	Active         bool   `json:"active"`
	Highlighted    bool   `json:"highlighted"`
	WindowID       int    `json:"windowId"`
	WindowFocused  bool   `json:"windowFocused"`
	WindowType     string `json:"windowType"`
	GroupID        *int   `json:"groupId"`
	GroupTitle     string `json:"groupTitle"`
	GroupColor     string `json:"groupColor"`
	GroupCollapsed bool   `json:"groupCollapsed"`
	GroupWarning   string `json:"groupWarning"`
	WebMCPArmed    bool   `json:"webmcpArmed"`
	OpenerTabID    int    `json:"openerTabId"`
	Discarded      bool   `json:"discarded"`
	Frozen         bool   `json:"frozen"`
}

func (t extTab) toBrowserTab() browser.Tab {
	windowType := strings.TrimSpace(t.WindowType)
	tabType := "page"
	if windowType == "popup" {
		tabType = "popup"
	}
	openerID := ""
	if t.OpenerTabID != 0 {
		openerID = strconv.Itoa(t.OpenerTabID)
	}
	return browser.Tab{
		ID:             strconv.Itoa(t.ID),
		URL:            t.URL,
		Title:          t.Title,
		Type:           tabType,
		WindowID:       t.WindowID,
		WindowType:     windowType,
		GroupID:        groupIDString(t.GroupID),
		GroupTitle:     t.GroupTitle,
		GroupColor:     t.GroupColor,
		GroupCollapsed: t.GroupCollapsed,
		GroupWarning:   t.GroupWarning,
		Active:         t.Active,
		Highlighted:    t.Highlighted,
		WindowFocused:  t.WindowFocused,
		OpenerTabID:    openerID,
		Popup:          windowType == "popup" || t.OpenerTabID != 0,
		Discarded:      t.Discarded,
		Frozen:         t.Frozen,
	}
}

type extTabGroup struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Color     string `json:"color"`
	Collapsed bool   `json:"collapsed"`
	WindowID  int    `json:"windowId"`
	TabIDs    []int  `json:"tabIds"`
	TabCount  int    `json:"tabCount"`
}

func (g extTabGroup) toBrowserTabGroup() browser.TabGroup {
	tabIDs := make([]string, 0, len(g.TabIDs))
	for _, id := range g.TabIDs {
		tabIDs = append(tabIDs, strconv.Itoa(id))
	}
	return browser.TabGroup{
		ID:        strconv.Itoa(g.ID),
		Title:     g.Title,
		Color:     g.Color,
		Collapsed: g.Collapsed,
		WindowID:  g.WindowID,
		TabIDs:    tabIDs,
		TabCount:  g.TabCount,
	}
}

func groupIDString(id *int) string {
	if id == nil || *id < 0 {
		return ""
	}
	return strconv.Itoa(*id)
}

func (b *Bridge) activeTabID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.active
}

func (b *Bridge) setActiveTabID(id string) {
	b.mu.Lock()
	b.active = strings.TrimSpace(id)
	b.mu.Unlock()
}

func actionTargets(tabs []browser.Tab, activeID string, limit int) []browser.Tab {
	if limit <= 0 || len(tabs) == 0 {
		return nil
	}
	out := make([]browser.Tab, 0, min(limit, len(tabs)))
	seen := map[string]bool{}
	add := func(tab browser.Tab) {
		if tab.ID == "" || seen[tab.ID] || len(out) >= limit {
			return
		}
		seen[tab.ID] = true
		out = append(out, tab)
	}
	for _, tab := range tabs {
		if tab.ID == activeID {
			add(tab)
		}
	}
	for _, tab := range tabs {
		if tab.Popup || tab.WindowType == "popup" {
			add(tab)
		}
	}
	for _, tab := range tabs {
		if tab.Active && tab.WindowFocused {
			add(tab)
		}
	}
	for _, tab := range tabs {
		if tab.Active {
			add(tab)
		}
	}
	return out
}

func parseTabID(id string) int {
	n, _ := strconv.Atoi(id)
	return n
}

// requireTabID validates a caller-supplied tab id for operations that target a
// specific tab (focus/close). An empty or non-numeric id used to be silently
// coerced to 0 by parseTabID, which the extension rejected with the opaque "No
// tab with id: 0" — surfacing here as a clear, actionable error instead so a
// batched script fails loudly at the offending step.
func requireTabID(id string) (int, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return 0, errors.New("tab id is required")
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid tab id %q", id)
	}
	return n, nil
}

func requireGroupID(id string) (int, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return 0, errors.New("group id is required")
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid group id %q", id)
	}
	return n, nil
}
