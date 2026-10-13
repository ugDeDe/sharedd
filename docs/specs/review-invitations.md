# review-invitations.md — независимый security-review

Ветка `dev`. Read-only ревью (код не менялся, коммитов нет). Область:
invitation-only registration, per-node secrets, «Разрешить общий секрет»,
матрица machine-auth, конфиг-миграция установщика. Источники — контракт
`docs/specs/invitations-migration-contract.md` (авторитетный) + код.

## Вердикт

| Слой | Итог |
|---|---|
| registry auth/toggle/invites/node_secret | **PASS** — матрица, атомарность, хеширование, durability корректны |
| node agent back-compat | **PASS (с оговоркой)** — см. F2 |
| installer config migration | **FAIL (edge-case)** — см. F1 |
| docs accuracy | **FAIL (частично)** — F2/F3 overclaim |
| bash -n / parity | **OK**; shellcheck здесь не установлен — не подтверждено независимо |

**Production deployment: CONDITIONAL NO-GO.** Код registry/node — production-ready.
Блокирует: **F1** (миграция установщика может создать невалидный TOML на валидном
входе). При деплое через установщик — сначала фикс F1 или mitigation. Переключатель
OFF не включать, пока `affected_nodes != 0` (см. F2).

Проверено в этой сессии:
`bash -n` × 3 скрипта — OK; `check_installer_parity.sh` — `parity ok` ×4;
`add_registry_key` в `install_node.sh`/`install_node_web.sh` — byte-identical (`diff`);
`gofmt -l` пусто; `go vet`/`go build -mod=readonly` — OK (registry+node);
`go test -run 'TestInvite|TestLegacy|TestAffectedNodes|TestInvitesAdmin|TestNodeSecret|TestRegister'`
— ok (control, server). Не запускались: полный `go test -race`, `golangci-lint`,
`build_ui.sh -check`, `shellcheck` (не установлен в этой среде).

---

## F1 — HIGH — Миграция установщика создаёт дублирующую TOML-таблицу на валидном входе

`scripts/install_registry.sh:120` (детект секции), вспомогательно `:75`, `:123`.

Причина: детектор заголовка секции — `body ~ /^[[:space:]]*\[[^]]+\][[:space:]]*$/`.
Он строже TOML: не принимает **inline-комментарий после заголовка** и **пробелы
внутри скобок** — обе формы валидны по TOML v1.0. Такой заголовок не помечается
`seen[cur]` и не становится `cur`, поэтому в `END` для него срабатывает
`emit_new()` и в файл добавляется ВТОРАЯ таблица того же имени → TOML невалиден
(«Cannot declare … twice»), сервис не стартует.

Наблюдаемое (реальные прогоны `bash install_registry.sh --migrate-config`, вход — валидный TOML):

- Вход `[users] # main users` + `legacy_max_nodes = 500` →
  dry-run добавляет новый блок `[users] enabled=true legacy_max_nodes=100000 …`.
  Явное `500` не сохраняется, таблица дублируется.
- Вход `[ users ]` + `enabled = false` → добавляется дублирующая `[users]`
  (`enabled = true`), тогда как явный `false` должен был сохраниться.

Проверка валидности (python3 tomllib): `[ users ]` — VALID, `[users] # c` — VALID,
получившийся дубль `[users]` — **INVALID**. Это falsify утверждения контракта
§4 «не может повредить реальный registry.toml» и таблицы верификации
implementation-summary «TOML-валидность … все варианты».

Воспроизведение:
```
printf '[users] # main\nlegacy_max_nodes = 500\n' > t.toml
bash scripts/install_registry.sh --migrate-config t.toml   # diff показывает новый [users]
```

Фикс (awk, тот же проход): нормализовать заголовок как TOML — снять inline-комментарий
и пробелы внутри скобок, затем уже сравнивать с целевыми именами:
```
function sectname(s,   t) {
    t = s
    sub(/^[[:space:]]*\[/, "", t); sub(/\].*$/, "", t)      # до ']', далее отбросить (в т.ч. '# ...')
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", t)               # trim внутренних пробелов
    return t
}
# детектор: body ~ /^[[:space:]]*\[/ { cur = sectname(body) ... }
```
Аналогично нормализовать детект `[state]` на `:75` (иначе inline-комментарий у
`[state]` молча уводит пути к STATE_DIR-дефолту). Добавить в тест-матрицу
установщика варианты `[users] # …` и `[ users ]`.

Замечание: затронуты только конфиги с рукописными «валидными, но нетипичными»
заголовками; installer-generated `registry.toml` их не содержит — отсюда HIGH, а не
CRITICAL. Есть бэкап `.bak-<ts>`, откат возможен, но сервис на новом конфиге не
поднимется до ручного восстановления.

