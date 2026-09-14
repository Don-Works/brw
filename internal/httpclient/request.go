package httpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Request issues one daemon call and hands back the response body exactly as the
// daemon wrote it, sharing this Controller's base URL, bounds, correlation
// headers and upstream-error extraction.
//
// It exists for callers that must show the daemon's own JSON rather than a
// re-encoded Go struct — `brw --json` is one: decoding a response through this
// build's result types and marshalling it again silently drops any field those
// types do not yet know about, which is precisely what someone piping the
// envelope into jq is looking for.
func (c *Controller) Request(ctx context.Context, method, path string, query url.Values, body any) (json.RawMessage, error) {
	var raw json.RawMessage
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet:
		if err := c.get(ctx, path, query, &raw); err != nil {
			return nil, err
		}
	case http.MethodPost:
		if err := c.post(ctx, path, body, &raw); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported method %q", method)
	}
	return raw, nil
}
