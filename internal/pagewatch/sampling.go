package pagewatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func (s *Service) run(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.mu.Lock()
			ids := make([]string, 0, len(s.records))
			for id := range s.records {
				ids = append(ids, id)
			}
			s.mu.Unlock()
			for _, id := range ids {
				if ctx.Err() != nil {
					break
				}
				s.mu.Lock()
				r, ok := s.records[id]
				if !ok || !r.Enabled || r.Status == "tab_closed" || r.NextSample.After(now) {
					s.mu.Unlock()
					continue
				}
				r.NextSample = now.Add(time.Duration(r.IntervalMS) * time.Millisecond)
				s.records[id] = r
				s.sample(ctx, id)
				s.mu.Unlock()
			}
		}
	}
}

func (s *Service) sample(ctx context.Context, id string) {
	r := s.records[id]
	ctx, cancel := context.WithTimeout(browser.WithBackgroundPage(ctx), 15*time.Second)
	defer cancel()
	fail := func(status string, err error) {
		r.Status = status
		r.LastError = err.Error()
		if !r.Unavailable {
			r.Unavailable = true
			appendEvent(&r, Event{Kind: "unavailable", Reason: status})
			if err := s.store(id, &r); err != nil {
				old := s.records[id]
				old.Status = "storage_error"
				old.LastError = "watcher could not persist its availability change"
				s.records[id] = old
			}
		} else {
			s.records[id] = r
		}
	}
	if err := s.authorize(r.URL); err != nil {
		fail("refused", err)
		return
	}
	if r.TabID == "" {
		opened, openErr := s.controller.Open(ctx, r.URL)
		if openErr != nil && opened.Tab.ID == "" {
			fail("error", errors.New("watcher could not open its page"))
			return
		}
		if opened.Tab.ID == "" {
			fail("error", errors.New("browser returned no watcher tab id"))
			return
		}
		r.TabID = opened.Tab.ID
		s.owned.Store(r.TabID, true)
		if err := s.store(id, &r); err != nil {
			r.Status = "storage_error"
			r.LastError = "watcher could not persist its owned tab"
			s.records[id] = r
			return
		}
		if openErr != nil {
			fail("error", errors.New("watcher page did not load; its partial tab remains reserved"))
			return
		}
	}
	tabs, err := s.controller.ListTabs(ctx)
	if err != nil {
		fail("disconnected", errors.New("watcher cannot list browser tabs"))
		return
	}
	if err := s.checkOwnership(ctx, r.TabID); err != nil {
		if errors.Is(err, browser.ErrBackgroundOwnershipLost) {
			if err := s.loseOwnership(id, &r); err != nil {
				fail("storage_error", errors.New("could not persist lost watcher ownership"))
				return
			}
			fail("ownership_lost", browser.ErrBackgroundOwnershipLost)
		} else {
			fail("disconnected", errors.New("cannot prove watcher tab ownership"))
		}
		return
	}
	current := ""
	for _, tab := range tabs {
		if tab.ID == r.TabID {
			current = tab.URL
			break
		}
	}
	if current == "" {
		fail("tab_closed", errors.New("watcher tab is closed; resume the watcher to open a new owned tab"))
		return
	}
	if err := s.authorize(current); err != nil {
		fail("refused", err)
		return
	}
	if current != r.URL {
		fail("url_mismatch", errors.New("watcher page moved away from its exact registered URL; login or navigation must be resolved before sampling"))
		return
	}
	if r.RefreshIntervalMS > 0 {
		now := time.Now()
		if r.NextRefresh.IsZero() {
			r.NextRefresh = now.Add(time.Duration(r.RefreshIntervalMS) * time.Millisecond)
		}
		if !r.NextRefresh.After(now) {
			if err := s.authorize(r.URL); err != nil {
				fail("refused", err)
				return
			}
			tabCtx := browser.WithTabID(ctx, r.TabID)
			reloader, ok := s.controller.(browser.PageReloader)
			if !ok {
				fail("refresh_failed", errors.New("browser transport cannot refresh watcher pages"))
				return
			}
			if err := reloader.ReloadPage(tabCtx); err != nil {
				fail("refresh_failed", errors.New("watcher page refresh failed"))
				return
			}
			if err := s.controller.WaitFor(tabCtx, "ready", 5*time.Second); err != nil {
				fail("refresh_failed", errors.New("watcher page refresh did not become ready"))
				return
			}
			r.NextRefresh = now.Add(time.Duration(r.RefreshIntervalMS) * time.Millisecond)
		}
	}
	value, err := s.controller.Evaluate(browser.WithTabID(ctx, r.TabID), sampleScript(r.RegisterOptions))
	if err != nil {
		if errors.Is(err, browser.ErrBackgroundOwnershipLost) {
			if err := s.loseOwnership(id, &r); err != nil {
				fail("storage_error", errors.New("could not persist lost watcher ownership"))
				return
			}
			fail("ownership_lost", browser.ErrBackgroundOwnershipLost)
			return
		}
		fail("error", errors.New("watcher page sample failed"))
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		fail("error", errors.New("watcher returned an invalid sample"))
		return
	}
	var sample struct {
		Value *string `json:"value"`
		Count int     `json:"count"`
		Error string  `json:"error"`
	}
	decodeErr := json.Unmarshal(encoded, &sample)
	if decodeErr == nil && sample.Error == "not_ready" {
		s.records[id] = r
		return
	}
	if sample.Error == "url_mismatch" {
		fail("url_mismatch", errors.New("watcher URL changed before the sample; the replacement document was not read"))
		return
	}
	if decodeErr != nil || sample.Error != "" || sample.Value == nil || sample.Count < 0 {
		fail("error", errors.New("watcher sample refused: invalid selector, changed URL or sample too large"))
		return
	}
	digest := sha256.Sum256([]byte(*sample.Value))
	nextDigest := hex.EncodeToString(digest[:])
	changed := r.Digest != "" && r.Digest != nextDigest
	needsSave := r.Digest != nextDigest || r.Status != "watching" || r.Unavailable
	now := time.Now().UTC()
	r.Status = "watching"
	r.LastError = ""
	r.LastSampleAt = &now
	if r.Unavailable {
		r.Unavailable = false
		appendEvent(&r, Event{Kind: "recovered"})
	}
	if changed {
		appendEvent(&r, Event{Kind: "changed", Digest: nextDigest, Count: sample.Count})
	}
	r.Digest = nextDigest
	if needsSave {
		if err := s.store(id, &r); err != nil {
			old := s.records[id]
			old.Status = "storage_error"
			old.LastError = "watcher could not persist its sample"
			s.records[id] = old
			return
		}
	} else {
		s.records[id] = r
	}
}

