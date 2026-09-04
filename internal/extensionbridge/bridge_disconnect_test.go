package extensionbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/usagelog"
	"github.com/coder/websocket"
)

func TestBridgeDisconnectReason(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantReason   string
		wantExpected bool
	}{
		{name: "nil", wantReason: "connection closed", wantExpected: true},
		{name: "context canceled", err: context.Canceled, wantReason: "context canceled", wantExpected: true},
		{
			name:       "normal close with peer text",
			err:        websocket.CloseError{Code: websocket.StatusNormalClosure, Reason: "PRIVATE_CLOSE_REASON"},
			wantReason: "normal closure", wantExpected: true,
		},
		{
			name:       "service worker going away",
			err:        websocket.CloseError{Code: websocket.StatusGoingAway, Reason: "PRIVATE_CLOSE_REASON"},
			wantReason: "peer going away", wantExpected: true,
		},
		{
			name:       "abnormal close with peer text",
			err:        websocket.CloseError{Code: websocket.StatusPolicyViolation, Reason: "PRIVATE_CLOSE_REASON"},
			wantReason: "websocket close status 1008",
		},
		{name: "abrupt transport failure", err: errors.New("connection reset by peer"), wantReason: "connection reset by peer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotReason, gotExpected := bridgeDisconnectReason(tc.err)
			if gotReason != tc.wantReason || gotExpected != tc.wantExpected {
				t.Fatalf("bridgeDisconnectReason(%v) = (%q, %t), want (%q, %t)", tc.err, gotReason, gotExpected, tc.wantReason, tc.wantExpected)
			}
			if strings.Contains(gotReason, "PRIVATE_CLOSE_REASON") {
				t.Fatal("canonical expected-close reason retained peer-controlled text")
			}
		})
	}
}

func TestNormalBridgeDisconnectIsNotLoggedAsFailure(t *testing.T) {
	usagePath := filepath.Join(t.TempDir(), "usage.ndjson")
	recorder, err := usagelog.New(usagelog.Config{Path: usagePath, MaxBytes: 1 << 20, Backups: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()

	b := New("", time.Second, "")
	b.SetUsageRecorder(recorder)
	srv := httptest.NewServer(http.HandlerFunc(b.handleExtension))
	defer srv.Close()

	var logs bytes.Buffer
	previousLogOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogOutput)

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/extension", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{testDefaultOrigin}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitUntil(t, func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.conn != nil
	})
	if err := conn.Close(websocket.StatusNormalClosure, "PRIVATE_CLOSE_REASON"); err != nil {
		t.Fatalf("close: %v", err)
	}

	var disconnect usagelog.Event
	waitUntil(t, func() bool {
		data, readErr := os.ReadFile(usagePath)
		if readErr != nil {
			return false
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
			var event usagelog.Event
			if json.Unmarshal(line, &event) == nil && event.Operation == "bridge_disconnect" {
				disconnect = event
				return true
			}
		}
		return false
	})
	if disconnect.Outcome != "ok" || disconnect.ErrorClass != "" || disconnect.ErrorFingerprint != "" || disconnect.Retryable {
		t.Fatalf("normal disconnect usage event = %+v", disconnect)
	}
	b.mu.RLock()
	disconnectReason := b.disconnectReason
	b.mu.RUnlock()
	if disconnectReason != "normal closure" {
		t.Fatalf("disconnectReason = %q, want canonical normal closure", disconnectReason)
	}
	if strings.Contains(logs.String(), "PRIVATE_CLOSE_REASON") || strings.Contains(logs.String(), "extension bridge read:") {
		t.Fatalf("normal-close logs retained peer text or duplicate read failure: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "extension bridge disconnected cleanly: normal closure") {
		t.Fatalf("normal-close log missing clean lifecycle message: %s", logs.String())
	}
}
