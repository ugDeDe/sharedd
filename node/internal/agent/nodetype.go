package agent

// Тип ноды: чисто информационно для панели (бейдж Classic / MTProxyL /
// MEKO). Детект — по наличию каталога менеджера прокси:
//
//	/opt/mtpr-simple/ — фикс MEKO;
//	/opt/mtproxyl/ — форк MTProxyL;
//	/etc/mtdd/config.json — mtdd (C++-прокси, JSON-конфиг и API);
//	иначе — классический telemt.
//
// Ноды-мастера всех типов в DNS пишутся A-записью на свой IP, SNI везде
// общий от регистратора (фича selfmask CNAME/SNI из выпилена — не
// завелась на практике).

import (
	"os"
	"strings"
	"sync/atomic"

	"sharedd/node/internal/config"
)

// Пути — var ради подмены в тестах.
var (
	mekoInstallDir     = "/opt/mtpr-simple"
	mtproxylInstallDir = "/opt/mtproxyl"
)

const (
	NodeTypeClassic  = "classic"
	NodeTypeMTProxyL = "mtproxyl"
	NodeTypeMEKO     = "meko"
	NodeTypeMTDD     = "mtdd"
	// NodeTypeMTProxyLMtdd — только в /register: MTProxyL Manager с движком
	// mtdd. Внутри агента нода остаётся NodeTypeMTProxyL (CLI тот же).
	NodeTypeMTProxyLMtdd = "mtproxyl_mtdd"
)

// nodeTypeLegacyOnly — регистратор не знает mtproxyl_mtdd (400 про node_type):
// до перезапуска агента регистрируемся как mtproxyl.
var nodeTypeLegacyOnly atomic.Bool

// reportedNodeType — тип для /register и бейджа в панели: MTProxyL
// помечается движком (telemt — mtproxyl, mtdd — mtproxyl_mtdd).
func reportedNodeType(cfg *config.NodeConfig) string {
	nt := detectNodeType()
	if nt == NodeTypeMTProxyL && cfg != nil && isMtproxylMtddConfig(cfg.Telemt.ConfigPath) && !nodeTypeLegacyOnly.Load() {
		return NodeTypeMTProxyLMtdd
	}
	return nt
}

// detectNodeType — тип установленного менеджера. os.Stat дёшев, дёргается
// на каждом /register, так что смена менеджера заметна без рестарта агента.
func detectNodeType() string {
	if fi, err := os.Stat(mekoInstallDir); err == nil && fi.IsDir() {
		return NodeTypeMEKO
	}
	if fi, err := os.Stat(mtproxylInstallDir); err == nil && fi.IsDir() {
		return NodeTypeMTProxyL
	}
	if fi, err := os.Stat(mtddConfigPath); err == nil && fi.Mode().IsRegular() {
		return NodeTypeMTDD
	}
	return NodeTypeClassic
}

// nodeTypeLabel — человекочитаемая форма для логов (панель рисует свою).
func nodeTypeLabel(t string) string {
	switch strings.ToLower(t) {
	case NodeTypeMTProxyL:
		return "MTProxyL"
	case NodeTypeMTProxyLMtdd:
		return "MTProxyL (mtdd)"
	case NodeTypeMEKO:
		return "MEKO"
	case NodeTypeMTDD:
		return "mtdd"
	default:
		return "Classic"
	}
}
