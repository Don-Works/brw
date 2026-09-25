package snapshot_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/Don-Works/brw/internal/snapshot"
)

// fakeNativeWebMCP stands in for Chrome's native document.modelContext, which a
// test browser does not enable: getTools answers a promise of tools whose
// inputSchema is a JSON string, a same-origin child frame's tool is included the
// way Chromium includes it, and executeTool answers a JSON string. It refuses an
// object input the way Chromium before 155 does.
const fakeNativeWebMCP = `(function(){
  var tools = [
    { name: 'lookup_order', description: 'Look up an order\nSecond line', window: window,
      inputSchema: '{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}',
      annotations: { readOnly: true } },
    { name: 'child_tool', description: 'belongs to a child frame', inputSchema: '{}', window: {} }
  ];
  window.__nativeInputs = [];
  Object.defineProperty(document, 'modelContext', { configurable: true, value: {
    getTools: function(){ return Promise.resolve(tools); },
    executeTool: function(tool, input){
      window.__nativeInputs.push(typeof input);
      if (typeof input !== 'string') return Promise.reject(new Error('Failed to parse input arguments'));
      return Promise.resolve(JSON.stringify({ order: JSON.parse(input).id, via: tool.name }));
    },
    registerTool: function(){}
  }});
})()`

// openArmed navigates a fresh headless tab to url with the given document-start
// scripts registered in order, then the brw shim.
func openArmed(t *testing.T, url string, before ...string) context.Context {
	t.Helper()
	browserCtx, cancel := newHeadlessChrome(t)
	t.Cleanup(cancel)
	ctx, ctxCancel := context.WithTimeout(browserCtx, 60*time.Second)
	t.Cleanup(ctxCancel)
	for _, source := range before {
		if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(source).Do(ctx)
			return err
		})); err != nil {
			t.Skipf("document-start registration unavailable: %v", err)
		}
	}
	if err := snapshot.RegisterWebMCPOnNewDocument(ctx); err != nil {
		t.Skipf("document-start registration unavailable: %v", err)
	}
	if err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible("h1", chromedp.ByQuery),
		chromedp.Sleep(150*time.Millisecond),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	return ctx
}

func listTools(t *testing.T, ctx context.Context, frame string) snapshot.PageToolListing {
	t.Helper()
	listing, err := snapshot.ListPageTools(ctx, chromedpEvaluator(ctx), frame)
	if err != nil {
		t.Fatalf("list page tools: %v", err)
	}
	return listing
}

