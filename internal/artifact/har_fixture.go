package artifact

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

const maxHARFixtureBytes = 32 << 20

const harFixtureChunkBytes = MaxReadBytes

// LoadHARFixture reads a stored HAR artifact and decodes it into the recorded exchanges a brw_route replay answers from.
func LoadHARFixture(ctx context.Context, api API, artifactID string) ([]browser.HAREntry, error) {
	if api == nil {
		return nil, errors.New("artifact service is not configured on the browser host")
	}
	artifactID = strings.TrimSpace(artifactID)
	if artifactID == "" {
		return nil, errors.New("har_artifact_id is required")
	}
	meta, err := api.ArtifactInfo(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	if meta.Kind != "har" {
		return nil, fmt.Errorf("artifact %s is a %s artifact, not a har; capture one with brw_artifact_capture{kind:\"har\"}", artifactID, meta.Kind)
	}
	if meta.SizeBytes > maxHARFixtureBytes {
		return nil, fmt.Errorf("HAR artifact %s is %d bytes, over the %d-byte replay limit", artifactID, meta.SizeBytes, maxHARFixtureBytes)
	}
	var buf bytes.Buffer
	for offset := int64(0); ; {
		chunk, err := api.ReadArtifact(ctx, artifactID, offset, harFixtureChunkBytes)
		if err != nil {
			return nil, err
		}
		if chunk.Encoding == "utf-8" {
			buf.WriteString(chunk.Text)
		} else {

			decoded, decodeErr := base64.StdEncoding.DecodeString(chunk.Base64)
			if decodeErr != nil {
				return nil, decodeErr
			}
			buf.Write(decoded)
		}
		if int64(buf.Len()) > maxHARFixtureBytes {
			return nil, fmt.Errorf("HAR artifact %s is over the %d-byte replay limit", artifactID, maxHARFixtureBytes)
		}
		if !chunk.More {
			break
		}
		if chunk.SizeBytes <= 0 || chunk.NextOffset <= offset {
			return nil, fmt.Errorf("HAR artifact %s stopped returning bytes before the end of the file", artifactID)
		}
		offset = chunk.NextOffset
	}
	return ParseHARFixture(buf.Bytes())
}

// ParseHARFixture decodes a HAR 1.2 log into replayable entries.
func ParseHARFixture(data []byte) ([]browser.HAREntry, error) {
	var log harLog
	if err := json.Unmarshal(data, &log); err != nil {
		return nil, fmt.Errorf("decode HAR: %w", err)
	}
	if log.Log.Version == "" && len(log.Log.Entries) == 0 {
		return nil, errors.New("this artifact is not a HAR log: it has no log.entries")
	}
	out := make([]browser.HAREntry, 0, len(log.Log.Entries))
	for _, entry := range log.Log.Entries {
		if strings.TrimSpace(entry.Request.URL) == "" {
			continue
		}
		replayable := browser.HAREntry{
			Method:      entry.Request.Method,
			URL:         entry.Request.URL,
			Status:      entry.Response.Status,
			ContentType: entry.Response.Content.MIMEType,
			Body:        entry.Response.Content.Text,
			Headers:     replayableHeaders(entry.Response.Headers),
			Truncated:   bodyWasTruncated(entry.Response.Content),
		}
		if entry.Request.PostData != nil {
			replayable.RequestBody = entry.Request.PostData.Text
			replayable.RequestBodyTruncated = requestBodyWasTruncated(entry.Request)
		}
		out = append(out, replayable)
	}
	if len(out) == 0 {
		return nil, errors.New("this HAR holds no request entries to replay")
	}
	return out, nil
}

func replayableHeaders(headers []harHeader) []browser.HARHeader {
	out := make([]browser.HARHeader, 0, len(headers))
	for _, header := range headers {
		if droppedReplayHeaders[strings.ToLower(strings.TrimSpace(header.Name))] {
			continue
		}
		out = append(out, browser.HARHeader{Name: header.Name, Value: header.Value})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func bodyWasTruncated(content harContent) bool {
	if strings.HasSuffix(content.Text, snapshot.BodyTruncationMarker) {
		return true
	}
	return content.Size > 0 && len(content.Text) > 0 && len(content.Text) < content.Size
}

func requestBodyWasTruncated(request harRequest) bool {
	if request.PostData == nil {
		return false
	}
	text := request.PostData.Text
	if strings.HasSuffix(text, snapshot.BodyTruncationMarker) {
		return true
	}
	return request.BodySize > 0 && len(text) > 0 && len(text) < request.BodySize
}

var droppedReplayHeaders = map[string]bool{
	"content-length":    true,
	"content-encoding":  true,
	"transfer-encoding": true,
	"connection":        true,
	"keep-alive":        true,
	"set-cookie":        true,
}
