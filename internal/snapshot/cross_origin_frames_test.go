package snapshot

import "testing"

func TestMergeCrossOriginFramesTranslatesToTopLevelCenter(t *testing.T) {
	snap := &PageSnapshot{
		Elements: []Element{{Ref: "e1", Role: "button", Name: "Existing"}},
		Metadata: map[string]interface{}{
			"cross_origin_frames": []interface{}{
				map[string]interface{}{"x": 100.0, "y": 200.0, "width": 400.0, "height": 300.0, "origin": "https://embed.test"},
			},
		},
	}
	frames := []CrossOriginFrame{
		{
			URL:    "https://embed.test/editor",
			Origin: "https://embed.test",
			Elements: []CrossOriginFrameElement{
				// Frame-local box at (10,20) sized 40x10 → top-level center is
				// (100+10+20, 200+20+5) = (130, 225).
				{Role: "textbox", Name: "Title", Tag: "input", Type: "text", X: 10, Y: 20, W: 40, H: 10},
				// Zero-size control is skipped (not actionable).
				{Role: "button", Name: "Hidden", Tag: "button", X: 0, Y: 0, W: 0, H: 0},
			},
		},
	}

	n := MergeCrossOriginFrames(snap, frames)
	if n != 1 {
		t.Fatalf("expected 1 merged element, got %d", n)
	}
	if len(snap.Elements) != 2 {
		t.Fatalf("expected 2 total elements, got %d", len(snap.Elements))
	}
	got := snap.Elements[1]
	if got.Ref != "f0:e0" {
		t.Fatalf("expected frame-qualified ref f0:e0, got %q", got.Ref)
	}
	if len(got.Source) != 1 || got.Source[0] != "frame" {
		t.Fatalf("expected source [frame], got %v", got.Source)
	}
	if got.CX != 130 || got.CY != 225 {
		t.Fatalf("expected top-level center (130,225), got (%v,%v)", got.CX, got.CY)
	}
	if got.Role != "textbox" || got.Name != "Title" {
		t.Fatalf("frame element fields not carried: %+v", got)
	}
	if snap.Metadata["cross_origin_frame_elements"] != 1 {
		t.Fatalf("expected metadata cross_origin_frame_elements=1, got %v", snap.Metadata["cross_origin_frame_elements"])
	}
	if _, ok := snap.Metadata["cross_origin_note"]; !ok {
		t.Fatalf("expected an updated cross_origin_note")
	}
}

func TestMergeCrossOriginFramesNoMetadataBoxUsesZeroOffset(t *testing.T) {
	// No metadata boxes (e.g. the top walker did not record them): elements are
	// still surfaced, just with frame-local coordinates (offset 0).
	snap := &PageSnapshot{}
	frames := []CrossOriginFrame{
		{URL: "https://x.test", Origin: "https://x.test", Elements: []CrossOriginFrameElement{
			{Role: "link", Name: "Go", Tag: "a", X: 5, Y: 5, W: 10, H: 10},
		}},
	}
	if n := MergeCrossOriginFrames(snap, frames); n != 1 {
		t.Fatalf("expected 1 merged element, got %d", n)
	}
	got := snap.Elements[0]
	if got.CX != 10 || got.CY != 10 { // 0 + 5 + 5
		t.Fatalf("expected center (10,10) with zero offset, got (%v,%v)", got.CX, got.CY)
	}
}

func TestMergeCrossOriginFramesEmptyIsNoOp(t *testing.T) {
	snap := &PageSnapshot{Elements: []Element{{Ref: "e1"}}}
	if n := MergeCrossOriginFrames(snap, nil); n != 0 {
		t.Fatalf("expected 0 for nil frames, got %d", n)
	}
	if len(snap.Elements) != 1 {
		t.Fatalf("snapshot elements must be unchanged, got %d", len(snap.Elements))
	}
}

func TestPromoteCrossOriginFramesSurfacesClickableFrameElements(t *testing.T) {
	snap := &PageSnapshot{
		Metadata: map[string]interface{}{
			"cross_origin_frames": []interface{}{
				map[string]interface{}{"x": 40.0, "y": 60.0, "width": 600.0, "height": 300.0, "origin": "https://example.com"},
			},
		},
	}
	n := PromoteCrossOriginFrames(snap, nil)
	if n != 1 || len(snap.Elements) != 1 {
		t.Fatalf("expected 1 promoted frame element, got n=%d len=%d", n, len(snap.Elements))
	}
	e := snap.Elements[0]
	if e.Ref != "f0" || e.Role != "iframe" || e.Name != "https://example.com" {
		t.Fatalf("unexpected promoted frame element: %+v", e)
	}
	// center = (40+300, 60+150) = (340, 210)
	if e.CX != 340 || e.CY != 210 {
		t.Fatalf("expected center (340,210), got (%v,%v)", e.CX, e.CY)
	}
	if len(e.Source) != 1 || e.Source[0] != "frame" {
		t.Fatalf("expected source [frame], got %v", e.Source)
	}
	if snap.Metadata["cross_origin_frames_promoted"] != 1 {
		t.Fatalf("expected metadata cross_origin_frames_promoted=1")
	}
}

func TestPromoteCrossOriginFramesSkipsAlreadyRead(t *testing.T) {
	snap := &PageSnapshot{
		Metadata: map[string]interface{}{
			"cross_origin_frames": []interface{}{
				map[string]interface{}{"x": 0.0, "y": 0.0, "width": 100.0, "height": 100.0, "origin": "https://a.test"},
				map[string]interface{}{"x": 0.0, "y": 0.0, "width": 100.0, "height": 100.0, "origin": "https://b.test"},
			},
		},
	}
	// Frame 0 already had its controls read; only frame 1 should be promoted.
	if n := PromoteCrossOriginFrames(snap, map[int]bool{0: true}); n != 1 {
		t.Fatalf("expected 1 promoted (skip already-read), got %d", n)
	}
	if snap.Elements[0].Ref != "f1" {
		t.Fatalf("expected only f1 promoted, got %q", snap.Elements[0].Ref)
	}
}

func TestMergeCrossOriginFramesMatchesBoxByOrigin(t *testing.T) {
	// Two frames of different origins listed out of order vs the metadata boxes;
	// each must pick the box of its OWN origin, not positional order.
	snap := &PageSnapshot{
		Metadata: map[string]interface{}{
			"cross_origin_frames": []interface{}{
				map[string]interface{}{"x": 0.0, "y": 0.0, "width": 50.0, "height": 50.0, "origin": "https://a.test"},
				map[string]interface{}{"x": 1000.0, "y": 0.0, "width": 50.0, "height": 50.0, "origin": "https://b.test"},
			},
		},
	}
	frames := []CrossOriginFrame{
		{Origin: "https://b.test", Elements: []CrossOriginFrameElement{{Role: "button", Tag: "button", X: 0, Y: 0, W: 10, H: 10}}},
		{Origin: "https://a.test", Elements: []CrossOriginFrameElement{{Role: "button", Tag: "button", X: 0, Y: 0, W: 10, H: 10}}},
	}
	MergeCrossOriginFrames(snap, frames)
	// frame 0 (b.test) → box x=1000 → center x = 1000+0+5 = 1005.
	if snap.Elements[0].CX != 1005 {
		t.Fatalf("frame 0 (b.test) should map to box x=1000, got CX=%v", snap.Elements[0].CX)
	}
	// frame 1 (a.test) → box x=0 → center x = 5.
	if snap.Elements[1].CX != 5 {
		t.Fatalf("frame 1 (a.test) should map to box x=0, got CX=%v", snap.Elements[1].CX)
	}
}
