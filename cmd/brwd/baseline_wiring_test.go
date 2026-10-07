package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/baseline"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/snapshot"
)

type wiredDaemon struct {
	*httpclient.Controller
	pageURL string
}

func (d *wiredDaemon) ListTabs(context.Context) ([]browser.Tab, error) {
	return []browser.Tab{{ID: "tab-1", URL: d.pageURL, Active: true}}, nil
}

func (d *wiredDaemon) ActiveTabID(context.Context) (string, error) {
	return "tab-1", nil
}

func (d *wiredDaemon) Screenshot(context.Context) (browser.Screenshot, error) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 8; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return browser.Screenshot{}, err
	}
	return browser.Screenshot{MIMEType: "image/png", Data: buf.Bytes(), Base64: base64.StdEncoding.EncodeToString(buf.Bytes())}, nil
}

func (d *wiredDaemon) Evaluate(_ context.Context, expression string) (any, error) {
	switch expression {
	case snapshot.AriaTreeExpression:
		return map[string]any{"nodes": []any{map[string]any{"role": "button", "name": "Download invoices"}}}, nil
	case baseline.EnvironmentExpression:
		return map[string]any{
			"browser_build":      "Chrome/141.0.0.0",
			"viewport_width":     1280,
			"viewport_height":    800,
			"device_pixel_ratio": 1,
			"locale":             "en-GB",
		}, nil
	}
	return nil, fmt.Errorf("unexpected expression: %s", expression)
}

func newWiredDaemon(t *testing.T, host, pageURL string) *wiredDaemon {
	t.Helper()
	upstream, err := httpclient.New(host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return &wiredDaemon{Controller: upstream, pageURL: pageURL}
}

func callBaselineOverMCP(t *testing.T, server *mcp.Server, arguments string) map[string]any {
	t.Helper()
	requestIn, requestOut := io.Pipe()
	var replies bytes.Buffer
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { done <- server.Serve(ctx, requestIn, &replies) }()

	request := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brw_baseline","arguments":%s}}`+"\n", arguments)
	if _, err := io.WriteString(requestOut, request); err != nil {
		t.Fatalf("write request: %v", err)
	}

	if err := requestOut.Close(); err != nil {
		t.Fatalf("close request stream: %v", err)
	}
	if err := <-done; err != nil && err != io.EOF {
		t.Fatalf("Serve: %v", err)
	}

	scanner := bufio.NewScanner(bytes.NewReader(replies.Bytes()))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var envelope struct {
			ID     any             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			continue
		}
		if envelope.ID == nil {
			continue
		}
		if envelope.Error != nil {
			t.Fatalf("brw_baseline: %s", envelope.Error.Message)
		}
		var result map[string]any
		if err := json.Unmarshal(envelope.Result, &result); err != nil {
			t.Fatalf("decode result: %v (%s)", err, line)
		}
		return result
	}
	t.Fatalf("no answer to the brw_baseline call: %s", replies.String())
	return nil
}

func baselineAnswer(t *testing.T, result map[string]any) (refused bool, text string) {
	t.Helper()
	refused, _ = result["isError"].(bool)
	content, _ := result["content"].([]any)
	var parts []string
	for _, entry := range content {
		item, _ := entry.(map[string]any)
		if value, ok := item["text"].(string); ok {
			parts = append(parts, value)
		}
	}
	return refused, strings.Join(parts, "\n")
}

