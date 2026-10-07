package httpclient

import (
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func TestPlanClientTimeoutCoversEveryStep(t *testing.T) {
	steps := []browser.PlanStep{
		{Action: "navigate_to", URL: "https://example.com/settings"},
		{Action: "wait", Condition: "fn:(()=>true)()", TimeoutMS: 20000},
		{Action: "wait", Condition: "fn:(()=>true)()", TimeoutMS: 20000},
		{Action: "evaluate"},
	}
	got := planClientTimeout(20*time.Second, steps)
	if want := 80*time.Second + 4*planStepMargin; got != want {
		t.Fatalf("planClientTimeout = %s, want %s", got, want)
	}
	c, err := New("http://127.0.0.1:1", 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if client := withMinimumTimeout(c.client, got); client.Timeout != got || c.client.Timeout != 20*time.Second {
		t.Fatalf("plan client timeout = %s, shared client = %s", client.Timeout, c.client.Timeout)
	}
	if short := planClientTimeout(20*time.Second, []browser.PlanStep{{Action: "press", Key: "Enter"}}); withMinimumTimeout(c.client, short).Timeout != short {
		t.Fatalf("a one-step plan keeps at least its own budget")
	}
}

func TestPlanClientTimeoutWithoutAFixedLimitStaysUnlimited(t *testing.T) {
	steps := make([]browser.PlanStep, 40)
	if got := planClientTimeout(browser.NoOperationTimeout, steps); got != browser.NoOperationTimeout {
		t.Fatalf("planClientTimeout = %s, want no fixed limit", got)
	}
}
