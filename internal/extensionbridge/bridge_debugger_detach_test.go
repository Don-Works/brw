package extensionbridge

import (
	"strings"
	"testing"
)

// TestServiceWorkerDetachesDebuggerLifecycle locks in the fix for the reported
// Chrome instability / profile corruption: the extension used to call
// chrome.debugger.attach but NEVER detach, so debugger sessions accumulated on
// the user's real Chrome (destabilizing renderers, corrupting tab storage like
// WhatsApp Web). The service worker must now release debuggers on disconnect,
// on suspend, on tab close, and after idle.
func TestServiceWorkerDetachesDebuggerLifecycle(t *testing.T) {
	src := readServiceWorker(t)

	for _, want := range []string{
		"async function detach(tabId)",
		"async function detachAll()",
		"async function sweepIdleDebuggers()",
		"await chrome.debugger.detach({ tabId })", // an actual detach call exists
		"sweepIdleDebuggers().catch(() => {});",   // wired into the keepalive tick
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker debugger-detach lifecycle missing %q", want)
		}
	}

	// detachAll must run when the daemon disconnects (socket.onclose) — the
	// primary release point so brw holds no debuggers while disconnected/idle.
	onclose := sliceBetween(src, "socket.onclose = (event) =>", "scheduleReconnect(")
	if !strings.Contains(onclose, "detachAll()") {
		t.Fatal("socket.onclose must call detachAll() so debuggers are released when the daemon disconnects")
	}

	// ...and when the service worker suspends.
	onSuspend := sliceBetween(src, "chrome.runtime.onSuspend.addListener", "});")
	if !strings.Contains(onSuspend, "detachAll()") {
		t.Fatal("onSuspend must call detachAll() so a suspend never leaves Chrome in a debugged state")
	}

	// close_tab must detach before removing the tab.
	closeTab := sliceBetween(src, `message.type === "close_tab"`, "chrome.tabs.remove")
	if !strings.Contains(closeTab, "detach(tabId)") {
		t.Fatal("close_tab must detach the debugger before removing the tab")
	}
}
