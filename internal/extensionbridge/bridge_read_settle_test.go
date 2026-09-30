package extensionbridge

import (
	"context"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/readability"
)

func TestBridgeForwardsReadSettleBudget(t *testing.T) {
	b := New("", 5*time.Second, "")
	rec, cleanup := connectEvalRecorder(t, b)
	defer cleanup()
	for _, ms := range []int{0, 1500} {
		if _, err := b.Read(readability.WithSettleMS(context.Background(), ms)); err != nil {
			t.Fatal(err)
		}
		rec.mu.Lock()
		found := false
		for _, params := range rec.evals {
			if params["expression"] == readability.ReadExpr(ms) {
				found = true
			}
		}
		rec.mu.Unlock()
		if !found {
			t.Fatalf("extension did not receive settle budget %d", ms)
		}
	}
}