## F2 — MEDIUM — Контракт overclaim: старые агенты НЕ «восстанавливаются перерегистрацией» после OFF

`docs/specs/invitations-migration-contract.md:32-35` vs
`registry/internal/server/api_auth.go:131-136`.

Контракт: «при OFF агент получает 401 на heartbeat → пере-регистрируется → получает
node_secret → работает». Это **неверно для агента без authkey/node_secret**:
`gateMachineAuth` на OFF при shared Bearer пропускает только `registerAuthKey != ""`,
иначе 401 `shared_token_disabled` (api_auth.go:131-136). Старый агент:
- не парсит `node_secret` из тела 200 (main.go:222-228 — только новый агент);
- не шлёт `authkey` (поле добавлено в этой работе);
→ его `/register` на OFF тоже 401, самолечения нет. Аналогично ломается **новый**
агент, впервые стартовавший уже при OFF без authkey.

Фактическая правда: ON — старые агенты работают (verified тестом
`TestLegacyOnSharedTokenWorksEverywhere`); после OFF работают только ноды, успевшие
получить `node_secret` (при ON хотя бы раз перерегистрировавшиеся новым агентом) или
имеющие authkey. `affected_nodes` (invites_admin_api.go:166) — правильный инструмент
предпроверки, но не доказательство самолечения.

Mitigation (без кода): перед OFF убедиться, что `affected_nodes == 0`; операторам
раздать authkey/`--node-secret` неперерегистрировавшимся нодам. Это **не** дефект
безопасности — fail-closed/анти-регресс контракта соблюдены.

Fix (docs): переписать §1: «после OFF нода без персонального секрета/authkey теряет
доступ до получения оператором authkey/секрета; автоматического самолечения по
heartbeat нет».

## F3 — LOW — implementation-summary overclaim по TOML-валидности

`docs/specs/implementation-summary-invitations.md:167` («python3 tomllib парсит все
варианты») и :211 «бэкенд-контракт готов». F1 показывает непокрытые формы.
Скорректировать формулировку до проверенного набора либо добавить тесты из F1.

## F4 — LOW — Нет `Cache-Control: no-store` на теле 200 `/register` (node_secret)

`registry/internal/server/run_main_impl.go:302-304`: 200 с `{"node_secret":…}`
не выставляет `Cache-Control` (в отличие от `/config`, :453). POST-ответы обычно не
кэшируются, риск низкий. Фикс: `w.Header().Set("Cache-Control","no-store")` перед
`WriteHeader`.

## F5 — LOW — Несогласованный формат ошибок авторизации

`registry/internal/server/users_admin_api.go:70-71` (`adminUsersGuard`) отдаёт
`http.Error` (plain text), остальные — JSON `{"error":…}`. Клиент, разбирающий
`error`-код, для 401 не получит JSON. Фикс: `controlAPIError(w, 401, "unauthorized", …)`.

## F6 — LOW — `invites.created_by` всегда 0

`registry/internal/control/invites.go:47` — `created_by` хардкод `0`. Колонка
заведена для аудита автора, но панель ходит под единым `panel.token` (нет per-admin
identity), так что атрибуции нет by-design. Не security. Отметить как known gap.

## F7 — LOW — shellcheck-claim не подтверждён независимо

`shellcheck` в среде ревью не установлен (`bash -n` и parity — OK). Заявление
implementation-summary:159 (0 замечаний, v0.10.0) не воспроизведено здесь; выполнить
в CI. Не блокер при наличии CI-гейта.

---

## Фальсификация по пунктам задания

**(a) OFF блокирует общий токен на всех 5 маршрутах, без обхода — PASS.**
Все 5 machine-маршрутов (`/register` :225, `/heartbeat` :307, `/report` :360→healthcheck.go:55,
`/retire` :368, `/config` :417) идут через `requireNodeAuth` и вызывают `gateMachineAuth`.
Других node-facing маршрутов в mux нет (проверен полный список `mux.Handle`).
OFF+shared → 401 `shared_token_disabled`, кроме `/register` с непустым authkey
(валидируется `authorizeUserRegister`). `node_secret` чужой ноды → 403 `node_id_mismatch`
(`/config` берёт id из `X-ShareDD-Node-ID`, пустой заголовок → mismatch). ON — прежнее
поведение. Тестом `TestLegacyOffAuthMatrix`/`TestLegacyOnSharedTokenWorksEverywhere` — green.
Пробел в matrix-логике не найден.

