// uigen — сборщик веб-интерфейса sharedd.
//
// Исходники интерфейса лежат в ui/, готовые страницы — в registry/*.html.
// Собранные страницы КОММИТЯТСЯ в репозиторий, поэтому сборка бинарника
// остаётся такой же, как была:
//
//	go build ./registry            # достаточно, uigen не нужен
//
// Запускать uigen надо только если правил ui/:
//
//	bash scripts/build_ui.sh          # пересобрать страницы
//	bash scripts/build_ui.sh -check   # проверить, что страницы актуальны (CI)
//	bash scripts/build_ui.sh -min     # то же, но со сжатием
//
// Или напрямую: cd tools/uigen && go run . — корень репозитория
// определяется автоматически (ищутся каталоги ui/ и registry/).
//
// Node.js не требуется: esbuild подключён как Go-библиотека.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Каждая страница — каталог в ui/pages: index.html (каркас), page.css и
// main.ts (точки входа). Имя каталога = имя файла в registry/.
const (
	pagesDir  = "ui/pages"
	outDir    = "registry"
	devOutDir = "dev/preview" // служебные страницы, вне репозитория
	cssMarker = "<!--{css}-->"
	jsMarker  = "<!--{js}-->"
)

var (
	minify = flag.Bool("min", false, "сжимать вывод (по умолчанию — читаемый)")
	check  = flag.Bool("check", false, "не писать файлы, а проверить, что они актуальны")
	root   = flag.String("root", "", "корень репозитория (по умолчанию — на уровень выше tools/)")
)

func main() {
	flag.Parse()
	base, err := repoRoot(*root)
	if err != nil {
		die(err)
	}

	pages, err := listPages(filepath.Join(base, pagesDir))
	if err != nil {
		die(err)
	}
	if len(pages) == 0 {
		die(fmt.Errorf("в %s нет ни одной страницы", pagesDir))
	}

	stale := []string{}
	for _, name := range pages {
		html, err := buildPage(base, name)
		if err != nil {
			die(fmt.Errorf("%s: %w", name, err))
		}
		// Страницы с именем на «_» — служебные (витрина компонентов и т.п.):
		// они собираются для стенда и в бинарник регистратора не попадают.
		dst := filepath.Join(base, outDir, name+".html")
		if strings.HasPrefix(name, "_") {
			dst = filepath.Join(base, devOutDir, strings.TrimPrefix(name, "_")+".html")
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				die(err)
			}
		}
		old, _ := os.ReadFile(dst)
		if string(old) == html {
			fmt.Printf("  без изменений  %s.html\n", name)
			continue
		}
		if *check {
			stale = append(stale, name+".html")
			continue
		}
		if err := os.WriteFile(dst, []byte(html), 0o644); err != nil {
			die(err)
		}
		fmt.Printf("  собрано        %s.html  (%d КБ)\n", name, len(html)/1024)
	}

	if *check && len(stale) > 0 {
		fmt.Fprintf(os.Stderr,
			"\nстраницы устарели: %s\nисходники в ui/ изменились — выполните: go run ./tools/uigen\n",
			strings.Join(stale, ", "))
		os.Exit(1)
	}
}

// buildPage собирает одну страницу: CSS и TS прогоняются через esbuild и
// вставляются в каркас index.html вместо маркеров.
func buildPage(base, name string) (string, error) {
	dir := filepath.Join(base, pagesDir, name)

	shell, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		return "", err
	}
	if !strings.Contains(string(shell), cssMarker) || !strings.Contains(string(shell), jsMarker) {
		return "", fmt.Errorf("в index.html нет маркеров %s / %s", cssMarker, jsMarker)
	}

	css, err := bundle(filepath.Join(dir, "page.css"), base)
	if err != nil {
		return "", err
	}
	js, err := bundle(filepath.Join(dir, "main.ts"), base)
	if err != nil {
		return "", err
	}

	out := strings.Replace(string(shell), cssMarker, indent(css, "  "), 1)
	out = strings.Replace(out, jsMarker, js, 1)
	return banner(name) + out, nil
}

// bundle прогоняет точку входа через esbuild. Charset обязателен: без него
// кириллица уезжает в \u-последовательности и файл нечитаем.
func bundle(entry, base string) (string, error) {
	res := api.Build(api.BuildOptions{
		EntryPoints:   []string{entry},
		AbsWorkingDir: base,
		Bundle:        true,
		Write:         false,
		Target:        api.ES2019,
		Format:        api.FormatIIFE,
		Charset:       api.CharsetUTF8,
		// Шрифт отдаёт сам регистратор по /assets/... — esbuild не должен
		// пытаться найти этот файл на диске и «зашить» его в CSS.
		External:          []string{"/assets/*"},
		LogLevel:          api.LogLevelSilent,
		MinifyWhitespace:  *minify,
		MinifyIdentifiers: *minify,
		MinifySyntax:      *minify,
	})
	if len(res.Errors) > 0 {
		msgs := api.FormatMessages(res.Errors, api.FormatMessagesOptions{Kind: api.ErrorMessage, Color: false})
		return "", fmt.Errorf("esbuild:\n%s", strings.Join(msgs, ""))
	}
	if len(res.OutputFiles) == 0 {
		return "", fmt.Errorf("esbuild ничего не вернул для %s", entry)
	}
	return strings.TrimRight(string(res.OutputFiles[0].Contents), "\n"), nil
}

func banner(name string) string {
	return "<!-- ВНИМАНИЕ: файл собран автоматически из ui/pages/" + name + "/.\n" +
		"     Правки здесь будут потеряны при следующей сборке.\n" +
		"     Исходники: ui/  ·  пересборка: bash scripts/build_ui.sh -->\n"
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

func listPages(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "index.html")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// repoRoot ищет корень: либо задан флагом, либо поднимаемся вверх до каталога,
// в котором есть и ui/, и registry/.
func repoRoot(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; {
		_, e1 := os.Stat(filepath.Join(dir, "ui"))
		_, e2 := os.Stat(filepath.Join(dir, "registry"))
		if e1 == nil && e2 == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("не найден корень репозитория (нужны каталоги ui/ и registry/)")
		}
		dir = parent
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "uigen:", err)
	os.Exit(1)
}
