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
)

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
	case NodeTypeMEKO:
		return "MEKO"
	case NodeTypeMTDD:
		return "mtdd"
	default:
		return "Classic"
	}
}