**(b) Приглашения — PASS.** `CreateUserWithInvite` (invites.go:104-149): валидация полей
ДО транзакции, INSERT user + условный UPDATE invite в одной транзакции; `used_by IS NULL
AND revoked_at IS NULL AND expires_at > ?` под `SetMaxOpenConns(1)`; `RowsAffected==0` →
`classifyInviteTx` (used/revoked/expired/invalid). Ошибка валидации/дубль username →
rollback, invite цел (`TestInviteDuplicateUsernameRollsBack`). Хранится только sha256 +
префикс; список отдаёт `prefix`, полный токен — только в `invite_url` 201. В логах не
встречается (grep log.*secret — чисто).

**(c) node_secret — PASS.** Хранится sha256+префикс (nodes_secret.go:18-34); полный
секрет — только `registerResponse` (run_main_impl.go:514). Перевыдача на каждый `/register`
(`SetNodeSecret` затирает старый хеш — инвалидация). Ни один endpoint не отдаёт hash/prefix
(grep). Утечки в webassets нет (в bundles только шаблон `invite_url` рядом с формой).

**(d) Toggle durability — PASS.** `settings` — источник истины; bootstrap из
`[users] allow_shared_secret` (nil=true) при первом чтении (settings.go:18-32);
`SetLegacySecret` — upsert (частичного успеха нет); POST пишет БД, затем зеркало
`atomic.Bool`; ошибка записи → 500, память не трогается (invites_admin_api.go:155-161).
Рестарт перечитывает (`TestLegacySecretSurvivesRestart`). `persisted:true` только после
успешного upsert. False-persisted не найден.

**(e) Migration — FAIL (F1).** Только missing keys, бэкап `cp -a`, retention 3,
идемпотентно, CRLF и комментарии-ключи обработаны верно (наблюдал C3/C4). Дефект —
TOML-нетипичные, но валидные заголовки `[users] # …` / `[ users ]` → дубль таблицы.
`awk`-край-кейсы «ключ в чужой секции» — корректны (`cur in kn`). Явные значения
(enabled=false/500/4) сохраняются на поддерживаемых формах.

**(f) Утечки в UI/логи — PASS.** `grep node_secret registry/internal/webassets/*.html` —
только `invite_url`/endpoint-имена; сырых секретов нет.

**(g) Agent back-compat — PASS с оговоркой (F2).** Новый агент терпит старый реестр:
пустое/`{}` тело 200 → `json.Unmarshal` err/пусто → секрет не сохраняется, остаётся
общий токен (main.go:222-228, тест `TestRegisterStoresIssuedSecret`). Старый агент терпит
новый реестр: тело 200 не парсит. «Старые работают при ON» — подтверждено. «Восстанавливаются
после OFF перерегистрацией» — **опровергнуто** для нод без authkey/node_secret.

**(h) Docs overclaim — есть (F2, F3).**

---

## Пред-деплойные проверки (точные)

1. Закрыть F1: либо фикс нормализации заголовков, либо подтвердить, что целевой
   `registry.toml` не содержит inline-комментариев/пробелов в `[…]`.
   Главы с `#` или внутренними пробелами: `grep -nE '^[[:space:]]*\[.*[[:space:]]|^[[:space:]]*\[.*#' registry.toml`
   (пусто = безопасно). Плюс валидность после миграции в dry-run:
   `python3 -c "import tomllib;tomllib.load(open('registry.toml','rb'))"`.
2. Бэкапы ДО замены (контракт §6): `registry.toml`, `registry_state.json`,
   `registry.db`, `master.key` (+ права; `.bak-<ts>` создаётся миграцией до старта).
3. `affected_nodes`: `curl -H "Authorization: Bearer $PANEL_TOKEN" …/panel/api/admin/legacy-secret`
   → `enabled=true`, `affected_nodes==0` перед любым OFF.
4. Полный гейт: `cd registry && gofmt -l . && go vet ./... && go test -race ./... && go build -mod=readonly ./... && golangci-lint run ./...`;
   повторить в `node/`; `bash scripts/build_ui.sh -check`;
   `bash -n scripts/*.sh && shellcheck -S warning scripts/*.sh && bash scripts/check_installer_parity.sh`.
5. Beget/VPS-создание — фича выключена по факту (раннер не подключён); кабинет не должен
   обещать успех.

## Known gaps (не блокеры)

- `created_by` без атрибуции (F6).
- Installer node-флаги `--authkey/--node-secret` не встраиваются в команду подключения панели
  (implementation-summary — открытый пункт).
- `[users] enabled=false` → переключатель/OFF/node_secret неприменимы by design (deviation §5).
