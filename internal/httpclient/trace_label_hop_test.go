package httpclient

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	httpapi "github.com/Don-Works/brw/internal/http"
)

type evaluateLabelController struct {
	browser.Controller
	label   browser.TraceLabel
	labeled bool
}

func (c *evaluateLabelController) Evaluate(ctx context.Context, _ string) (any, error) {
	c.label, c.labeled = browser.TraceLabelFromCtx(ctx)
	return "ok", nil
}

func (c *evaluateLabelController) OpenInGroup(context.Context, string, browser.TabGroupOptions) (browser.OpenResult, error) {
	return browser.OpenResult{Tab: browser.Tab{ID: "tab-1"}, Ready: true}, nil
}

func (c *evaluateLabelController) ListTabs(context.Context) ([]browser.Tab, error) {
	return []browser.Tab{{ID: "tab-1"}}, nil
}

func TestPageToolLabelCrossesTheHTTPSurface(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ctx       func() context.Context
		wantLabel bool
		want      browser.TraceLabel
	}{
		{
			name: "a proxied page-tool poll",
			ctx: func() context.Context {
				return browser.WithTraceLabel(context.Background(), browser.TraceActionPageTool, "result 0a1b2c3d4e5f6071-3")
			},
			wantLabel: true,
			want:      browser.TraceLabel{Action: browser.TraceActionPageTool, Value: "result 0a1b2c3d4e5f6071-3"},
		},
		{
			name: "a proxied get",
			ctx: func() context.Context {
				return browser.WithTraceLabel(context.Background(), browser.TraceActionGet, "text #pad")
			},
			wantLabel: true,
			want:      browser.TraceLabel{Action: browser.TraceActionGet, Value: "text #pad"},
		},
		{
			name: "a proxied frame switch",
			ctx: func() context.Context {
				return browser.WithTraceLabel(context.Background(), browser.TraceActionFrame, "main")
			},
			wantLabel: true,
			want:      browser.TraceLabel{Action: browser.TraceActionFrame, Value: "main"},
		},
		{
			name: "an unlabelled evaluation",
			ctx:  context.Background,
		},
		{

			name: "a caller cannot claim an input action",
			ctx: func() context.Context {
				return browser.WithTraceLabel(context.Background(), "click", "e3")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &evaluateLabelController{}
			daemon := httptest.NewServer(httpapi.New("", ctrl).Handler())
			t.Cleanup(daemon.Close)

			client, err := New(daemon.URL, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Evaluate(tc.ctx(), "1 + 1"); err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if ctrl.labeled != tc.wantLabel {
				t.Fatalf("the daemon's controller saw label %+v (labelled %v), want labelled %v", ctrl.label, ctrl.labeled, tc.wantLabel)
			}
			if tc.wantLabel && ctrl.label != tc.want {
				t.Fatalf("the daemon's controller saw label %+v, want %+v", ctrl.label, tc.want)
			}
		})
	}
}
