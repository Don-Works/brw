package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/readability"
)

type settleController struct {
	browser.Controller
	budget int
	calls  int
}

func (c *settleController) Read(ctx context.Context) (readability.PageRead, error) {
	c.budget = readability.SettleMS(ctx)
	c.calls++
	return readability.PageRead{Main: "Ready"}, nil
}

func TestReadSettleCrossesTheHTTPHop(t *testing.T) {
	controller := &settleController{}
	daemon := httpapi.New("", controller)
	server := httptest.NewServer(daemon.Handler())
	defer server.Close()
	client, err := New(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	zero, longer := 0, 1500
	for _, tc := range []struct {
		value *int
		want  int
	}{{nil, 800}, {&zero, 0}, {&longer, 1500}} {
		if _, err := client.ReadWindow(browser.WithTabID(context.Background(), "77"), readability.ReadOptions{SettleMS: tc.value}); err != nil {
			t.Fatal(err)
		}
		if controller.budget != tc.want {
			t.Fatalf("host budget=%d want=%d", controller.budget, tc.want)
		}
	}
	for _, raw := range []string{"-1", "5001", "1.5", "", "invalid"} {
		before := controller.calls
		response, err := http.Get(server.URL + "/api/page/read?settle_ms=" + raw)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest || controller.calls != before {
			t.Fatalf("invalid budget %q: status=%d calls=%d", raw, response.StatusCode, controller.calls-before)
		}
	}
}
