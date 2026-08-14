package main

// Управление прокси-сервисом (V7.9.1).
//
// Правка telemt.toml из sync-цикла раньше делалась «на живую»: файл писался,
// а работающий прокси его не перечитывал — юзеры/SNI лежали мёртвым грузом до
// чьего-нибудь ручного рестарта. Теперь применение идёт по фиксированному
// конвейеру:
//
//	стоп прокси → патч и сохранение конфига → старт прокси → ожидание подъёма
//	(поллим /metrics — ответил HTTP 200 = конфиг принят и прокси реально встал)
//
// Если после старта прокси не встал за таймаут — откатываем файл на исходную
// версию и поднимаем прокси обратно: кривая правка не должна ронять ноду.
//
// Детект юнита: у Classic и MEKO-фикса сервис называется telemt.service —
// его ищем первым. MTProxyL управляет прокси через свой CLI (mtproxyl
// restart пересобирает рабочий config.toml из superexpert.toml), тогда
// systemd-юнита может не быть — fallback на CLI.

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Таймауты — var ради подмены в тестах.
var (
	// proxyUpTimeout — сколько ждём подъёма прокси после рестарта (первая
	// загрузка сертификата маскировки может занимать десятки секунд).
	proxyUpTimeout = 60 * time.Second
	// proxyRollbackTimeout — ожидание после отката конфига.
	proxyRollbackTimeout = 30 * time.Second
	// gpProxyWaitTimeout — сколько globalping-цикл ждёт listen-порт прокси
	// перед созданием measurement (иначе пробы идут в ещё не поднятый
	// порт и верификация рисует 0).
	gpProxyWaitTimeout = 75 * time.Second
)

// proxyUnitCandidates — в порядке приоритета. Первым telemt.service: на
// Classic (ванильный telemt) и MEKO-фиксе (/opt/mtpr-simple) юнит именно такой.
var proxyUnitCandidates = []string{
	"telemt.service",
	"mtproxy.service",
	"mtproxyl-telemt.service",
}

func systemdAvailable() bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// unitLoaded — юнит известен systemd. list-units тут НЕ подходит: он видит
// только загруженные в менеджер юниты, а установленный, но ещё ни разу не
// загруженный telemt.service (свежее развёртывание MEKO/Classic) из него
// выпадает — ровно баг «не находит службу телемт».
func unitLoaded(unit string) bool {
	out, err := exec.Command("systemctl", "show", unit, "-p", "LoadState", "--value").Output()
	if err == nil {
		// --value есть не везде (systemd <230 вернёт "LoadState=loaded").
		s := strings.TrimSpace(string(out))
		if s == "loaded" || s == "LoadState=loaded" {
			return true
		}
	}
	// fallback: юнит-файл на диске есть, менеджер его ещё не подхватил
	// (daemon-reload не делали) — cat читает с диска напрямую.
	return exec.Command("systemctl", "cat", unit).Run() == nil
}

// systemdUnitDirs — где лежат юнит-файлы. var ради подмены в тестах.
var systemdUnitDirs = []string{
	"/etc/systemd/system",
	"/run/systemd/system",
	"/usr/lib/systemd/system",
	"/lib/systemd/system",
}

// unitFileOnDisk — юнит-файл физически существует (включая симлинк из
// *.wants). Последний рубеж детекта: даже когда systemctl в целом не
// отвечает (частичный systemd, дохлый dbus на дешёвых VPS), restart по
// имени файла обычно всё равно срабатывает.
func unitFileOnDisk(unit string) bool {
	for _, dir := range systemdUnitDirs {
		if _, err := os.Stat(dir + "/" + unit); err == nil {
			return true
		}
	}
	if matches, _ := filepath.Glob("/etc/systemd/system/*.wants/" + unit); len(matches) > 0 {
		return true
	}
	return false
}

// detectProxyUnit — имя systemd-юнита прокси ("" — не нашли: например,
// MTProxyL без юнита, управление через CLI). Три уровня: опрос менеджера,
// индекс юнит-файлов, голый поиск файлов на диске.
func detectProxyUnit() string {
	for _, u := range proxyUnitCandidates {
		if unitLoaded(u) {
			return u
		}
	}
	// list-unit-files читает индекс с диска — видит юниты, которые менеджер
	// ещё не загрузил (свежая установка без daemon-reload).
	if out, err := exec.Command("systemctl", "list-unit-files", "--no-legend", "--no-pager").Output(); err == nil {
		present := make(map[string]bool)
		for _, ln := range strings.Split(string(out), "\n") {
			if f := strings.Fields(ln); len(f) > 0 {
				present[f[0]] = true
			}
		}
		for _, u := range proxyUnitCandidates {
			if present[u] {
				return u
			}
		}
	}
	// последний рубеж — просто файлы на диске
	for _, u := range proxyUnitCandidates {
		if unitFileOnDisk(u) {
			return u
		}
	}
	return ""
}

