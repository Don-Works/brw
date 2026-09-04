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
// on suspend, through Chrome's onDetach/onRemoved events after tab close, and
// after idle.
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

	// close_tab deliberately keeps Page attached until removal. A beforeunload
	// dialog can appear during chrome.tabs.remove; detaching first prevents the
	// Page.javascriptDialogOpening handler from accepting the explicit close and
	// leaves the remove request stuck forever.
	closeTab := sliceBetween(src, `message.type === "close_tab"`, `send({ id: message.id, ok: true, result: { closed: tabId } })`)
	for _, want := range []string{"await attach(tabId, { skipRevive: true, requirePageEvents: true })", "markActing(tabId)", `chrome.debugger.sendCommand({ tabId }, "Page.close", {})`, "await waitForTabGone(tabId, 2000)"} {
		if !strings.Contains(closeTab, want) {
			t.Fatalf("close_tab must preserve dialog handling until removal; missing %q", want)
		}
	}
	if detachAt, closeAt := strings.Index(closeTab, "await detach(tabId)"), strings.Index(closeTab, `chrome.debugger.sendCommand({ tabId }, "Page.close", {})`); detachAt >= 0 && detachAt < closeAt {
		t.Fatal("close_tab must not detach before Page.close; doing so wedges beforeunload-protected tabs")
	}
}
