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
)

// maxHARFixtureBytes bounds what a replay will pull out of the store. A HAR is
// held in memory for the life of the route, so a caller pointing brw_route at a
// multi-gigabyte artifact must be refused rather than served.
const maxHARFixtureBytes = 32 << 20

// harFixtureChunkBytes is the store's per-read ceiling (MaxReadBytes), so a HAR
// is paged in at the largest window the artifact API will serve.
const harFixtureChunkBytes = MaxReadBytes

// LoadHARFixture reads a stored HAR artifact and decodes it into the recorded
// exchanges a brw_route replay answers from.
//
// It goes through the artifact API rather than the store directly so a daemon
// proxying to a browser host (--upstream-http) replays from the HAR that host
// holds, exactly like every other artifact read.
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
			// A read window can bisect a multi-byte rune, and the API answers that
			// window as base64 rather than replacing the split rune. Decoding it
			// back is what lets the two halves rejoin into valid JSON.
			decoded, decodeErr := base64.StdEncoding.DecodeString(chunk.Base64)
			if decodeErr != nil {
				return nil, decodeErr
			}
			buf.Write(decoded)
		}
		if !chunk.More {
			break
		}
		if chunk.SizeBytes <= 0 {
			return nil, fmt.Errorf("HAR artifact %s stopped returning bytes before the end of the file", artifactID)
		}
		offset = chunk.NextOffset
		if int64(buf.Len()) > maxHARFixtureBytes {
			return nil, fmt.Errorf("HAR artifact %s is over the %d-byte replay limit", artifactID, maxHARFixtureBytes)
		}
	}
	return ParseHARFixture(buf.Bytes())
}

// ParseHARFixture decodes a HAR 1.2 log into replayable entries.
//
// Redaction is a property of the recording, not of the replay: a HAR exported
// with the default redaction carries "[redacted by brw]" where a credential
// header or a request body was, and replays with those values. There is
// deliberately no path here that recovers them.
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
			Headers:     responseHeaderMap(entry.Response.Headers),
		}
		if entry.Request.PostData != nil {
			replayable.RequestBody = entry.Request.PostData.Text
		}
		out = append(out, replayable)
	}
	if len(out) == 0 {
		return nil, errors.New("this HAR holds no request entries to replay")
	}
	return out, nil
}

// responseHeaderMap keeps only headers that are safe to hand back to a page.
//
// A HAR records what the server sent, including hop-by-hop and framing headers.
// Replaying Content-Length or Content-Encoding against a body brw re-encodes
// itself would describe the response wrongly and the renderer would reject it,
// and Set-Cookie from a recording would write real cookies into the profile
// running the fixture.
func responseHeaderMap(headers []harHeader) map[string]string {
	out := make(map[string]string, len(headers))
	for _, header := range headers {
		if droppedReplayHeaders[strings.ToLower(strings.TrimSpace(header.Name))] {
			continue
		}
		out[header.Name] = header.Value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var droppedReplayHeaders = map[string]bool{
	"content-length":    true,
	"content-encoding":  true,
	"transfer-encoding": true,
	"connection":        true,
	"keep-alive":        true,
	"set-cookie":        true,
}