func appendEvent(r *record, event Event) {
	r.Seq++
	event.WatcherID = r.ID
	event.Seq = r.Seq
	event.At = time.Now().UTC()
	event.URL = r.URL
	event.Mode = r.Mode
	r.Events = append(append([]Event(nil), r.Events...), event)
	if len(r.Events) > maxEvents {
		r.Events = r.Events[len(r.Events)-maxEvents:]
	}
}

func sampleScript(opts RegisterOptions) string {
	args, _ := json.Marshal(opts)
	return `(() => {
 const o = ` + string(args) + `;
 const expected = new URL(o.url);
 if (location.href !== expected.href || location.origin !== expected.origin)
   return {error: "url_mismatch"};
 if (document.readyState !== "complete") return {error: "not_ready"};
 if (o.mode === "title") {
   const value = document.title;
   return value.length > 65536 ? {error: "sample_too_large"} : {value, count: 1};
 }
 let nodes;
 try { nodes = document.querySelectorAll(o.selector); }
 catch (_) { return {error: "invalid_selector"}; }
 if (o.mode === "count") return {value: String(nodes.length), count: nodes.length};
 if (nodes.length > 1024) return {error: "sample_too_large"};
 const parts = [];
 let size = 0;
 for (const node of nodes) {
   const text = node.textContent || "";
   size += text.length;
   if (size > 65536) return {error: "sample_too_large"};
   parts.push(text);
 }
 return {value: JSON.stringify(parts), count: nodes.length};
})()`
}
