package agenteval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultJudgeModel is the model the optional judge uses unless the environment
// names another.
const DefaultJudgeModel = "claude-opus-5"

const defaultJudgeBaseURL = "https://api.anthropic.com"

// anthropicVersion is the dated API version header the Messages API requires.
const anthropicVersion = "2023-06-01"

// Judge is the optional LLM layer over the deterministic end-state check.
//
// It is shown the task, the page-observable criteria and the end state the
// harness probed. It is NOT shown what the solver did or what it claimed:
// Grade's signature cannot express that, which is the point. A judge that read
// the transcript would be gradeable by the thing it is grading.
type Judge struct {
	APIKey  string
	Model   string
	BaseURL string
	Client  *http.Client
}

// JudgeVerdict is what the layer returned.
type JudgeVerdict struct {
	Model  string `json:"model"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason,omitempty"`
	// Error records a judge that could not answer. It never changes the
	// deterministic verdict — a broken judge must not turn into a failed run,
	// or the harness stops being runnable the moment the API is down.
	Error string `json:"error,omitempty"`
}

// NewJudgeFromEnv builds a judge when the environment carries an API key, and
// reports false when it does not. No key means no judge, no network, and the
// deterministic check alone.
func NewJudgeFromEnv() (*Judge, bool) {
	key := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if key == "" {
		return nil, false
	}
	model := strings.TrimSpace(os.Getenv("BRW_AGENTEVAL_JUDGE_MODEL"))
	if model == "" {
		model = DefaultJudgeModel
	}
	base := strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
	if base == "" {
		base = defaultJudgeBaseURL
	}
	return &Judge{
		APIKey:  key,
		Model:   model,
		BaseURL: strings.TrimRight(base, "/"),
		Client:  &http.Client{Timeout: 3 * time.Minute},
	}, true
}

// Prompt is the exact text the judge is given. Exported so a test can check
// what does and does not reach the model.
func Prompt(task Task, end EndState) string {
	observed, err := json.MarshalIndent(end, "", "  ")
	if err != nil {
		observed = []byte("{}")
	}
	var builder strings.Builder
	builder.WriteString("You are grading a browser automation run.\n\n")
	builder.WriteString("You are given the task that was set, the criteria the finished page has to satisfy, and the state a harness read out of the page AFTER the run stopped.\n")
	builder.WriteString("You are not given what the agent did or what it said about its own run. Judge the observed state only.\n\n")
	fmt.Fprintf(&builder, "TASK\n%s\n\n", task.Goal)
	builder.WriteString("CRITERIA\n")
	for _, criterion := range task.EndStateCriteria {
		fmt.Fprintf(&builder, "- %s\n", criterion)
	}
	fmt.Fprintf(&builder, "\nOBSERVED END STATE\n%s\n\n", observed)
	builder.WriteString(`Reply with one JSON object and nothing else: {"passed": true or false, "reason": "one sentence"}.`)
	return builder.String()
}

// serverSideFallbackBeta routes a policy decline to another model inside the
// same call, rather than returning a refusal the caller has to handle. "default"
// picks the destination by refusal category, so there is no model list here to
// go stale.
//
// It matters here because ANTHROPIC_BASE_URL is operator-settable and the judge
// is optional: without the fallback a decline surfaces as "judge unavailable"
// and the run is graded by the deterministic check alone, which is safe but
// silently drops the layer the operator asked for.
const serverSideFallbackBeta = "server-side-fallback-2026-07-01"

// judgeBodyLimit bounds what is decoded from the endpoint's answer. The base
// URL is operator-settable, and a misbehaving or hostile one can otherwise
// stream into the decoder for the client's whole three-minute timeout with
// nothing bounding the allocation.
const judgeBodyLimit = 1 << 20

type judgeRequest struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	Messages  []judgeMessage `json:"messages"`
	Fallbacks string         `json:"fallbacks,omitempty"`
}

type judgeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type judgeResponse struct {
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Grade asks the model whether the observed end state satisfies the criteria.
func (j *Judge) Grade(ctx context.Context, task Task, end EndState) (JudgeVerdict, error) {
	verdict := JudgeVerdict{Model: j.Model}
	body, err := json.Marshal(judgeRequest{
		Model: j.Model,
		// Room for the model's own reasoning as well as the one-line verdict;
		// thinking is on by default on this model family and is billed against
		// the same ceiling, so a small max_tokens truncates the answer away.
		MaxTokens: 8192,
		Messages:  []judgeMessage{{Role: "user", Content: Prompt(task, end)}},
		Fallbacks: "default",
	})
	if err != nil {
		return verdict, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, j.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return verdict, err
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("anthropic-version", anthropicVersion)
	request.Header.Set("anthropic-beta", serverSideFallbackBeta)
	request.Header.Set("x-api-key", j.APIKey)

	client := j.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	response, err := client.Do(request)
	if err != nil {
		return verdict, err
	}
	defer response.Body.Close()

	answer, err := io.ReadAll(io.LimitReader(response.Body, judgeBodyLimit))
	if err != nil {
		return verdict, fmt.Errorf("read the judge response: %w", err)
	}

	// The status comes first. A 502 answered with a gateway's HTML error page
	// used to be reported as "judge response was not JSON", which names the
	// decoder rather than the failure and sends the reader looking in the wrong
	// place.
	var decoded judgeResponse
	decodeErr := json.Unmarshal(answer, &decoded)
	if response.StatusCode != http.StatusOK {
		if decodeErr == nil && decoded.Error != nil {
			return verdict, fmt.Errorf("judge request failed with %s: %s", response.Status, decoded.Error.Message)
		}
		return verdict, fmt.Errorf("judge request failed with %s: %s", response.Status, truncate(strings.TrimSpace(string(answer)), 200))
	}
	if decodeErr != nil {
		return verdict, fmt.Errorf("judge response was not JSON: %w", decodeErr)
	}
	if decoded.StopReason == "refusal" {
		return verdict, errors.New("judge declined to answer")
	}

	var text strings.Builder
	for _, block := range decoded.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	parsed, err := parseJudgeVerdict(text.String())
	if err != nil {
		return verdict, err
	}
	verdict.Passed = parsed.Passed
	verdict.Reason = parsed.Reason
	return verdict, nil
}

// parseJudgeVerdict pulls the verdict object out of the reply. The model is
// asked for bare JSON; this tolerates it arriving wrapped in prose or a fence
// rather than failing a run over formatting.
func parseJudgeVerdict(text string) (JudgeVerdict, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return JudgeVerdict{}, fmt.Errorf("judge reply carried no JSON object: %q", truncate(text, 200))
	}
	var parsed struct {
		Passed *bool  `json:"passed"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &parsed); err != nil {
		return JudgeVerdict{}, fmt.Errorf("judge reply was not a verdict: %w", err)
	}
	if parsed.Passed == nil {
		return JudgeVerdict{}, errors.New("judge reply named no verdict")
	}
	return JudgeVerdict{Passed: *parsed.Passed, Reason: parsed.Reason}, nil
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
