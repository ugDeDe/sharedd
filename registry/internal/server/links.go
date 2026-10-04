package server

import (
	"net/http"
)

// Публичная страница прокси-ссылок — /links.
//
// Существующий /proxylinks остаётся без изменений: он как отдавал JSON, так
// и отдаёт (на него могут быть завязаны чужие скрипты и боты-раздатчики).
// Страница — отдельный маршрут, который тянет тот же JSON с клиента и
// показывает его человеку: домен, кнопка «копировать», QR для телефона.

// mountLinks — GET /links (и /links/): HTML поверх /proxylinks.
// Открыт без токена, как /statistics и /dashboard: ссылки и так
// предназначены для раздачи конечным пользователям.
func (r *Registry) mountLinks(mux *http.ServeMux) {
	serve := func(w http.ResponseWriter, req *http.Request) {
		setPublicHTMLHeaders(w)
		_, _ = w.Write(linksHTML)
	}
	mux.HandleFunc("GET /links", serve)
	mux.HandleFunc("GET /links/", serve)
}
