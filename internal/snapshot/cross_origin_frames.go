package snapshot

import "fmt"

// frameBox is a cross-origin iframe's top-level viewport box, recovered from the
// snapshot metadata the same-origin walker produced (__abInaccessibleFrames).
type frameBox struct {
	x, y, w, h float64
	origin     string
}

// crossOriginBoxesFromMetadata parses snap.Metadata["cross_origin_frames"] into
// typed boxes. The metadata survives a JSON round-trip as []interface{} of
// map[string]interface{} with float64 numbers, so we read it defensively and
// skip anything malformed rather than failing the whole snapshot.
func crossOriginBoxesFromMetadata(meta map[string]interface{}) []frameBox {
	if meta == nil {
		return nil
	}
	raw, ok := meta["cross_origin_frames"].([]interface{})
	if !ok {
		return nil
	}
	boxes := make([]frameBox, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
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

func toFloat(v interface{}) float64 {
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

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// MergeCrossOriginFrames appends the interactive controls extracted from
// cross-origin iframes to snap.Elements with frame-qualified refs (f<i>:e<j>) and
// top-level viewport CENTER coordinates (CX/CY) computed from each frame's
// top-level box. It returns the number of elements appended.
//
// Each returned frame is matched to a top-level box (from the snapshot metadata)
// by origin — preferring an as-yet-unconsumed box of the same origin, falling
// back to positional order, then to no offset — so frame-local coordinates land
// in the same coordinate space brw_click_xy expects. Elements with no positive
// size are skipped (not actionable). A frames metadata summary records what was
// read so the agent can tell "no cross-origin frames" from "frames present but
// unread".
func MergeCrossOriginFrames(snap *PageSnapshot, frames []CrossOriginFrame) int {
	if snap == nil || len(frames) == 0 {
		return 0
	}
	boxes := crossOriginBoxesFromMetadata(snap.Metadata)
	consumed := make([]bool, len(boxes))

	pickBox := func(idx int, origin string) (frameBox, bool) {
		// Prefer an unconsumed box of the same origin.
		for i := range boxes {
			if !consumed[i] && origin != "" && boxes[i].origin == origin {
				consumed[i] = true
				return boxes[i], true
			}
		}
		// Fall back to the positional box if it is still free.
		if idx < len(boxes) && !consumed[idx] {
			consumed[idx] = true
			return boxes[idx], true
		}
		// Otherwise any remaining unconsumed box.
		for i := range boxes {
			if !consumed[i] {
				consumed[i] = true
				return boxes[i], true
			}
		}
		return frameBox{}, false
	}

	appended := 0
	readFrames := 0
	for i, frame := range frames {
		box, _ := pickBox(i, frame.Origin)
		frameHadElement := false
		for j, el := range frame.Elements {
			if el.W <= 0 || el.H <= 0 {
				continue
			}
			snap.Elements = append(snap.Elements, Element{
				Ref:        fmt.Sprintf("f%d:e%d", i, j),
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
			readFrames++
		}
	}

	if snap.Metadata == nil {
		snap.Metadata = map[string]interface{}{}
	}
	snap.Metadata["cross_origin_frames_read"] = readFrames
	snap.Metadata["cross_origin_frame_elements"] = appended
	if appended > 0 {
		// Supersede the "cannot be read" note from the same-origin walker: those
		// controls ARE now readable, just actionable by coordinate rather than ref.
		snap.Metadata["cross_origin_note"] = "Cross-origin iframe controls were read and added as elements with source:[\"frame\"] and refs f<i>:e<j>. Their DOM is isolated, so act on them with brw_click_xy at their cx/cy (top-level viewport center), then type with the keyboard; do not pass an f<i>:e<j> ref to brw_click."
	}
	return appended
}
