package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

const (
	antiscanURL      = "https://raw.githubusercontent.com/stamparm/ipsum/master/levels/1.txt"
	antiscanSet      = "sharedd_scanners"
	antiscanTempSet  = "sharedd_scan_tmp"
	antiscanChain    = "ANTISCAN_MTPROTO"
	antiscanInterval = 30 * time.Minute
)

var antiscanKick = make(chan struct{}, 1)

var antiscanRun = func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd.CombinedOutput()
}

var antiscanFetch = func(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, antiscanURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

func parseAntiscanIPs(data []byte) []string {
	seen := map[string]bool{}
	out := []string{}
	s := bufio.NewScanner(bytes.NewReader(data))
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 0 {
			continue
		}
		ip := net.ParseIP(fields[0])
		if ip == nil || ip.To4() == nil || seen[fields[0]] {
			continue
		}
		seen[fields[0]] = true
		out = append(out, fields[0])
	}
	return out
}

func runAntiscan(ctx context.Context, stdin []byte, name string, args ...string) error {
	out, err := antiscanRun(ctx, stdin, name, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func removeAntiscanHooks(ctx context.Context) {
	out, err := antiscanRun(ctx, nil, "iptables", "-S", "INPUT")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "-A" || fields[1] != "INPUT" || !strings.Contains(line, "-j "+antiscanChain) {
			continue
		}
		args := append([]string{"-D", "INPUT"}, fields[2:]...)
		_, _ = antiscanRun(ctx, nil, "iptables", args...)
	}
}

func applyAntiscan(cfg *NodeConfig) error {
	shared := sharedConfigCache.Get()
	if shared.ProxyPort < 1 || shared.ProxyPort > 65535 {
		return fmt.Errorf("registry returned invalid proxy_port %d", shared.ProxyPort)
	}
	telemtCfg, err := loadTelemtConfig(cfg.Telemt.ConfigPath)
	if err != nil {
		return err
	}
	localPort := telemtProxyPort(telemtCfg)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if localPort != shared.ProxyPort {
		removeAntiscanHooks(ctx)
		return fmt.Errorf("local proxy port %d differs from registry shared port %d; antiscan disabled", localPort, shared.ProxyPort)
	}

	data, err := antiscanFetch(ctx)
	if err != nil {
		return fmt.Errorf("download scanner list: %w", err)
	}
	ips := parseAntiscanIPs(data)
	if len(ips) == 0 {
		return fmt.Errorf("downloaded scanner list contains no IPv4 addresses")
	}

	_ = runAntiscan(ctx, nil, "ipset", "destroy", antiscanTempSet)
	var restore strings.Builder
	fmt.Fprintf(&restore, "create %s hash:ip hashsize 4096 maxelem 1048576\n", antiscanTempSet)
	for _, ip := range ips {
		fmt.Fprintf(&restore, "add %s %s\n", antiscanTempSet, ip)
	}
	if err := runAntiscan(ctx, []byte(restore.String()), "ipset", "restore", "-exist"); err != nil {
		return err
	}
	if err := runAntiscan(ctx, nil, "ipset", "create", antiscanSet, "hash:ip", "hashsize", "4096", "maxelem", "1048576", "-exist"); err != nil {
		return err
	}
	if err := runAntiscan(ctx, nil, "ipset", "swap", antiscanTempSet, antiscanSet); err != nil {
		return err
	}
	_ = runAntiscan(ctx, nil, "ipset", "destroy", antiscanTempSet)
	if _, err := antiscanRun(ctx, nil, "iptables", "-N", antiscanChain); err != nil {
		// Existing chain is expected and harmless.
	}
	if _, err := antiscanRun(ctx, nil, "iptables", "-C", antiscanChain, "-m", "set", "--match-set", antiscanSet, "src", "-j", "DROP"); err != nil {
		if err := runAntiscan(ctx, nil, "iptables", "-A", antiscanChain, "-m", "set", "--match-set", antiscanSet, "src", "-j", "DROP"); err != nil {
			return err
		}
	}
	removeAntiscanHooks(ctx)
	if err := runAntiscan(ctx, nil, "iptables", "-I", "INPUT", "1", "-p", "tcp", "--dport", fmt.Sprint(shared.ProxyPort), "-j", antiscanChain); err != nil {
		return err
	}
	log.Printf("antiscan updated: %d scanner IPs blocked on tcp/%d", len(ips), shared.ProxyPort)
	return nil
}

func kickAntiscan() {
	select {
	case antiscanKick <- struct{}{}:
	default:
	}
}

func antiscanLoop(cfg *NodeConfig) {
	run := func() {
		if err := applyAntiscan(cfg); err != nil {
			log.Printf("antiscan: %v", err)
		}
	}
	// Initial /config is normally applied before this goroutine starts and may
	// have queued a kick. Consume it so startup performs exactly one update.
	select {
	case <-antiscanKick:
	default:
	}
	run()
	ticker := time.NewTicker(antiscanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			run()
		case <-antiscanKick:
			run()
		}
	}
}
