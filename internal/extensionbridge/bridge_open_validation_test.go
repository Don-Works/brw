package extensionbridge

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func TestBridgeOpenRejectsInvalidReturnedTabID(t *testing.T) {
	for _, id := range []int{0, -1} {
		for _, background := range []bool{false, true} {
			t.Run(fmt.Sprintf("id=%d/background=%t", id, background), func(t *testing.T) {
				b := New("", time.Second, "")
				stub := &cdpStub{reply: func(cdpCall, int) (map[string]any, string) {
					return map[string]any{"id": id}, ""
				}}
				cleanup := serveCDPStub(t, b, stub)
				defer cleanup()
				ctx := context.Background()
				if background {
					ctx = browser.WithBackgroundPage(ctx)
				}
				result, err := b.Open(ctx, "https://example.test/")
				if err == nil || !strings.Contains(err.Error(), "no tab id") || result.Tab.ID != "" {
					t.Fatalf("invalid returned ID accepted: %+v %v", result, err)
				}
				if b.activeTabID() != "" || len(stub.methods()) != 1 {
					t.Fatal("invalid tab acquired ownership or performed further work")
				}
			})
		}
	}
}
