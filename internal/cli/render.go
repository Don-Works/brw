package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
)

// Element names are the widest column and a page can put a paragraph in one.
const maxNameChars = 72

func renderOpen(w io.Writer, _ *options, body []byte) error {
	var result browser.OpenResult
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	state := "ready"
	if !result.Ready {
		state = "not ready yet — brw wait load"
	}
	fmt.Fprintf(w, "opened %s (tab %s, %s)\n", result.Tab.URL, result.Tab.ID, state)
	if result.Tab.Title != "" {
		fmt.Fprintf(w, "%s\n", result.Tab.Title)
	}
	return nil
}

func renderAction(w io.Writer, _ *options, body []byte) error {
	var result browser.ActionResult
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	line := "ok"
	if result.Message != "" {
		line = result.Message
	}
	if result.URL != "" {
		line += "  " + result.URL
	}
	fmt.Fprintln(w, line)
	if result.Warning != "" {
		fmt.Fprintf(w, "warning: %s\n", result.Warning)
	}
	if len(result.Elements) > 0 {
		renderElements(w, result.Elements)
	}
	return nil
}

func renderElementList(w io.Writer, _ *options, body []byte) error {
	var result struct {
		URL      string             `json:"url"`
		Title    string             `json:"title"`
		Elements []snapshot.Element `json:"elements"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	if result.Title != "" || result.URL != "" {
		fmt.Fprintf(w, "%s  %s\n\n", result.Title, result.URL)
	}
	if len(result.Elements) == 0 {
		fmt.Fprintln(w, "no matching elements")
		return nil
	}
	renderElements(w, result.Elements)
	return nil
}

// renderElements prints one element per line as `@ref role name [state]`, the
// shape the whole CLI is built around: the first column pastes straight into
// `brw click`.
func renderElements(w io.Writer, elements []snapshot.Element) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, el := range elements {
		fmt.Fprintf(tw, "@%s\t%s\t%s\t%s\n", el.Ref, el.Role, truncate(el.Name, maxNameChars), elementState(el))
	}
	_ = tw.Flush()
}

func elementState(el snapshot.Element) string {
	var parts []string
	if el.Value != "" && !el.Sensitive {
		parts = append(parts, "value="+truncate(el.Value, 32))
	}
	if el.Sensitive {
		parts = append(parts, "sensitive")
	}
	if el.Href != "" {
		parts = append(parts, truncate(el.Href, 48))
	}
	if el.Disabled {
		parts = append(parts, "disabled")
	}
	if !el.Visible {
		parts = append(parts, "hidden")
	}
	return strings.Join(parts, " ")
}

func renderRead(w io.Writer, _ *options, body []byte) error {
	var read readability.PageRead
	if err := json.Unmarshal(body, &read); err != nil {
		return err
	}
	if read.Title != "" {
		fmt.Fprintf(w, "%s\n", read.Title)
	}
	if read.URL != "" {
		fmt.Fprintf(w, "%s\n", read.URL)
	}
	fmt.Fprintf(w, "\n%s\n", strings.TrimRight(read.Main, "\n"))
	if read.MainTruncated {
		fmt.Fprintf(w, "\n— truncated at %d of %d chars; continue with --offset %d\n",
			len([]rune(read.Main)), read.MainTotalChars, read.NextOffset)
	}
	return nil
}

func renderTabs(w io.Writer, _ *options, body []byte) error {
	var tabs []browser.Tab
	if err := json.Unmarshal(body, &tabs); err != nil {
		return err
	}
	if len(tabs) == 0 {
		fmt.Fprintln(w, "no open tabs")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, tab := range tabs {
		marker := " "
		if tab.Active {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", marker, tab.ID, truncate(tab.Title, 48), tab.URL)
	}
	return tw.Flush()
}

func renderDownloads(w io.Writer, _ *options, body []byte) error {
	var result browser.DownloadsResult
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	if !result.Supported {
		note := result.Note
		if note == "" {
			note = "this transport cannot observe downloads"
		}
		fmt.Fprintf(w, "downloads unavailable: %s\n", note)
		return nil
	}
	if len(result.Downloads) == 0 {
		fmt.Fprintln(w, "no downloads")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, entry := range result.Downloads {
		target := entry.Path
		if target == "" {
			target = entry.SuggestedFilename
		}
		fmt.Fprintf(tw, "%s\t%d/%d bytes\t%s\n", entry.State, entry.ReceivedBytes, entry.TotalBytes, target)
	}
	return tw.Flush()
}

func renderScreenshot(w io.Writer, opts *options, body []byte) error {
	var shot browser.Screenshot
	if err := json.Unmarshal(body, &shot); err != nil {
		return err
	}
	data, err := base64.StdEncoding.DecodeString(shot.Base64)
	if err != nil {
		return fmt.Errorf("decode screenshot: %w", err)
	}
	path := opts.out
	if strings.TrimSpace(path) == "" {
		path = "screenshot.png"
	}
	// A screenshot of a signed-in browser is as sensitive as the session it
	// shows, so it lands owner-only rather than at the process umask.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(w, "wrote %s (%d bytes, %s)\n", path, len(data), shot.MIMEType)
	return nil
}

func renderArtifactChunk(w io.Writer, _ *options, body []byte) error {
	var chunk artifact.Chunk
	if err := json.Unmarshal(body, &chunk); err != nil {
		return err
	}
	switch {
	case chunk.Text != "":
		fmt.Fprintf(w, "%s\n", strings.TrimRight(chunk.Text, "\n"))
	case chunk.Base64 != "":
		fmt.Fprintf(w, "binary artifact: %d bytes of %d, base64 in --json\n", chunk.SizeBytes, chunk.TotalBytes)
	default:
		fmt.Fprintln(w, "empty artifact window")
	}
	if chunk.More {
		fmt.Fprintf(w, "— more: %d of %d bytes; continue with --offset %d\n",
			chunk.Offset+int64(chunk.SizeBytes), chunk.TotalBytes, chunk.NextOffset)
	}
	return nil
}

func renderHealth(w io.Writer, _ *options, body []byte) error {
	var health httpclient.Health
	if err := json.Unmarshal(body, &health); err != nil {
		return err
	}
	state := "ok"
	if !health.OK {
		state = "not ok"
	}
	fields := []string{state}
	id := health.Identity
	for _, field := range []struct{ name, value string }{
		{"workspace", id.Workspace},
		{"profile", id.Profile},
		{"mode", id.Mode},
		{"transport", id.Transport},
	} {
		if field.value != "" {
			fields = append(fields, field.name+"="+field.value)
		}
	}
	if id.Headless {
		fields = append(fields, "headless")
	}
	fmt.Fprintln(w, strings.Join(fields, "  "))
	return nil
}

func truncate(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max-1]) + "…"
}
