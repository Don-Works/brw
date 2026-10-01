package recipe

import (
	"bytes"
	"encoding/json"
	"testing"
)

func FuzzParseRoundTripAndTrailingData(f *testing.F) {
	valid, err := json.Marshal(validRecipe("https://billing.example.test"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add([]byte(`{"schema_version":1,"id":"synthetic.recipe"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		value, err := Parse(data)
		if err != nil {
			return
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > 1<<20 {
			return
		}
		roundTrip, err := Parse(encoded)
		if err != nil {
			t.Fatalf("accepted recipe failed round trip: %v", err)
		}
		roundTripEncoded, err := json.Marshal(roundTrip)
		if err != nil || !bytes.Equal(encoded, roundTripEncoded) {
			t.Fatal("round-trip representation changed")
		}
		firstDigest, err := Digest(value)
		if err != nil {
			t.Fatal(err)
		}
		secondDigest, err := Digest(roundTrip)
		if err != nil || firstDigest != secondDigest {
			t.Fatal("round-trip digest changed")
		}
		if _, err := Parse(append(encoded, []byte("\n{}")...)); err == nil {
			t.Fatal("accepted trailing recipe JSON")
		}
	})
}
