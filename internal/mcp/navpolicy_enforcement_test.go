package mcp

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/navpolicy"
)

// callOpen drives a single brw_open through a server (optionally policy-gated)
// and returns the raw JSON-RPC response line.
func callOpen(t *testing.T, policy *navpolicy.Policy, url string) string {
	t.Helper()
	ctrl := &recordingController{}
	srv := New(ctrl)
	srv.SetNavigationPolicy(policy)
	input := lineJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "brw_open",
			"arguments": map[string]any{"url": url},
		},
	})
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	return out.String()
}

func TestNavPolicyBlocksDeniedOpen(t *testing.T) {
	policy := navpolicy.Parse("", "evil.com")
	resp := callOpen(t, policy, "https://evil.com/login")
	if !strings.Contains(resp, `"isError":true`) {
		t.Fatalf("expected an error result for a blocked domain, got: %s", resp)
	}
	if !strings.Contains(resp, "blocked") {
		t.Fatalf("error should explain the block, got: %s", resp)
	}
}

func TestNavPolicyAllowsPermittedOpen(t *testing.T) {
	policy := navpolicy.Parse("", "evil.com")
	resp := callOpen(t, policy, "https://good.com/")
	if strings.Contains(resp, `"isError":true`) {
		t.Fatalf("permitted domain should not error, got: %s", resp)
	}
}

func TestNavPolicyAllowlistDeniesUnlisted(t *testing.T) {
	policy := navpolicy.Parse("corp.example.com", "")
	if resp := callOpen(t, policy, "https://random.net/"); !strings.Contains(resp, `"isError":true`) {
		t.Fatalf("allowlist must deny an unlisted domain, got: %s", resp)
	}
	if resp := callOpen(t, policy, "https://corp.example.com/app"); strings.Contains(resp, `"isError":true`) {
		t.Fatalf("allowlist must permit a listed domain, got: %s", resp)
	}
}

func TestNoPolicyAllowsEverything(t *testing.T) {
	if resp := callOpen(t, nil, "https://anything.example/"); strings.Contains(resp, `"isError":true`) {
		t.Fatalf("nil policy must allow everything, got: %s", resp)
	}
}

// callNavTool drives any single navigation tool through a policy-gated server so
// every navigation entrypoint — not just brw_open — is proven to honor the
// guardrail and can't silently drift.
func callNavTool(t *testing.T, policy *navpolicy.Policy, tool string, args map[string]any) string {
	t.Helper()
	ctrl := &recordingController{}
	srv := New(ctrl)
	srv.SetNavigationPolicy(policy)
	input := lineJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	return out.String()
}

// TestNavPolicyGatesEveryNavEntrypoint proves the allowlist confines every tool
// that can steer the browser or make the daemon fetch a URL, including the new
// brw_navigate_to and the brw_upload_file url source (SSRF reach).
func TestNavPolicyGatesEveryNavEntrypoint(t *testing.T) {
	policy := navpolicy.Parse("corp.example.com", "")
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"brw_navigate_to", map[string]any{"url": "https://evil.com/x"}},
		{"brw_open_incognito", map[string]any{"url": "https://evil.com/x"}},
		{"brw_replay_request", map[string]any{"method": "GET", "url": "https://evil.com/x"}},
		{"brw_upload_file", map[string]any{"query": "file", "url": "http://169.254.169.254/latest/meta-data/"}},
		{"brw_navigate_to", map[string]any{"url": "file:///etc/passwd"}},
	}
	for _, c := range cases {
		resp := callNavTool(t, policy, c.tool, c.args)
		if !strings.Contains(resp, `"isError":true`) {
			t.Errorf("%s with %v must be denied under the allowlist, got: %s", c.tool, c.args, resp)
		}
	}
	// A permitted destination must still pass through navigate_to.
	if resp := callNavTool(t, policy, "brw_navigate_to", map[string]any{"url": "https://corp.example.com/app"}); strings.Contains(resp, `"isError":true`) {
		t.Fatalf("allowlisted navigate_to must pass, got: %s", resp)
	}
}

// TestWebMCPToolsAdvertised confirms the WebMCP tools are present in tools/list so
// agents can discover them.
func TestWebMCPToolsAdvertised(t *testing.T) {
	names := map[string]bool{}
	for _, tl := range tools() {
		if n, ok := tl["name"].(string); ok {
			names[n] = true
		}
	}
	for _, want := range []string{"brw_page_tools", "brw_call_page_tool"} {
		if !names[want] {
			t.Errorf("tools/list missing %q", want)
		}
	}
}
