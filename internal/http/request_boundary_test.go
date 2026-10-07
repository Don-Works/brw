package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func TestDecodeRejectsTrailingData(t *testing.T) {
	for _, body := range []string{`{"ref":"e1"} {}`, `{"ref":"e1"} broken`, `{"ref":"e1"}` + strings.Repeat(" ", maxRequestBodyBytes)} {
		rec := httptest.NewRecorder()
		var dst struct{ Ref string }
		if decode(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), &dst) {
			t.Fatal("accepted trailing or oversized request")
		}
	}
	rec := httptest.NewRecorder()
	var dst struct{ Ref string }
	if !decode(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ref":"e1","future_field":true} `)), &dst) || dst.Ref != "e1" {
		t.Fatal("ordinary decoder lost unknown-field compatibility")
	}
}

type inputBoundaryController struct {
	*takeoverFake
	calls int
}

func (c *inputBoundaryController) DispatchTakeoverInput(context.Context, string, browser.TakeoverInput) error {
	c.calls++
	return nil
}

func TestTakeoverInputRejectsTrailingDataBeforeDispatch(t *testing.T) {
	t.Setenv(dashboardEnvVar, "1")
	ctrl := &inputBoundaryController{takeoverFake: newTakeoverFake()}
	s := New("127.0.0.1:17310", ctrl)
	for _, suffix := range []string{` {}`, ` broken`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/dashboard/input", strings.NewReader(`{"token":"held","event":{"kind":"key"}}`+suffix))
		req.RemoteAddr = "127.0.0.1:1"
		s.dashboardInput(rec, req)
		if rec.Code != http.StatusBadRequest || ctrl.calls != 0 {
			t.Fatalf("status=%d dispatches=%d", rec.Code, ctrl.calls)
		}
	}
}

type frameBoundaryController struct {
	fakeController
	shots  []string
	cancel context.CancelFunc
}

func (c *frameBoundaryController) Screenshot(context.Context) (browser.Screenshot, error) {
	if len(c.shots) == 0 {
		c.cancel()
		return browser.Screenshot{}, errors.New("done")
	}
	shot := c.shots[0]
	c.shots = c.shots[1:]
	return browser.Screenshot{Base64: shot}, nil
}

func TestDashboardEmitsChangedFramesOfEqualLength(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, second := base64.StdEncoding.EncodeToString([]byte("first")), base64.StdEncoding.EncodeToString([]byte("other"))
	ctrl := &frameBoundaryController{shots: []string{first, second, second}, cancel: cancel}
	rec := httptest.NewRecorder()
	(&Server{manager: ctrl}).streamViaScreenshots(ctx, rec, rec, time.Millisecond)
	if count := strings.Count(rec.Body.String(), "event: frame"); count != 2 || !strings.Contains(rec.Body.String(), second) {
		t.Fatalf("changed equal-length frame was lost or duplicate emitted: %s", rec.Body.String())
	}
}
