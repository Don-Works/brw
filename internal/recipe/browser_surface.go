package recipe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// API is implemented by a local Service and by the upstream HTTP controller.
type API interface {
	SearchRecipes(context.Context, string, string, int) ([]Match, error)
	RunRecipe(context.Context, RunRequest) (RunResult, error)
}

type BrowserSurface struct {
	Browser     browser.Controller
	Artifacts   artifact.API
	downloadsMu sync.Mutex
	downloads   []cachedDownload
}

type cachedDownload struct {
	tabID string
	entry browser.DownloadEntry
}

const maxCachedRecipeDownloads = 200

const (
	initialEventPollInterval = 25 * time.Millisecond
	maximumEventPollInterval = 250 * time.Millisecond
)

func (s *BrowserSurface) Origin(ctx context.Context) (string, error) {
	if provider, ok := s.Browser.(browser.DocumentIdentityProvider); ok {
		identity, err := provider.DocumentIdentity(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", ctxErr
			}
			return "", errors.New("could not verify the browser's main-document origin")
		}
		if identity.ID == "" || identity.Origin == "" || identity.Origin == "null" {
			return "", errors.New("browser returned an invalid main-document identity")
		}
		return identity.Origin, nil
	}
	// Compatibility path for custom/in-memory Controller implementations. Both
	// production transports implement DocumentIdentityProvider, whose browser
	// metadata cannot be shadowed by page JavaScript.
	value, err := s.Browser.Evaluate(ctx, `location.origin`)
	if err != nil {
		return "", err
	}
	origin, ok := value.(string)
	if !ok || origin == "" || origin == "null" {
		return "", errors.New("current page has no executable origin")
	}
	return origin, nil
}

func (s *BrowserSurface) Resolve(ctx context.Context, target Target) ([]ResolvedElement, error) {
	query := target.Name
	if query == "" {
		query = target.NameContains
	}
	if query == "" {
		query = target.TestID
	}
	if query == "" {
		query = target.HrefContains
	}
	result, err := s.Browser.Find(ctx, snapshot.FindOptions{
		Query: query, Role: target.Role, Limit: 200, ViewportOnly: false,
		IncludeHidden: target.Visible != nil && !*target.Visible,
	})
	if err != nil {
		return nil, err
	}
	if truncated, _ := result.Metadata["truncated"].(bool); truncated {
		return nil, errors.New("semantic target search was truncated; refine the recipe target before acting")
	}
	matches := make([]ResolvedElement, 0, len(result.Elements))
	for _, element := range result.Elements {
		if element.Role != target.Role ||
			target.Name != "" && element.Name != target.Name ||
			target.NameContains != "" && !strings.Contains(element.Name, target.NameContains) ||
			target.TestID != "" && element.TestID != target.TestID ||
			target.HrefContains != "" && !strings.Contains(element.Href, target.HrefContains) ||
			target.Visible != nil && element.Visible != *target.Visible {
			continue
		}
		matches = append(matches, ResolvedElement{Ref: element.Ref, Role: element.Role, Name: element.Name})
	}
	return matches, nil
}

func (s *BrowserSurface) Click(ctx context.Context, ref string) error {
	_, err := s.Browser.Click(ctx, ref)
	return err
}

func (s *BrowserSurface) Fill(ctx context.Context, ref, value string) error {
	_, err := s.Browser.Fill(ctx, snapshot.FillOptions{Ref: ref, Text: value, Replace: true})
	return err
}

func (s *BrowserSurface) Type(ctx context.Context, ref, value string) error {
	_, err := s.Browser.Type(ctx, ref, value)
	return err
}

func (s *BrowserSurface) Select(ctx context.Context, ref, value string) error {
	_, err := s.Browser.Select(ctx, ref, value)
	return err
}

type refFocuser interface {
	FocusRef(context.Context, string) error
}

func (s *BrowserSurface) Press(ctx context.Context, ref, key string) error {
	focuser, ok := s.Browser.(refFocuser)
	if !ok {
		return errors.New("deterministic targeted key press is unavailable on this browser transport")
	}
	if err := focuser.FocusRef(ctx, ref); err != nil {
		return err
	}
	_, err := s.Browser.Press(ctx, key)
	return err
}

func (s *BrowserSurface) NavigateTo(ctx context.Context, url string) error {
	_, err := s.Browser.NavigateTo(ctx, url)
	return err
}