func localBaselineCount(t *testing.T, root string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, "*", "step-*", "*", "baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

func privateRecipeRoot(t *testing.T) (root string, value recipe.Recipe) {
	t.Helper()
	visible := true
	value = recipe.Recipe{
		SchemaVersion: recipe.SchemaVersion,
		ID:            "example.billing.download-invoices",
		Version:       "1.2.3",
		Name:          "Download billing invoices",
		Description:   "Download monthly statements from the billing portal.",
		Intents:       []string{"download invoices"},
		Origins:       []string{"https://billing.example.test"},
		Risk:          "read_only",
		Steps: []recipe.Step{{
			ID: "download", Action: "click", Effect: "read",
			Target: &recipe.Target{Role: "button", TestID: "download-invoices", Visible: &visible},
		}},
	}
	root = t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "recipe.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, value
}

func routingHost(t *testing.T, destination recipe.BaselineDestination) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/baselines/route", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"destination": string(destination)})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestAProxyingDaemonRefusesAProviderBaselineRatherThanWritingItLocally(t *testing.T) {
	host := routingHost(t, recipe.BaselineProvider)
	controller := browser.Controller(newWiredDaemon(t, host.URL, "https://billing.example.test/invoices"))

	router, _ := controller.(recipe.BaselineRouter)

	root := filepath.Join(t.TempDir(), "baselines")
	store, err := baseline.NewStore(root)
	if err != nil {
		t.Fatalf("baseline.NewStore: %v", err)
	}
	server := mcp.New(controller)
	server.SetBaselineStore(store)

	line, err := installBaselineDestinations(server, nil, host.URL, router)
	if err != nil {
		t.Fatalf("installBaselineDestinations on a proxy: %v", err)
	}
	if line == "" {
		t.Fatal("a proxying daemon started without telling the operator where a baseline goes")
	}

	digest := strings.Repeat("ab", 32)
	refused, message := baselineAnswer(t, callBaselineOverMCP(t, server,
		fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":0}`, digest)))
	if !refused {
		t.Fatalf("a capture that belongs with the upstream provider was accepted here: %s", message)
	}
	if !strings.Contains(message, "--upstream-http") {
		t.Fatalf("refusal = %q, which does not tell the operator the provider is on the daemon this one proxies", message)
	}
	if got := localBaselineCount(t, store.Root()); got != 0 {
		t.Fatalf("%d captures were written to this daemon's --baseline-root", got)
	}
}

func TestAProxyingDaemonThatCannotAskWhereABaselineBelongsRefusesToStart(t *testing.T) {
	server := mcp.New(&wiredDaemon{})
	line, err := installBaselineDestinations(server, nil, "http://127.0.0.1:1", nil)
	if err == nil {
		t.Fatalf("a proxy with no routing hop started anyway, and said %q", line)
	}
	if !strings.Contains(err.Error(), "--baseline-root") {
		t.Fatalf("refusal = %q, which does not name what would have happened to the captures", err)
	}
}

func TestABaselineLandsWhereTheStartupLineSaidItWould(t *testing.T) {
	const page = "https://billing.example.test/invoices"

	t.Run("a browser host whose provider holds baselines", func(t *testing.T) {
		root, value := privateRecipeRoot(t)
		provider, err := recipe.NewDirectoryProvider(context.Background(), recipe.DirectoryConfig{Root: root})
		if err != nil {
			t.Fatalf("directory provider: %v", err)
		}
		digest, err := recipe.Digest(value)
		if err != nil {
			t.Fatal(err)
		}
		recipeBaselines := recipeBaselinesFor(provider)
		if recipeBaselines == nil {
			t.Fatal("the directory provider was not recognised as a baseline store")
		}
		line := recipeBaselineStatusLine(recipeBaselines)

		localRoot := filepath.Join(t.TempDir(), "baselines")
		store, err := baseline.NewStore(localRoot)
		if err != nil {
			t.Fatal(err)
		}
		server := mcp.New(&wiredDaemon{pageURL: page})
		server.SetBaselineStore(store)
		if _, err := installBaselineDestinations(server, recipeBaselines, "", recipeBaselines); err != nil {
			t.Fatalf("installBaselineDestinations on a browser host: %v", err)
		}

		refused, message := baselineAnswer(t, callBaselineOverMCP(t, server,
			fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":0}`, digest)))
		if refused {
			t.Fatalf("the provider's own recipe on its own page was refused: %s", message)
		}
		var answer struct {
			StoredIn string `json:"stored_in"`
		}
		if err := json.Unmarshal([]byte(message), &answer); err != nil {
			t.Fatalf("decode %q: %v", message, err)
		}
		if answer.StoredIn == "" {
			t.Fatal("the answer did not say where the capture went")
		}

		if !strings.Contains(line, answer.StoredIn) {
			t.Fatalf("startup line %q does not name %q, where the capture actually went", line, answer.StoredIn)
		}
		if got := localBaselineCount(t, store.Root()); got != 0 {
			t.Fatalf("%d captures of a private recipe's page were written to --baseline-root", got)
		}
	})

	t.Run("a browser host whose provider holds none", func(t *testing.T) {
		recipeBaselines := recipeBaselinesFor(providerWithoutBaselines{})
		if recipeBaselines != nil {
			t.Fatalf("a provider with no baseline capability was used as a store: %T", recipeBaselines)
		}
		line := recipeBaselineStatusLine(recipeBaselines)
		if !strings.Contains(line, "--baseline-root") {
			t.Fatalf("startup line %q does not name the destination left for this provider's recipes", line)
		}

		localRoot := filepath.Join(t.TempDir(), "baselines")
		store, err := baseline.NewStore(localRoot)
		if err != nil {
			t.Fatal(err)
		}
		server := mcp.New(&wiredDaemon{pageURL: page})
		server.SetBaselineStore(store)
		if _, err := installBaselineDestinations(server, recipeBaselines, "", nil); err != nil {
			t.Fatalf("installBaselineDestinations with no provider store: %v", err)
		}

		refused, message := baselineAnswer(t, callBaselineOverMCP(t, server,
			fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":0}`, strings.Repeat("ab", 32))))
		if refused {
			t.Fatalf("a daemon with a local root refused a capture nothing claims: %s", message)
		}

		if got := localBaselineCount(t, store.Root()); got != 1 {
			t.Fatalf("%d captures under --baseline-root, want the one the startup line said would land there", got)
		}
	})

	t.Run("a proxying daemon", func(t *testing.T) {
		host := routingHost(t, recipe.BaselineProvider)
		controller := browser.Controller(newWiredDaemon(t, host.URL, page))
		router, _ := controller.(recipe.BaselineRouter)
		localRoot := filepath.Join(t.TempDir(), "baselines")
		store, err := baseline.NewStore(localRoot)
		if err != nil {
			t.Fatal(err)
		}
		server := mcp.New(controller)
		server.SetBaselineStore(store)
		line, err := installBaselineDestinations(server, nil, host.URL, router)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(line, "refused here") {
			t.Fatalf("startup line %q does not tell the operator a provider baseline is refused rather than written locally", line)
		}
		refused, message := baselineAnswer(t, callBaselineOverMCP(t, server,
			fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":0}`, strings.Repeat("ab", 32))))
		if !refused {
			t.Fatalf("the proxy stored a capture the startup line said it would refuse: %s", message)
		}
		if got := localBaselineCount(t, store.Root()); got != 0 {
			t.Fatalf("%d captures under --baseline-root on a daemon whose startup line said none would land there", got)
		}
	})
}

type providerWithoutBaselines struct{ recipe.Provider }
