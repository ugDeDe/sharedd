package agent

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeProc — /proc с двумя процессами прокси (мастер и воркер держат
// слушающий сокет :443) и посторонним процессом на :80.
func fakeProc(t *testing.T, ticks map[int]int) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hdr := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	write("net/tcp", hdr+
		"   0: 00000000:01BB 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1111 1 0\n"+
		"   1: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2222 1 0\n"+
		"   2: 0100007F:01BB 0200007F:9C40 01 00000000:00000000 00:00000000 00000000     0        0 3333 1 0\n")
	write("net/tcp6", hdr)
	write("stat", "cpu  1 2 3 4 5 6 7 8\ncpu0 1 2 3 4\ncpu1 1 2 3 4\nintr 1\n")
	write("meminfo", "MemTotal:       2048000 kB\nMemAvailable:   1024000 kB\n")
	proc := func(pid int, inode string, rssKB string) {
		write(filepath.Join(itoaPid(pid), "stat"), itoaPid(pid)+" (tele mt) S 1 1 1 0 -1 0 0 0 0 0 "+
			itoaPid(ticks[pid])+" 0 0 0 20 0 8 0 100 0 0\n")
		write(filepath.Join(itoaPid(pid), "status"), "Name:\ttelemt\nVmRSS:\t"+rssKB+" kB\n")
		fd := filepath.Join(root, itoaPid(pid), "fd")
		if err := os.MkdirAll(fd, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("socket:["+inode+"]", filepath.Join(fd, "3")); err != nil {
			t.Fatal(err)
		}
	}
	proc(10, "1111", "1000")
	proc(11, "1111", "500")
	proc(20, "2222", "9999")
	return root
}

func itoaPid(n int) string { return string(appendInt(nil, n)) }

func appendInt(b []byte, n int) []byte {
	if n >= 10 {
		b = appendInt(b, n/10)
	}
	return append(b, byte('0'+n%10))
}

func TestProxyResourcesSumsSocketHolders(t *testing.T) {
	old := procRoot
	t.Cleanup(func() { procRoot = old; proxyPrev = proxySample{} })
	proxyPrev = proxySample{}
	now := time.Unix(1_800_000_000, 0)

	procRoot = fakeProc(t, map[int]int{10: 1000, 11: 500, 20: 7})
	if _, rss, procs, cpuOK := proxyResources(443, now); procs != 2 || cpuOK || rss != 1500*1024 {
		t.Fatalf("first sample: procs=%d cpuOK=%v rss=%v", procs, cpuOK, rss)
	}
	// +150 тиков за 10 с при USER_HZ 100 = 0.15 ядра.
	procRoot = fakeProc(t, map[int]int{10: 1100, 11: 550, 20: 9999})
	cores, _, _, cpuOK := proxyResources(443, now.Add(10*time.Second))
	if !cpuOK || math.Abs(cores-0.15) > 1e-9 {
		t.Fatalf("cores=%v ok=%v, want 0.15", cores, cpuOK)
	}
	if n, ok := hostCPUCount(); !ok || n != 2 {
		t.Fatalf("cpu count %d", n)
	}
	if v, ok := hostRAMTotalBytes(); !ok || v != 2048000*1024 {
		t.Fatalf("ram total %v", v)
	}
	if _, _, procs, _ := proxyResources(8443, now); procs != 0 {
		t.Fatal("no listener on the port means no proxy processes")
	}
}