func callTool(t *testing.T, ctx context.Context, name, args string) snapshot.PageToolInvocation {
	t.Helper()
	call, err := snapshot.InvokePageTool(ctx, chromedpEvaluator(ctx), snapshot.PageToolInvokeOptions{
		Name: name, Arguments: json.RawMessage(args), Validate: true, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return call
}

func TestNativeWebMCPIsUsedAndNeverShimmed(t *testing.T) {
	srv := servePage(t, `<!doctype html><html><body><h1>native</h1></body></html>`)
	ctx := openArmed(t, srv.URL, fakeNativeWebMCP)

	var shimmed bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`!!(document.modelContext && document.modelContext.__brw)`, &shimmed)); err != nil {
		t.Fatal(err)
	}
	if shimmed {
		t.Fatal("brw replaced a native document.modelContext with its shim")
	}

	listing := listTools(t, ctx, "")
	if listing.Runtime != "native" || !listing.Supported {
		t.Fatalf("runtime = %q supported = %v, want native and supported", listing.Runtime, listing.Supported)
	}
	if len(listing.Tools) != 1 || listing.Tools[0].Name != "lookup_order" {
		t.Fatalf("tools = %+v, want only lookup_order (the child frame's tool belongs to the frame)", listing.Tools)
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(listing.Tools[0].InputSchema, &schema); err != nil || !reflect.DeepEqual(schema.Required, []string{"id"}) {
		t.Fatalf("inputSchema %s was not parsed from its JSON string (err %v)", listing.Tools[0].InputSchema, err)
	}
	if !listing.Tools[0].Annotations["readOnlyHint"] {
		t.Fatalf("annotations = %v, want the CDP-style readOnly normalised to readOnlyHint", listing.Tools[0].Annotations)
	}

	call := callTool(t, ctx, "lookup_order", `{"id":"A1"}`)
	var result struct {
		Order string `json:"order"`
		Via   string `json:"via"`
	}
	if err := json.Unmarshal(call.Result, &result); err != nil || !call.OK || result.Order != "A1" || result.Via != "lookup_order" {
		t.Fatalf("native executeTool result = %+v (%s), err %v", call, call.Result, err)
	}
	var inputs []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__nativeInputs`, &inputs)); err != nil {
		t.Fatal(err)
	}
	if len(inputs) == 0 || inputs[len(inputs)-1] != "string" {
		t.Fatalf("executeTool inputs = %v, want the call to land as a JSON string", inputs)
	}
}

const shimToolsPage = `<!doctype html><html><head>
<link rel="alternate" type="text/markdown" href="/page.md">
<link rel="service-desc" href="/openapi.json">
<link rel="mcp" href="https://app.test/mcp">
</head><body><h1>tools</h1>
<script>
  var ctl = new AbortController();
  document.modelContext.registerTool({ name: 'temp', description: 'goes away', execute: function(){ return 1; } }, { signal: ctl.signal });
  document.modelContext.registerTool({ name: 'get_weather', description: 'Weather for a city\nmore detail',
    inputSchema: { type: 'object', properties: { city: { type: 'string' } } },
    annotations: { readOnlyHint: true },
    execute: function(a){ return { city: a.city, temp: 20 }; } });
  document.modelContext.registerTool({ name: 'delete_account', description: 'Delete the account',
    annotations: { destructiveHint: true }, execute: function(){ return 'gone'; } });
  ctl.abort();
</script></body></html>`

func TestShimFollowsTheCurrentWebMCPAPI(t *testing.T) {
	srv := servePage(t, shimToolsPage)
	ctx := openArmed(t, srv.URL)

	listing := listTools(t, ctx, "")
	var names []string
	for _, tool := range listing.Tools {
		names = append(names, tool.Name)
	}
	if listing.Runtime != "brw" || !reflect.DeepEqual(names, []string{"get_weather", "delete_account"}) {
		t.Fatalf("runtime %q tools %v, want brw with the aborted registration gone", listing.Runtime, names)
	}
	if !listing.Tools[1].Consequential() {
		t.Fatalf("delete_account should count as consequential: %+v", listing.Tools[1])
	}

	var viaAPI struct {
		Count  int    `json:"count"`
		Schema string `json:"schema"`
		Output string `json:"output"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(async function(){
	  var tools = await document.modelContext.getTools();
	  var output = await document.modelContext.executeTool(tools[0], '{"city":"Leeds"}');
	  return { count: tools.length, schema: typeof tools[0].inputSchema === 'string' ? tools[0].inputSchema : '', output: output };
	})()`, &viaAPI, awaitPromise)); err != nil {
		t.Fatal(err)
	}
	if viaAPI.Count != 2 || viaAPI.Schema == "" || viaAPI.Output != `{"city":"Leeds","temp":20}` {
		t.Fatalf("getTools/executeTool through the shim = %+v", viaAPI)
	}

	call := callTool(t, ctx, "get_weather", `{"city":"York"}`)
	if !call.OK || string(call.Result) != `{"city":"York","temp":20}` {
		t.Fatalf("get_weather = %+v (%s)", call, call.Result)
	}

	digest, err := snapshot.ReadPageSurfaces(ctx, chromedpEvaluator(ctx))
	if err != nil {
		t.Fatal(err)
	}
	want := []snapshot.PageToolSummary{
		{Name: "get_weather", Description: "Weather for a city", ReadOnly: true},
		{Name: "delete_account", Description: "Delete the account", Consequential: true},
	}
	if !reflect.DeepEqual(digest.Tools, want) {
		t.Fatalf("digest tools = %+v, want %+v", digest.Tools, want)
	}
	wantSurfaces := snapshot.PageSurfaces{
		Markdown:        []string{srv.URL + "/page.md"},
		APIDescriptions: []string{srv.URL + "/openapi.json"},
		MCP:             []string{"https://app.test/mcp"},
	}
	if !reflect.DeepEqual(digest.Surfaces, wantSurfaces) {
		t.Fatalf("digest surfaces = %+v, want %+v", digest.Surfaces, wantSurfaces)
	}
}

const declarativeFormsPage = `<!doctype html><html><body><h1>forms</h1>
<form toolname="search_products" tooldescription="Search the catalogue" toolautosubmit>
  <input name="q" toolparamdescription="What to search for" required>
  <select name="sort"><option value="price">price</option><option value="rating">rating</option></select>
  <input type="password" name="pw">
</form>
<form toolname="draft_note" tooldescription="Draft a note">
  <textarea name="body"></textarea>
</form>
<script>
  document.forms[0].addEventListener('submit', function(e){
    e.preventDefault();
    if (e.agentInvoked) e.respondWith(Promise.resolve({ results: [e.target.q.value + ':' + e.target.sort.value] }));
  });
</script></body></html>`

func TestDeclarativeFormToolsWithoutNativeSupport(t *testing.T) {
	srv := servePage(t, declarativeFormsPage)
	ctx := openArmed(t, srv.URL)

	listing := listTools(t, ctx, "")
	if len(listing.Tools) != 2 {
		t.Fatalf("tools = %+v, want the two declarative forms", listing.Tools)
	}
	search := listing.Tools[0]
	var schema struct {
		Properties map[string]struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(search.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if !search.Declarative || search.Name != "search_products" || !search.Consequential() {
		t.Fatalf("search_products descriptor = %+v", search)
	}
	if _, leaked := schema.Properties["pw"]; leaked {
		t.Fatal("a password field was offered as a tool parameter")
	}
	if schema.Properties["q"].Description != "What to search for" || !reflect.DeepEqual(schema.Required, []string{"q"}) ||
		!reflect.DeepEqual(schema.Properties["sort"].Enum, []string{"price", "rating"}) {
		t.Fatalf("schema = %+v", schema)
	}

	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{name: "an autosubmit form answers through respondWith", tool: "search_products", args: `{"q":"boots","sort":"rating"}`, want: `{"results":["boots:rating"]}`},
		{name: "a form without autosubmit is only filled", tool: "draft_note", args: `{"body":"hi"}`, want: `"submitted":false`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := callTool(t, ctx, tc.tool, tc.args)
			if !call.OK || !json.Valid(call.Result) || !strings.Contains(string(call.Result), tc.want) {
				t.Fatalf("%s = %+v (%s), want it to contain %s", tc.tool, call, call.Result, tc.want)
			}
		})
	}
	var body string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.forms[1].body.value`, &body)); err != nil || body != "hi" {
		t.Fatalf("draft_note textarea = %q (err %v)", body, err)
	}
}

func TestPageSurfacesOnAPlainPageAreEmpty(t *testing.T) {
	srv := servePage(t, `<!doctype html><html><body><h1>plain</h1></body></html>`)
	ctx := openArmed(t, srv.URL)
	digest, err := snapshot.ReadPageSurfaces(ctx, chromedpEvaluator(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if len(digest.Tools) != 0 || !digest.Surfaces.Empty() {
		t.Fatalf("plain page digest = %+v, want nothing", digest)
	}
}
