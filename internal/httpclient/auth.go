package httpclient

import "net/http"

// SetAuthToken sends "Authorization: Bearer <token>" on every request to the
// upstream daemon, and only to it: a redirect to another host does not carry
// the token. Call it before the first request.
func (c *Controller) SetAuthToken(token string) {
	if token == "" {
		return
	}
	base := c.client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	host := ""
	if u, err := http.NewRequest(http.MethodGet, c.baseURL, nil); err == nil {
		host = u.URL.Host
	}
	c.client.Transport = &bearerTransport{base: base, token: token, host: host}
}

type bearerTransport struct {
	base  http.RoundTripper
	token string
	host  string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != t.host {
		return t.base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}
