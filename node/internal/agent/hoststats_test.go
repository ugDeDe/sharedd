package agent

import (
	"math"
	"testing"
)

func TestParseMeminfo(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantTotal float64
		wantAvail float64
		wantOK    bool
	}{
		{
			name: "realistic",
			input: "MemTotal:       16384000 kB\n" +
				"MemFree:         1234567 kB\n" +
				"MemAvailable:    8192000 kB\n" +
				"Buffers:          123456 kB\n",
			wantTotal: 16384000,
			wantAvail: 8192000,
			wantOK:    true,
		},
		{
			name:      "missing available",
			input:     "MemTotal: 1000 kB\nMemFree: 500 kB\n",
			wantTotal: 1000, wantAvail: 0, wantOK: false,
		},
		{
			name:      "malformed values",
			input:     "MemTotal: notanumber kB\nMemAvailable: alsobad kB\n",
			wantTotal: 0, wantAvail: 0, wantOK: false,
		},
		{
			name:      "empty",
			input:     "",
			wantTotal: 0, wantAvail: 0, wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, avail, ok := parseMeminfo(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if total != tt.wantTotal {
				t.Errorf("total = %v, want %v", total, tt.wantTotal)
			}
			if avail != tt.wantAvail {
				t.Errorf("avail = %v, want %v", avail, tt.wantAvail)
			}
		})
	}
}

func TestParseCPUStat(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantTotal float64
		wantIdle  float64
		wantOK    bool
	}{
		{
			name:      "realistic",
			input:     "cpu  100 5 50 1000 20 0 3 0 0 0\ncpu0 50 2 25 500 10 0 1 0 0 0\n",
			wantTotal: 1178, // 100+5+50+1000+20+0+3+0+0+0
			wantIdle:  1020, // idle 1000 + iowait 20
			wantOK:    true,
		},
		{
			name:      "minimal four fields",
			input:     "cpu  10 20 30 40\n",
			wantTotal: 100,
			wantIdle:  40,
			wantOK:    true,
		},
		{
			name:      "no cpu line",
			input:     "intr 12345\nctxt 999\n",
			wantTotal: 0, wantIdle: 0, wantOK: false,
		},
		{
			name:      "malformed tick",
			input:     "cpu  10 abc 30 40\n",
			wantTotal: 0, wantIdle: 0, wantOK: false,
		},
		{
			name:      "too few fields",
			input:     "cpu  10 20\n",
			wantTotal: 0, wantIdle: 0, wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, idle, ok := parseCPUStat(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if math.Abs(total-tt.wantTotal) > 1e-9 {
				t.Errorf("total = %v, want %v", total, tt.wantTotal)
			}
			if math.Abs(idle-tt.wantIdle) > 1e-9 {
				t.Errorf("idle = %v, want %v", idle, tt.wantIdle)
			}
		})
	}
}
