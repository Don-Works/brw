package extensionbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// foreignExtensionFrame is one other-extension frame the extension reported as
// the reason Chrome refused the debugger.
type foreignExtensionFrame struct {
	FrameID     int    `json:"frame_id"`
	URL         string `json:"url"`
	ExtensionID string `json:"extension_id"`
}

// pageTransport is what the extension appends to a CDP answer it produced
// without the debugger.
type pageTransport struct {
	Path                   string                  `json:"path"`
	FrameID                int                     `json:"frame_id"`
	SkippedExtensionFrames int                     `json:"skipped_extension_frames"`
	BlockedBy              []foreignExtensionFrame `json:"blocked_by"`
}

// pageTransportNote collects, across every CDP call one observation makes,
// whether any of them was answered without the debugger and how many
// other-extension frames were left out.
type pageTransportNote struct {
	mu        sync.Mutex
	scripting bool
	skipped   int
	blockedBy map[string]foreignExtensionFrame
}

type pageTransportNoteKey struct{}

func withPageTransportNote(ctx context.Context) (context.Context, *pageTransportNote) {
	note := &pageTransportNote{blockedBy: map[string]foreignExtensionFrame{}}
	return context.WithValue(ctx, pageTransportNoteKey{}, note), note
}

func pageTransportNoteFrom(ctx context.Context) *pageTransportNote {
	note, _ := ctx.Value(pageTransportNoteKey{}).(*pageTransportNote)
	return note
}

func (b *Bridge) cdp(ctx context.Context, tabID, method string, params map[string]any) (json.RawMessage, error) {
	raw, err := b.cdpDispatch(ctx, tabID, method, params)
	if err == nil {
		notePageTransport(ctx, raw)
	}
	return raw, err
}

func notePageTransport(ctx context.Context, raw json.RawMessage) {
	note := pageTransportNoteFrom(ctx)
	if note == nil || len(raw) == 0 || !strings.Contains(string(raw), `"brwTransport"`) {
		return
	}
	var payload struct {
		Transport *pageTransport `json:"brwTransport"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Transport == nil {
		return
	}
	note.mu.Lock()
	defer note.mu.Unlock()
	note.scripting = true
	note.addSkippedLocked(payload.Transport.SkippedExtensionFrames, payload.Transport.BlockedBy)
}

func (n *pageTransportNote) noteSkipped(count int) {
	if n == nil || count <= 0 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.addSkippedLocked(count, nil)
}

func (n *pageTransportNote) addSkippedLocked(count int, frames []foreignExtensionFrame) {
	for _, frame := range frames {
		n.blockedBy[frame.URL] = frame
	}
	if count > n.skipped {
		n.skipped = count
	}
	if len(n.blockedBy) > n.skipped {
		n.skipped = len(n.blockedBy)
	}
}

// apply records the note on a snapshot's metadata. It leaves metadata alone
// when every call went through the debugger and nothing was skipped.
func (n *pageTransportNote) apply(metadata map[string]any) map[string]any {
	if n == nil {
		return metadata
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.scripting && n.skipped == 0 {
		return metadata
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	extensions := make([]string, 0, len(n.blockedBy))
	seen := map[string]bool{}
	for _, frame := range n.blockedBy {
		if frame.ExtensionID != "" && !seen[frame.ExtensionID] {
			seen[frame.ExtensionID] = true
			extensions = append(extensions, frame.ExtensionID)
		}
	}
	metadata["skipped_extension_frames"] = n.skipped
	if len(extensions) > 0 {
		metadata["skipped_extension_ids"] = extensions
	}
	text := fmt.Sprintf("%d frame(s) belonging to other browser extensions were skipped", n.skipped)
	if n.scripting {
		metadata["page_transport"] = "scripting"
		skipped := "the other extension's frames were skipped"
		if n.skipped > 0 {
			skipped = fmt.Sprintf("%d frame(s) belonging to other extensions were skipped", n.skipped)
		}
		text = "Chrome refused brw's debugger for this tab because the page embeds another extension's frame; the page was read through chrome.scripting in the top frame, and " + skipped
	}
	metadata["frames_note"] = text
	return metadata
}