// ArmEvent prepares event sources before the causative action. DOM/URL/text
// conditions are durable state and can be checked afterward; network/download
// sources must be enabled and baselined first, while tab events need a baseline
// so an already-open matching tab cannot produce a false acknowledgement.
func (s *BrowserSurface) ArmEvent(ctx context.Context, event Event) (func(context.Context) error, error) {
	switch event.Kind {
	case "network.response":
		baselineRequests, err := s.Browser.NetworkCapture(ctx, event.Match)
		if err != nil {
			return nil, err
		}
		// Drains intentionally retain in-flight rows so a slow response cannot be
		// lost between polls. Capture their stable lifecycle IDs at arm time and
		// ignore only those exact pre-action requests when they later complete.
		// IDs include a per-document epoch, so a navigation resetting the local
		// sequence cannot collide with this baseline.
		baseline := make(map[string]struct{}, len(baselineRequests))
		for _, request := range baselineRequests {
			if request.CaptureID != "" && !request.Completed {
				baseline[request.CaptureID] = struct{}{}
			}
		}
		return func(waitCtx context.Context) error { return s.waitNetwork(waitCtx, event, baseline) }, nil
	case "download.completed":
		baseline, err := s.Browser.Downloads(ctx)
		if err != nil {
			return nil, err
		}
		if !baseline.Supported {
			return nil, downloadsUnsupportedError(baseline.Note)
		}
		// Intentionally discard the pre-arm baseline result. Only a download changed
		// after the action may satisfy or feed a capture step. Clear this tab's
		// prior recipe cache too, so a later filename selector cannot fall back to
		// an older same-name download after a different fresh event.
		s.clearCompletedDownloads(ctx)
		return func(waitCtx context.Context) error { return s.waitDownload(waitCtx, event) }, nil
	case "tab.opened":
		tabs, err := s.Browser.ListTabs(ctx)
		if err != nil {
			return nil, err
		}
		baseline := make(map[string]bool, len(tabs))
		for _, tab := range tabs {
			baseline[tab.ID] = true
		}
		return func(waitCtx context.Context) error { return s.waitTab(waitCtx, event, baseline) }, nil
	default:
		return func(waitCtx context.Context) error { return s.WaitEvent(waitCtx, event) }, nil
	}
}

func (s *BrowserSurface) EventSatisfied(ctx context.Context, event Event) (bool, error) {
	switch event.Kind {
	case "network.response", "download.completed", "tab.opened":
		return false, nil
	case "element.visible", "element.hidden":
		matches, err := s.Resolve(ctx, *event.Target)
		if err != nil {
			return false, err
		}
		if event.Kind == "element.hidden" {
			return len(matches) == 0, nil
		}
		if len(matches) > 1 {
			return false, fmt.Errorf("element.visible target resolved to %d elements; refusing to guess", len(matches))
		}
		return len(matches) == 1, nil
	case "element.value", "element.value_contains":
		return s.elementValueSatisfied(ctx, event)
	case "page.ready":
		return s.evaluateBool(ctx, `document.readyState==='complete'||document.readyState==='interactive'`)
	case "url.matches", "text.present", "text.absent":
		matchJSON, _ := json.Marshal(event.Match)
		expression := ""
		switch event.Kind {
		case "url.matches":
			expression = `location.href.includes(` + string(matchJSON) + `)`
		case "text.present":
			expression = `!!document.body&&document.body.innerText.includes(` + string(matchJSON) + `)`
		case "text.absent":
			expression = `!document.body||!document.body.innerText.includes(` + string(matchJSON) + `)`
		}
		return s.evaluateBool(ctx, expression)
	default:
		return false, fmt.Errorf("unsupported event %q", event.Kind)
	}
}

func (s *BrowserSurface) evaluateBool(ctx context.Context, expression string) (bool, error) {
	value, err := s.Browser.Evaluate(ctx, expression)
	if err != nil {
		return false, err
	}
	result, ok := value.(bool)
	if !ok {
		return false, errors.New("browser returned a non-boolean postcondition preflight")
	}
	return result, nil
}

func (s *BrowserSurface) WaitEvent(ctx context.Context, event Event) error {
	timeout := time.Duration(event.TimeoutMS) * time.Millisecond
	switch event.Kind {
	case "page.ready":
		return s.Browser.WaitFor(ctx, "ready", timeout)
	case "url.matches":
		return s.Browser.WaitFor(ctx, "url:"+event.Match, timeout)
	case "text.present":
		return s.Browser.WaitFor(ctx, "text:"+event.Match, timeout)
	case "text.absent":
		return s.Browser.WaitFor(ctx, "not_text:"+event.Match, timeout)
	case "element.visible", "element.hidden":
		return s.waitElement(ctx, event)
	case "element.value", "element.value_contains":
		return s.waitElementValue(ctx, event)
	case "download.completed":
		if _, err := s.Browser.Downloads(ctx); err != nil {
			return err
		}
		return s.waitDownload(ctx, event)
	case "tab.opened":
		tabs, err := s.Browser.ListTabs(ctx)
		if err != nil {
			return err
		}
		baseline := make(map[string]bool, len(tabs))
		for _, tab := range tabs {
			baseline[tab.ID] = true
		}
		return s.waitTab(ctx, event, baseline)
	case "network.response":
		// The first call installs the bounded in-page capture if it was not
		// already armed; following calls drain matching completed responses.
		return s.waitNetwork(ctx, event, nil)
	default:
		return fmt.Errorf("unsupported event %q", event.Kind)
	}
}

