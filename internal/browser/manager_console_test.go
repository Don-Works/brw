package browser

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestManagerConsoleCaptureDrainsAndSurvivesNavigation(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	if _, err := m.ConsoleMessages(tabCtx); err != nil { // arm and drain
		t.Fatal(err)
	}
	if _, err := m.Evaluate(tabCtx, `console.log('brw-console', {answer: 42}); true`); err != nil {
		t.Fatal(err)
	}
	messages, err := m.ConsoleMessages(tabCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Level != "log" || !strings.Contains(messages[0].Text, "brw-console {\"answer\":42}") || messages[0].Timestamp == "" {
		t.Fatalf("captured console messages = %+v", messages)
	}
	if drained, err := m.ConsoleMessages(tabCtx); err != nil || len(drained) != 0 {
		t.Fatalf("second drain = %+v, err=%v", drained, err)
	}

	// The document-start registration must catch a destination page's inline
	// script, not merely logs emitted after brw_console is called.
	nav, err := m.NavigateTo(tabCtx, "data:text/html,%3Cscript%3Econsole.warn('after-navigation')%3C/script%3E")
	if err != nil || !nav.OK {
		t.Fatalf("navigate = %+v, err=%v", nav, err)
	}
	messages, err = m.ConsoleMessages(tabCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) == 0 || messages[0].Level != "warn" || !strings.Contains(messages[0].Text, "after-navigation") {
		t.Fatalf("navigation console messages = %+v", messages)
	}

	// Native Runtime.exceptionThrown capture must include load-time errors too;
	// page-level console monkey patches cannot observe these.
	nav, err = m.NavigateTo(tabCtx, "data:text/html,%3Cscript%3Ethrow%20new%20Error('load-time-boom')%3C/script%3E")
	if err != nil || !nav.OK {
		t.Fatalf("exception navigation = %+v, err=%v", nav, err)
	}
	messages, err = m.ConsoleMessages(tabCtx)
	if err != nil {
		t.Fatal(err)
	}
	foundException := false
	for _, message := range messages {
		if message.Level == "error" && strings.Contains(message.Text, "load-time-boom") {
			foundException = true
			break
		}
	}
	if !foundException {
		t.Fatalf("load-time exception missing from %+v", messages)
	}
}
