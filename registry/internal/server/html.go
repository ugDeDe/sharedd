package server

import "sharedd/registry/internal/webassets"

// Алиасы вшитых страниц: исторические имена в нижнем регистре используются
// по всему пакету и в тестах; сами файлы лежат в webassets (см. комментарий
// там про ограничения //go:embed).
var (
	panelHTML     = webassets.PanelHTML
	statsHTML     = webassets.StatsHTML
	dashboardHTML = webassets.DashboardHTML
	linksHTML     = webassets.LinksHTML
)
