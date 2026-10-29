package server

// HTML-оболочки кабинета пользователя (/signin, /cabinet). Отдаются как есть:
// аутентификация — на стороне /api/* (cookie-сессия), страницы лишь редиректят
// неавторизованных в /signin на клиенте. Источники — ui/pages/{signin,cabinet}/.

import "net/http"

func (r *Registry) mountCabinetPages(mux *http.ServeMux) {
	serve := func(html []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			setPublicHTMLHeaders(w)
			_, _ = w.Write(html)
		}
	}
	mux.HandleFunc("GET /signin", serve(signinHTML))
	mux.HandleFunc("GET /cabinet", serve(cabinetHTML))
}
