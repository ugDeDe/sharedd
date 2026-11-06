package agent

// Ресурсы самого прокси (для «цены 1000 соединений» на регистраторе):
// CPU в ядрах и RSS всех процессов, держащих слушающий сокет общего порта.
// Процесс ищется по сокету, а не по systemd-юниту: так одинаково находятся
// telemt/MEKO, MTProxyL (в т.ч. в контейнере), mtdd и mtproxy с воркерами
// (воркеры наследуют сокет — их CPU и память суммируются).
//
// ponytail: /proc only, stdlib only. Linux is the only node target.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// clockTicks — USER_HZ: 100 на всех поддерживаемых ядрах Linux (x86_64,
// arm64); sysconf(_SC_CLK_TCK) без cgo недоступен.
const clockTicks = 100.0

// procRoot — корень procfs; var ради тестов.
var procRoot = "/proc"

// listenSocketInodes — inode слушающих TCP-сокетов на порту (IPv4 и IPv6).
func listenSocketInodes(port int) map[string]bool {
	want := strings.ToUpper(strconv.FormatInt(int64(port), 16))
	for len(want) < 4 {
		want = "0" + want
	}
	out := map[string]bool{}
	for _, f := range []string{"net/tcp", "net/tcp6"} {
		b, err := os.ReadFile(filepath.Join(procRoot, f))
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			fields := strings.Fields(line)
			if i == 0 || len(fields) < 10 {
				continue
			}
			// local_address = HEXIP:HEXPORT, st 0A = LISTEN, поле 9 — inode.
			if fields[3] != "0A" || !strings.HasSuffix(fields[1], ":"+want) {
				continue
			}
			if fields[9] != "0" {
				out[fields[9]] = true
			}
		}
	}
	return out
}

// pidsHoldingSockets — процессы, у которых открыт один из сокетов.
func pidsHoldingSockets(inodes map[string]bool) []int {
	if len(inodes) == 0 {
		return nil
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(procRoot, e.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(procRoot, e.Name(), "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			if inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

// procCPUTicks — utime+stime процесса (все потоки) в тиках.
func procCPUTicks(pid int) (float64, bool) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	s := string(b)
	// comm в скобках может содержать пробелы — поля считаем после ")".
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(s[i+1:])
	// после ")": state=0, ..., utime=11, stime=12
	if len(fields) < 13 {
		return 0, false
	}
	u, err1 := strconv.ParseFloat(fields[11], 64)
	st, err2 := strconv.ParseFloat(fields[12], 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return u + st, true
}

// procRSSBytes — VmRSS процесса.
func procRSSBytes(pid int) (float64, bool) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "VmRSS:" {
			kb, err := strconv.ParseFloat(f[1], 64)
			return kb * 1024, err == nil
		}
	}
	return 0, false
}

type proxySample struct {
	pids  string // набор pid: при смене (рестарт прокси) дельта CPU не считается
	ticks float64
	at    time.Time
}

var (
	proxySampleMu sync.Mutex
	proxyPrev     proxySample
)

// proxyResources — CPU прокси в ядрах (дельта тиков к прошлому вызову),
// RSS и число процессов. Первый вызов и смена набора процессов CPU не дают.
func proxyResources(port int, now time.Time) (cores, rss float64, procs int, cpuOK bool) {
	if port <= 0 {
		return 0, 0, 0, false
	}
	pids := pidsHoldingSockets(listenSocketInodes(port))
	if len(pids) == 0 {
		return 0, 0, 0, false
	}
	var ticks float64
	keys := make([]string, 0, len(pids))
	for _, pid := range pids {
		t, ok := procCPUTicks(pid)
		if !ok {
			continue
		}
		ticks += t
		if r, ok := procRSSBytes(pid); ok {
			rss += r
		}
		keys = append(keys, strconv.Itoa(pid))
		procs++
	}
	if procs == 0 {
		return 0, 0, 0, false
	}
	set := strings.Join(keys, ",")
	proxySampleMu.Lock()
	prev := proxyPrev
	proxyPrev = proxySample{pids: set, ticks: ticks, at: now}
	proxySampleMu.Unlock()
	if prev.pids == set && now.After(prev.at) && ticks >= prev.ticks {
		cores = (ticks - prev.ticks) / clockTicks / now.Sub(prev.at).Seconds()
		cpuOK = true
	}
	return cores, rss, procs, cpuOK
}

// hostCPUCount — число CPU хоста (строки cpuN в /proc/stat): та же база,
// что у процента загрузки хоста.
func hostCPUCount() (int, bool) {
	b, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return 0, false
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "cpu") && len(line) > 3 && line[3] >= '0' && line[3] <= '9' {
			n++
		}
	}
	return n, n > 0
}

// hostRAMTotalBytes — MemTotal хоста.
func hostRAMTotalBytes() (float64, bool) {
	b, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0, false
	}
	total, _, ok := parseMeminfo(string(b))
	return total * 1024, ok && total > 0
}
