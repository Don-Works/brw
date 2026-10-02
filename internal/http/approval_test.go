package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/approval"
)

const testApprovalOperatorToken = "test-operator-token-separate-from-agent-123456789"

func newApprovalHTTPTest(t *testing.T) (*Server, approval.Request) {
	t.Helper()
	store, err := approval.Open(filepath.Join(t.TempDir(), "private", "approvals.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New("127.0.0.1:17310", nil)
	if err = s.SetApprovalStore(store, testApprovalOperatorToken); err != nil {
		t.Fatal(err)
	}
	request, err := store.Enqueue(approval.Request{
		Fingerprint: strings.Repeat("a", 64),
		Tool:        "brw_click",
		Origin:      "https://example.test",
		TabID:       "tab-1",
		SessionID:   "session-1",
		Summary:     "<script>alert('page data')</script>",
		StateDigest: strings.Repeat("b", 64),
		Arguments:   json.RawMessage(`{"selector":"<img src=x onerror=alert(1)>"}`),
		ExpiresAt:   time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, request
}

func serveApprovalHTTP(s *Server, method, path, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://127.0.0.1:17310"+path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:40001"
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(recorder, request)
	return recorder
}

func TestApprovalInboxConfiguration(t *testing.T) {
	s, _ := newApprovalHTTPTest(t)
	for _, tt := range []struct {
		name  string
		addr  string
		store *approval.Store
		token string
	}{
		{"missing store", "127.0.0.1:17310", nil, testApprovalOperatorToken},
		{"short token", "127.0.0.1:17310", s.approvals, "short"},
		{"whitespace token", "127.0.0.1:17310", s.approvals, testApprovalOperatorToken + "\n"},
		{"wildcard bind", ":17310", s.approvals, testApprovalOperatorToken},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := New(tt.addr, nil)
			if err := target.SetApprovalStore(tt.store, tt.token); err == nil {
				t.Fatal("invalid approval configuration accepted")
			}
			if target.approvals != nil || target.approvalOperatorToken != "" {
				t.Fatal("invalid configuration partially enabled approval inbox")
			}
		})
	}
	for _, path := range []string{"/approvals", "/operator/approvals", "/api/approvals/missing"} {
		if got := serveApprovalHTTP(New("127.0.0.1:17310", nil), "GET", path, "", ""); got.Code != http.StatusNotFound {
			t.Fatalf("disabled %s = %d", path, got.Code)
		}
	}
	if got := serveApprovalHTTP(New("127.0.0.1:17310", nil), "POST", "/operator/approvals/missing/decision", `{"decision":"approved"}`, testApprovalOperatorToken); got.Code != http.StatusNotFound {
		t.Fatalf("disabled decision = %d", got.Code)
	}
}

func TestApprovalInboxRequiresIndependentOperatorToken(t *testing.T) {
	s, request := newApprovalHTTPTest(t)
	for _, token := range []string{"", "agent-token", testApprovalOperatorToken + "wrong"} {
		for _, call := range []struct{ method, path, body string }{
			{"GET", "/operator/approvals", ""},
			{"POST", "/operator/approvals/" + request.ID + "/decision", `{"decision":"approved"}`},
		} {
			got := serveApprovalHTTP(s, call.method, call.path, call.body, token)
			if got.Code != http.StatusUnauthorized {
				t.Fatalf("%s without correct operator token = %d: %s", call.path, got.Code, got.Body.String())
			}
			if strings.Contains(got.Body.String(), token) && token != "" {
				t.Fatal("rejected token reflected in response")
			}
		}
	}
	for _, source := range []string{"query", "cookie", "alternate header"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:17310/operator/approvals", nil)
		r.RemoteAddr = "127.0.0.1:40001"
		switch source {
		case "query":
			r.URL.RawQuery = "token=" + testApprovalOperatorToken
		case "cookie":
			r.AddCookie(&http.Cookie{Name: "operator_token", Value: testApprovalOperatorToken})
		case "alternate header":
			r.Header.Set("X-Brw-Operator-Token", testApprovalOperatorToken)
		}
		w := httptest.NewRecorder()
		s.server.Handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), testApprovalOperatorToken) {
			t.Fatalf("%s authentication accepted or reflected: %d %s", source, w.Code, w.Body.String())
		}
	}
	got := serveApprovalHTTP(s, "GET", "/operator/approvals", "", testApprovalOperatorToken)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), request.ID) {
		t.Fatalf("authenticated operator list: %d %s", got.Code, got.Body.String())
	}
}

func TestApprovalStatusDoesNotExposeOrDecide(t *testing.T) {
	s, request := newApprovalHTTPTest(t)
	got := serveApprovalHTTP(s, "GET", "/api/approvals/"+request.ID, "", "")
	var status map[string]json.RawMessage
	if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &status) != nil {
		t.Fatalf("public status: %d %s", got.Code, got.Body.String())
	}
	if len(status) != 3 || status["id"] == nil || status["status"] == nil || status["expires_at"] == nil {
		t.Fatalf("public status leaks fields: %s", got.Body.String())
	}
	for _, forbidden := range []string{request.Tool, request.Origin, request.Summary, "arguments", testApprovalOperatorToken} {
		if strings.Contains(got.Body.String(), forbidden) {
			t.Fatalf("public status leaks %q", forbidden)
		}
	}
	got = serveApprovalHTTP(s, "POST", "/api/approvals/"+request.ID, `{"decision":"approved"}`, "")
	if got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("public decision attempt: %d", got.Code)
	}
	current, _ := s.approvals.Get(request.ID)
	if current.Status != "pending" {
		t.Fatalf("public status changed request to %s", current.Status)
	}
}

