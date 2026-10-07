package browser

import (
	"testing"
	"time"
)

func TestOperationTimeoutZeroMeansNoFixedLimit(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
		err  bool
	}{
		{"0", NoOperationTimeout, false},
		{"none", NoOperationTimeout, false},
		{"off", NoOperationTimeout, false},
		{"0s", NoOperationTimeout, false},
		{"45s", 45 * time.Second, false},
		{"soon", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseOperationTimeout(tc.raw)
		if (err != nil) != tc.err || got != tc.want {
			t.Fatalf("ParseOperationTimeout(%q) = %s, %v; want %s, err=%v", tc.raw, got, err, tc.want, tc.err)
		}
	}
	if OperationTimeout(0) != NoOperationTimeout || OperationTimeout(20*time.Second) != 20*time.Second {
		t.Fatal("OperationTimeout must keep a positive limit and turn zero into no fixed limit")
	}
	if d := NoOperationTimeout*4 + 10*time.Minute; d <= NoOperationTimeout {
		t.Fatalf("small sums of NoOperationTimeout overflow: %s", d)
	}
}
