package snapshot_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

const getSurfaceFixture = `<!doctype html><html><head><meta charset="utf-8"><title>Get surface</title></head>
<body style="margin:0">
<h1 id="heading">Hello</h1>
<input id="name" value="Ada" data-kind="person">
<input id="agree" type="checkbox" checked>
<button id="go" disabled>Go</button>
<p class="row">one</p><p class="row">two</p><p class="row">three</p>
<div id="gone" style="display:none">invisible</div>
</body></html>`

// TestGetRequestMatchesGetterScript pins the first-class get surface to the
// getters script it exposes: every question routed through GetRequest must come
// back with exactly what evaluating the script directly produces. Without this,
// the surface could quietly grow its own JavaScript and answer differently from
// the walker the rest of brw uses.
func TestGetRequestMatchesGetterScript(t *testing.T) {
	ctx, _ := openFixture(t, getSurfaceFixture)

	tests := []struct {
		name string
		req  snapshot.GetRequest
	}{
		{name: "title", req: snapshot.GetRequest{What: "title"}},
		{name: "url", req: snapshot.GetRequest{What: "url"}},
		{name: "page text", req: snapshot.GetRequest{What: "text"}},
		{name: "element text", req: snapshot.GetRequest{What: "text", Target: "#heading"}},
		{name: "value", req: snapshot.GetRequest{What: "value", Target: "#name"}},
		{name: "attr", req: snapshot.GetRequest{What: "attr", Target: "#name", Name: "data-kind"}},
		{name: "count", req: snapshot.GetRequest{What: "count", Target: ".row"}},
		{name: "box", req: snapshot.GetRequest{What: "box", Target: "#heading"}},
		{name: "styles", req: snapshot.GetRequest{What: "styles", Target: "#gone"}},
		{name: "single style property", req: snapshot.GetRequest{What: "styles", Target: "#gone", Name: "display"}},
		{name: "visible", req: snapshot.GetRequest{What: "visible", Target: "#heading"}},
		{name: "hidden", req: snapshot.GetRequest{What: "hidden", Target: "#gone"}},
		{name: "enabled", req: snapshot.GetRequest{What: "enabled", Target: "#name"}},
		{name: "disabled", req: snapshot.GetRequest{What: "disabled", Target: "#go"}},
		{name: "checked", req: snapshot.GetRequest{What: "checked", Target: "#agree"}},
		// Case and padding are normalized by the surface, not by the page script.
		{name: "mixed case what", req: snapshot.GetRequest{What: " Title "}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.req.Validate(); err != nil {
				t.Fatalf("validate %+v: %v", tt.req, err)
			}
			what := strings.ToLower(strings.TrimSpace(tt.req.What))
			var viaSurface, viaScript map[string]any
			if err := chromedp.Run(ctx, chromedp.Evaluate(tt.req.Expression(), &viaSurface)); err != nil {
				t.Fatalf("evaluate the get surface: %v", err)
			}
			if err := chromedp.Run(ctx, chromedp.Evaluate(snapshot.BuildGetExpression(what, tt.req.Target, tt.req.Name), &viaScript)); err != nil {
				t.Fatalf("evaluate the getters script: %v", err)
			}
			if !reflect.DeepEqual(viaSurface["value"], viaScript["value"]) {
				t.Fatalf("get %s(%s) returned %#v through the surface and %#v through the script",
					what, tt.req.Target, viaSurface["value"], viaScript["value"])
			}
			if viaSurface["value"] == nil && what != "attr" {
				t.Fatalf("get %s(%s) returned nothing at all", what, tt.req.Target)
			}
		})
	}
}

func TestGetRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		req     snapshot.GetRequest
		wantErr string
	}{
		{name: "page level needs no target", req: snapshot.GetRequest{What: "title"}},
		{name: "text with no target reads the body", req: snapshot.GetRequest{What: "text"}},
		{name: "attr with a name", req: snapshot.GetRequest{What: "attr", Target: "#a", Name: "href"}},
		{name: "unknown question", req: snapshot.GetRequest{What: "colour", Target: "#a"}, wantErr: "unknown get target"},
		{name: "count without a selector", req: snapshot.GetRequest{What: "count"}, wantErr: "requires target"},
		{name: "value without a target", req: snapshot.GetRequest{What: "value"}, wantErr: "requires target"},
		{name: "attr without a name", req: snapshot.GetRequest{What: "attr", Target: "#a"}, wantErr: "requires name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate(%+v) = %v, want no error", tt.req, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want an error mentioning %q", tt.req, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate(%+v) = %q, want it to mention %q", tt.req, err, tt.wantErr)
			}
		})
	}
}
