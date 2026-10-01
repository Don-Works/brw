package extensionbridge

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestExtensionDoesNotReplayAmbiguousMouseAcknowledgements(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	raw, err := os.ReadFile("../../extension/service_worker.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	start := strings.Index(source, "async function sendDebuggerCommand(")
	end := strings.Index(source[start:], "async function clearForcedHover(") + start
	script := `let attempts=0,attached=0;
 const state={attachUsedAt:new Map(),attachedTabs:new Set()};
 const queueAgentActivity=()=>{};
 const isForeignExtensionRefusal=()=>false;
 const isDetachedDebuggerError=()=>true;
 const attach=async()=>{attached++};
 const chrome={debugger:{sendCommand:async()=>{attempts++;if(attempts===1)throw Error('detached while handling command');return 'ok'}}};
 ` + source[start:end] + `
 (async()=>{
 for(const [method,type,retries] of [['Input.dispatchMouseEvent','mousePressed',false],['Input.dispatchMouseEvent','mouseReleased',false],['Input.dispatchTouchEvent','touchStart',false],['Input.dispatchTouchEvent','touchEnd',false],['Input.dispatchTouchEvent','touchMove',false],['Input.dispatchTouchEvent','touchCancel',false],['Input.dispatchMouseEvent','mouseMoved',true],['Runtime.evaluate',null,true]]){
 attempts=0;attached=0;let failed=false;
 try{await sendDebuggerCommand(42,method,{type})}catch(e){failed=true}
 if(failed===retries || attempts!==(retries?2:1) || attached!==(retries?1:0))throw Error(method+' '+type+' attempts='+attempts+' attached='+attached+' failed='+failed);
 }
 })().catch(e=>{console.error(e);process.exitCode=1});`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("extension retry contract: %v %s", err, out)
	}
}
