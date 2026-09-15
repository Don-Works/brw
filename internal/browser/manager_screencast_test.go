package browser

import (
	"sync/atomic"
	"testing"
	"time"
)

var screencastAdmissionNames = map[screencastAdmission]string{
	frameInOrder:       "in order",
	frameUnstamped:     "unstamped",
	frameOutOfOrder:    "out of order",
	frameDuplicateSwap: "duplicate swap",
}

func (a screencastAdmission) name() string {
	if name, ok := screencastAdmissionNames[a]; ok {
		return name
	}
	return "unknown"
}

// Chrome's frame-swap clock and brw's wall clock are different domains and only
// one of them orders the stream. A stamp brw cannot place in its own domain
// reads as far in the future; admitting it would push the ordering floor there
// and discard every real frame behind it for the life of the stream, so it must
// be dropped from the frame and leave the floor alone.
//
// This is table-driven against the decision itself rather than against a live
// screencast because headless Chrome stamps every frame it sends, from a clock
// it does share: the frames below are the ones a live run cannot produce.
func TestScreencastFrameAdmission(t *testing.T) {
	read := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

	type frame struct {
		stamped   time.Time
		arrivedAt time.Time
		want      screencastAdmission
		wantSwap  time.Time
	}
	tests := []struct {
		name   string
		frames []frame
	}{
		{
			name: "a swap behind the read advances the floor",
			frames: []frame{
				{stamped: read.Add(-30 * time.Millisecond), arrivedAt: read, want: frameInOrder, wantSwap: read.Add(-30 * time.Millisecond)},
				{stamped: read.Add(-20 * time.Millisecond), arrivedAt: read.Add(5 * time.Millisecond), want: frameInOrder, wantSwap: read.Add(-20 * time.Millisecond)},
				{stamped: read.Add(-10 * time.Millisecond), arrivedAt: read.Add(10 * time.Millisecond), want: frameInOrder, wantSwap: read.Add(-10 * time.Millisecond)},
			},
		},
		{
			name: "a swap at or before the floor is discarded, and the two are told apart",
			frames: []frame{
				{stamped: read.Add(-20 * time.Millisecond), arrivedAt: read, want: frameInOrder, wantSwap: read.Add(-20 * time.Millisecond)},
				{stamped: read.Add(-20 * time.Millisecond), arrivedAt: read.Add(5 * time.Millisecond), want: frameDuplicateSwap, wantSwap: read.Add(-20 * time.Millisecond)},
				{stamped: read.Add(-25 * time.Millisecond), arrivedAt: read.Add(10 * time.Millisecond), want: frameOutOfOrder, wantSwap: read.Add(-25 * time.Millisecond)},
				{stamped: read.Add(-15 * time.Millisecond), arrivedAt: read.Add(15 * time.Millisecond), want: frameInOrder, wantSwap: read.Add(-15 * time.Millisecond)},
			},
		},
		{
			name: "a swap from a clock brw does not share never becomes the floor",
			frames: []frame{
				{stamped: read.Add(-5 * time.Millisecond), arrivedAt: read, want: frameInOrder, wantSwap: read.Add(-5 * time.Millisecond)},
				// An hour ahead of the read: a monotonic-since-boot figure, or
				// any other clock the browser process keeps to itself.
				{stamped: read.Add(time.Hour), arrivedAt: read.Add(10 * time.Millisecond), want: frameUnstamped},
				// The real frames behind it still stream, which is the whole
				// point of refusing the one above.
				{stamped: read.Add(8 * time.Millisecond), arrivedAt: read.Add(20 * time.Millisecond), want: frameInOrder, wantSwap: read.Add(8 * time.Millisecond)},
				{stamped: read.Add(18 * time.Millisecond), arrivedAt: read.Add(30 * time.Millisecond), want: frameInOrder, wantSwap: read.Add(18 * time.Millisecond)},
			},
		},
		{
			name: "a swap equal to the read is our own clock wearing the compositor's name",
			frames: []frame{
				{stamped: read, arrivedAt: read, want: frameUnstamped},
				{stamped: read.Add(-1 * time.Millisecond), arrivedAt: read.Add(5 * time.Millisecond), want: frameInOrder, wantSwap: read.Add(-1 * time.Millisecond)},
			},
		},
		{
			name: "a frame Chrome sent no swap time for is forwarded and sets nothing",
			frames: []frame{
				{arrivedAt: read, want: frameUnstamped},
				{stamped: read.Add(-40 * time.Millisecond), arrivedAt: read.Add(5 * time.Millisecond), want: frameInOrder, wantSwap: read.Add(-40 * time.Millisecond)},
				{arrivedAt: read.Add(10 * time.Millisecond), want: frameUnstamped},
				{stamped: read.Add(-30 * time.Millisecond), arrivedAt: read.Add(15 * time.Millisecond), want: frameInOrder, wantSwap: read.Add(-30 * time.Millisecond)},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var floor atomic.Int64
			for i, f := range tt.frames {
				admission, swap := admitScreencastFrame(&floor, f.stamped, f.arrivedAt)
				if admission != f.want {
					t.Errorf("frame %d admitted as %s, want %s", i, admission.name(), f.want.name())
				}
				if !swap.Equal(f.wantSwap) {
					t.Errorf("frame %d publishes swap %v, want %v", i, swap, f.wantSwap)
				}
			}
		})
	}
}
