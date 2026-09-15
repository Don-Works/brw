package agenteval

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestJudgeIsAbsentWithoutAKey is what makes the harness runnable: with no key
// there is no judge, and therefore no request to anywhere.
func TestJudgeIsAbsentWithoutAKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if judge, ok := NewJudgeFromEnv(); ok || judge != nil {
		t.Fatalf("a judge was built with no API key: %+v", judge)
	}
}

func TestJudgeReadsItsConfigurationFromTheEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "fake")
	t.Setenv("BRW_AGENTEVAL_JUDGE_MODEL", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	judge, ok := NewJudgeFromEnv()
	if !ok {
		t.Fatal("no judge was built despite a key being set")
	}
	if judge.Model != DefaultJudgeModel {
		t.Errorf("model = %q, want the default %q", judge.Model, DefaultJudgeModel)
	}
	if judge.BaseURL != "https://api.anthropic.com" {
		t.Errorf("base url = %q, want the public API", judge.BaseURL)
	}

	t.Setenv("BRW_AGENTEVAL_JUDGE_MODEL", "claude-haiku-4-5")
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example.test/")
	judge, _ = NewJudgeFromEnv()
	if judge.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q, want the override", judge.Model)
	}
	if judge.BaseURL != "https://gateway.example.test" {
		t.Errorf("base url = %q, want the override with no trailing slash", judge.BaseURL)
	}
}

