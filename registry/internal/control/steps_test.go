package control

import (
	"strings"
	"testing"
)

// ParseProfileSteps: заголовки `# Имя`, многострочные команды, shell-комментарий
// (#без пробела) внутри команды, отсутствие заголовка → «Шаг».
func TestParseProfileSteps(t *testing.T) {
	raw := "# Первый\necho one\n\n# Второй\necho two \\\n  && echo three\n#not-a-header\necho four\n"
	steps := ParseProfileSteps(raw)
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2: %+v", len(steps), steps)
	}
	if steps[0].Name != "Первый" || !strings.Contains(steps[0].Command, "echo one") {
		t.Fatalf("step0 = %+v", steps[0])
	}
	if steps[1].Name != "Второй" {
		t.Fatalf("step1 name = %q", steps[1].Name)
	}
	// многострочная команда и shell-комментарий (# без пробела) — часть команды.
	if !strings.Contains(steps[1].Command, "echo three") || !strings.Contains(steps[1].Command, "#not-a-header") {
		t.Fatalf("multi-line/comment lost: %q", steps[1].Command)
	}
	// без заголовка — имя по умолчанию.
	anon := ParseProfileSteps("echo solo\n")
	if len(anon) != 1 || anon[0].Name != "Шаг" {
		t.Fatalf("anonymous step = %+v", anon)
	}
	// пустой заголовок без команды отбрасывается.
	if got := ParseProfileSteps("# только заголовок\n"); len(got) != 0 {
		t.Fatalf("header-only must yield no steps: %+v", got)
	}
}

// ShellQuote: одинарные кавычки не вырываются.
func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":            "''",
		"plain":       "'plain'",
		"a'b":         `'a'\''b'`,
		"$(id)":       `'$(id)'`,
		"`id`":        "'`id`'",
		"a\nb":        "'a\nb'",
		"a; rm -rf /": "'a; rm -rf /'",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Fatalf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// RenderStep: подстановка переменных с обязательным shell-квотированием —
// враждебные значения не могут выполнить свой код.
func TestRenderStepQuotesHostileValues(t *testing.T) {
	hostile := []string{"a b", "a'b", "$(touch /tmp/pwned)", "`id`", "x\ny", "; rm -rf /", "a\"b"}
	for _, v := range hostile {
		out, err := RenderStep("echo {{.v}}", map[string]string{"v": v})
		if err != nil {
			t.Fatalf("render %q: %v", v, err)
		}
		// значение заключено в одинарные кавычки; внутренняя кавычка удвоена
		// по схеме '\'' — то есть значение не может разорвать аргумент.
		if !strings.HasPrefix(out, "echo '") {
			t.Fatalf("value not single-quoted: %q", out)
		}
		if strings.Contains(out, "$(touch") && !strings.Contains(out, "'$(touch") {
			t.Fatalf("command substitution escaped its quotes: %q", out)
		}
	}
	// точная форма экранирования одинарной кавычки
	out, _ := RenderStep("echo {{.v}}", map[string]string{"v": "a'b"})
	if out != `echo 'a'\''b'` {
		t.Fatalf("quote escaping = %q", out)
	}
}

// RenderStep: неизвестная переменная подставляется пустой строкой (”), а не
// "<no value>".
func TestRenderStepMissingVar(t *testing.T) {
	out, err := RenderStep("echo {{.missing}} {{.present}}", map[string]string{"present": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<no value>") {
		t.Fatalf("missing var leaked template placeholder: %q", out)
	}
	if out != "echo '' 'x'" {
		t.Fatalf("render = %q", out)
	}
}

// RenderStep (H2): документированный синтаксис `{{ var }}` работает наравне с
// `{{.var}}`, включая смешивание в одной команде и значение, квотируемое ровно
// один раз в обоих.
func TestRenderStepBothSyntaxes(t *testing.T) {
	vars := map[string]string{"v": "a'b", "port": "443"}
	cases := map[string]string{
		"echo {{ v }}":                    `echo 'a'\''b'`,
		"echo {{.v}}":                     `echo 'a'\''b'`,
		"echo {{ v }} {{.v}}":             `echo 'a'\''b' 'a'\''b'`,
		"--port={{ port }} --p={{.port}}": "--port='443' --p='443'",
	}
	for in, want := range cases {
		got, err := RenderStep(in, vars)
		if err != nil {
			t.Fatalf("RenderStep(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("RenderStep(%q) = %q, want %q", in, got, want)
		}
	}
	// missing var в обоих синтаксисах → '', без <no value> и без ошибки парсера.
	got, err := RenderStep("echo {{ missing }} {{.missing}}", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo '' ''" {
		t.Fatalf("missing both syntaxes = %q", got)
	}
}

// RenderStep (H2): имя переменной, совпадающее со встроенной функцией
// text/template (`printf`), не ломает рендер — пользовательская функция
// перекрывает встроенную, и в обоих синтаксисах подставляется значение.
func TestRenderStepBuiltinNamedVar(t *testing.T) {
	got, err := RenderStep("echo {{ printf }} {{.printf}}", map[string]string{"printf": "hello"})
	if err != nil {
		t.Fatalf("builtin-named var: %v", err)
	}
	if got != "echo 'hello' 'hello'" {
		t.Fatalf("builtin-named var = %q", got)
	}
	// control-слово text/template нельзя перекрыть функцией — bare-форма идёт
	// через dot-доступ.
	got, err = RenderStep("echo {{ if }} {{.if}}", map[string]string{"if": "value"})
	if err != nil {
		t.Fatalf("control-word var: %v", err)
	}
	if got != "echo 'value' 'value'" {
		t.Fatalf("control-word var = %q", got)
	}
}

// RenderStep: встроенные и переопределяемые переменные подставляются.
func TestRenderStepBuiltinsAndDefaults(t *testing.T) {
	out, err := RenderStep("cmd --name {{.node_id}} --secret {{.node_secret}} --port {{.port}}", map[string]string{
		"node_id": "vpsabc-12345", "node_secret": "s3cr3t", "port": "8443",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "cmd --name 'vpsabc-12345' --secret 's3cr3t' --port '8443'"
	if out != want {
		t.Fatalf("render = %q, want %q", out, want)
	}
}

// H8/H3/M5: штатный профиль фиксирует предсозданный node_id (--name), проверяет
// реальный старт агента и НЕ несёт общий node_token.
func TestDefaultProfileStepsContract(t *testing.T) {
	steps := DefaultProfileSteps
	if !strings.Contains(steps, "--name={{ node_id }}") {
		t.Fatalf("default profile must pass --name with the node id:\n%s", steps)
	}
	if !strings.Contains(steps, "systemctl is-active --quiet sharedd-node-agent") {
		t.Fatalf("default profile must verify the agent came up:\n%s", steps)
	}
	// DEF-B: verify-шаг доказывает факт регистрации (а не только что юнит жив).
	if !strings.Contains(steps, "/api/node/status") {
		t.Fatalf("default profile must verify registration via /api/node/status:\n%s", steps)
	}
	if strings.Contains(steps, "node_token") {
		t.Fatalf("default profile must not reference the shared node_token:\n%s", steps)
	}
	// синтаксис `{{ var }}` — документированный (H2); переменные резолвятся.
	names := ProfileVarNames(steps)
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for _, want := range []string{"port", "node_id", "registry_url", "node_secret"} {
		if !seen[want] {
			t.Fatalf("default profile must reference %q, got %v", want, names)
		}
	}
}