func TestApprovalInboxLoopbackAndSameOrigin(t *testing.T) {
	s, _ := newApprovalHTTPTest(t)
	for _, tt := range []struct {
		name, peer, host, origin, fetchSite string
	}{
		{"remote peer", "203.0.113.2:40001", "127.0.0.1:17310", "", ""},
		{"untrusted host", "127.0.0.1:40001", "evil.example:17310", "", ""},
		{"origin port mismatch", "127.0.0.1:40001", "127.0.0.1:17310", "http://127.0.0.1:17311", ""},
		{"origin hostname mismatch", "127.0.0.1:40001", "127.0.0.1:17310", "http://localhost:17310", ""},
		{"origin scheme mismatch", "127.0.0.1:40001", "127.0.0.1:17310", "https://127.0.0.1:17310", ""},
		{"opaque origin", "127.0.0.1:40001", "127.0.0.1:17310", "null", ""},
		{"origin with path", "127.0.0.1:40001", "127.0.0.1:17310", "http://127.0.0.1:17310/path", ""},
		{"cross site fetch", "127.0.0.1:40001", "127.0.0.1:17310", "", "cross-site"},
		{"same site fetch", "127.0.0.1:40001", "127.0.0.1:17310", "", "same-site"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://127.0.0.1:17310/operator/approvals", nil)
			r.RemoteAddr, r.Host = tt.peer, tt.host
			r.Header.Set("Authorization", "Bearer "+testApprovalOperatorToken)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.fetchSite != "" {
				r.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			}
			w := httptest.NewRecorder()
			s.server.Handler.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("request accepted: %d %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("cross-origin CORS enabled")
			}
		})
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1:17310/operator/approvals", nil)
	r.RemoteAddr = "[::1]:40001"
	r.Header.Set("Origin", "http://127.0.0.1:17310")
	r.Header.Set("Authorization", "Bearer "+testApprovalOperatorToken)
	w := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("same-origin loopback request = %d %s", w.Code, w.Body.String())
	}
}

func TestApprovalDecisionStrictSchemaAndOperatorActor(t *testing.T) {
	s, request := newApprovalHTTPTest(t)
	path := "/operator/approvals/" + request.ID + "/decision"
	for _, body := range []string{
		`{"decision":"approved","actor":"agent"}`,
		`{"decision":"approved","tool":"brw_evaluate"}`,
		`{"decision":"approved","decision":"denied"}`,
		`{"decision":"approved","note":null}`,
		`{"decision":true}`,
		`{"decision":"consumed"}`,
		`{"decision":"approve"}`,
		`{"note":"no decision"}`,
		`{"decision":"approved"} {}`,
		`[]`,
		`{"decision":"approved","note":"` + strings.Repeat("n", 2001) + `"}`,
		`{"decision":"approved","note":"` + strings.Repeat("n", 9<<10) + `"}`,
	} {
		got := serveApprovalHTTP(s, "POST", path, body, testApprovalOperatorToken)
		if got.Code != http.StatusBadRequest {
			t.Fatalf("invalid schema = %d for %s", got.Code, body)
		}
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:17310"+path, strings.NewReader(`{"decision":"approved"}`))
	r.RemoteAddr = "127.0.0.1:40001"
	r.Header.Set("Authorization", "Bearer "+testApprovalOperatorToken)
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("simple request accepted: %d", w.Code)
	}
	got := serveApprovalHTTP(s, "POST", path, `{"decision":"approved","note":"Reviewed exact action"}`, testApprovalOperatorToken)
	if got.Code != http.StatusOK {
		t.Fatalf("valid decision = %d: %s", got.Code, got.Body.String())
	}
	current, _ := s.approvals.Get(request.ID)
	if current.Status != "approved" || current.DecisionBy != "operator" || current.DecisionNote != "Reviewed exact action" {
		t.Fatalf("incorrect recorded decision: %+v", current)
	}
	got = serveApprovalHTTP(s, "POST", path, `{"decision":"denied"}`, testApprovalOperatorToken)
	if got.Code != http.StatusConflict {
		t.Fatalf("repeat decision accepted: %d", got.Code)
	}
}

func TestApprovalPageStaticLockedAndSafe(t *testing.T) {
	s, request := newApprovalHTTPTest(t)
	got := serveApprovalHTTP(s, "GET", "/approvals", "", "")
	if got.Code != http.StatusOK {
		t.Fatalf("approval page: %d %s", got.Code, got.Body.String())
	}
	body := got.Body.String()
	for _, forbidden := range []string{request.ID, request.Summary, string(request.Arguments), testApprovalOperatorToken, "innerHTML", "localStorage", "sessionStorage", "document.cookie"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("static locked page contains unsafe or private data %q", forbidden)
		}
	}
	for _, required := range []string{`type="password"`, `id="inbox" hidden`, "Approve once", "Reject", "Lock inbox", "textContent", "Notification.requestPermission()", "setTimeout(refresh,3000)", "document.hidden", "Authorization: Bearer"} {
		if !strings.Contains(body, required) {
			t.Fatalf("missing approval page behavior %q", required)
		}
	}
	scriptHash := sha256.Sum256([]byte(approvalScript))
	csp := got.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'sha256-"+base64.StdEncoding.EncodeToString(scriptHash[:])) || !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("unsafe or invalid approval CSP: %s", csp)
	}
	if got.Header().Get("Cache-Control") != "no-store" || got.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("approval page missing privacy headers")
	}
}
