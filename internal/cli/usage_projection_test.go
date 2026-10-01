package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/usagelog"
)

func TestCLIUsageMeasuresActualProjectionAndSharesRequestID(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		var report usagelog.Event
		var operationID, reportID string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/usage/report" {
				reportID = r.Header.Get(usagelog.HeaderRequestID)
				if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
					t.Error(err)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			operationID = r.Header.Get(usagelog.HeaderRequestID)
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `{"ok":true,"url":"https://PRIVATE.test/雪"}`)
		}))
		args := []string{"fill", "@e17", "PRIVATE_TYPED_VALUE", "--daemon", server.URL}
		if jsonOutput {
			args = append(args, "--json")
		}
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr); code != ExitOK {
			t.Fatalf("exit=%d stderr=%s", code, &stderr)
		}
		server.Close()
		input, _ := json.Marshal(args)
		if report.Operation != "brw_fill" || report.Layer != "cli" || report.Scope != "projection" || report.Representation != "cli_stdout" || operationID == "" || reportID != operationID || report.OutputBytes == nil || *report.OutputBytes != int64(stdout.Len()) || report.InputBytes == nil || *report.InputBytes != int64(len(input)) {
			t.Fatalf("report=%+v stdout=%s ids=%s/%s", report, &stdout, operationID, reportID)
		}
		data, _ := json.Marshal(report)
		if strings.Contains(string(data), "PRIVATE") || strings.Contains(string(data), "e17") || strings.Contains(string(data), server.URL) {
			t.Fatalf("report leaked input: %s", data)
		}
	}
}

func TestCLIUsageReportsRenderedFailureWithoutChangingExit(t *testing.T) {
	var report usagelog.Event
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/usage/report" {
			_ = json.NewDecoder(r.Body).Decode(&report)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"PRIVATE_ERROR"}`)
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"fill", "@e17", "PRIVATE_INPUT", "--daemon", server.URL, "--json"}, &stdout, &stderr)
	if code != ExitActionFailed || report.Outcome != "error" || report.OutputBytes == nil || *report.OutputBytes != int64(stdout.Len()) || !strings.Contains(stdout.String(), "PRIVATE_ERROR") {
		t.Fatalf("code=%d report=%+v stdout=%s", code, report, &stdout)
	}
	data, _ := json.Marshal(report)
	if strings.Contains(string(data), "PRIVATE") {
		t.Fatalf("report leaked failure: %s", data)
	}
}

func TestCLIUsageCountingWriterExcludesBase64FromEstimates(t *testing.T) {
	var output bytes.Buffer
	writer := &cliUsageWriter{Writer: &output}
	data := []byte(`{"base64":"c2VjcmV0","mime_type":"image/png"}` + "\n")
	n, err := writer.Write(data)
	if err != nil || n != len(data) || writer.bytes != int64(len(data)) || writer.binary != 8 || writer.chars != int64(len(data)-8) {
		t.Fatalf("bytes=%d chars=%d binary=%d", writer.bytes, writer.chars, writer.binary)
	}
}