func (s *BrowserSurface) waitDownload(ctx context.Context, event Event) error {
	return s.poll(ctx, time.Duration(event.TimeoutMS)*time.Millisecond, func(callCtx context.Context) (bool, error) {
		result, err := s.Browser.Downloads(callCtx)
		if err != nil {
			return false, err
		}
		if !result.Supported {
			return false, downloadsUnsupportedError(result.Note)
		}
		s.cacheCompletedDownloads(callCtx, result.Downloads)
		for _, item := range result.Downloads {
			if item.State == "completed" && (strings.Contains(item.SuggestedFilename, event.Match) || strings.Contains(item.URL, event.Match)) {
				return true, nil
			}
		}
		return false, nil
	})
}

func downloadsUnsupportedError(note string) error {
	if note = strings.TrimSpace(note); note != "" {
		return errors.New(note)
	}
	return errors.New("download events are unavailable on this browser transport")
}

func (s *BrowserSurface) waitTab(ctx context.Context, event Event, baseline map[string]bool) error {
	return s.poll(ctx, time.Duration(event.TimeoutMS)*time.Millisecond, func(callCtx context.Context) (bool, error) {
		tabs, err := s.Browser.ListTabs(callCtx)
		if err != nil {
			return false, err
		}
		for _, tab := range tabs {
			if baseline != nil && baseline[tab.ID] {
				continue
			}
			if strings.Contains(tab.URL, event.Match) || strings.Contains(tab.Title, event.Match) {
				return true, nil
			}
		}
		return false, nil
	})
}

func (s *BrowserSurface) waitNetwork(ctx context.Context, event Event, baseline map[string]struct{}) error {
	return s.poll(ctx, time.Duration(event.TimeoutMS)*time.Millisecond, func(callCtx context.Context) (bool, error) {
		requests, err := s.Browser.NetworkCapture(callCtx, event.Match)
		if err != nil {
			return false, err
		}
		for _, request := range requests {
			if _, existedBeforeAction := baseline[request.CaptureID]; request.CaptureID != "" && existedBeforeAction {
				continue
			}
			if strings.Contains(request.URL, event.Match) && request.Status > 0 {
				return true, nil
			}
		}
		return false, nil
	})
}

func (s *BrowserSurface) waitElement(ctx context.Context, event Event) error {
	timeout := time.Duration(event.TimeoutMS) * time.Millisecond
	return s.poll(ctx, timeout, func(callCtx context.Context) (bool, error) {
		matches, err := s.Resolve(callCtx, *event.Target)
		if err != nil {
			return false, err
		}
		if event.Kind == "element.hidden" {
			return len(matches) == 0, nil
		}
		if len(matches) > 1 {
			return false, fmt.Errorf("element.visible target resolved to %d elements; refusing to guess", len(matches))
		}
		return len(matches) == 1, nil
	})
}

func (s *BrowserSurface) waitElementValue(ctx context.Context, event Event) error {
	return s.poll(ctx, time.Duration(event.TimeoutMS)*time.Millisecond, func(callCtx context.Context) (bool, error) {
		return s.elementValueSatisfied(callCtx, event)
	})
}

func (s *BrowserSurface) elementValueSatisfied(ctx context.Context, event Event) (bool, error) {
	matches, err := s.Resolve(ctx, *event.Target)
	if err != nil {
		return false, err
	}
	if len(matches) == 0 {
		return false, nil
	}
	if len(matches) != 1 {
		return false, fmt.Errorf("element.value target resolved to %d elements; refusing to guess", len(matches))
	}
	var assertionErr error
	if event.Kind == "element.value_contains" {
		asserter, ok := s.Browser.(interface {
			AssertValueContains(context.Context, string, string, time.Duration) error
		})
		if !ok {
			return false, errors.New("element.value_contains is unavailable on this browser transport")
		}
		assertionErr = asserter.AssertValueContains(ctx, matches[0].Ref, event.Match, time.Millisecond)
	} else {
		assertionErr = s.Browser.AssertValue(ctx, matches[0].Ref, event.Match, time.Millisecond)
	}
	if assertionErr != nil {
		if strings.Contains(assertionErr.Error(), "assertion did not pass within timeout") {
			return false, nil
		}
		return false, assertionErr
	}
	return true, nil
}

