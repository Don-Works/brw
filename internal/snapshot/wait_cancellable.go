package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

// WaitForConditionCancellable removes its page observer and timers when the caller cancels.
func WaitForConditionCancellable(ctx context.Context, condition string, timeoutMs int64) (bool, error) {
	key := "__brw_wait_" + randomToken()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		encoded, _ := json.Marshal(key)
		expr := fmt.Sprintf(`(() => { const key=%s; if(typeof window[key]==='function') window[key](); })()`, encoded)
		_ = chromedp.Run(cleanupCtx, chromedp.Evaluate(expr, nil))
	}()
	return waitForCondition(ctx, condition, timeoutMs, key)
}
