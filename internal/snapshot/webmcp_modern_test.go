package snapshot_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

const modernNativeWebMCP = `(function(){
 Object.defineProperty(navigator, 'userAgent', { configurable: true, value: 'Chrome/155.0.0.0' });
 var tools = [];
 var handlers = {};
 window.__modern = { calls: 0, inputs: [], aborted: false, settled: false };
 var mc = {
  getTools: function(){ return Promise.resolve(tools.slice()); },
  registerTool: function(tool, options){
   tools.push({name:tool.name,description:tool.description,inputSchema:tool.inputSchema,annotations:tool.annotations,window:window});
   handlers[tool.name] = tool.execute;
   if (options && options.signal) options.signal.addEventListener('abort',function(){ tools=tools.filter(function(row){ return row.name !== tool.name; }); });
  },
  executeTool: function(tool, input, options){
   window.__modern.calls++;
   window.__modern.inputs.push(typeof input);
   if (!input || typeof input !== 'object') return Promise.reject(new TypeError('object required'));
   return Promise.resolve(handlers[tool.name](input,options)).then(JSON.stringify);
  }
 };
 Object.defineProperty(document,'modelContext',{configurable:true,value:mc});
 mc.registerTool({name:'lookup',description:'Read a local fixture',inputSchema:{type:'object',properties:{id:{type:'string'}},required:['id']},annotations:{readOnlyHint:true},execute:function(args){return {id:args.id};}});
 window.__registration = new AbortController();
 mc.registerTool({name:'pending',description:'Await local fixture completion',inputSchema:{type:'object'},annotations:{readOnlyHint:true},execute:function(args,options){
  return new Promise(function(resolve,reject){
   window.__finish = function(){ window.__modern.settled=true; resolve({finished:true}); };
   options.signal.addEventListener('abort',function(){window.__modern.aborted=true;reject(new Error('fixture aborted'));});
  });
 }},{signal:window.__registration.signal});
 mc.registerTool({name:'reject_lookup',description:'Read a malformed local fixture',inputSchema:{type:'object'},annotations:{readOnlyHint:true},execute:function(){return Promise.reject(new Error('Unable to parse input fixture'));}});
})()`

func TestModernNativeWebMCPUsesObjectInputOnce(t *testing.T) {
	srv := servePage(t, `<!doctype html><html><body><h1>fixture</h1></body></html>`)
	ctx := openArmed(t, srv.URL, modernNativeWebMCP)
	call := callTool(t, ctx, "lookup", `{"id":"fixture-1"}`)
	if !call.OK || string(call.Result) != `{"id":"fixture-1"}` {
		t.Fatalf("result: %+v", call)
	}
	var state struct {
		Calls  int      `json:"calls"`
		Inputs []string `json:"inputs"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__modern`, &state)); err != nil {
		t.Fatal(err)
	}
	if state.Calls != 1 || !reflect.DeepEqual(state.Inputs, []string{"object"}) {
		t.Fatalf("input dispatches: %+v", state)
	}
}

func TestModernNativeWebMCPRejectsWithoutRetry(t *testing.T) {
	srv := servePage(t, `<!doctype html><html><body><h1>fixture</h1></body></html>`)
	ctx := openArmed(t, srv.URL, modernNativeWebMCP)
	call := callTool(t, ctx, "reject_lookup", `{}`)
	if call.OK || call.Status != snapshot.PageToolFailed || call.Error != "Unable to parse input fixture" {
		t.Fatalf("rejection: %+v", call)
	}
	var calls int
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__modern.calls`, &calls)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("read-only rejection invoked %d times, want one", calls)
	}
}

func TestModernNativeWebMCPUnregisterKeepsPendingExecution(t *testing.T) {
	srv := servePage(t, `<!doctype html><html><body><h1>fixture</h1></body></html>`)
	ctx := openArmed(t, srv.URL, modernNativeWebMCP)
	eval := chromedpEvaluator(ctx)
	start, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{Name: "pending", Arguments: json.RawMessage(`{}`), Detach: true, Validate: true})
	if err != nil || start.Status != snapshot.PageToolRunning {
		t.Fatalf("start %+v: %v", start, err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__registration.abort()`, nil)); err != nil {
		t.Fatal(err)
	}
	listing := listTools(t, ctx, "")
	for _, tool := range listing.Tools {
		if tool.Name == "pending" {
			t.Fatal("aborted registration still discoverable")
		}
	}
	state, err := snapshot.AwaitPageTool(ctx, eval, start.ID, 0)
	if err != nil || state.Status != snapshot.PageToolRunning {
		t.Fatalf("unregister changed pending outcome %+v: %v", state, err)
	}
	var aborted bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__modern.aborted`, &aborted)); err != nil {
		t.Fatal(err)
	}
	if aborted {
		t.Fatal("registration abort reached execution signal")
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__finish()`, nil)); err != nil {
		t.Fatal(err)
	}
	done, err := snapshot.AwaitPageTool(ctx, eval, start.ID, time.Second)
	if err != nil || !done.OK || done.Status != snapshot.PageToolDone || string(done.Result) != `{"finished":true}` {
		t.Fatalf("asynchronous outcome %+v: %v", done, err)
	}
}

func TestModernNativeWebMCPCancelForwardsExecutionSignal(t *testing.T) {
	srv := servePage(t, `<!doctype html><html><body><h1>fixture</h1></body></html>`)
	ctx := openArmed(t, srv.URL, modernNativeWebMCP)
	eval := chromedpEvaluator(ctx)
	start, err := snapshot.InvokePageTool(ctx, eval, snapshot.PageToolInvokeOptions{Name: "pending", Arguments: json.RawMessage(`{}`), Detach: true, Validate: true})
	if err != nil || start.Status != snapshot.PageToolRunning {
		t.Fatalf("start %+v: %v", start, err)
	}
	cancelled, err := snapshot.CancelPageTool(ctx, eval, start.ID)
	if err != nil || !cancelled.Cancelled || cancelled.Status != snapshot.PageToolCancelled {
		t.Fatalf("cancel %+v: %v", cancelled, err)
	}
	var aborted bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__modern.aborted`, &aborted)); err != nil {
		t.Fatal(err)
	}
	if !aborted {
		t.Fatal("execution signal not forwarded")
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__finish()`, nil)); err != nil {
		t.Fatal(err)
	}
	after, err := snapshot.AwaitPageTool(ctx, eval, start.ID, time.Second)
	if err != nil || after.Status != snapshot.PageToolCancelled || after.OK {
		t.Fatalf("late settlement overwrote cancellation %+v: %v", after, err)
	}
	listing := listTools(t, ctx, "")
	found := false
	for _, tool := range listing.Tools {
		if tool.Name == "pending" {
			found = true
		}
	}
	if !found {
		t.Fatal("execution cancellation removed registration")
	}
}
