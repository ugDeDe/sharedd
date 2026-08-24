package server

import (
	"net/http"
	"strings"

	"sharedd/registry/internal/webassets"
)

// mountAssets — GET /assets/<файл>.woff2. Содержимое неизменно и лежит
// в бинарнике (см. webassets: шрифт вшит ради закрытого контура), поэтому
// кэшируется навсегда: смена шрифта = смена имени файла.
func mountAssets(mux *http.ServeMux) {
	mux.HandleFunc("GET /assets/{name}", func(w http.ResponseWriter, req *http.Request) {
		name := req.PathValue("name")
		// PathValue одного сегмента не содержит «/», плюс белый список
		// расширений — обхода по каталогам тут быть не может.
		if !strings.HasSuffix(name, ".woff2") {
			http.NotFound(w, req)
			return
		}
		data, err := webassets.Fonts.ReadFile("fonts/" + name)
		if err != nil {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		_, _ = w.Write(data)
	})
}
