package browser

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A browser brw did not start can leave the DevTools handshake unanswered for
// as long as it likes, and Chrome 144+ does exactly that: it asks the person at
// the browser to approve each remote debugging connection, so the WebSocket
// upgrade sits there until somebody clicks. Measured against an opted-in Chrome
// 153, the dial never completed and nothing on the wire said why — an
// unattended daemon hung until it was killed.
//
// The fixture is that wire: a listener that accepts the connection and answers
// nothing. What the test pins is both halves of the answer — that brw stops
// waiting, and that what it says names the prompt, because no retry and no
// reconfiguration of brw can clear one.
func TestAttachedBrowserThatNeverAnswersNamesTheApprovalPrompt(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for _, conn := range held {
			_ = conn.Close()
		}
		mu.Unlock()
	})

	restore := attachApprovalWindow
	attachApprovalWindow = 500 * time.Millisecond
	t.Cleanup(func() { attachApprovalWindow = restore })

	// The context is far longer than the window, so returning at all is the
	// window doing it. Without the bound this call waits for the context, which
	// on a real daemon has no deadline to wait for.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	started := time.Now()
	manager, err := New(ctx, Config{
		RemoteURL:    "http://" + listener.Addr().String(),
		BrowserWSURL: "ws://" + listener.Addr().String() + "/devtools/browser/never-answers",
		AttachOnly:   true,
		Timeout:      5 * time.Second,
	})
	waited := time.Since(started)
	if err == nil {
		_ = manager.Close()
		t.Fatal("attaching to a listener that answers nothing succeeded")
	}
	if !errors.Is(err, ErrAttachNotApproved) {
		t.Fatalf("error = %v, want ErrAttachNotApproved", err)
	}
	if waited > 15*time.Second {
		t.Fatalf("New waited %s for a browser that never answered; the approval window (%s) is not bounding it", waited, attachApprovalWindow)
	}
	// The message is the whole value of the error: the fix is a person clicking
	// something in a window brw cannot reach.
	for _, want := range []string{"approve", "prompt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q, so nobody reading it learns what to do", err, want)
		}
	}
}

// The wait is bounded; the browser is not. chromedp gives the CDP session the
// lifetime of the context the first Run was handed, so bounding that Run
// directly closed the session as soon as connect returned and released the
// bound — on the success path too, which left every later call on an attached
// lane failing with "context canceled".
//
// No wait is needed to see it: the release happens when connect returns, so the
// first call after New is already too late.
func TestBoundingTheApprovalWaitDoesNotCloseTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	_, port := startChromeOutsideBrw(ctx, t, "")

	manager, err := New(ctx, Config{
		RemoteURL: "http://127.0.0.1:" + strconv.Itoa(port),
		Timeout:   20 * time.Second,
	})
	if err != nil {
		t.Skipf("could not attach to the browser the test started: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	if _, err := manager.ListTabs(ctx); err != nil {
		t.Fatalf("listing tabs on a browser brw attached to: %v", err)
	}
}
