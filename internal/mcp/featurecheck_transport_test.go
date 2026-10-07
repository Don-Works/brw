package mcp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/approvalgate"
	"github.com/Don-Works/brw/internal/browser"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/testbed"
	"github.com/Don-Works/brw/internal/usagelog"
)

func TestFeatureTransportExtras(t *testing.T) {
	if testing.Short() {
		t.Skip("disposable Chrome transport checks")
	}
	t.Setenv("BRW_DASHBOARD", "1")
	t.Setenv("BRW_STREAM_SCOPE", "")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	fixture, err := testbed.Start(testbed.Config{Seed: 7, Chaos: 2, MaxEvents: 64, FrameAddress: "[::1]:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	manager := featureManager(t, ctx)
	h := &featureHarness{Ctx: ctx, Manager: manager, Fixture: fixture, Root: t.TempDir()}
	h.reset(t)
	daemon := httpapi.New("127.0.0.1:0", manager)
	daemon.SetSiteConsent(h.Consent)
	gate, err := approvalgate.New(manager, h.Approvals, "risky")
	if err != nil {
		t.Fatal(err)
	}
	daemon.SetApprovalGate(gate)
	server := httptest.NewServer(daemon.Handler())
	defer server.Close()
	request := func(owner, method, path string, args map[string]any) (int, map[string]any) {
		t.Helper()
		data, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, method, server.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(usagelog.HeaderOwnerID, owner)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var value map[string]any
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&value); err != nil {
			t.Fatalf("%s: HTTP%d decode: %v", path, resp.StatusCode, err)
		}
		return resp.StatusCode, value
	}
	open := func(owner string) string {
		t.Helper()
		status, result := request(owner, http.MethodPost, "/api/browser/open", map[string]any{"url": fixture.URL() + "/?frames=none"})
		tab, _ := result["tab"].(map[string]any)
		id, _ := tab["id"].(string)
		if status != http.StatusOK || id == "" {
			t.Fatalf("owned HTTP open: HTTP%d %+v", status, result)
		}
		return id
	}
	tabA, tabB := open("feature-owner-a"), open("feature-owner-b")
	if tabA == tabB {
		t.Fatal("distinct HTTP owners shared one tab")
	}
	stream, stopStream := featureStream(t, ctx, server.URL+"/api/session/stream", "feature-owner-a")
	defer stopStream()
	fill := func(owner, tab, value string) {
		t.Helper()
		status, result := request(owner, http.MethodPost, "/api/page/fill", map[string]any{"tab_id": tab, "query": "Fixture note", "value": value})
		if status != http.StatusOK || result["ok"] != true {
			t.Fatalf("owned HTTP fill: HTTP%d %+v", status, result)
		}
		h.Wait(t, func(s testbed.State) bool { return s.FormState.Note == value })
	}
	fill("feature-owner-b", tabB, "Other owner's synthetic note")
	withheld := <-stream
	if withheld.Err != nil || withheld.Event != "withheld" || strings.Contains(withheld.Data, tabB) || strings.Contains(withheld.Data, "synthetic note") {
		t.Fatalf("session SSE exposed another owner's real action: %+v", withheld)
	}
	fill("feature-owner-a", tabA, "This owner's synthetic note")
	for {
		event := <-stream
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		if event.Event == "withheld" {
			continue
		}
		var entry browser.TraceEntry
		if err := json.Unmarshal([]byte(event.Data), &entry); err != nil {
			t.Fatal(err)
		}
		if event.Event != "action" || entry.TabID != tabA || !strings.Contains(event.Data, "This owner's synthetic note") {
			t.Fatalf("session SSE missed the owner's actual fill: %+v", event)
		}
		break
	}
	stopStream()
	status, conflict := request("feature-owner-b", http.MethodGet, "/api/page/snapshot?tab_id="+tabA, nil)
	if status != http.StatusConflict || conflict["code"] != "tab_contended" {
		t.Fatalf("cross-owner real-page read was not refused: HTTP%d %+v", status, conflict)
	}
	status, released := request("feature-owner-b", http.MethodPost, "/api/session/release", map[string]any{"close_tabs": true})
	if status != http.StatusOK || !featureContains(released["closed"], tabB) || featureContains(released["closed"], tabA) {
		t.Fatalf("release did not close only the owner's tab: HTTP%d %+v", status, released)
	}
	tabs, err := manager.ListTabs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundA := false
	for _, tab := range tabs {
		if tab.ID == tabB {
			t.Fatal("released HTTP owner's actual target remained open")
		}
		foundA = foundA || tab.ID == tabA
	}
	if !foundA {
		t.Fatal("release closed the other owner's actual target")
	}
	status, released = request("feature-owner-a", http.MethodPost, "/api/session/release", map[string]any{"close_tabs": false})
	if status != http.StatusOK || !featureContains(released["released"], tabA) {
		t.Fatalf("owner lease not released: HTTP%d %+v", status, released)
	}
	status, focused := request("feature-owner-b", http.MethodPost, "/api/browser/focus", map[string]any{"tab_id": tabA})
	if status != http.StatusOK {
		t.Fatalf("released target could not be claimed: HTTP%d %+v", status, focused)
	}
	frames, stopFrames := featureStream(t, ctx, server.URL+"/dashboard/stream?fps=10&width=640", "")
	defer stopFrames()
	frame := <-frames
	stopFrames()
	if frame.Err != nil || frame.Event != "frame" {
		t.Fatalf("owned compositor stream returned no frame: %+v", frame)
	}
	var payload struct {
		JPEG   string  `json:"jpeg_base64"`
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	}
	if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
		t.Fatal(err)
	}
	pixels, err := base64.StdEncoding.DecodeString(payload.JPEG)
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(pixels))
	if err != nil || format != "jpeg" || config.Width <= 0 || config.Width > 640 || config.Height <= 0 || payload.Width <= 0 || payload.Height <= 0 {
		t.Fatalf("actual compositor JPEG invalid: %+v %s %v", config, format, err)
	}
	t.Log("Owned HTTP lease isolation/release, session SSE withholding/delivery, and dashboard compositor JPEG verified")
}

func featureContains(value any, wanted string) bool {
	items, _ := value.([]any)
	for _, item := range items {
		if item == wanted {
			return true
		}
	}
	return false
}

type featureStreamEvent struct {
	Event, Data string
	Err         error
}

func featureStream(t *testing.T, ctx context.Context, url, owner string) (<-chan featureStreamEvent, func()) {
	t.Helper()
	streamCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set(usagelog.HeaderOwnerID, owner)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stop := func() { cancel(); resp.Body.Close() }
	t.Cleanup(stop)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("owned SSE: HTTP%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := make(chan featureStreamEvent, 32)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		event := featureStreamEvent{}
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				event.Event = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				event.Data = strings.TrimPrefix(line, "data: ")
			}
			if line == "" && event.Event != "" {
				select {
				case events <- event:
				case <-streamCtx.Done():
					return
				}
				event = featureStreamEvent{}
			}
		}
		select {
		case events <- featureStreamEvent{Err: scanner.Err()}:
		case <-streamCtx.Done():
		}
	}()
	return events, stop
}
