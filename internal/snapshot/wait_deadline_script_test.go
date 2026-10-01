package snapshot

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestWaitDeadlineConsumesRejectedAndThrowingThenables(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable for standalone wait-script regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	script := `
 global.window=globalThis;
 global.document={documentElement:{},addEventListener(){},removeEventListener(){}};
 global.addEventListener=()=>{};
 global.removeEventListener=()=>{};
 let observers=0,unhandled=0,uncaught=0;
 global.MutationObserver=class{constructor(){observers++}observe(){}disconnect(){observers--}};
 process.on('unhandledRejection',()=>{unhandled++});
 process.on('uncaughtException',()=>{uncaught++});
 const wait=` + WaitConditionScript + `;
 (async()=>{
  for(const value of ["Promise.reject(new Error('fixture rejection'))","({get then(){throw new Error('fixture getter')}})"]){
   window.calls=0;
   const condition='fn:(++window.calls === 1 ? false : '+value+')';
   let guard;
   const result=await Promise.race([wait(condition,8,'__brw_wait_fixture'),new Promise(resolve=>{guard=setTimeout(()=>resolve('outer-deadline'),80)})]);
   clearTimeout(guard);
   await new Promise(resolve=>setTimeout(resolve,20));
   if(result!==false || unhandled || uncaught || observers || typeof window.__brw_wait_fixture!=='undefined'){
    throw new Error(JSON.stringify({result,unhandled,uncaught,observers,registered:typeof window.__brw_wait_fixture}));
   }
  }
  console.log('ok');
 })().catch(error=>{console.error(error.message);process.exitCode=1});
 `
	command := exec.CommandContext(ctx, node, "--input-type=commonjs")
	command.Stdin = strings.NewReader(script)
	output, err := command.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "ok" {
		t.Fatalf("standalone wait deadline regression: err=%v output=%s", err, output)
	}
}