func (s *BrowserSurface) poll(ctx context.Context, timeout time.Duration, check func(context.Context) (bool, error)) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	interval := initialEventPollInterval
	for {
		if err := deadlineCtx.Err(); err != nil {
			return fmt.Errorf("event timed out: %w", err)
		}
		matched, err := check(deadlineCtx)
		if err != nil {
			return err
		}
		if matched {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
			interval = nextEventPollInterval(interval)
		case <-deadlineCtx.Done():
			timer.Stop()
			return fmt.Errorf("event timed out: %w", deadlineCtx.Err())
		}
	}
}

func nextEventPollInterval(current time.Duration) time.Duration {
	if current < initialEventPollInterval {
		return initialEventPollInterval
	}
	if current >= maximumEventPollInterval {
		return maximumEventPollInterval
	}
	next := current * 2
	if next > maximumEventPollInterval {
		return maximumEventPollInterval
	}
	return next
}

func (s *BrowserSurface) Capture(ctx context.Context, capture CaptureSpec) (artifact.Meta, error) {
	if s.Artifacts == nil {
		return artifact.Meta{}, errors.New("artifact service is not configured")
	}
	if capture.Target != nil {
		matches, err := s.Resolve(ctx, *capture.Target)
		if err != nil {
			return artifact.Meta{}, err
		}
		if len(matches) != 1 {
			return artifact.Meta{}, fmt.Errorf("semantic capture target resolved to %d elements; refusing to guess", len(matches))
		}
		capture.Ref = matches[0].Ref
	}
	opts := artifact.CaptureOptions{
		Kind: capture.Kind, Ref: capture.Ref, Redaction: capture.Redaction,
		TTLSeconds: capture.TTLSeconds, DurationMS: capture.DurationMS, FPS: capture.FPS,
		DownloadGUID: capture.DownloadGUID, Filename: capture.Filename,
	}
	if capture.Kind == "download" {
		completed, ok, err := s.takeCompletedDownload(ctx, capture.DownloadGUID, capture.Filename)
		if err != nil {
			return artifact.Meta{}, err
		}
		if ok {
			capturer, supported := s.Artifacts.(interface {
				CaptureCompletedDownload(context.Context, browser.DownloadEntry, artifact.CaptureOptions) (artifact.Meta, error)
			})
			if !supported {
				s.cacheCompletedDownloads(ctx, []browser.DownloadEntry{completed})
				return artifact.Meta{}, errors.New("artifact service cannot preserve a completed recipe download")
			}
			meta, err := capturer.CaptureCompletedDownload(ctx, completed, opts)
			if err != nil {
				s.cacheCompletedDownloads(ctx, []browser.DownloadEntry{completed})
			}
			return meta, err
		}
	}
	return s.Artifacts.CaptureArtifact(ctx, opts)
}

func (s *BrowserSurface) cacheCompletedDownloads(ctx context.Context, entries []browser.DownloadEntry) {
	tabID := browser.TabIDFromContext(ctx)
	s.downloadsMu.Lock()
	defer s.downloadsMu.Unlock()
	for _, entry := range entries {
		if entry.State == "completed" && strings.TrimSpace(entry.Path) != "" {
			s.downloads = append(s.downloads, cachedDownload{tabID: tabID, entry: entry})
		}
	}
	if overflow := len(s.downloads) - maxCachedRecipeDownloads; overflow > 0 {
		s.downloads = append([]cachedDownload(nil), s.downloads[overflow:]...)
	}
}

func (s *BrowserSurface) takeCompletedDownload(ctx context.Context, guid, filename string) (browser.DownloadEntry, bool, error) {
	if guid == "" && filename == "" {
		return browser.DownloadEntry{}, false, nil
	}
	tabID := browser.TabIDFromContext(ctx)
	s.downloadsMu.Lock()
	defer s.downloadsMu.Unlock()
	matchIndex := -1
	for index := len(s.downloads) - 1; index >= 0; index-- {
		candidate := s.downloads[index]
		if candidate.tabID != tabID || guid != "" && candidate.entry.GUID != guid || filename != "" && candidate.entry.SuggestedFilename != filename {
			continue
		}
		if matchIndex >= 0 {
			return browser.DownloadEntry{}, false, errors.New("multiple completed downloads match; use a unique download_guid")
		}
		matchIndex = index
	}
	if matchIndex < 0 {
		return browser.DownloadEntry{}, false, nil
	}
	completed := s.downloads[matchIndex].entry
	s.downloads = append(s.downloads[:matchIndex], s.downloads[matchIndex+1:]...)
	return completed, true, nil
}

func (s *BrowserSurface) clearCompletedDownloads(ctx context.Context) {
	tabID := browser.TabIDFromContext(ctx)
	s.downloadsMu.Lock()
	defer s.downloadsMu.Unlock()
	kept := s.downloads[:0]
	for _, candidate := range s.downloads {
		if candidate.tabID != tabID {
			kept = append(kept, candidate)
		}
	}
	s.downloads = kept
}
