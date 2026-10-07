package extensionbridge

import (
	"strings"
	"testing"
)

func TestServiceWorkerDetachesDebuggerLifecycle(t *testing.T) {
	src := readServiceWorker(t)

	for _, want := range []string{
		"async function detach(tabId)",
		"async function detachAll()",
		"async function sweepIdleDebuggers()",
		"await chrome.debugger.detach({ tabId })",
		"sweepIdleDebuggers().catch(() => {});",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("service worker debugger-detach lifecycle missing %q", want)
		}
	}

	onclose := sliceBetween(src, "socket.onclose = (event) =>", "scheduleReconnect(")
	if !strings.Contains(onclose, "detachAll()") {
		t.Fatal("socket.onclose must call detachAll() so debuggers are released when the daemon disconnects")
	}

	onSuspend := sliceBetween(src, "chrome.runtime.onSuspend.addListener", "});")
	if !strings.Contains(onSuspend, "detachAll()") {
		t.Fatal("onSuspend must call detachAll() so a suspend never leaves Chrome in a debugged state")
	}

	closeTab := sliceBetween(src, `message.type === "close_tab"`, `send({ id: message.id, ok: true, result: { closed: tabId } })`)
	for _, want := range []string{"attach(tabId, { skipRevive: true, requirePageEvents: true })", "markActing(tabId)", `chrome.debugger.sendCommand({ tabId }, "Page.close", {})`, "await waitForTabGone(tabId, 2000)"} {
		if !strings.Contains(closeTab, want) {
			t.Fatalf("close_tab must preserve dialog handling until removal; missing %q", want)
		}
	}
	if detachAt, closeAt := strings.Index(closeTab, "await detach(tabId)"), strings.Index(closeTab, `chrome.debugger.sendCommand({ tabId }, "Page.close", {})`); detachAt >= 0 && detachAt < closeAt {
		t.Fatal("close_tab must not detach before Page.close; doing so wedges beforeunload-protected tabs")
	}
}
