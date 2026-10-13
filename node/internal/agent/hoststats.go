package agent

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

// ponytail: /proc only, stdlib only. Linux is the only node target.

// hostCPUPercent возвращает загрузку хоста в процентах, вычисленную как
// дельта между текущим и ПРЕДЫДУЩИМ сэмплом /proc/stat. Первый вызов (нет
// предыдущего сэмпла) или ошибка парсинга → (0,false).
func hostCPUPercent() (float64, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	total, idle, ok := parseCPUStat(string(b))
	if !ok {
		return 0, false
	}

	cpuSampleMu.Lock()
	prev, hadPrev := cpuPrev, cpuHadPrev
	cpuPrev, cpuHadPrev = cpuSample{total: total, idle: idle}, true
	cpuSampleMu.Unlock()

	if !hadPrev {
		return 0, false
	}
	dTotal := total - prev.total
	dIdle := idle - prev.idle
	if dTotal <= 0 {
		return 0, false
	}
	pct := (dTotal - dIdle) / dTotal * 100
	return pct, true
}

// hostRAMPercent возвращает использованную RAM хоста в процентах:
// (1 - MemAvailable/MemTotal) * 100. Ошибка → (0,false).
func hostRAMPercent() (float64, bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	total, avail, ok := parseMeminfo(string(b))
	if !ok || total <= 0 {
		return 0, false
	}
	return (1 - avail/total) * 100, true
}

// parseMeminfo вытаскивает MemTotal/MemAvailable (в kB) из /proc/meminfo.
func parseMeminfo(s string) (totalKB, availKB float64, ok bool) {
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB, haveTotal = v, true
		case "MemAvailable:":
			availKB, haveAvail = v, true
		}
	}
	return totalKB, availKB, haveTotal && haveAvail
}

// parseCPUStat парсит первую строку "cpu ..." /proc/stat и возвращает
// суммарное и idle-время тиков (idle + iowait).
func parseCPUStat(s string) (total, idle float64, ok bool) {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		if len(fields) < 4 { // нужен хотя бы idle на индексе 3
			return 0, 0, false
		}
		for i, f := range fields {
			v, err := strconv.ParseFloat(f, 64)
			if err != nil {
				return 0, 0, false
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		return total, idle, true
	}
	return 0, 0, false
}

type cpuSample struct{ total, idle float64 }

var (
	cpuSampleMu sync.Mutex
	cpuPrev     cpuSample
	cpuHadPrev  bool
)
