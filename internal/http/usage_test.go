package httpapi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/usagelog"
)

func TestUsageMiddlewareNeverLogsRequestOrErrorText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.ndjson")
	recorder, err := usagelog.New(usagelog.Config{
		Path: path, MaxBytes: 1 << 20, Backups: 1,
		Identity: brwidentity.Identity{Workspace: "brw-test", Profile: "test", Mode: "bridge"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{usage: recorder}
	h := s.usageMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, errors.New("password=SENSITIVE_SENTINEL_A token=TOKEN_SENTINEL_B"))
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/page/fill?secret=QUERY_SENTINEL_C", strings.NewReader(`{"text":"SENSITIVE_SENTINEL_A","url":"https://x.test/?token=TOKEN_SENTINEL_B"}`))
	req.Header.Set(usagelog.HeaderSessionID, "session-1")
	req.Header.Set(usagelog.HeaderRequestID, "session-1:4")
	req.Header.Set(usagelog.HeaderClient, "brw-httpclient")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"SENSITIVE_SENTINEL_A", "TOKEN_SENTINEL_B", "QUERY_SENTINEL_C", "x.test", `"text"`, `"url"`} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("usage log retained %q: %s", forbidden, data)
		}
	}
	if !strings.Contains(string(data), `"operation":"brw_fill"`) || !strings.Contains(string(data), `"outcome":"error"`) {
		t.Fatalf("missing safe usage metadata: %s", data)
	}
	if body, _ := io.ReadAll(res.Result().Body); !strings.Contains(string(body), "SENSITIVE_SENTINEL_A") {
		t.Fatalf("middleware changed API error response: %s", body)
	}
}
