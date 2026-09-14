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

// RequestExact is Request for a route whose handler decodes a fixed schema with
// DisallowUnknownFields — today the artifact handle operations. It sends the
// body exactly as the caller built it, with no tab_id or snapshot field folded
// in from the context (those handlers answer 400 to a field they do not
// declare), and holds the response to that operation's own bound rather than
// the generic 64 MiB upstream one.
func (c *Controller) RequestExact(ctx context.Context, method, path string, query url.Values, body any) (json.RawMessage, error) {
	var raw json.RawMessage
	limit := exactResponseBound(path)
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet:
		if err := c.getExact(ctx, path, query, &raw, limit); err != nil {
			return nil, err
		}
	case http.MethodPost:
		if err := c.postExactWithLimit(ctx, path, body, &raw, limit); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported method %q", method)
	}
	return raw, nil
}

// getExact is get without the context's tab_id: a strict-schema route rejects
// query parameters it does not declare for the same reason it rejects body
// fields.
func (c *Controller) getExact(ctx context.Context, path string, values url.Values, out any, maxResponseBytes int64) error {
	reqURL := c.baseURL + path
	if len(values) > 0 {
		reqURL += "?" + values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	return c.doWithClientLimit(c.client, req, out, maxResponseBytes)
}

// exactResponseBound gives an untyped request the same response bound the typed
// method for that route applies. An unrecognised strict route gets the smallest
// of them: a route whose shape this build does not know should not be able to
// return more than its siblings.
func exactResponseBound(path string) int64 {
	switch path {
	case "/api/artifacts/read":
		return maxArtifactReadResponseBytes
	case "/api/artifacts/search":
		return maxArtifactSearchResponseBytes
	case "/api/artifacts/info":
		return maxArtifactInfoResponseBytes
	default:
		return maxArtifactDeleteResponseBytes
	}
}
