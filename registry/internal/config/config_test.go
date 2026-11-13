package config

import "testing"

func TestParseAdDayStart(t *testing.T) {
	for in, want := range map[string]int{"": 0, "00:00": 0, "08:00": 480, "8:05": 485, " 23:59 ": 1439} {
		if got, err := ParseAdDayStart(in); err != nil || got != want {
			t.Fatalf("ParseAdDayStart(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"24:00", "08:60", "8", "08:00:00", "утро"} {
		if _, err := ParseAdDayStart(in); err == nil {
			t.Fatalf("ParseAdDayStart(%q) must fail", in)
		}
	}
}
