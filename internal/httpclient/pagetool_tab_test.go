package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/mcp"
)

const daemonPageToolID = "0a1b2c3d4e5f6071-3"

// pageToolDaemonController is the browser side of a daemon running WebMCP page
// tools: it answers the start and poll scripts the way a document does, and
// names the tab an untargeted page call lands in. The embedded interface is nil,
// so any call the routes under test do not make panics rather than quietly
// answering.
type pageToolDaemonController struct {
	browser.Controller
}

func (c *pageToolDaemonController) Evaluate(_ context.Context, expression string) (any, error) {
	if strings.Contains(expression, "var ARGS = ") {
		return map[string]any{"ok": true, "id": daemonPageToolID, "name": "export_ledger", "status": "running", "frame": "main"}, nil
	}
	return map[string]any{"ok": false, "id": daemonPageToolID, "status": "running"}, nil
}

// The client sends a session header, so the daemon's lease middleware opens the
// session's working tab before a page route runs.
func (c *pageToolDaemonController) OpenInGroup(context.Context, string, browser.TabGroupOptions) (browser.OpenResult, error) {
	return browser.OpenResult{Tab: browser.Tab{ID: "daemon-tab-4"}, Ready: true}, nil
}

func (c *pageToolDaemonController) ListTabs(context.Context) ([]browser.Tab, error) {
	return []browser.Tab{{ID: "daemon-tab-4"}}, nil
}

// ActiveTabID is the capability the whole hop rests on: only the daemon knows
// which tab it ran the page tool in.
func (c *pageToolDaemonController) ActiveTabID(context.Context) (string, error) {
	return "daemon-tab-4", nil
}

// callToolOverMCP drives the MCP server the way an agent does — one framed
// tools/call over the stdio transport — and returns the tool's text result.
func callToolOverMCP(t *testing.T, srv *mcp.Server, tool, args string) string {
	t.Helper()
	request := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`+"\n", tool, args)
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(request), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &response); err != nil {
		t.Fatalf("decode %s: %v", out.String(), err)
	}
	if response.Error != nil {
		t.Fatalf("tools/call error: %s", response.Error.Message)
	}
	if len(response.Result.Content) == 0 {
		t.Fatalf("tools/call returned no content: %s", out.String())
	}
	return response.Result.Content[0].Text
}

// A WebMCP page-tool report has to carry a tab for the agent to poll back into —
// a poll walks only the windows of the tab it lands in, and a page tool that
// opens a tab moves the active one. With --upstream-http the MCP server's
// controller is this HTTP client: nothing pins a tab into the context (only the
// extension bridge resolves one per tool call) and the process holds no browser
// of its own, so without asking the daemon the report names no tab at all —
// while the lost-invocation message tells the agent to pass back "the tab_id the
// invocation reported".
//
// This runs the real MCP tool over the real client against internal/http's real
// handler, because each layer passes its own unit test while the tab goes
// missing in the hop between them.
func TestProxiedPageToolReportNamesTheDaemonsTab(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
		args string
	}{
		{
			name: "a detached start",
			tool: "brw_call_page_tool",
			args: `{"name":"export_ledger","arguments":{"format":"csv"},"detach":true}`,
		},
		{
			name: "a poll",
			tool: "brw_page_tool_result",
			args: `{"invocation_id":"` + daemonPageToolID + `"}`,
		},
		{
			name: "a cancel",
			tool: "brw_page_tool_cancel",
			args: `{"invocation_id":"` + daemonPageToolID + `"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := httptest.NewServer(httpapi.New("", &pageToolDaemonController{}).Handler())
			t.Cleanup(daemon.Close)

			client, err := New(daemon.URL, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			text := callToolOverMCP(t, mcp.New(client), tc.tool, tc.args)
			if !strings.Contains(text, `"tab_id":"daemon-tab-4"`) {
				t.Fatalf("proxied report = %s, want it to name the daemon's tab", text)
			}
			if !strings.Contains(text, daemonPageToolID) {
				t.Fatalf("proxied report = %s, want the invocation id back", text)
			}
		})
	}
}

// The client half on its own: the route it asks for has to be the one the daemon
// registers, and an answer carrying no tab is an error rather than an empty
// string a caller would go on to report as the tab.
func TestActiveTabIDCrossesTheHTTPSurface(t *testing.T) {
	daemon := httptest.NewServer(httpapi.New("", &pageToolDaemonController{}).Handler())
	t.Cleanup(daemon.Close)

	client, err := New(daemon.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tabID, err := client.ActiveTabID(context.Background())
	if err != nil {
		t.Fatalf("active tab: %v", err)
	}
	if tabID != "daemon-tab-4" {
		t.Fatalf("active tab = %q, want the daemon's", tabID)
	}
}
