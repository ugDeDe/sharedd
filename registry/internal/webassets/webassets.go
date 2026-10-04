// Package webassets — сгенерированные страницы UI (tools/uigen) и шрифты,
// вшитые в бинарник. Отдельный пакет, потому что //go:embed разрешает пути
// только внутри каталога пакета: страницы физически живут здесь рядом
// с этим файлом, а не в корне модуля.
//
// Шрифт вшит, а не подключается из CDN: панель обязана открываться в
// закрытом контуре, без внешних запросов (и без утечки факта захода
// в чужую аналитику).
//
// Inter, SIL Open Font License 1.1 — https://github.com/rsms/inter
// Два сабсета вариативного начертания (400..700): латиница и кириллица;
// браузер тянет только нужный по unicode-range.
package webassets

import "embed"

//go:embed panel.html
var PanelHTML []byte

//go:embed stats.html
var StatsHTML []byte

//go:embed dashboard.html
var DashboardHTML []byte

//go:embed links.html
var LinksHTML []byte

//go:embed fonts/inter-latin.woff2 fonts/inter-cyrillic.woff2
var Fonts embed.FS
