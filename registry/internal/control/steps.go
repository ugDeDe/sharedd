package control

// Профиль установки — упорядоченный список shell-команд (формат mtplane):
// строка-заголовок `# Название` открывает шаг, последующие непустые строки —
// его команда (многострочная команда сохраняется как есть). Строка `#без
// пробела` — shell-комментарий внутри команды, не заголовок.
//
// Подстановка переменных — Go text/template. КАЖДОЕ значение переменной
// shell-квотится РОВНО ОДИН РАЗ здесь, поэтому ни одно значение не может
// разорвать аргумент и выполнить произвольный код (M-4). Никакого YAML.

import (
	"bytes"
	"regexp"
	"strings"
	"text/template"
)

// ProfileStep — один шаг профиля.
type ProfileStep struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

// ParseProfileSteps разбирает текст профиля в шаги.
func ParseProfileSteps(raw string) []ProfileStep {
	var steps []ProfileStep
	var cur *ProfileStep
	var buf []string
	flush := func() {
		if cur == nil {
			return
		}
		cur.Command = strings.TrimSpace(strings.Join(buf, "\n"))
		if cur.Command != "" {
			steps = append(steps, *cur)
		}
		cur, buf = nil, nil
	}
	for _, ln := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "# ") {
			flush()
			cur = &ProfileStep{Name: strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))}
			continue
		}
		if trimmed == "" {
			continue
		}
		if cur == nil {
			cur = &ProfileStep{Name: "Шаг"}
		}
		buf = append(buf, ln)
	}
	flush()
	return steps
}

// ShellQuote оборачивает s в одинарные кавычки, экранируя вложенные кавычки,
// так что значение не может вырваться из аргумента.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// profileVarRe ловит ОБА документированных синтаксиса: `{{ var }}` (владелец
// проекта и UI) и `{{.var}}` (исторический Go-действие). Имя обязано быть
// идентификатором.
var profileVarRe = regexp.MustCompile(`\{\{\s*\.?\s*([a-zA-Z_]\w*)\s*\}\}`)

// templateControlWords — ключевые слова text/template: их нельзя перекрыть
// функцией, поэтому bare-форму `{{ kw }}` переводим в dot-доступ `{{.kw}}`,
// чтобы имя переменной, совпавшее с ключевым словом, тоже подставлялось.
var templateControlWords = map[string]bool{
	"if": true, "else": true, "end": true, "range": true,
	"with": true, "template": true, "define": true, "block": true,
}

var profileControlWordRe = regexp.MustCompile(`\{\{\s*(if|else|end|range|with|template|define|block)\s*\}\}`)

func normalizeBareControlWords(cmd string) string {
	return profileControlWordRe.ReplaceAllString(cmd, `{{.$1}}`)
}

// ProfileVarNames — имена переменных, встречающихся в тексте профиля (оба
// синтаксиса).
func ProfileVarNames(steps string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range profileVarRe.FindAllStringSubmatch(steps, -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	return out
}

// RenderStep подставляет vars в команду, поддерживая оба синтаксиса:
// `{{ var }}` (функция) и `{{.var}}` (карта). Каждое значение shell-квотится
// РОВНО ОДИН РАЗ — и функция, и карта возвращают уже заквоченную строку.
// Отсутствующие в vars переменные подставляются пустой строкой (”).
func RenderStep(cmd string, vars map[string]string) (string, error) {
	quoted := make(map[string]string, len(vars))
	for k, v := range vars {
		quoted[k] = ShellQuote(v)
	}
	// Всё, что шаблон упоминает, но не задано, получает пустое значение, чтобы
	// и функция, и карта вернули '' вместо "<no value>"/ошибки парсера.
	for _, name := range ProfileVarNames(cmd) {
		if _, ok := quoted[name]; !ok {
			quoted[name] = ShellQuote("")
		}
	}
	// Каждая переменная регистрируется и функцией (для `{{ var }}`), и через
	// карту-точку (для `{{.var}}`). Пользовательские функции перекрывают
	// встроенные text/template — имя вроде `printf`/`len` тоже подставится.
	funcs := template.FuncMap{}
	for name, val := range quoted {
		if !varNameRe.MatchString(name) || templateControlWords[name] {
			continue // некорректное имя / control-слово: работает через карту (dot)
		}
		value := val
		funcs[name] = func() string { return value }
	}
	t, err := template.New("step").Funcs(funcs).Option("missingkey=zero").Parse(normalizeBareControlWords(cmd))
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := t.Execute(&out, quoted); err != nil {
		return "", err
	}
	return out.String(), nil
}
