// Команда sharedd-registry — единая точка решений пула: приём нод, здоровье,
// выбор мастеров, Cloudflare DNS, панель и публичные страницы.
//
// Вся реализация живёт в internal/server; здесь только разбор --version
// и вызов server.Run (см. комментарий в run_main_impl.go).
package main

import (
	"fmt"
	"os"

	"sharedd/registry/internal/server"
)

func main() {
	for _, arg := range os.Args[1:] {
		if arg == "-version" || arg == "--version" {
			fmt.Println("sharedd-registry")
			return
		}
	}
	server.Run()
}
