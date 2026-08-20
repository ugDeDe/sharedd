package main

import (
	"embed"
	"net/http"
	"strings"
)

// Статика интерфейса. Шрифт вшит в бинарник, а не подключается из CDN:
// панель обязана открываться в закрытом контуре, без внешних запросов
// (и без утечки факта захода в чужую аналитику).
//
// Inter, SIL Open Font License 1.1 — https://github.com/rsms/inter
// Два сабсета вариативного начертания (400..700): латиница и кириллица;
// браузер тянет только нужный по unicode-range.
//
//go:embed assets/inter-latin.woff2 assets/inter-cyrillic.woff2
var uiAssets embed.FS

// mountAssets — GET /assets/<файл>.woff2. Содержимое неизменно и лежит
// в бинарнике, поэтому кэшируется навсегда: смена шрифта = смена имени файла.
func mountAssets(mux *http.ServeMux) {
	mux.HandleFunc("GET /assets/{name}", func(w http.ResponseWriter, req *http.Request) {
		name := req.PathValue("name")
		// PathValue одного сегмента не содержит «/», плюс белый список
		// расширений — обхода по каталогам тут быть не может.
		if !strings.HasSuffix(name, ".woff2") {
			http.NotFound(w, req)
			return
		}
		data, err := uiAssets.ReadFile("assets/" + name)
		if err != nil {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(data)
	})
}
