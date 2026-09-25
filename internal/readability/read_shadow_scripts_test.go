package readability

import (
	"strings"
	"testing"
)

// TestReadShadowAppNeverReturnsScriptSource is the shape of a Polymer app such
// as developer.chrome.com/origintrials: the light DOM holds only <script>,
// <noscript> and a custom element whose content is in its shadow root. The body's
// innerText is empty, and the reader used to fall back to textContent, which is
// the source code of those scripts.
func TestReadShadowAppNeverReturnsScriptSource(t *testing.T) {
	ctx, cancel := readTestContext(t)
	defer cancel()
	html := `<!DOCTYPE html><html><body>
<noscript>This page requires Javascript.</noscript>
<script>var cookieAcceptance = localStorage.getItem("x"); window.dataLayer = [];</script>
<ot-app></ot-app>
<script>
  customElements.define('ot-app', class extends HTMLElement {
    connectedCallback() {
      const root = this.attachShadow({ mode: 'open' });
      root.innerHTML = '<style>h2 { color: red }</style><h2>Register for WebMCP Trial</h2>' +
        '<p>Origin trial for WebMCP, which allows websites to register tools for use by agents hosted by the site or in Chrome.</p>';
    }
  });
</script>
</body></html>`
	read := navigateRead(t, ctx, html)
	for _, leaked := range []string{"cookieAcceptance", "dataLayer", "customElements", "color: red"} {
		if strings.Contains(read.Main, leaked) {
			t.Fatalf("main text leaked script or style source %q: %q", leaked, read.Main)
		}
	}
	if !strings.Contains(read.Main, "Register for WebMCP Trial") {
		t.Fatalf("main = %q, want the shadow-root content", read.Main)
	}
}
