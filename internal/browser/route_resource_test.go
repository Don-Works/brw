package browser

import (
	"testing"

	"github.com/chromedp/cdproto/network"
)

func TestNormalizeResourceTypes(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    []string
		wantErr bool
	}{
		{name: "empty means every kind", in: nil, want: nil},
		{name: "one type", in: []string{"script"}, want: []string{"script"}},
		{name: "case and space are normalized", in: []string{" Script ", "IMAGE"}, want: []string{"script", "image"}},
		{name: "document expands to both frame kinds", in: []string{"document"}, want: []string{"main_frame", "sub_frame"}},
		{name: "deduplicates", in: []string{"script", "script"}, want: []string{"script"}},
		{name: "unknown type", in: []string{"coffee"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeResourceTypes(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeResourceTypes(%v) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestResourceTypeMatches(t *testing.T) {
	tests := []struct {
		name     string
		filter   []string
		resource network.ResourceType
		want     bool
	}{
		{name: "empty filter matches everything", filter: nil, resource: network.ResourceTypeImage, want: true},
		{name: "script matches script", filter: []string{"script"}, resource: network.ResourceTypeScript, want: true},
		{name: "script does not match image", filter: []string{"script"}, resource: network.ResourceTypeImage, want: false},
		{name: "xhr maps to xmlhttprequest", filter: []string{"xmlhttprequest"}, resource: network.ResourceTypeXHR, want: true},
		{name: "fetch maps to xmlhttprequest", filter: []string{"xmlhttprequest"}, resource: network.ResourceTypeFetch, want: true},
		{name: "a document matches either frame kind", filter: []string{"sub_frame"}, resource: network.ResourceTypeDocument, want: true},
		{name: "an unmapped type folds to other", filter: []string{"other"}, resource: network.ResourceTypeTextTrack, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resourceTypeMatches(tt.filter, tt.resource); got != tt.want {
				t.Fatalf("resourceTypeMatches(%v, %v) = %v, want %v", tt.filter, tt.resource, got, tt.want)
			}
		})
	}
}

// A route that names resource types must decline a request of another kind, so
// the scan continues to a later rule rather than the first match winning.
func TestRouteStateMatchHonoursResourceTypes(t *testing.T) {
	state := &routeState{}
	state.routes = map[string][]*Route{
		"t1": {
			{Pattern: "*", Behaviour: RouteAbort, ResourceTypes: []string{"script"}},
		},
	}
	if got := state.match("t1", "https://example.com/a.js", network.ResourceTypeScript); got == nil {
		t.Fatal("a script request should match a script-only route")
	}
	if got := state.match("t1", "https://example.com/a.png", network.ResourceTypeImage); got != nil {
		t.Fatal("an image request should not match a script-only route")
	}
	// An unfiltered route still matches everything.
	state.routes["t2"] = []*Route{{Pattern: "*", Behaviour: RouteAbort}}
	if got := state.match("t2", "https://example.com/a.png", network.ResourceTypeImage); got == nil {
		t.Fatal("an unfiltered route should match an image request")
	}
}
