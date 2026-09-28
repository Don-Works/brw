package extensionbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// downloadsUnsupportedNote is returned when the connected extension is too old to
// answer get_downloads (pre-issue-#6 builds) so callers still get the graceful
// Supported=false contract instead of a hard error.
const downloadsUnsupportedNote = "Download capture is unavailable: the connected brw extension predates chrome.downloads support (issue #6). Reload the brw extension, or restart brw with the direct-CDP backend. Check Supported=false to detect this programmatically."

// Downloads captures retained snapshots from the extension's chrome.downloads
// registry. Page.downloadWillBegin provenance is correlated extension-side and
// arrives as DownloadEntry.TabID. Recipe-scoped contexts receive only entries
// changed after their per-tab baseline and require exact provenance; ordinary
// calls receive the complete bounded snapshot. An extension too old to know the
// message returns the legacy Supported=false note rather than erroring.
func (b *Bridge) Downloads(ctx context.Context) (browser.DownloadsResult, error) {
	payload, err := b.downloadSnapshot(ctx)
	if err != nil {
		return browser.DownloadsResult{}, err
	}
	if !payload.Supported {
		return browser.DownloadsResult{
			Downloads: payload.Downloads,
			Count:     len(payload.Downloads),
			Supported: false,
			Note:      payload.Note,
		}, nil
	}

	b.downloadsMu.Lock()
	result := append([]browser.DownloadEntry(nil), payload.Downloads...)
	if _, recipeScoped := browser.AllowedOriginsFromContext(ctx); recipeScoped {
		tabID := browser.TabIDFromContext(ctx)
		if tabID == "" {
			b.downloadsMu.Unlock()
			return browser.DownloadsResult{}, errors.New("recipe download polling requires a pinned browser tab")
		}
		cursor := b.downloadCursors[tabID]
		result = result[:0]
		for _, entry := range payload.Downloads {
			if entry.TabID == tabID && b.downloadVersions[entry.GUID] > cursor {
				result = append(result, entry)
			}
		}
		b.downloadCursors[tabID] = b.downloadSequence
	}
	b.downloadsMu.Unlock()
	if result == nil {
		result = []browser.DownloadEntry{}
	}
	return browser.DownloadsResult{
		Downloads: result,
		Count:     len(result),
		Supported: payload.Supported,
		// chrome.downloads reports the file's own local path, so an entry the
		// bridge supports always carries one.
		FilePaths: payload.Supported,
		Note:      payload.Note,
	}, nil
}

// downloadSnapshotPayload is the extension's bounded registry as it arrives,
// before any caller-scoped filtering.
type downloadSnapshotPayload struct {
	Downloads []browser.DownloadEntry `json:"downloads"`
	Supported bool                    `json:"supported"`
	Note      string                  `json:"note"`
	// ChangedAt maps a download guid to when the extension last saw that
	// download change state. It is decoded separately from Downloads (see
	// decodeDownloadChangeTimes) so the wire field does not widen
	// browser.DownloadEntry, which is both the shape brw_downloads returns and
	// the shape this bridge fingerprints for change detection.
	ChangedAt map[string]time.Time `json:"-"`
}

// decodeDownloadChangeTimes reads the per-entry changed_at_ms the extension
// sends alongside each download. An extension build that predates the field
// reports nothing here, and a wait then treats every already-terminal download
// as old news, exactly as it did before the field existed.
func decodeDownloadChangeTimes(raw []byte) map[string]time.Time {
	var timing struct {
		Downloads []struct {
			GUID        string `json:"guid"`
			ChangedAtMS int64  `json:"changed_at_ms"`
		} `json:"downloads"`
	}
	if err := json.Unmarshal(raw, &timing); err != nil {
		return nil
	}
	out := make(map[string]time.Time, len(timing.Downloads))
	for _, entry := range timing.Downloads {
		if entry.GUID == "" || entry.ChangedAtMS <= 0 {
			continue
		}
		out[entry.GUID] = time.UnixMilli(entry.ChangedAtMS)
	}
	return out
}

// downloadSnapshot performs the get_downloads RPC and updates the change
// bookkeeping, without applying a recipe context's per-tab cursor. A download
// wait polls through here so repeatedly asking "has it finished yet?" cannot
// consume the baseline a recipe's own download polling depends on.
func (b *Bridge) downloadSnapshot(ctx context.Context) (downloadSnapshotPayload, error) {
	var payload downloadSnapshotPayload
	raw, err := b.call(ctx, "get_downloads", nil)
	if err != nil {
		if isUnknownMessageTypeErr(err) {
			return downloadSnapshotPayload{
				Downloads: []browser.DownloadEntry{},
				Supported: false,
				Note:      downloadsUnsupportedNote,
			}, nil
		}
		return downloadSnapshotPayload{}, err
	}
	if len(raw) > 0 {
		if jsonErr := json.Unmarshal(raw, &payload); jsonErr != nil {
			return downloadSnapshotPayload{}, fmt.Errorf("parse downloads: %w", jsonErr)
		}
		payload.ChangedAt = decodeDownloadChangeTimes(raw)
	}
	if payload.Downloads == nil {
		payload.Downloads = []browser.DownloadEntry{}
	}
	if !payload.Supported {
		return payload, nil
	}
	if len(payload.Downloads) > maxBridgeTrackedDownloads {
		payload.Downloads = append([]browser.DownloadEntry(nil), payload.Downloads[len(payload.Downloads)-maxBridgeTrackedDownloads:]...)
	}
	b.downloadsMu.Lock()
	b.ensureDownloadTrackingMapsLocked()
	b.ingestDownloadSnapshotLocked(payload.Downloads)
	b.downloadsMu.Unlock()
	return payload, nil
}

const maxBridgeTrackedDownloads = 200

func (b *Bridge) ensureDownloadTrackingMapsLocked() {
	if b.downloadFingerprints == nil {
		b.downloadFingerprints = map[string]string{}
	}
	if b.downloadVersions == nil {
		b.downloadVersions = map[string]uint64{}
	}
	if b.downloadCursors == nil {
		b.downloadCursors = map[string]uint64{}
	}
}

func (b *Bridge) ingestDownloadSnapshotLocked(entries []browser.DownloadEntry) {
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.GUID == "" || len(entry.GUID) > 500 {
			continue
		}
		present[entry.GUID] = true
		encoded, _ := json.Marshal(entry)
		fingerprint := string(encoded)
		if b.downloadFingerprints[entry.GUID] != fingerprint {
			b.downloadSequence++
			b.downloadFingerprints[entry.GUID] = fingerprint
			b.downloadVersions[entry.GUID] = b.downloadSequence
		}
	}
	for guid := range b.downloadFingerprints {
		if !present[guid] {
			delete(b.downloadFingerprints, guid)
			delete(b.downloadVersions, guid)
		}
	}
}
