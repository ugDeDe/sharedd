package nodehost

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecimalUnmarshal(t *testing.T) {
	t.Run("precision beyond float64", func(t *testing.T) {
		raw := "0.12345678901234567890123456789012345678901234567890"
		var d Decimal
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatal(err)
		}
		if string(d) != raw {
			t.Fatalf("precision lost: %s", d)
		}
	})
	t.Run("exponent forms", func(t *testing.T) {
		cases := map[string]string{
			"1.5e3":   "1500",
			"1e2":     "100",
			"-2.5e-2": "-0.025",
			"1E1":     "10",
		}
		for in, want := range cases {
			var d Decimal
			if err := json.Unmarshal([]byte(in), &d); err != nil {
				t.Fatalf("%s: %v", in, err)
			}
			if string(d) != want {
				t.Errorf("%s -> %s, want %s", in, d, want)
			}
		}
	})
	t.Run("string form", func(t *testing.T) {
		var d Decimal
		if err := json.Unmarshal([]byte(`"12.34"`), &d); err != nil {
			t.Fatal(err)
		}
		if string(d) != "12.34" {
			t.Fatalf("got %s", d)
		}
		var d2 Decimal
		if err := json.Unmarshal([]byte("-12.5"), &d2); err != nil {
			t.Fatal(err)
		}
		if string(d2) != "-12.5" {
			t.Fatalf("got %s", d2)
		}
	})
	t.Run("invalid forms rejected", func(t *testing.T) {
		for _, in := range []string{`abc`, `"abc"`, `""`, `1.2.3`, `"1.2.3"`, `0x10`, `+1`, `.5`, `5.`, `01`, `1e`, `1e999`, strings.Repeat("9", 200)} {
			var d Decimal
			if err := json.Unmarshal([]byte(in), &d); err == nil {
				t.Errorf("%q accepted", in)
			}
		}
	})
	t.Run("zero value is not monetary zero", func(t *testing.T) {
		type doc struct {
			Amount Decimal `json:"amount"`
		}
		b, err := json.Marshal(doc{})
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != `{"amount":""}` {
			t.Fatalf("zero value marshaled as %s, must be absent (empty), not \"0\"", b)
		}
		var d Decimal
		if d != "" {
			t.Fatalf("zero value %q", d)
		}
	})
	t.Run("round trip", func(t *testing.T) {
		type doc struct {
			Amount Decimal `json:"amount"`
		}
		in := []byte(`{"amount":"9.90"}`)
		var d doc
		if err := json.Unmarshal(in, &d); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(d)
		if string(b) != `{"amount":"9.90"}` {
			t.Fatalf("round trip: %s", b)
		}
	})
}
