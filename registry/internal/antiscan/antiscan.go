// Package antiscan — сборка списка сканеров для дропа на нодах: регистратор
// скачивает abuse-листы (IPv4-адреса и подсети), оставляет только адреса,
// попадающие в российские диапазоны (RIPEstat country-resource-list), и
// сворачивает результат в минимальный набор CIDR. Пакет чистый: сеть и
// расписание — в server (antiscan.go), здесь только разбор и арифметика
// интервалов. Только IPv4: агент применяет список через iptables/ipset inet.
package antiscan

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/bits"
	"net/netip"
	"sort"
	"strings"
)

// Interval — замкнутый диапазон IPv4 [Lo, Hi] в виде uint32.
type Interval struct{ Lo, Hi uint32 }

// Set — отсортированные, непересекающиеся и несмежные интервалы.
type Set []Interval

// MaxEntries — потолок итогового списка (ipset hash:net maxelem агента 1M;
// реальный RU-срез abuse-листов на порядки меньше — это защита от мусора).
const MaxEntries = 500000

func addrToU32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func u32ToAddr(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func prefixInterval(p netip.Prefix) Interval {
	p = p.Masked()
	lo := addrToU32(p.Addr())
	size := uint64(1) << (32 - p.Bits())
	return Interval{Lo: lo, Hi: uint32(uint64(lo) + size - 1)}
}

// parseEntry — одна запись: адрес, CIDR или диапазон "a-b". IPv6 и мусор — ok=false.
func parseEntry(s string) (Interval, bool) {
	if lo, hi, found := strings.Cut(s, "-"); found {
		a, err1 := netip.ParseAddr(strings.TrimSpace(lo))
		b, err2 := netip.ParseAddr(strings.TrimSpace(hi))
		if err1 != nil || err2 != nil || !a.Is4() || !b.Is4() || b.Less(a) {
			return Interval{}, false
		}
		return Interval{Lo: addrToU32(a), Hi: addrToU32(b)}, true
	}
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() {
			return Interval{}, false
		}
		return prefixInterval(p), true
	}
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return Interval{}, false
	}
	v := addrToU32(a)
	return Interval{Lo: v, Hi: v}, true
}

// ParseList — текстовый abuse-лист: запись — первое поле строки (ipsum кладёт
// рядом счётчик), комментарии '#'/';' и нераспознанные строки пропускаются.
func ParseList(data []byte) Set {
	var out []Interval
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if iv, ok := parseEntry(fields[0]); ok {
			out = append(out, iv)
		}
	}
	return Normalize(out)
}

// ParseRIPE — ответ RIPEstat country-resource-list: data.resources.ipv4 —
// CIDR (v4_format=prefix) или диапазоны "a-b" (формат по умолчанию).
func ParseRIPE(data []byte) (Set, error) {
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Resources struct {
				IPv4 []string `json:"ipv4"`
			} `json:"resources"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("ripestat: %w", err)
	}
	if resp.Status != "" && resp.Status != "ok" {
		return nil, fmt.Errorf("ripestat: status %q", resp.Status)
	}
	out := make([]Interval, 0, len(resp.Data.Resources.IPv4))
	for _, s := range resp.Data.Resources.IPv4 {
		if iv, ok := parseEntry(strings.TrimSpace(s)); ok {
			out = append(out, iv)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ripestat: no IPv4 resources")
	}
	return Normalize(out), nil
}

// ParseGeo — гео-список: JSON RIPEstat либо (для своего URL) текст CIDR по строке.
func ParseGeo(data []byte) (Set, error) {
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '{' {
		return ParseRIPE(data)
	}
	s := ParseList(data)
	if len(s) == 0 {
		return nil, fmt.Errorf("geo list contains no IPv4 ranges")
	}
	return s, nil
}

// Normalize — сортирует и сливает пересекающиеся и смежные интервалы.
func Normalize(in []Interval) Set {
	if len(in) == 0 {
		return nil
	}
	s := append([]Interval(nil), in...)
	sort.Slice(s, func(i, j int) bool { return s[i].Lo < s[j].Lo })
	out := Set{s[0]}
	for _, iv := range s[1:] {
		last := &out[len(out)-1]
		if last.Hi == ^uint32(0) || iv.Lo <= last.Hi+1 {
			if iv.Hi > last.Hi {
				last.Hi = iv.Hi
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// Union — объединение нескольких наборов.
func Union(sets ...Set) Set {
	var all []Interval
	for _, s := range sets {
		all = append(all, s...)
	}
	return Normalize(all)
}

// Intersect — пересечение двух нормализованных наборов: подсеть, частично
// попадающая в RU, обрезается до российской части.
func Intersect(a, b Set) Set {
	var out Set
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		lo, hi := max(a[i].Lo, b[j].Lo), min(a[i].Hi, b[j].Hi)
		if lo <= hi {
			out = append(out, Interval{Lo: lo, Hi: hi})
		}
		if a[i].Hi < b[j].Hi {
			i++
		} else {
			j++
		}
	}
	return out
}

// CIDRs — минимальное покрытие набора префиксами; /32 пишется голым адресом.
func (s Set) CIDRs() []string {
	var out []string
	for _, iv := range s {
		lo := uint64(iv.Lo)
		hi := uint64(iv.Hi)
		for lo <= hi {
			// крупнейший блок, выровненный по lo и не выходящий за hi
			size := uint64(1) << 32
			if lo != 0 {
				size = uint64(1) << bits.TrailingZeros64(lo)
			}
			for size > hi-lo+1 {
				size >>= 1
			}
			plen := 32 - (bits.Len64(size) - 1)
			if plen == 32 {
				out = append(out, u32ToAddr(uint32(lo)).String())
			} else {
				out = append(out, netip.PrefixFrom(u32ToAddr(uint32(lo)), plen).String())
			}
			lo += size
		}
	}
	return out
}

// Revision — стабильный идентификатор содержимого (агент сравнивает его с
// применённым и скачивает список только при смене).
func Revision(entries []string) string {
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