// mtproxylCLIAvailable — MTProxyL CLI в PATH (его restart пересобирает рабочий
// config.toml из superexpert.toml).
func mtproxylCLIAvailable() bool {
	_, err := exec.LookPath("mtproxyl")
	return err == nil
}

// preferMtproxylCLI — на MTProxyL-ноде CLI первичен: systemctl-рестарт
// поднял бы прокси с ранее сгенерированным config.toml, а наш патч лежит в
// superexpert.toml — до рабочего конфига его доносит только mtproxyl restart.
func preferMtproxylCLI() bool {
	return detectNodeType() == NodeTypeMTProxyL && mtproxylCLIAvailable()
}

// ensureMetricsTimeout — сколько ждём метрики в ensureProxyUp при первой
// проверке, прежде чем считать прокси лежащим и (пере)запускать. var ради тестов.
var ensureMetricsTimeout = 8 * time.Second

// ensureProxyUp — финальный шаг пайплайна «запустить прокси»: метрики должны
// отвечать; если нет — перезапускаем тем, что есть: systemd-юнит, mtproxyl CLI
// или последним рубежом вслепую `systemctl restart telemt.service` (на
// Classic/MEKO юнит гарантированно называется так, даже если детект споткнулся).
func ensureProxyUp(cfg *NodeConfig) error {
	if err := waitMetricsReady(cfg, ensureMetricsTimeout); err == nil {
		return nil
	}
	unit := ""
	if systemdAvailable() {
		unit = detectProxyUnit()
	}
	if preferMtproxylCLI() {
		log.Printf("proxy not answering; restarting via mtproxyl CLI...")
		if rerr := tryMtproxylRestart(); rerr != nil {
			return rerr
		}
		return waitMetricsReady(cfg, proxyUpTimeout)
	}
	if unit != "" {
		log.Printf("proxy not answering; restarting %s ...", unit)
		if err := proxyCtl("restart", unit); err != nil {
			return err
		}
		return waitMetricsReady(cfg, proxyUpTimeout)
	}
	return blindRestartTelemt(cfg)
}

// blindRestartTelemt — слепой рестарт telemt.service: юнит мог ускользнуть от
// всех уровней детекта (битый dbus и т.п.), а restart по имени — сработать.
func blindRestartTelemt(cfg *NodeConfig) error {
	if !systemdAvailable() {
		return fmt.Errorf("no systemd and no mtproxyl CLI — start proxy manually")
	}
	log.Printf("proxy unit not detected; trying blind `systemctl restart telemt.service` ...")
	if err := proxyCtl("restart", "telemt.service"); err != nil {
		return err
	}
	return waitMetricsReady(cfg, proxyUpTimeout)
}

func proxyCtl(action, unit string) error {
	out, err := exec.Command("systemctl", action, unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %v: %s", action, unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// metricsURLForConfig — URL /metrics по ТЕКУЩЕМУ содержимому telemt.toml.
func metricsURLForConfig(cfg *NodeConfig) (string, error) {
	telemtCfg, err := loadTelemtConfig(cfg.Telemt.ConfigPath)
	if err != nil {
		return "", err
	}
	return metricsURLFromTelemt(telemtCfg.Server.MetricsPort, telemtCfg.Server.MetricsListen), nil
}

// waitMetricsReady — ждём, пока прокси реально встанет: метрики должны
// отвечать HTTP 200 (заодно это и «проверка конфига»: кривой telemt.toml
// прокси просто не поднимется).
func waitMetricsReady(cfg *NodeConfig, timeout time.Duration) error {
	url, err := metricsURLForConfig(cfg)
	if err != nil {
		return fmt.Errorf("resolve metrics url: %w", err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	for {
		resp, herr := client.Get(url)
		if herr == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("metrics endpoint %s not answering after %s", url, timeout)
		}
		time.Sleep(time.Second)
	}
}

// waitProxyTCP — порт прокси слушается (globalping-цикл ждёт это перед
// созданием measurement, чтобы не отстреливаться в лежащий прокси).
func waitProxyTCP(port int, timeout time.Duration) bool {
	if port <= 0 {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
		if err == nil {
			conn.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
}

// tryMtproxylRestart — перезапуск прокси через CLI MTProxyL (superexpert.toml
// → config.toml пересобирается самим CLI). ASSUME_YES, как в установщике.
func tryMtproxylRestart() error {
	bin, err := exec.LookPath("mtproxyl")
	if err != nil {
		return fmt.Errorf("mtproxyl CLI not found in PATH")
	}
	cmd := exec.Command(bin, "restart")
	cmd.Env = append(os.Environ(), "MTPROXYL_ASSUME_YES=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mtproxyl restart: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
