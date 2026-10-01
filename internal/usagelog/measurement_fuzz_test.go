package usagelog

import (
	"encoding/json"
	"math"
	"testing"
	"unicode/utf8"
)

func FuzzEstimateTokensNumericBounds(f *testing.F) {
	for _, value := range []int64{0, 1, 3, 4, 5, -1, math.MinInt64, math.MaxInt64 - 3, math.MaxInt64} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, chars int64) {
		want := int64(0)
		if chars > 0 {
			want = chars / 4
			if chars%4 != 0 {
				want++
			}
		}
		if got := EstimateTokens(chars); got != want {
			t.Fatalf("chars=%d got=%d want=%d", chars, got, want)
		}
	})
}

func FuzzMeasurementJSONShapes(f *testing.F) {
	for _, seed := range []string{`null`, `{}`, `[]`, `{"text":"雪","bytes_base64":"YWJj"}`, `{"type":"image","data":"\u0059WJj"}`, `{"base64":3}`, `{"nested":[{"type":"audio","data":"YWJj"}]}`, `{`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		chars, binary := MeasureJSON(data)
		if chars < 0 || chars > int64(utf8.RuneCount(data)) || binary < 0 || binary > int64(len(data)) {
			t.Fatalf("invalid measurement chars=%d binary=%d bytes=%d", chars, binary, len(data))
		}
		if !json.Valid(data) && (chars != int64(utf8.RuneCount(data)) || binary != 0) {
			t.Fatal("invalid JSON changed literal measurements")
		}
		for _, operation := range []string{"brw_snapshot", "brw_read", "unknown"} {
			if observation := ObservationOptions(operation, data); observation != nil {
				var event Event
				observation.Apply(&event)
				if !ValidObservation(event) {
					t.Fatal("unsafe observation survived sanitization")
				}
			}
		}
	})
}
