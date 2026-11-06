package antiscan

import (
	"strings"
	"testing"
)

func TestParseListAcceptsIPsCIDRsAndRanges(t *testing.T) {
	data := "# ipsum header\n1.2.3.4\t3\n10.0.0.0/30 ; comment\n2001:db8::1\ngarbage\n5.6.7.8-5.6.7.9\n1.2.3.4 7\n"
	got := strings.Join(ParseList([]byte(data)).CIDRs(), ",")
	want := "1.2.3.4,5.6.7.8/31,10.0.0.0/30"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestParseRIPEPrefixAndRangeFormats(t *testing.T) {
	data := `{"status":"ok","data":{"resources":{"ipv4":["5.0.0.0/24","95.0.0.0-95.0.1.255","bad"],"ipv6":["2a00::/16"]}}}`
	s, err := ParseRIPE([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.CIDRs(), ","); got != "5.0.0.0/24,95.0.0.0/23" {
		t.Fatalf("got %s", got)
	}
	if _, err := ParseRIPE([]byte(`{"status":"error","data":{}}`)); err == nil {
		t.Fatal("non-ok status must fail")
	}
	if _, err := ParseRIPE([]byte(`{"status":"ok","data":{"resources":{"ipv4":[]}}}`)); err == nil {
		t.Fatal("empty resource list must fail (never filter against an empty geo set)")
	}
}

func TestParseGeoPlainText(t *testing.T) {
	s, err := ParseGeo([]byte("5.0.0.0/24\n"))
	if err != nil || strings.Join(s.CIDRs(), ",") != "5.0.0.0/24" {
		t.Fatalf("got %v %v", s, err)
	}
	if _, err := ParseGeo([]byte("\n# nothing\n")); err == nil {
		t.Fatal("empty geo list must fail")
	}
}

func TestIntersectCropsPartialOverlap(t *testing.T) {
	ru := ParseList([]byte("10.0.0.128/25\n10.0.2.0/24\n"))
	src := ParseList([]byte("10.0.0.0/24\n10.0.1.5\n10.0.2.7\n10.0.3.0/24\n"))
	got := strings.Join(Intersect(src, ru).CIDRs(), ",")
	// /24 обрезан до российской половины, адрес вне RU выброшен
	if got != "10.0.0.128/25,10.0.2.7" {
		t.Fatalf("got %s", got)
	}
}

func TestNormalizeMergesAdjacentAndNested(t *testing.T) {
	s := Union(ParseList([]byte("10.0.0.0/25\n10.0.0.128/25\n10.0.0.5\n")), ParseList([]byte("10.0.1.0/24\n")))
	if got := strings.Join(s.CIDRs(), ","); got != "10.0.0.0/23" {
		t.Fatalf("got %s", got)
	}
}

func TestCIDRsUnalignedRangeAndEdges(t *testing.T) {
	s := ParseList([]byte("10.0.0.1-10.0.0.6\n255.255.255.254-255.255.255.255\n"))
	if got := strings.Join(s.CIDRs(), ","); got != "10.0.0.1,10.0.0.2/31,10.0.0.4/31,10.0.0.6,255.255.255.254/31" {
		t.Fatalf("got %s", got)
	}
	if got := strings.Join(ParseList([]byte("0.0.0.0/0\n")).CIDRs(), ","); got != "0.0.0.0/0" {
		t.Fatalf("got %s", got)
	}
}

func TestRevisionStableAndContentBound(t *testing.T) {
	a := Revision([]string{"1.2.3.4", "5.0.0.0/24"})
	if a != Revision([]string{"1.2.3.4", "5.0.0.0/24"}) || a == Revision([]string{"1.2.3.4"}) || len(a) != 16 {
		t.Fatalf("revision not stable/bound: %s", a)
	}
}
