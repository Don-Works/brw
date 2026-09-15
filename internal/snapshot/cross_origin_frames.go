package snapshot

import "fmt"

// frameBox is a cross-origin iframe's top-level viewport box, recovered from the
// snapshot metadata the same-origin walker produced (__abInaccessibleFrames).
type frameBox struct {
	x, y, w, h float64
	origin     string
}

// crossOriginBoxesFromMetadata parses snap.Metadata["cross_origin_frames"] into
// typed boxes. The metadata survives a JSON round-trip as []any of
// map[string]any with float64 numbers, so we read it defensively and
// skip anything malformed rather than failing the whole snapshot.
func crossOriginBoxesFromMetadata(meta map[string]any) []frameBox {
	if meta == nil {
		return nil
	}
	raw, ok := meta["cross_origin_frames"].([]any)
	if !ok {
		return nil
	}
	boxes := make([]frameBox, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		boxes = append(boxes, frameBox{
			x:      toFloat(m["x"]),
			y:      toFloat(m["y"]),
			w:      toFloat(m["width"]),
			h:      toFloat(m["height"]),
			origin: toString(m["origin"]),
		})
	}
	return boxes
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// MergeCrossOriginFrames appends the interactive controls extracted from
// cross-origin iframes to snap.Elements with frame-qualified refs (f<i>:<ref>)
// and top-level viewport CENTER coordinates (CX/CY) computed from each frame's
// top-level box. It returns the number of elements appended and the set of frame
// indices whose controls were read, which the caller passes to
// PromoteCrossOriginFrames so a frame read as refs is not also emitted as a box.
//
// Each returned frame is matched to a top-level box (from the snapshot metadata)
// by origin — preferring an as-yet-unconsumed box of the same origin, falling
// back to positional order, then to no offset — so frame-local coordinates land
// in the same coordinate space brw_click_xy expects. The i in f<i> is the BOX's
// index, not the extraction order: brw_frame resolves f<i> against the same
// metadata list, and the two transports enumerate frames in different orders, so
// numbering a ref by extraction order would point brw_frame at a different frame
// than the ref came from. Elements with no positive size are skipped (not
// actionable). A frames metadata summary records what was read so the agent can
// tell "no cross-origin frames" from "frames present but unread".
func MergeCrossOriginFrames(snap *PageSnapshot, frames []CrossOriginFrame) (int, map[int]bool) {
	readBoxes := map[int]bool{}
	if snap == nil || len(frames) == 0 {
		return 0, readBoxes
	}
	boxes := crossOriginBoxesFromMetadata(snap.Metadata)
	consumed := make([]bool, len(boxes))

	pickBox := func(idx int, origin string) (frameBox, int) {
		// Prefer an unconsumed box of the same origin.
		for i := range boxes {
			if !consumed[i] && origin != "" && boxes[i].origin == origin {
				consumed[i] = true
				return boxes[i], i
			}
		}
		// Fall back to the positional box if it is still free.
		if idx < len(boxes) && !consumed[idx] {
			consumed[idx] = true
			return boxes[idx], idx
		}
		// Otherwise any remaining unconsumed box.
		for i := range boxes {
			if !consumed[i] {
				consumed[i] = true
				return boxes[i], i
			}
		}
		// No metadata box at all: fall back to extraction order so the elements
		// still carry distinct refs, with no offset applied.
		return frameBox{}, idx
	}

	appended := 0
	for i, frame := range frames {
		box, boxIndex := pickBox(i, frame.Origin)
		frameHadElement := false
		if frame.Snapshot != nil {
			// The frame was walked by the SHARED walker (include_boxes), so its
			// controls arrive with the same roles, names, ranking and ref rules the
			// top document gets. Keep those refs, namespaced by frame.
			for _, el := range frame.Snapshot.Elements {
				if el.Ref == "" || el.W <= 0 || el.H <= 0 {
					continue
				}
				el.Ref = fmt.Sprintf("f%d:%s", boxIndex, el.Ref)
				el.Source = []string{"frame"}
				el.Key = ""
				el.CX = box.x + el.X + el.W/2
				el.CY = box.y + el.Y + el.H/2
				el.X, el.Y, el.W, el.H = 0, 0, 0, 0
				snap.Elements = append(snap.Elements, el)
				appended++
				frameHadElement = true
			}
		}
		for j, el := range frame.Elements {
			if el.W <= 0 || el.H <= 0 {
				continue
			}
			snap.Elements = append(snap.Elements, Element{
				Ref:        fmt.Sprintf("f%d:e%d", boxIndex, j),
				Role:       el.Role,
				Name:       el.Name,
				Tag:        el.Tag,
				Type:       el.Type,
				Visible:    true,
				InViewport: true,
				Source:     []string{"frame"},
				CX:         box.x + el.X + el.W/2,
				CY:         box.y + el.Y + el.H/2,
			})
			appended++
			frameHadElement = true
		}
		if frameHadElement {
			readBoxes[boxIndex] = true
		}
	}

	if snap.Metadata == nil {
		snap.Metadata = map[string]any{}
	}
	snap.Metadata["cross_origin_frames_read"] = len(readBoxes)
	snap.Metadata["cross_origin_frame_elements"] = appended
	if appended > 0 {
		// Supersede the "cannot be read" note from the same-origin walker: those
		// controls ARE now readable, just actionable by coordinate rather than ref.
		snap.Metadata["cross_origin_note"] = "Cross-origin iframe controls were read and added as elements with source:[\"frame\"] and refs f<i>:<ref>. Their document is isolated from this one, so on the extension bridge act on them with brw_click_xy at their cx/cy (top-level viewport center) and type with the keyboard; passing an f<i>:<ref> ref to a ref-taking verb there is refused by name rather than acting on the frame instead."
	}
	return appended, readBoxes
}

// PromoteCrossOriginFrames turns each cross-origin iframe the same-origin walker
// recorded (snap.Metadata["cross_origin_frames"]) into a first-class CLICKABLE
// element: ref f<i>, role "iframe", with CX/CY at the frame's top-level center.
//
// This is the fallback half of include_frames, for a frame whose controls could
// not be read: the frame has no CDP target of its own (cross-origin but sharing
// the embedder's process), its target was already owned by another debugger, or
// the evaluate failed. Rather than leave the agent blind to a frame it can see
// on screen, the frame itself becomes a targetable element to brw_click_xy into
// (optionally after a brw_screenshot). alreadyRead frame indices — those whose
// controls WERE read — are skipped to avoid a redundant box element. Returns the
// count promoted.
func PromoteCrossOriginFrames(snap *PageSnapshot, alreadyRead map[int]bool) int {
	if snap == nil {
		return 0
	}
	boxes := crossOriginBoxesFromMetadata(snap.Metadata)
	if len(boxes) == 0 {
		return 0
	}
	promoted := 0
	for i, b := range boxes {
		if alreadyRead[i] {
			continue
		}
		name := b.origin
		if name == "" {
			name = "cross-origin iframe"
		}
		snap.Elements = append(snap.Elements, Element{
			Ref:        fmt.Sprintf("f%d", i),
			Role:       "iframe",
			Name:       name,
			Tag:        "iframe",
			Visible:    true,
			InViewport: true,
			Source:     []string{"frame"},
			CX:         b.x + b.w/2,
			CY:         b.y + b.h/2,
		})
		promoted++
	}
	if snap.Metadata == nil {
		snap.Metadata = map[string]any{}
	}
	snap.Metadata["cross_origin_frames_promoted"] = promoted
	if promoted > 0 {
		snap.Metadata["cross_origin_note"] = "Cross-origin iframes whose controls could not be read are surfaced as clickable elements (source:[\"frame\"], ref f<i>) with cx/cy at the frame center. Interact by brw_click_xy at that cx/cy (brw_screenshot first to see the contents); for uploads inside such a frame use brw_upload_file with click_ref/click_text, which works across frames."
	}
	return promoted
}