// TestJudgeSendsTheEndStateAndNotTheClaim is the property the judge exists for.
// It is shown the task, the criteria and what the harness observed; anything
// the run said about itself must not reach it.
func TestJudgeSendsTheEndStateAndNotTheClaim(t *testing.T) {
	var captured struct {
		path   string
		method string
		header http.Header
		body   map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		captured.method = r.Method
		captured.header = r.Header.Clone()
		payload, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(payload, &captured.body)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"stop_reason":"end_turn","content":[{"type":"text","text":"{\"passed\": true, \"reason\": \"the status region matches\"}"}]}`)
	}))
	defer server.Close()

	judge := &Judge{APIKey: "fake", Model: "claude-opus-5", BaseURL: server.URL, Client: server.Client()}
	task := formSubmitTask()
	end := EndState{URL: "http://127.0.0.1:1/forms.html", Fields: map[string]string{
		"result": "Submitted harness@example.test Fixture Runner pro accepted benchmark run no-file",
		"terms":  "checked",
	}}

	verdict, err := judge.Grade(context.Background(), task, end)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if !verdict.Passed || verdict.Reason == "" {
		t.Fatalf("verdict = %+v, want a pass with a reason", verdict)
	}
	if verdict.Model != "claude-opus-5" {
		t.Errorf("verdict model = %q, want the configured model", verdict.Model)
	}

	if captured.path != "/v1/messages" || captured.method != http.MethodPost {
		t.Errorf("request was %s %s, want POST /v1/messages", captured.method, captured.path)
	}
	if got := captured.header.Get("x-api-key"); got != "fake" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := captured.header.Get("anthropic-version"); got != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", got, anthropicVersion)
	}
	// Both halves of the refusal fallback, because either alone does nothing:
	// the beta header without the field is an unused opt-in, and the field
	// without the header is rejected.
	if got := captured.header.Get("anthropic-beta"); !strings.Contains(got, serverSideFallbackBeta) {
		t.Errorf("anthropic-beta = %q, want it to carry %q so a policy decline is re-served rather than dropping the judge", got, serverSideFallbackBeta)
	}
	if got, _ := captured.body["fallbacks"].(string); got != "default" {
		t.Errorf("request fallbacks = %q, want \"default\"", got)
	}
	if got, _ := captured.body["model"].(string); got != "claude-opus-5" {
		t.Errorf("request model = %q", got)
	}

	prompt := Prompt(task, end)
	for _, want := range []string{task.Goal, "checked", "Submitted harness@example.test"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not carry %q", want)
		}
	}
	for _, criterion := range task.EndStateCriteria {
		if !strings.Contains(prompt, criterion) {
			t.Errorf("prompt does not carry the criterion %q", criterion)
		}
	}
	for _, forbidden := range []string{"claimed_success", "claim", "the agent said", "transcript"} {
		if strings.Contains(strings.ToLower(prompt), strings.ToLower(forbidden)) &&
			!strings.Contains(prompt, "You are not given what the agent did") {
			t.Errorf("prompt leaks the run's own account of itself via %q", forbidden)
		}
	}
}

func TestJudgeSurfacesAnErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer server.Close()

	judge := &Judge{APIKey: "fake", Model: "claude-opus-5", BaseURL: server.URL, Client: server.Client()}
	if _, err := judge.Grade(context.Background(), formSubmitTask(), EndState{}); err == nil {
		t.Fatal("a 429 was accepted as a verdict")
	} else if !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("error %q does not carry what the API said", err)
	}
}

// TestJudgeReportsWhatFailedRatherThanTheDecoder covers the endpoints that do
// not answer in the API's own shape. A proxy in front of an operator-set
// ANTHROPIC_BASE_URL answers a 502 with an HTML page, and decoding before
// checking the status reported every one of those as "judge response was not
// JSON" — the decoder's complaint, not the failure, and it sends whoever reads
// the log looking at the wrong thing.
func TestJudgeReportsWhatFailedRatherThanTheDecoder(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantIn      []string
		wantNotIn   []string
	}{
		{
			name: "gateway html error", status: http.StatusBadGateway, contentType: "text/html",
			body:   "<html><head><title>502 Bad Gateway</title></head><body>nginx</body></html>",
			wantIn: []string{"502"}, wantNotIn: []string{"was not JSON"},
		},
		{
			name: "api error object", status: http.StatusTooManyRequests, contentType: "application/json",
			body:   `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			wantIn: []string{"429", "slow down"},
		},
		{
			name: "empty body", status: http.StatusServiceUnavailable, contentType: "text/plain",
			body:   "",
			wantIn: []string{"503"}, wantNotIn: []string{"was not JSON"},
		},
		{
			name: "success that is not the API's shape", status: http.StatusOK, contentType: "text/html",
			body:   "<html>a captive portal</html>",
			wantIn: []string{"was not JSON"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", testCase.contentType)
				w.WriteHeader(testCase.status)
				_, _ = io.WriteString(w, testCase.body)
			}))
			defer server.Close()

			judge := &Judge{APIKey: "fake", Model: "claude-opus-5", BaseURL: server.URL, Client: server.Client()}
			_, err := judge.Grade(context.Background(), formSubmitTask(), EndState{})
			if err == nil {
				t.Fatal("a failed request was accepted as a verdict")
			}
			for _, want := range testCase.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			for _, unwanted := range testCase.wantNotIn {
				if strings.Contains(err.Error(), unwanted) {
					t.Errorf("error %q blames %q instead of the failure", err, unwanted)
				}
			}
		})
	}
}

// TestJudgeBoundsTheBodyItDecodes drives an endpoint that answers with more
// than the limit. The reply below is valid JSON carrying a valid verdict at the
// very end, so an unbounded decoder reads all of it and reports a pass: the
// error this test requires IS the bound.
func TestJudgeBoundsTheBodyItDecodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if _, err := io.WriteString(w, `{"stop_reason":"end_turn","content":[{"type":"text","text":"`); err != nil {
			return
		}
		padding := strings.Repeat("a", 64*1024)
		for written := 0; written < 4*judgeBodyLimit; written += len(padding) {
			if _, err := io.WriteString(w, padding); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `{\"passed\": true, \"reason\": \"an unbounded read got this far\"}"}]}`)
	}))
	defer server.Close()

	judge := &Judge{APIKey: "fake", Model: "claude-opus-5", BaseURL: server.URL, Client: server.Client()}
	verdict, err := judge.Grade(context.Background(), formSubmitTask(), EndState{})
	if err == nil {
		t.Fatalf("a %d-byte reply was decoded whole into %+v; nothing bounds the allocation", 4*judgeBodyLimit, verdict)
	}
	if !strings.Contains(err.Error(), "was not JSON") {
		t.Fatalf("error %q is not the truncated decode the limit produces", err)
	}
}

func TestJudgeRefusalIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"stop_reason":"refusal","content":[]}`)
	}))
	defer server.Close()

	judge := &Judge{APIKey: "fake", Model: "claude-opus-5", BaseURL: server.URL, Client: server.Client()}
	if _, err := judge.Grade(context.Background(), formSubmitTask(), EndState{}); err == nil {
		t.Fatal("a refusal was treated as a verdict")
	}
}

// TestJudgeHonoursAContextDeadline pins that the caller's context reaches the
// request. A judge that ignored it would hang a whole evaluation run.
func TestJudgeHonoursAContextDeadline(t *testing.T) {
	// The stall has a bound of its own: httptest.Server.Close waits for
	// outstanding handlers, so a handler that only watched the request context
	// would hang this test instead of failing it.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer server.Close()

	judge := &Judge{APIKey: "fake", Model: "claude-opus-5", BaseURL: server.URL, Client: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := judge.Grade(ctx, formSubmitTask(), EndState{})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a stalled judge did not return an error")
	}
	// Both halves matter. Any error at all would also be returned by a judge that
	// ignored the context and waited for the handler's own bound, so the call has
	// to come back on the deadline and say that is why.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v is not the caller's deadline; the context did not reach the request", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the call took %s to give up on a 100ms deadline", elapsed)
	}
}

func TestParseJudgeVerdict(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		wantErr    bool
		wantPassed bool
	}{
		{name: "bare object", text: `{"passed": true, "reason": "fine"}`, wantPassed: true},
		{name: "fenced object", text: "```json\n{\"passed\": false, \"reason\": \"no\"}\n```", wantPassed: false},
		{name: "prose around it", text: `Here is my verdict: {"passed": false, "reason": "missing"} — done.`, wantPassed: false},
		{name: "no object", text: "it passed", wantErr: true},
		{name: "no verdict field", text: `{"reason": "unsure"}`, wantErr: true},
		{name: "not json", text: `{passed: yes}`, wantErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			verdict, err := parseJudgeVerdict(testCase.text)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("parsed %q into %+v, want an error", testCase.text, verdict)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %q: %v", testCase.text, err)
			}
			if verdict.Passed != testCase.wantPassed {
				t.Fatalf("passed = %v, want %v", verdict.Passed, testCase.wantPassed)
			}
		})
	}
}

// TestJudgeCanFailARunButNeverRescueOne pins the layering. A judge that could
// turn a failed end-state check into a pass would make the page stop being the
// evidence.
func TestJudgeCanFailARunButNeverRescueOne(t *testing.T) {
	cases := []struct {
		name        string
		passed      bool
		judged      JudgeVerdict
		wantPassed  bool
		wantReasons int
	}{
		{name: "judge agrees with a pass", passed: true, judged: JudgeVerdict{Passed: true}, wantPassed: true},
		{name: "judge fails a pass", passed: true, judged: JudgeVerdict{Passed: false, Reason: "the form is empty"}, wantPassed: false, wantReasons: 1},
		{name: "judge passes a failure", passed: false, judged: JudgeVerdict{Passed: true, Reason: "looks fine to me"}, wantPassed: false, wantReasons: 1},
		{name: "broken judge leaves a pass alone", passed: true, judged: JudgeVerdict{Error: "429"}, wantPassed: true},
		{name: "broken judge leaves a failure alone", passed: false, judged: JudgeVerdict{Error: "429"}, wantPassed: false, wantReasons: 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reasons := []string(nil)
			if !testCase.passed {
				reasons = []string{"end state not reached"}
			}
			gotPassed, gotReasons := applyJudge(testCase.passed, reasons, testCase.judged)
			if gotPassed != testCase.wantPassed {
				t.Fatalf("passed = %v, want %v", gotPassed, testCase.wantPassed)
			}
			if len(gotReasons) != testCase.wantReasons {
				t.Fatalf("reasons = %v, want %d of them", gotReasons, testCase.wantReasons)
			}
		})
	}
}
