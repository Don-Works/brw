package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSetAuthTokenSendsBearerOnlyToUpstream(t *testing.T) {
	bearer := strings.Repeat("k", 32)
	var otherSaw string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherSaw = r.Header.Get("Authorization")
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer other.Close()
	var upstreamSaw string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamSaw = r.Header.Get("Authorization")
		http.Redirect(w, r, other.URL+"/health", http.StatusFound)
	}))
	defer upstream.Close()

	c, err := New(upstream.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetAuthToken(bearer)
	_, _ = c.Health(context.Background())
	if upstreamSaw != "Bearer "+bearer {
		t.Fatalf("upstream Authorization = %q", upstreamSaw)
	}
	if otherSaw != "" {
		t.Fatalf("redirect target received Authorization %q", otherSaw)
	}
}
