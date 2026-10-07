package httpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Request issues one daemon call and hands back the response body exactly as the daemon wrote it, sharing this Controller's base URL, bounds, correlation headers and upstream-error extraction.
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

// RequestExact is Request for a route whose handler decodes a fixed schema with DisallowUnknownFields — today the artifact handle operations.
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
