package testbed

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

func start(t *testing.T, seed int64, maxEvents int) *Server {
	t.Helper()
	s, err := Start(Config{Seed: seed, Chaos: 3, MaxEvents: maxEvents})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func call(t *testing.T, s *Server, path string, body any, code int) []byte {
	t.Helper()
	method := http.MethodGet
	var reader io.Reader
	if body != nil {
		method = http.MethodPost
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, s.URL()+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != code {
		t.Fatalf("%s: status %d want %d: %s", path, resp.StatusCode, code, data)
	}
	return data
}

func state(t *testing.T, s *Server) State {
	t.Helper()
	var v State
	if err := json.Unmarshal(call(t, s, "/api/state", nil, 200), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func step(t *testing.T, s *Server, runID, kind string, count int) []Event {
	t.Helper()
	var events []Event
	data := call(t, s, "/api/step", map[string]any{"run_id": runID, "count": count, "kind": kind}, 200)
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestSeededSequenceAndBoundedBudget(t *testing.T) {
	one, two := start(t, 431, 64), start(t, 431, 64)
	a, b := state(t, one), state(t, two)
	eventsA, eventsB := step(t, one, a.RunID, "", 64), step(t, two, b.RunID, "", 64)
	if !reflect.DeepEqual(eventsA, eventsB) {
		t.Fatal("identical seeds produced different logical events")
	}
	if !reflect.DeepEqual(state(t, one).Reading, state(t, two).Reading) {
		t.Fatal("reading oracle varies across identical seeds")
	}
	call(t, one, "/api/step", map[string]any{"run_id": a.RunID, "count": 1}, 409)
	if state(t, one).Cursor != 64 {
		t.Fatal("exhausted run changed")
	}
	three := start(t, 432, 64)
	c := state(t, three)
	if reflect.DeepEqual(eventsA, step(t, three, c.RunID, "", 64)) {
		t.Fatal("different seeds failed to change the sequence")
	}
}

func TestConfigurationAndConnectionBudgets(t *testing.T) {
	for _, config := range []Config{{Seed: 1 << 53}, {Seed: -(1 << 53)}, {Chaos: 4}, {MaxEvents: 4097}} {
		if s, err := Start(config); err == nil {
			s.Close()
			t.Fatalf("invalid configuration accepted: %+v", config)
		}
	}
	s := start(t, 1, 12)
	initial := state(t, s)
	var channels []chan Event
	for range 64 {
		_, ch, err := s.subscribe(initial.RunID, 0, "sse")
		if err != nil {
			t.Fatal(err)
		}
		channels = append(channels, ch)
	}
	if _, _, err := s.subscribe(initial.RunID, 0, "sse"); err == nil {
		t.Fatal("unbounded stream connections")
	}
	s.unsubscribe(channels[0])
	if _, _, err := s.subscribe(initial.RunID, 0, "sse"); err != nil {
		t.Fatal("connection budget did not recover")
	}
}

func TestResetStaleRunAndDocument(t *testing.T) {
	s := start(t, 7, 12)
	initial := state(t, s)
	step(t, s, initial.RunID, "hydrate", 2)
	call(t, s, "/api/action", map[string]any{"run_id": initial.RunID, "document_epoch": 1, "kind": "delete-account"}, 409)
	if state(t, s).FormState.AccountDeleted {
		t.Fatal("stale node changed the form")
	}
	call(t, s, "/api/action", map[string]any{"run_id": initial.RunID, "document_epoch": 3, "kind": "save-draft", "note": "synthetic note"}, 200)
	call(t, s, "/api/ack", acknowledgement{RunID: initial.RunID, Cursor: 2, AppliedCursor: 2}, 200)
	var reset State
	if err := json.Unmarshal(call(t, s, "/api/reset", Config{Seed: 7, Chaos: 3, MaxEvents: 12}, 200), &reset); err != nil {
		t.Fatal(err)
	}
	if reset.RunID == initial.RunID || reset.Cursor != 0 || reset.AppliedCursor != 0 || reset.AcknowledgedCursor != 0 || reset.FormState.DraftSaved || reset.FormState.Note != "" || len(reset.ActionCounts) != 0 {
		t.Fatalf("reset retained run state: %+v", reset)
	}
	call(t, s, "/api/step", map[string]any{"run_id": initial.RunID}, 409)
	call(t, s, "/api/ack", acknowledgement{RunID: initial.RunID}, 409)
	if state(t, s).Cursor != 0 {
		t.Fatal("old run request changed new run")
	}
}

func openSSE(t *testing.T, s *Server, runID string, cursor uint64, lastID string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	endpoint := s.URL() + "/events?" + url.Values{"run_id": {runID}, "cursor": {strconv.FormatUint(cursor, 10)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != 200 {
		t.Fatalf("SSE status %d", resp.StatusCode)
	}
	return resp
}

func readSSE(t *testing.T, scanner *bufio.Scanner) Event {
	t.Helper()
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e Event
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e) != nil {
			t.Fatal("invalid SSE JSON")
		}
		if e.ID != 0 {
			return e
		}
	}
	t.Fatalf("SSE ended before an event: %v", scanner.Err())
	return Event{}
}

func openWS(t *testing.T, s *Server, runID string, cursor uint64) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := "ws" + strings.TrimPrefix(s.URL(), "http") + "/ws?" + url.Values{"run_id": {runID}, "cursor": {strconv.FormatUint(cursor, 10)}}.Encode()
	conn, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	var hello struct {
		Type string `json:"type"`
	}
	readWS(t, conn, &hello)
	if hello.Type != "hello" {
		t.Fatalf("WS hello: %+v", hello)
	}
	return conn
}

func readWS(t *testing.T, conn *websocket.Conn, value any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}

func TestLiveDisconnectReplayReconnectAndAcknowledgement(t *testing.T) {
	s := start(t, 7, 12)
	initial := state(t, s)
	sse := openSSE(t, s, initial.RunID, 0, "")
	scan := bufio.NewScanner(sse.Body)
	ws := openWS(t, s, initial.RunID, 0)
	step(t, s, initial.RunID, "disconnect", 1)
	if e := readSSE(t, scan); e.ID != 1 || e.Kind != "disconnect" || e.Replay {
		t.Fatalf("live SSE event: %+v", e)
	}
	var live Event
	readWS(t, ws, &live)
	if live.ID != 1 || live.Kind != "disconnect" || live.Replay {
		t.Fatalf("live WS event: %+v", live)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := ws.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusServiceRestart {
		t.Fatalf("expected forced restart: %v", err)
	}
	for scan.Scan() {
	}
	if scan.Err() != nil {
		t.Fatalf("forced SSE disconnect: %v", scan.Err())
	}
	sse2 := openSSE(t, s, initial.RunID, 0, "")
	scan2 := bufio.NewScanner(sse2.Body)
	ws2 := openWS(t, s, initial.RunID, 0)
	if e := readSSE(t, scan2); e.ID != 1 || !e.Replay {
		t.Fatalf("SSE replay: %+v", e)
	}
	var replay Event
	readWS(t, ws2, &replay)
	if replay.ID != 1 || !replay.Replay {
		t.Fatalf("WS replay: %+v", replay)
	}
	step(t, s, initial.RunID, "mutation", 1)
	if e := readSSE(t, scan2); e.ID != 2 || e.Replay {
		t.Fatalf("SSE reconnect failed: %+v", e)
	}
	var next Event
	readWS(t, ws2, &next)
	if next.ID != 2 || next.Replay {
		t.Fatalf("WS reconnect failed: %+v", next)
	}
	data, _ := json.Marshal(wsMessage{Type: "ack", acknowledgement: acknowledgement{RunID: initial.RunID, Cursor: 2, AppliedCursor: 2}})
	if err := ws2.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
	var ack struct {
		Type   string `json:"type"`
		Cursor uint64 `json:"cursor"`
	}
	readWS(t, ws2, &ack)
	if ack.Type != "ack" || ack.Cursor != 2 {
		t.Fatalf("WS acknowledgement: %+v", ack)
	}
	call(t, s, "/api/ack", acknowledgement{RunID: initial.RunID, Cursor: 3, AppliedCursor: 3}, 409)
	oracle := state(t, s)
	if oracle.SSEConnectionCount != 2 || oracle.WSConnectionCount != 2 || oracle.DisconnectCount != 1 || oracle.AcknowledgedCursor != 2 || oracle.AppliedCursor != 2 {
		t.Fatalf("liveness oracle: %+v", oracle)
	}
}

func TestSSELastEventIDAndHeartbeat(t *testing.T) {
	s := start(t, 4, 12)
	initial := state(t, s)
	step(t, s, initial.RunID, "mutation", 3)
	response := openSSE(t, s, initial.RunID, 0, "2")
	scanner := bufio.NewScanner(response.Body)
	if e := readSSE(t, scanner); e.ID != 3 || !e.Replay {
		t.Fatalf("Last-Event-ID was not used: %+v", e)
	}
	found := false
	for scanner.Scan() {
		if scanner.Text() == "event: heartbeat" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("SSE has no observable heartbeat: %v", scanner.Err())
	}
	call(t, s, "/events?run_id="+initial.RunID+"&cursor=9", nil, 409)
	call(t, s, "/events?run_id="+initial.RunID+"&cursor=nope", nil, 400)
}

func TestSensitiveInputsAreOnlyBooleansAndStrictJSON(t *testing.T) {
	s := start(t, 1, 12)
	initial := state(t, s)
	for _, kind := range []string{"input-password", "input-card"} {
		call(t, s, "/api/action", map[string]any{"run_id": initial.RunID, "document_epoch": 1, "kind": kind, "sensitive_supplied": true}, 200)
	}
	call(t, s, "/api/action", map[string]any{"run_id": initial.RunID, "document_epoch": 1, "kind": "input-password", "password": "never-export-this-sensitive-value"}, 400)
	raw := call(t, s, "/api/state", nil, 200)
	if bytes.Contains(raw, []byte("never-export-this-sensitive-value")) {
		t.Fatal("oracle retained a sensitive value")
	}
	var oracle State
	json.Unmarshal(raw, &oracle)
	if !oracle.FormState.PasswordSupplied || !oracle.FormState.CardSupplied || !oracle.FormState.SensitiveRedacted {
		t.Fatalf("redacted oracle: %+v", oracle.FormState)
	}
	for _, body := range []string{`null`, `[]`, `{} {}`, `{"seed":1,"bogus":true}`, strings.Repeat(" ", 16<<10) + `{}`} {
		req, _ := http.NewRequest("POST", s.URL()+"/api/reset", strings.NewReader(body))
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("invalid reset accepted: %q", body[:min(len(body), 50)])
		}
	}
	if state(t, s).RunID != initial.RunID {
		t.Fatal("malformed reset erased state")
	}
}

func TestLoopbackHostOriginAndRedirectBoundaries(t *testing.T) {
	for _, address := range []string{"0.0.0.0:0", ":0", "example.invalid:0", "192.0.2.1:0"} {
		if s, err := Start(Config{Address: address}); err == nil {
			s.Close()
			t.Fatalf("nonloopback listener accepted: %s", address)
		}
	}
	s := start(t, 1, 12)
	initial := state(t, s)
	for _, host := range []string{"example.invalid", "localhost:1"} {
		req, _ := http.NewRequest("GET", s.URL()+"/api/state", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatal("foreign host accepted")
		}
	}
	data, _ := json.Marshal(Config{Seed: 1, MaxEvents: 12})
	req, _ := http.NewRequest("POST", s.URL()+"/api/reset", bytes.NewReader(data))
	req.Header.Set("Origin", "https://example.invalid")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 || state(t, s).RunID != initial.RunID {
		t.Fatal("foreign origin changed fixture")
	}
	for _, target := range []string{"https://example.invalid/", "http://127.0.0.1:1/", "http://user:pass@127.0.0.1/", "javascript:alert(1)"} {
		call(t, s, "/fixture/redirect?to="+url.QueryEscape(target), nil, 400)
	}
	call(t, s, "/fixture/redirect?to="+url.QueryEscape(s.FrameURL()+"/frame"), nil, 302)
	call(t, s, "/fixture/status/429", nil, 429)
	call(t, s, "/fixture/status/600", nil, 400)
	call(t, s, "/download?name="+url.QueryEscape("../escape.txt"), nil, 400)
}

func TestDownloadUploadCookiesAuthAndMeasurements(t *testing.T) {
	s := start(t, 2, 12)
	initial := state(t, s)
	body := call(t, s, "/download", nil, 200)
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != initial.DownloadSHA256 {
		t.Fatal("download disagrees with known digest")
	}
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	writer.WriteField("run_id", initial.RunID)
	file, err := writer.CreateFormFile("file", "fixture.txt")
	if err != nil {
		t.Fatal(err)
	}
	file.Write(body)
	writer.Close()
	req, _ := http.NewRequest("POST", s.URL()+"/upload", &buffer)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	oracle := state(t, s)
	if oracle.LastUpload == nil || oracle.LastUpload.SHA256 != initial.DownloadSHA256 || oracle.LastUpload.Bytes != int64(len(body)) {
		t.Fatalf("upload oracle: %+v", oracle.LastUpload)
	}
	resp, err = http.Get(s.URL() + "/api/cookies")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cookies := resp.Cookies()
	if len(cookies) != 2 || !cookies[1].HttpOnly || cookies[1].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie fixtures: %+v", cookies)
	}
	call(t, s, "/fixture/basic-auth", nil, 401)
	req, _ = http.NewRequest("GET", s.URL()+"/fixture/basic-auth", nil)
	req.SetBasicAuth("fixture", "fixture")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("fixed fixture auth failed")
	}
	resp, err = http.Get(s.URL() + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("X-Testbed-Body-Bytes") != strconv.Itoa(len(data)) || resp.Header.Get("X-Testbed-Chars4-Estimate") != strconv.Itoa((utf8.RuneCount(data)+3)/4) {
		t.Fatal("response measurement header differs from body")
	}
	oracle = state(t, s)
	if oracle.Measurements.BodyBytes <= oracle.Measurements.TextCharacters || oracle.Measurements.Chars4Estimate == 0 {
		t.Fatal("UTF-8 bytes and character estimates are not distinguished")
	}
}
