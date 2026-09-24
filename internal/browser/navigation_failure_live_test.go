package browser

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serveNavigationOutcomes answers /basic with the challenge a Vercel preview
// deployment sends, /missing with a rendered 404, and anything else with a page.
func serveNavigationOutcomes(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/basic":
			w.Header().Set("WWW-Authenticate", `Basic realm="Preview"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("<h1>Authentication required</h1>"))
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("<h1>not here</h1>"))
		default:
			_, _ = w.Write([]byte("<h1>fine</h1>"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// closedLoopbackURL is an http URL on a loopback port nothing listens on.
func closedLoopbackURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr + "/"
}

func TestOpenReportsHowTheNavigationEnded(t *testing.T) {
	m := newHeadlessManager(t)
	base := serveNavigationOutcomes(t)
	refused := closedLoopbackURL(t)
	tests := []struct {
		name      string
		url       string
		wantReady bool
		wantError string
		wantHTTP  int
		wantAuth  *AuthChallenge
		wantHint  string
	}{
		{"page", base + "/", true, "", 0, nil, ""},
		{"rendered 404", base + "/missing", true, "", 404, nil, ""},
		{"basic auth challenge", base + "/basic", false, "net::ERR_INVALID_AUTH_CREDENTIALS", 401,
			&AuthChallenge{Scheme: "Basic", Realm: "Preview"}, "Call brw_authenticate"},
		{"connection refused", refused, false, "net::ERR_CONNECTION_REFUSED", 0, nil, "did not load"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			result, err := m.Open(ctx, tt.url)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = m.CloseTab(context.Background(), result.Tab.ID) }()
			if result.Ready != tt.wantReady || result.NavigationError != tt.wantError || result.HTTPStatus != tt.wantHTTP {
				t.Fatalf("result = %+v, want ready=%t navigation_error=%q http_status=%d", result, tt.wantReady, tt.wantError, tt.wantHTTP)
			}
			if (result.AuthRequired == nil) != (tt.wantAuth == nil) || (tt.wantAuth != nil && *result.AuthRequired != *tt.wantAuth) {
				t.Fatalf("auth_required = %+v, want %+v", result.AuthRequired, tt.wantAuth)
			}
			if !strings.Contains(result.Warning, tt.wantHint) || (tt.wantHint == "" && result.Warning != "") {
				t.Fatalf("warning = %q, want it to contain %q", result.Warning, tt.wantHint)
			}
		})
	}
}

func TestNavigateToAndBatchOpenFailOnAnAuthChallenge(t *testing.T) {
	m := newHeadlessManager(t)
	base := serveNavigationOutcomes(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	emulationTab(t, m, ctx, base+"/")
	_, err := m.NavigateTo(ctx, base+"/basic")
	var failed *NavigationFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("NavigateTo error = %v, want a NavigationFailedError", err)
	}
	if failed.Outcome.HTTPStatus != 401 || failed.Outcome.AuthRequired == nil || failed.Outcome.AuthRequired.Realm != "Preview" {
		t.Fatalf("navigate_to outcome = %+v", failed.Outcome)
	}
	if !strings.Contains(err.Error(), "Call brw_authenticate") {
		t.Fatalf("navigate_to error %q does not name brw_authenticate", err)
	}

	batch, err := m.ExecuteBatch(ctx, []BatchStep{
		{Action: "open", URL: base + "/basic"},
		{Action: "wait", Condition: "text:fine", TimeoutMS: 500},
	})
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}
	if batch.OK || len(batch.Steps) == 0 || batch.Steps[0].OK || !IsNavigationFailedMessage(batch.Steps[0].Error) {
		t.Fatalf("batch = %+v, want the open step to fail with the navigation failure", batch)
	}
}
