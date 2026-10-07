package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/usagelog"
)

func usageRecorderFixture(t *testing.T) (*usagelog.Recorder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	r, err := usagelog.New(usagelog.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, path
}

func readUsageEvents(t *testing.T, path string) []usagelog.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []usagelog.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var event usagelog.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func TestUsageMeasurementHTTPBoundaryAndPrivacy(t *testing.T) {
	recorder, path := usageRecorderFixture(t)
	server := &Server{usage: recorder}
	const input = `{"text":"private雪"}`
	const output = `{"result":"private雪"}`
	handler := server.usageMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, output)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/page/fill?value=secret", strings.NewReader(input)))
	events := readUsageEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events=%+v", events)
	}
	event := events[0]
	if event.InputBytes == nil || *event.InputBytes != int64(len(input)) || event.OutputBytes == nil || *event.OutputBytes != int64(len(output)) || event.InputQueryBytes == nil || *event.InputQueryBytes != 12 || event.Scope != "transport" || event.InputTextChars != nil {
		t.Fatalf("measurement=%+v", event)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "private") || strings.Contains(string(data), "secret") {
		t.Fatalf("retained content: %s", data)
	}
}

func TestUsageReportStrictMetadataAndNoRecursiveRecord(t *testing.T) {
	recorder, path := usageRecorderFixture(t)
	server := New("", &fakeController{})
	server.SetUsageRecorder(recorder)
	payload := `{"layer":"mcp","operation":"brw_fill","outcome":"ok","scope":"tool","representation":"mcp_arguments_result","input_bytes":4,"output_bytes":0,"duration_us":32,"workspace":"SENSITIVE_OVERRIDE","request_id":"untrusted-body-id"}`
	req := httptest.NewRequest(http.MethodPost, "/api/usage/report", strings.NewReader(payload))
	req.Header.Set(usagelog.HeaderRequestID, "trusted-header-id")
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	events := readUsageEvents(t, path)
	if len(events) != 1 || events[0].RequestID != "trusted-header-id" || events[0].DurationUS != 32 || events[0].OutputBytes == nil || *events[0].OutputBytes != 0 {
		t.Fatalf("events=%+v", events)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "SENSITIVE_OVERRIDE") || strings.Contains(string(data), "untrusted-body-id") {
		t.Fatalf("body metadata leaked: %s", data)
	}
	for _, bad := range []string{
		strings.Replace(payload, `"brw_fill"`, `"SENSITIVE_OPERATION"`, 1),
		strings.Replace(payload, `"input_bytes":4`, `"input_bytes":-1`, 1),
		strings.Replace(payload, `"scope":"tool"`, `"scope":"SECRET"`, 1),
		strings.TrimSuffix(payload, "}") + `,"text":"secret"}`,
		strings.TrimSuffix(payload, "}") + `,"snapshot_mode":"SENSITIVE_MODE"}`,
		strings.TrimSuffix(payload, "}") + `,"returned_elements":-1}`,
		strings.TrimSuffix(payload, "}") + `,"delta_returned":"secret"}`,
		payload + `{}`,
	} {
		response = httptest.NewRecorder()
		server.server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/usage/report", strings.NewReader(bad)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("bad report status=%d", response.Code)
		}
	}
	if len(readUsageEvents(t, path)) != 1 {
		t.Fatal("invalid reporting was recorded")
	}
}

func TestUsageReportRetainsHostGuard(t *testing.T) {
	recorder, _ := usageRecorderFixture(t)
	server := New("127.0.0.1:17310", &fakeController{})
	server.SetUsageRecorder(recorder)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:17310/api/usage/report", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://other.example")
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestUsageReportRetainsOnlySafeFailureMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, class, fingerprint, wantClass string
		wantRetryable                                bool
	}{
		{"timeout", "error", "timeout", "0123456789abcdef01234567", "timeout", true},
		{"page failure", "error", "page_script_error", "0123456789abcdef01234567", "page_script_error", false},
		{"approval", "error", "approval_consumed", "0123456789abcdef01234567", "approval_consumed", false},
		{"private class", "error", "SENSITIVE_ERROR_CLASS", "SENSITIVE_FINGERPRINT", "tool", false},
		{"success", "ok", "timeout", "0123456789abcdef01234567", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder, path := usageRecorderFixture(t)
			server := New("", &fakeController{})
			server.SetUsageRecorder(recorder)
			payload, err := json.Marshal(usagelog.Event{
				Layer: "mcp", Operation: "brw_fill", Outcome: tc.outcome, Scope: "tool", Representation: "mcp_arguments_result",
				ErrorClass: tc.class, ErrorFingerprint: tc.fingerprint, Retryable: !tc.wantRetryable,
			})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/usage/report", bytes.NewReader(payload)))
			if response.Code != http.StatusNoContent {
				t.Fatalf("status=%d", response.Code)
			}
			events := readUsageEvents(t, path)
			if len(events) != 1 || events[0].ErrorClass != tc.wantClass || events[0].Retryable != tc.wantRetryable {
				t.Fatalf("failure metadata lost: %+v", events)
			}
			wantFingerprint := ""
			if tc.outcome == "error" {
				wantFingerprint = usagelog.SafeFingerprint(tc.fingerprint)
			}
			if events[0].ErrorFingerprint != wantFingerprint {
				t.Fatalf("fingerprint=%s want=%s", events[0].ErrorFingerprint, wantFingerprint)
			}
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), "SENSITIVE") {
				t.Fatalf("private failure metadata retained: %s", data)
			}
		})
	}
}

func TestUsageProxyRecordsHTTPAndActualMCPContextInCanonicalLedger(t *testing.T) {
	recorder, path := usageRecorderFixture(t)
	daemon := New("", &fakeController{})
	daemon.SetUsageRecorder(recorder)
	upstream := httptest.NewServer(daemon.server.Handler)
	defer upstream.Close()
	controller, err := httpclient.New(upstream.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	proxy := mcp.New(controller)
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_list_tabs","arguments":{}}}` + "\n"
	var out bytes.Buffer
	if err := proxy.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	events := readUsageEvents(t, path)
	if len(events) != 2 {
		t.Fatalf("events=%+v", events)
	}
	var transport, tool *usagelog.Event
	for i := range events {
		switch events[i].Layer {
		case "http":
			transport = &events[i]
		case "mcp":
			tool = &events[i]
		}
	}
	if transport == nil || tool == nil || transport.RequestID != tool.RequestID || transport.SessionID != tool.SessionID || transport.Scope != "transport" || tool.Scope != "tool" {
		t.Fatalf("events=%+v", events)
	}
	var response struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if tool.OutputBytes == nil || *tool.OutputBytes != int64(len(response.Result)) || tool.InputBytes == nil || *tool.InputBytes != 2 {
		t.Fatalf("tool=%+v result=%s", tool, response.Result)
	}
}
