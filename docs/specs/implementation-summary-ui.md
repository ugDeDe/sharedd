# implementation-summary-ui.md — sharedd web UI (вход, кабинет, пользователи)

Ветка `dev`. Phase 2 (реализация `@frontend`). Источники: `AGENTS.md` (UI Generation),
`DESIGN.md`, `docs/specs/cabinet.md`, `docs/api-contract.md` §2/§2.1, авторитетный код
`registry/internal/server/cabinet_api.go`, `cabinet_beget.go`, `users_admin_api.go`.

## Страницы и файлы

| Страница | Файлы | Маршрут | Аутентификация |
|---|---|---|---|
| Вход/регистрация | `ui/pages/signin/{index.html,page.css,main.ts}` | `/signin` | cookie (ставит сервер) |
| Кабинет | `ui/pages/cabinet/{index.html,page.css,main.ts,api.ts}` | `/cabinet` | cookie `sharedd_session` |
| Пользователи (вид панели) | `ui/pages/panel/views/users.ts` (+ правки `index.html`, `main.ts`, `page.css`) | панель `#/users` | `Bearer panel.token` |

Общие (вынесены, чтобы не дублировать между страницами):
`ui/components/nodecard.css` (карточка-строка ноды, из `panel/page.css`),
`ui/components/modal.css` (модалка, из `links/page.css`).
Правка компонента: `ui/components/topbar.css` — `.mobile-nav` теперь
`grid-auto-flow: column` (панель получила 7-й пункт меню).

Сгенерированные (коммитятся): `registry/internal/webassets/{signin,cabinet}.html`,
обновлены `{panel,dashboard,links,stats}.html` (следствие правки `topbar.css`).

Минимальная правка Go (только монтирование страниц): `internal/webassets/webassets.go`
(+embed `signin.html`,`cabinet.html`), `internal/server/html.go` (алиасы),
`internal/server/cabinet_pages.go` (новый: `GET /signin`, `GET /cabinet` через
`setPublicHTMLHeaders`), `internal/server/run_main_impl.go` (+`mountCabinetPages`).

## Ключевые решения (сняты с кода, не из прозы)

- **CSRF**: cookie `sharedd_csrf` (не HttpOnly) + заголовок `X-CSRF-Token` на всех
  мутациях. Открытые вопросы DESIGN §11.2 закрыты кодом.
- **Self-rotate authkey** (addendum-manual-servers.md §1): раздел «Ключ доступа»
  показывает `{prefix,created_at,active}` и даёт «Выпустить/перевыпустить»
  (`POST /api/me/authkey/rotate`, полный ключ — один раз в `.secret-reveal` модалке)
  и «Отозвать» (`DELETE /api/me/authkey`). Копирование — с честным результатом
  (без ложного «Скопировано»); текстом сказано: один ключ на ВСЕ VPS, перевыпуск
  немедленно отзывает старый.
- **Формы ответов** (§2.1): `me={username,role,max_nodes,used_nodes}`;
  `nodes={nodes:[{node_id,ip,status,quarantine?,master_domains?,availability,dc2_ok}]}`
  (`status`: healthy|unhealthy|quarantined|deleting|offline); `authkey={prefix,created_at,active}`;
  `profiles={profiles:[{id,name,description,vars_schema?}]}`; `beget/link={verified,last_error?}`;
  `regions={regions:[{id,name,name_en,country,available}]}`; операция
  `{id,node_id?,kind,state,last_error?,created_at,updated_at}`.
- **`dc2_reason` в API нет** — DC2-чип выводится из `dc2_ok` (ok/bad/unknown), без выдуманных причин.
- **Каталог тарифов/цены API не отдаёт** — шаг «Тариф» визарда = необязательный
  `configuration_id` с честной подсказкой; выдуманных сумм нет (DESIGN §11.4/5).
- **`idem_key`** генерируется один раз на попытку (при входе в шаг «Подтверждение»),
  не меняется при ретрае → повтор не создаёт второй платный заказ.
- **Инвариант «отказ ≠ пересоздание»** — на `#/nodes`, `#/create`, `#/operations`
  (`.card-note`/`.banner`), кнопки «пересоздать» нет.
- **Секреты**: полный authkey — только в `.secret-reveal` модалке (админ rotate);
  пароль Beget очищается после отправки; SSH — только факт «зарегистрирован».

## API, к которым подключён UI

Кабинет (`api.ts`): `POST /api/auth/login|register|logout`, `GET /api/auth/me`,
`GET /api/me`, `GET|DELETE /api/me/nodes[/{id}]`, `GET /api/me/authkey`,
`POST /api/me/authkey/rotate`, `DELETE /api/me/authkey`,
`GET|POST /api/me/servers`, `POST /api/me/servers/{id}/install|check`,
`DELETE /api/me/servers/{id}`,
`GET /api/me/profiles`, `GET|PUT /api/me/beget/link`, `GET /api/me/beget/regions`,
`POST /api/me/vps`, `GET /api/me/operations`, `POST /api/me/operations/{id}/cancel`.

Панель (`views/users.ts`, `Bearer panel.token`): `GET|POST /panel/api/admin/users`,
`PATCH /panel/api/admin/users/{id}`, `POST .../{id}/authkey/rotate`,
`DELETE .../{id}/authkey`, `POST .../{id}/release-node`.

## Состояния

Каждая значимая область: loading (`.skeleton`/`:empty`), empty (`.empty-state`),
error (`.banner`/тост + «Повторить» где нужно), disabled (`.btn[disabled]`).
Кабинет автообновляется раз в 5 с, пауза при `document.hidden`, in-place без
сброса фокуса/скролла. Тосты — `#toast` с `aria-live="polite"`.

## Verification

- `bash scripts/build_ui.sh` — сборка успешна (esbuild bundle всех 6 страниц).
- `bash scripts/build_ui.sh -check` — exit 0 (страницы актуальны).
- `cd registry && gofmt -l internal/server internal/webassets` — пусто.
- `cd registry && go build ./... && go vet ./...` — без ошибок.
- `cd registry && go test -race ./...` — все пакеты ok.
- `grep console.log ui/` — нет.
- Typecheck-эквивалент: esbuild (uigen) собирает TS без ошибок; отдельного `tsc` в проекте нет.

## Fix round — Phase 2 (ответ на `docs/specs/design-qa-report.md`)

QA-вердикт был FAIL. Ниже каждый finding и что сделано.

| # | Finding | Статус | Где / как |
|---|---|---|---|
| **H1** | Модалка не удерживает фокус | Исправлено | `cabinet/main.ts:56-104`, `panel/views/users.ts:39-88`. `openModal` уводит фон в `inert` (все прямые дети `body` кроме `#modal`/`#toast`), `closeModal` снимает; единый `keydown`-слушатель ловит Tab/Shift+Tab и зацикливает по фокусируемым узлам `.modal-box`, Escape по-прежнему закрывает. |
| **M1** | Поллинг 5 с сбрасывал фокус в визарде | Исправлено | `cabinet/main.ts`: `refresh()` вызывает `wizard.render()` только если `document.activeElement` вне `#wiz-body, #wiz-foot`. Пока оператор печатает — перерисовки нет; по blur состояние догоняется. |
| **M2** | Дублирование слушателей в `refreshFoot`/`bindStep` | Исправлено | `cabinet/main.ts`: `bindStep()` разделён на `bindStepBody()` (вызывается один раз из `render()`) и `bindFoot()` (вызывается из `render()` **и** `refreshFoot()`). Слушатели «живучих» `[data-var]`/`wiz-confirm` больше не копятся. |
| **M3** | Кнопки мутаций не блокируются на время запроса | Исправлено | `users.ts`: `createUser`, `revokeKey`, `releaseNode`, `resetPassword` (и инлайн-`saveQuota`) на старте запроса делают `btn.disabled=true` + busy-подпись («Создание…», «Отзыв…», «Освобождение…», «Сброс…», «Сохранение…»), возвращают состояние в `.catch`. Модалка не закрывается до успеха. `toggleDisabled`/`rotateKey` закрыты follow-up-раундом ниже. |
| **M4** | `#toast` панели без `aria-live` | Исправлено | `panel/index.html:241` → `aria-live="polite"`. |
| **L1** | Правка квоты через модалку вместо инлайн-редактора | Исправлено | `users.ts`: `quotaCell` при `EDITING_QUOTA` рисует `input[type=number]` + «Сохранить»/«Отмена» (`PATCH`); функции `editQuota`/модалка удалены. Стили `.q-edit` — `panel/page.css`. |
| **L2** | Инлайновый `background:var(--acc-soft)` в таблице операций | Исправлено | `cabinet/main.ts`: строка-подсветка → класс `.row-highlight`; правило добавлено в `cabinet/page.css`. |
| **L3** | `h2` входа 16px вне шкалы | Исправлено | `signin/page.css`: переопределение `.auth-card .card-head h2 { font-size:16px }` удалено → размер из шкалы (13.5px, `base.css`). |
| **L4** | Мобильная подпись «Люди» вместо «Пользователи» | Исправлено | `panel/index.html`: span мобильной навигации → «Пользователи». |
| **L5** | Вкладки без roving tabindex / стрелок | Исправлено | `signin/index.html`: `tabindex="0"/"-1"`; `signin/main.ts`: `showTab` выставляет `tabIndex`, добавлен ArrowLeft/ArrowRight на `.tabrow` (APG). |
| **L6** | Статус-тумблер без понятного имени | Исправлено | `users.ts` `statusCell`: `aria-label="<Заблокировать|Разблокировать> пользователя <login>"` + `aria-pressed`. |
| **L7** | Собственный `.auth-alert` вместо `.banner` | Исправлено | `signin/index.html`: `div.banner.bad#auth-alert[role=alert]`; `main.ts` `alertBox` переключает `.bad`/`.info`; из `signin/page.css` удалён `.auth-alert`, добавлены только отступы `.auth-card .banner` и `[hidden]` (класс `.banner` иначе перебивает UA-правило). |
| **L8** | Конфликт спеки: «автоповтор»/«пересоздаёт» | Частично (код) | `cabinet/main.ts`: ops-note переведён на формулировку `DESIGN.md` §6.3 — «Повторный платный заказ запрещён». Фразу DC2 «отказ здоровья не пересоздаёт VPS» **не** менял: она дословно закреплена `DESIGN.md` §6.2. Остаётся док-конфликт `DESIGN §8 ↔ cabinet.md §2.4/§6.2` — на решение @leader (правку спецификаций не делал). |
| **L9** | Двойной бейдж карантина | Исправлено | `cabinet/main.ts` `nodeCardHTML`: отдельный `.badge.warn «карантин»` рисуется только если `status !== "quarantined"` (иначе дублирует статус-бейдж «карантин Globalping»). |

Проверка fix-round: `build_ui.sh` → собрано cabinet/panel/signin; `build_ui.sh -check` → exit 0;
`cd registry && gofmt -l .` пусто, `go vet ./...`, `go build -mod=readonly ./...`, `go test -race ./...` — ok.

Рендерер-браузерная a11y-проверка (контраст/фокус в рантайме) — `not_verified`: сервер не поднимался, рекомендован `/impeccable audit` + keyboard-pass.

## Follow-up fix round — busy-state `rotateKey`/`toggleDisabled`

Дизайн-QA отметила два оставшихся пропуска в busy-состоянии (`ui/pages/panel/views/users.ts`).

| # | Finding | Статус | Где / как |
|---|---|---|---|
| **F1** | `rotateKey` не блокирует кнопку-триггер на время запроса | Исправлено | `users.ts` `rotateKey(id, name, btn)`: `btn.disabled=true` + «Открытие…», восстановление в `.catch`; вызывающий пробрасывает кнопку (`data-rotate` → `as HTMLButtonElement`). |
| **F2** | `toggleDisabled` не блокирует кнопку-триггер на время запроса | Исправлено | `users.ts` `toggleDisabled(id, btn)`: `btn.disabled=true` + «…», восстановление в `.catch`; вызывающий пробрасывает кнопку (`data-toggle` → `as HTMLButtonElement`). |

Тот же паттерн, что в уже исправленных `createUser`/`saveQuota`/`revokeKey`/`releaseNode`/`resetPassword`.

Проверка follow-up: `build_ui.sh` → собрано panel; `build_ui.sh -check` → exit 0;
`cd registry && gofmt -l .` пусто, `go vet ./...`, `go build -mod=readonly ./...`, `go test -race ./...` — ok.

## Multi-master display — ссылки (домены) в публичной статистике и панели

Спека: `docs/specs/public-multi-master-view.md`. Бэкенд-поле `domains[]`
в `GET /statistics/api/list` уже живое (`implementation-summary-public-domains.md`).
Только фронт, ванильный TS + esbuild, без новых зависимостей/цветов.

### (1) Публичная `/statistics` — карточка «Ссылки (домены)»

`ui/pages/stats/main.ts` — `renderListView()` вставляет карточку **между**
баннером-инфо и «Ноды пула». Колонки: `Домен · Порт · Мастера · Заполнено
слотов`. Чипы мастеров — `.chips.link-masters > .chip.link-master`
(`.mono` node_id + `.badge.ok «жив»`/`.badge.warn «мёртв»` + TTL `.dim`),
плюс бейдж-счётчик «N мастеров» через `plural()`. Сборка чипов — сортировка
«живые → мёртвые → по `node_id`». Слоты — `.meter` (`fill .ok` при полном,
`.warn` при неполном), значение `slots_filled` строкой.

Деградация (всё в одном ответе, отдельных запросов нет):

| Ответ | Поведение |
|---|---|
| ключа `domains` нет | карточка не рендерится вообще (`Array.isArray(d.domains)`) |
| `domains: []` | карточка с `.empty-state` «Доменов-ссылок нет» |
| у домена нет `masters`/`[]` | ячейка «Мастера» — `—`, счётчик слотов остаётся |
| нет `port` | ячейка порта — `—` |
| нет `ip_masked` | IP-часть чипа отсутствует (без `?`), статус и node_id на месте |
| `ttl_remaining_sec ≤ 0` | `.dim` «переключение сейчас» вместо таймера |
| нет `ttl_remaining_sec` | таймер в чипе не рисуется |

Summary в `.hint`: `ссылок: N · мастеров: M · слоты: F/T` (F/T — сумма по
доменам; `… · слоты` опускается, если слот-строк нет).

`ui/pages/stats/page.css` — минимальная раскладка: `.domains-card`
(margin), `.link-masters` (чипы, курсор обычный — строка не кликабельна).

### (2) Панель — топология рисует все мастера домена

`ui/pages/panel/views/overview.ts`:
- `domainItemHTML` — внутри `.topo-b` строка `.chips.topo-masters` с чипом на
  каждый `m.masters[]` (`node_id` `.mono`, `.badge.warn «мёртв»` только для
  мёртвого слота, `.dim «с {fmtClock(since)}»`); карточка получает класс
  `.has-masters`. `title` чипа = `"{node_id} · {жив|мёртв} · с {время}"`.
- `slots_filled` всегда видим — `.badge.mono` у заголовка домена (спека
  допускает `.topo-r` **или** бейдж; выбран бейдж, чтобы не терять счётчик
  клиентов, который занимает `.topo-r`).
- Связь «домен → мастер» прежняя (первый `node_id`, координаты по
  `getBoundingClientRect`) — fan-out линий не рисуем.
  `// ponytail: один wire на домен → первый мастер; fan-out, если понадобится.`

`ui/pages/panel/page.css` — `.topo-item.has-masters`, `.topo-masters`,
`.topo-masters .chip` (существующие `.chips .chip` и `.badge`; новых
цветов/радиусов нет).

Метрика: «Мастера» → **«Домены с мастером»**, значение
`{liveDomains}/{domainsTotal}`, класс `.ok` при `liveDomains === domainsTotal
&& domainsTotal > 0`, иначе `.bad`. `liveDomains` — активные домены с ≥1
живым слотом (`m.masters.some(x => !x.dead)` либо легаси `node_id && !dead`).
Foot: `домены с ≥1 живым мастером` + `· свёрнуто СРМД: {folded}` при `folded>0`.

### Верификация

`bash scripts/build_ui.sh` → собрано `panel.html` (177 КБ), `stats.html`
(67 КБ); `bash scripts/build_ui.sh -check` → exit 0.
`cd registry && gofmt -l .` пусто, `go vet ./...`, `go build -mod=readonly ./...`,
`go test -race ./...` — ok (server 78 с). `console.log`/unused imports — нет.
Состояния loading/empty/error: loading — общий каркас страницы (`загрузка…`),
empty — `.empty-state`/`.table-empty`, error — существующий `.empty-state`
страницы (домены в том же ответе, что и ноды, поэтому отдельного error-баннера
карточки не существует — независимо упасть она не может).

**Файлы:** `ui/pages/stats/main.ts`, `ui/pages/stats/page.css`,
`ui/pages/panel/views/overview.ts`, `ui/pages/panel/page.css`,
`registry/internal/webassets/{panel,stats}.html` (regenerated).

## Раунд — приглашения + «Разрешить общий секрет» (invitations-legacy-ui.md)

Phase 2 реализации `docs/specs/invitations-legacy-ui.md` поверх живого бэкенда
(контракт `docs/specs/invitations-migration-contract.md` §2–3, фактические
ответы — `docs/specs/implementation-summary-invitations.md`).

### Файлы

| Файл | Изменение |
|---|---|
| `ui/pages/panel/index.html` | В `#/users` после карточки пользователей добавлены секции «Приглашения» (`#invites-card`: таблица 5 колонок, `#invites-stale`, `#invites-create`) и «Совместимость агентов» (`#legacy-card`: `.check` + бейдж состояния, `#legacy-load`/`#legacy-err`/`#legacy-warn`, постоянное пояснение в `.card-note`) |
| `ui/pages/panel/views/users.ts` | Приглашения: `loadInvites`/`renderInvites` (skeleton→empty/error/список, stale-пометка при неудачном обновлении), `createInvite` (модалка: срок 24/72/168/720ч, default 72; ошибка внутри модалки, ввод сохраняется), `showCreatedInvite` (одноразовое раскрытие `invite_url` через `fillModal` — точка возврата фокуса не перезаписывается; копирование с честным фолбэком; закрытие/Escape очищают DOM и замыкания), `revokeInvite` (подтверждение, «Отзыв…», статус после серверного подтверждения, объяснение used/expired). Общий секрет: `loadLegacy` (skeleton→контрол или error+«Повторить», OFF не подставляется), `applyLegacy` (бейдж «Включено/Выключено», warn-баннер `affected_nodes>0` с `as_of`, unknown-баннер при `null`), `saveLegacy` (persisted:false / 5xx / сеть → разные явные неудачи, перечитывание GET, авто-повтора нет), `confirmLegacyOff` (модалка «Отключить общий секрет?», «Отмена» в начальном фокусе, danger-кнопка → «Сохранение…») |
| `ui/pages/panel/page.css` | `.legacy-row`, `.legacy-check` (цель ≥24×24), `--fg` для важных баннеров секции |
| `ui/pages/signin/index.html` | Вкладка «Регистрация» → tabpanel-обёртка `#panel-register` с тремя состояниями: `#reg-gate` (без токена: «Регистрация только по приглашению…» + «Войти»), `#reg-invite-alert` (терминальные причины + «Войти»), `#reg-invite-note` + `#reg-form` (с токеном) |
| `ui/pages/signin/main.ts` | Токен из `?invite=` читается один раз, адресная строка сразу чистится (`replaceState`); `INVITE` живёт только в памяти. `renderRegister()` переключает gate/терминал/форму. POST `/api/auth/register` с `invite`; причины `invite_expired/used/revoked/invalid(/required)` → различимые `.banner.bad` + форма закрывается; 409/429/400/503/сеть — прежние тексты, приглашение не расходуется; успех — существующий поток + очистка invite-контекста |
| `ui/pages/signin/page.css` | Раскладка gate/терминального баннера внутри tabpanel, отступы баннеров, `--fg` для invite-алерта |
| `registry/internal/webassets/{panel,signin}.html` | regenered (`build_ui.sh`), не закоммичены |

### Что куда подключено

- `POST/GET/DELETE /panel/api/admin/invites`, `GET/POST /panel/api/admin/legacy-secret` — через существующий `panelReq` (Bearer `panel.token`).
- Список приглашений: `created_at`/`expires_at` — unix-секунды (int64) → абсолютные даты с таймзоной (`Intl.DateTimeFormat ru-RU + timeZoneName`); `expires_at` из 201 — RFC3339 → тот же форматтер.
- Статусы: active→`.badge.ok` «Активно», used→`.badge.info` «Использовано», expired/revoked→нейтральный `.badge`; действие «Отозвать» только у active.

### Верификация раунда

`bash scripts/build_ui.sh` → собраны `panel.html` (195 КБ), `signin.html` (42 КБ);
`bash scripts/build_ui.sh -check` → exit 0. `cd registry && gofmt -l .` пусто,
`go vet ./...`, `go build -mod=readonly ./...`, `go test -race ./...` — ok
(301 тестов, 13 пакетов). `console.log`/unused imports — нет. Незакоммиченная
работа других задач не тронута; коммит/пуш не выполнялись.

## Fix round — Phase 3 QA Round 3 (приглашения + «Разрешить общий секрет»)

Ответ на `docs/specs/design-qa-report.md`, раздел «Round 3» (вердикт FAIL).
Исправлены все blocker/medium + дешёвые low. Ветка `dev`, без коммита.

| # | Finding | Статус | Где / как |
|---|---|---|---|
| **H-INV1** | `expires_at` из 201 приходит unix-секундами, а фронт парсил как ISO → «21.01.1970» | ✅ Исправлено | `users.ts:420` тип поля → `expires_at: number`; `showCreatedInvite` (`:445,448`) рендерит `fmtDate(r.expires_at)` (тот же unix-секундный форматтер, что и список). `fmtISO` остаётся только для `as_of` (RFC3339). |
| **H-INV2** | `.banner`/`.btn`/`.legacy-row` задают `display` и перебивают UA `[hidden]` — 7 элементов всегда видимы | ✅ Исправлено | `panel/page.css` (конец файла): глобальное правило `[hidden] { display: none; }`. Специфичность (0,1,0) + порядок после `@import` перекрывают `.banner{display:flex}` (misc.css) и `.btn{display:inline-flex}` (form.css). Проверено в собранном `panel.html`: правило на offset 59740, после `.banner`(37730)/`.btn`(22888)/`.legacy-row`(59538). Возвращены корректные тумблеры `#inv-copy-err` (не виден на успехе), `#inv-create-err`, `.legacy-row`/`#legacy-err`/`#legacy-warn` (скрыты при загрузке/ошибке чтения), `#invites-stale`, `#invites-create`. |
| **M-INV1** | Список приглашений шёл в серверном ASC, «Новые сверху» (spec §3) не выполнялось | ✅ Исправлено | `users.ts:379`: `INVITES = [...(data.invites || [])].sort((a, b) => b.id - a.id)`. |
| **M-INV2** | Явное включение ON не показывало «Сохранение…» | ✅ Исправлено | `users.ts` `saveLegacy`: при старте (общий путь ON и OFF) `#legacy-state-badge` ← `<span class="badge">Сохранение…</span>`; `applyLegacy` восстанавливает состояние с сервера при успехе/перечитывании. |
| **M-INV3** | Модалка не ограничена viewport, тело не прокручивается | ✅ Исправлено | `ui/components/modal.css`: `.modal-box { max-height: calc(100vh - 40px); max-height: calc(100dvh - 40px); }`, `.modal-body { overflow-y:auto; min-height:0 }`, `h2`/`.modal-foot { flex:none }` — кнопки остаются доступны при 200% zoom / низком viewport. |
| **L-INV1** | Успех объявлялся `role="alert"` вместо polite status | ✅ Исправлено | `signin/main.ts` `alertBox`: `el.setAttribute("role", info ? "status" : "alert")` — ошибки alert, info-успех status. |
| **L-INV2** | Ошибка создания после Escape терялась (баннер отцеплен от DOM) | ✅ Исправлено | `users.ts` catch `createInvite`: если `#modal` без `.show` → `showToast(msg, true)`, иначе баннер в модалке; lost-response (`status === undefined`) по-прежнему обновляет список. |
| **L-INV3** | Lost-response регистрации не предлагал войти | ✅ Исправлено | `signin/main.ts:190`: «Нет связи с регистратором. Повторите позже. Если аккаунт уже создан — войдите.» |
| **L-INV4** | `persisted === false` — мёртвая проверка | ✅ Исправлено | `users.ts` `saveLegacy`: `if (r.persisted !== true)` — отсутствующее поле тоже считается ошибкой. |

Проверка fix-round: `bash scripts/build_ui.sh` → собраны cabinet/links/panel/signin;
`bash scripts/build_ui.sh -check` → exit 0; `cd registry && gofmt -l .` пусто,
`go vet ./...`, `go build -mod=readonly ./...`, `go test -race ./...` — ok
(301 тест, 13 пакетов). `console.log` в `ui/` — нет.

Файлы фикса: `ui/pages/panel/views/users.ts`, `ui/pages/panel/page.css`,
`ui/components/modal.css`, `ui/pages/signin/main.ts`,
`registry/internal/webassets/{panel,signin}.html` (regenerated).

## Риски / открытые пункты

1. `GET /api/me/beget/regions` теперь отдаёт и `configurations` (см. раунд addendum
   ниже); соответствие полей реальному ответу Beget нужно подтвердить на живом API
   (транспорт в тестах мокается).
2. `PATCH` пользователя принимает `password`, но отдельного ответа на сброс нет —
   UI просто перезагружает список.
3. Монтирование `/signin`,`/cabinet` сделано безусловно (как `mountCabinet`);
   за флагом `[users] enabled` следит API, страницы — только оболочка.

## Раунд — addendum: selection, приглашения N, профили, VPS-поток

Phase 2 реализации `docs/specs/addendum-profiles-vps.md` поверх живого бэкенда
(фактические ответы — `docs/specs/implementation-summary-addendum.md`,
контракт — `docs/api-contract.md` §8). Ванильный TS + esbuild (`tools/uigen`),
без новых зависимостей/цветов, русский операторский регистр, модалки через
существующий `openModal`, a11y-паттерны прежних раундов сохранены.

### Файлы

| Файл | Изменение |
|---|---|
| `ui/pages/panel/views/settings.ts` | Секция «Мастера на домен» (группа «DNS и пул»): `#s-sel-masters` (number 1..8), подсказка «НЕ `[srmd] max_nodes_per_domain`», предупреждение о перестройке A-записей (ссылки не меняются), инлайн-баннер `#s-sel-err`; load из `config.selection.masters_per_domain`; save → `PUT /panel/api/config {selection:{masters_per_domain}}`, 400 показывается inline |
| `ui/pages/panel/state.ts` | `saveSection`: на не-2xx ошибка теперь несёт человекочитаемый `message` сервера (+ `code`), а не только код — нужно для инлайн-валидации секции |
| `ui/pages/panel/views/users.ts` | Приглашения: `max_uses`/`used_count` в типе, статус `exhausted` (бейдж warn), колонка «N/M использовано», поле «Число регистраций» (1..1000, default 1) в модалке; отзыв различает `exhausted`. Экспортированы `panelReq/fillModal/openModal/closeModal` для переиспользования `profiles.ts` |
| `ui/pages/panel/views/profiles.ts` | Новый вид: список профилей (имя, описание, published, версий), create/edit/delete, отдельная кнопка publish/unpublish; редактор = textarea playbook + vars_schema JSON; ошибки песочницы/валидации — дословно inline; копирайт про append-only версии |
| `ui/pages/panel/main.ts` | Импорт/инициализация `profiles`, загрузка при `#/users` |
| `ui/pages/panel/index.html` | Таблица приглашений: колонка «Регистраций» (colspan 5→6), обновлён hint/note; секция `#profiles-card` |
| `ui/pages/cabinet/api.ts` | Типы `OwnProfile/MyProfiles/Configuration/Catalog`; `Operation.state` += `installing`,`done`; методы `myProfiles/createProfile/updateProfile/deleteProfile`; `regions()` → `{regions,configurations}` |
| `ui/pages/cabinet/index.html` | Навигация «Мои профили» (top + mobile), вид `#view-profiles` |
| `ui/pages/cabinet/main.ts` | Раздел «Мои профили» (list/create/edit/delete, `profile_limit_reached`/`user_profiles_disabled`); визард: шаг «Тариф» — список конфигураций с CPU/RAM/диск и ценой (`цена не указана` при null), шаг «Профиль» — свои+опубликованные (черновик недоступен, degraded), подтверждение с тарифом/ценой, результат операции installing/done/failed с обрезанным выводом и явным «повтор установки НЕ создаёт новый VPS»; `OP_META` += installing/done |
| `registry/internal/webassets/{panel,cabinet}.html` | regenerated (`build_ui.sh`), не закоммичены |

### Что куда подключено

- Панель (Bearer `panel.token`): `PUT /panel/api/config` (merge, `selection`),
  `GET|POST /panel/api/admin/profiles`, `PUT|DELETE /panel/api/admin/profiles/{id}`,
  `POST|GET|DELETE /panel/api/admin/invites` (`max_uses`).
- Кабинет (cookie + CSRF): `GET /api/me/profiles` (`{profiles,published,enabled,max}`),
  `POST /api/me/profiles`, `PUT|DELETE /api/me/profiles/{id}`,
  `GET /api/me/beget/regions` (`{regions,configurations}`), `POST /api/me/vps`.
- Результат VPS: `POST /api/me/vps` синхронно отдаёт операцию со `state`
  `done|failed|unknown|installing`; вывод прогона — `last_error` (обрезан сервером).
  Идемпотентность по `idem_key`: повтор того же ключа возвращает ту же операцию.

### Деградации (бэкенд не отдаёт — не выдумываем)

- **Число версий профиля**: `profileAdminJSON` не содержит счётчик версий —
  колонка «Версий» показывает «—», под карточкой пояснено.
- **Свой черновик-профиль в визарде**: `handleMeVps` требует **опубликованную**
  версию (`LatestPublishedVersion`), поэтому локальный `published=false` профиль
  показывается, но **не выбирается** (disabled + пояснение: доступен после
  публикации администратором). Опубликованные админские профили — выбираемы.
- **Пустой каталог тарифов**: если `configurations` пуст, шаг «Тариф» показывает
  info-баннер и позволяет продолжить без `configuration_id`.

### Верификация раунда

- `bash scripts/build_ui.sh` → собраны `panel.html`, `cabinet.html`.
- `bash scripts/build_ui.sh -check` → exit 0.
- `cd registry && gofmt -l .` — пусто; `go vet ./...`, `go build -mod=readonly ./...`,
  `go test -race -count=1 ./...` — все пакеты ok (control 56 с, server 90 с).
- `console.log` в изменённых UI-файлах — нет; unused imports — нет.
- Состояния: loading (`.skeleton`), empty (`.empty-state`/`.table-empty`),
  error (`.banner`/inline + «Повторить»), pending/busy (disabled-кнопки) —
  во всех новых областях.
- Секреты не раскрываются: playbook/vars_schema — не секреты; authkey/пароли
  не логируются и не отображаются.
- Не отдельный `tsc` (в проекте нет), typecheck-эквивалент — esbuild (uigen).
- Незакоммиченная работа других задач не тронута; коммит/пуш не выполнялись.

## Раунд — addendum §6/§7 UI: переменные профиля, versions, waiting_vps + retry-install, свои профили

Phase 2 реализации `docs/specs/addendum-profiles-vps.md` §6/§7 поверх живого
бэкенда (фактические ответы — `docs/specs/implementation-summary-addendum.md`
раздел «Догон», контракт — `docs/api-contract.md` §8). Ванильный TS + esbuild
(`tools/uigen`), без новых зависимостей/цветов, русский операторский регистр,
модалки/тосты/a11y-паттерны прежних раундов.

### Файлы

| Файл | Изменение |
|---|---|
| `ui/lib/vars.ts` | **новый** общий модуль: `ProfileVar`, `parseVarsSchema`/`serializeVarsSchema` (формат `{"vars":[{name,default,description,secret}]}`), `varNameHint`/`firstVarNameError` (клиентская подсказка: regex имени, `ansible_*`, `hosts`/`connection`/`delegate_to`/`local_action`/`remote_user`/`inventory`), `renderVarsEditor` (таблица name/default/description/секрет + add/remove + live-подсказка), `collectVarsEditor` |
| `ui/components/vars.css` | **новый** компонент раскладки `.vars-editor/.vars-rows/.vars-row/-x/.vars-hint` поверх `.field/.check/.btn` (без новых цветов); импортируется обеими страницами |
| `ui/pages/panel/views/profiles.ts` | Колонка «Версий» ← `versions`; текстареа JSON-схемы заменена таблицей переменных (`renderVarsEditor`), save собирает `vars_schema` через `serializeVarsSchema`, клиентская ошибка имени — в `#p-err`, серверный текст (`playbook_rejected`/`validation_failed`) — дословно |
| `ui/pages/panel/index.html`, `panel/page.css` | `@import` `vars.css`; card-note: версии теперь отдаются сервером, пояснение про Ansible-подстановку и маскирование секретов |
| `ui/pages/cabinet/api.ts` | `Operation.state` += `waiting_vps`; `OwnProfile.versions`; метод `retryInstall(id, body?)` → `POST /api/me/operations/{id}/retry-install` |
| `ui/pages/cabinet/main.ts` | Редактор «Мои профили» — таблица переменных (то же); card-список профилей показывает `версий: N`; визард: шаг «Переменные» строится из `vars_schema.vars` (дефолты предзаполнены, переопределение + «свои переменные» с add/remove), подсказка имён, подтверждение маскирует `secret`-значения как `•••`; шаг «Профиль» — свой неопубликованный профиль выбираем с бейджем «мой профиль» (disable убран); `OP_META` += `waiting_vps`; действия операций: `Повторить установку` для `failed`/`waiting_vps`; результат визарда обрабатывает `waiting_vps` + кнопка повтора |
| `ui/pages/cabinet/index.html`, `page.css` | `@import` `vars.css`; card-note профилей: локальный профиль выбираем владельцем |
| `registry/internal/webassets/{panel,cabinet}.html` | regenerated (`build_ui.sh`), не закоммичены |

### Что куда подключено

- **Переменные (§6)**: и админский, и пользовательский редактор пишут
  `vars_schema = {"vars":[{name,default,description,secret}]}` — точная форма
  `control.ProfileVar`/`ParseProfileVars`. Пустые строки не отправляются
  (сервер `omitempty`), нет строк → `vars_schema:""`.
- **Визард (§6)**: объявленные переменные предзаполнены `default`; пустое поле
  ⇒ сервер применит дефолт (`MergeProfileVars`); «свои переменные» уходят
  поверх. Всё — плоские строки в `vars` тела `POST /api/me/vps` (сервер сам
  инъектирует `registry_url`/`node_secret`).
- **Маскирование (§6)**: на шаге подтверждения значения объявленных
  `secret:true` показаны как `•••`; в редакторах — нативный `type=password` для
  секретных дефолтов визарда. Значения из вывода операции маскирует сервер
  (`MaskSecrets`), UI их не дублирует. *(Fix round Round 5: в редакторе профиля
  значение `secret:true` тоже `type=password` с «глазом»; на подтверждении к
  маскируемому набору добавлены конвенциональные ключи `node_secret`/`authkey`/
  `password` — как в `provision.secretValues`.)*
- **Версии (§7.5)**: `profileAdminJSON.versions` (admin-список и свои профили).
- **`waiting_vps` + retry-install (§7.2/§7.3)**: состояние —
  отдельный бейдж `.badge.warn` «ждём готовности VPS» (не терминальное),
  пояснение в `#ops-note` и баннере результата визарда: место в квоте занято,
  новый заказ невозможен. «Повторить установку» вызывается только из
  `failed`/`waiting_vps`; копирайт явно говорит, что повтор НЕ покупает новый
  VPS. Контроля второго платного заказа в UI нет. *(Fix round Round 5: повтор
  дополнительно гейтится `has_vps` из `operationJSON` — для операции без VPS
  кнопка не показывается; поле отсутствует у старого backend'а ⇒ прежнее
  поведение.)*
- **Свой профиль (§7.4)**: свой неопубликованный профиль выбираем, помечен
  «мой профиль» (title объясняет, что он виден только владельцу).

### Деградации (бэкенд не отдаёт — не выдумываем)

- ~~**`profile_id` операции не сериализуется**~~ — **устарело**: `operationJSON`
  теперь включает `profile_id` (`cabinet_beget.go`), повтор из раздела
  «Операции» работает без client-контекста (QA Round 5 L6).
- **`has_vps`** отсутствует у старого backend'а: повтор гейтится как раньше
  (показывается для `failed`/`waiting_vps`); при наличии поля `has_vps:false`
  кнопка повтора скрыта.

### Верификация раунда

- `bash scripts/build_ui.sh` → собраны `cabinet.html` (113 КБ), `panel.html`
  (216 КБ); `bash scripts/build_ui.sh -check` → exit 0.
- `cd registry && gofmt -l .` — пусто; `go vet ./...`, `go build -mod=readonly ./...`,
  `go test -race -count=1 ./...` — все пакеты ok (control 50 с, server 79 с,
  provision 18 с).
- `console.log` в `ui/` — нет; unused imports/функции — нет (`Schema`/`parseSchema`
  удалены вместе со старым JSON-Schema-редактором).
- Состояния: loading (`.skeleton`), empty (`.empty-state`/`vars-empty`),
  error (`.banner`/inline + «Повторить»), pending/busy (disabled-кнопки «Повтор…»)
  — во всех новых областях.
- Секреты: `secret:true` значения не печатаются в подтверждении (`•••`) и в
  списках; endpoint-секреты (authkey/пароли) не отображаются.
- Незакоммиченная работа других задач не тронута; коммит/пуш не выполнялись.

## Fix round — Phase 2 QA Round 5 (M1, L2, L4)

Ответ на findings Round 5 (`docs/specs/design-qa-report.md`). Только UI, ванильный
TS + esbuild (`tools/uigen`), без новых цветов/зависимостей.

### Файлы

| Файл | Изменение |
|---|---|
| `ui/pages/cabinet/api.ts` | `Operation.has_vps?: boolean` (добавлено backend'ом; отсутствие — старый backend) |
| `ui/pages/cabinet/main.ts` | **M1**: `actionCell` и `renderResult` показывают «Повторить установку» только при `has_vps !== false`; баннер «сервер уже создан» тоже под `has_vps`. **L4**: `varsSummaryHTML` маскирует `•••` и конвенциональные ключи `node_secret`/`authkey`/`password` |
| `ui/lib/vars.ts` | **L2**: секретный `default` рендерится `type=password` + «глаз» (`data-vreveal`, локальный делегированный обработчик); значение сохраняется дословно |
| `ui/components/vars.css` | Стиль `input[type=password]` в `.vars-row`; `.vars-row .input-group` (переиспользован `.input-group`/`.in-btn` из `form.css`) |
| `registry/internal/webassets/{cabinet,panel}.html` | regenerated (`build_ui.sh`), не закоммичены |

### Что куда подключено

- **M1 (retry только на VPS)**: backend сериализует `has_vps` (VPS создан).
  Повтор доступен для `failed`/`waiting_vps` **и** `has_vps !== false`; для
  VPS-less операции — только `—`. `has_vps` вне JSON (старый backend) ⇒ прежнее
  поведение. Второго платного заказа ни один контрол не создаёт — `retry-install`
  переиспользует VPS, `createVps` вызывается лишь из визарда.
- **L2 (секрет в редакторе)**: значение переменной с `secret:true` показывает
  `type=password`, переключатель «глаз» локально меняет `type`; значение
  читается/сериализуется без изменений.
- **L4 (маскирование на подтверждении)**: набор маскируемых = объявленные
  `secret:true` + `node_secret`/`authkey`/`password`, как `provision.secretValues`.

### Верификация раунда

- `bash scripts/build_ui.sh` → собраны `cabinet.html` (114 КБ), `panel.html`
  (217 КБ); `bash scripts/build_ui.sh -check` → exit 0, все страницы «без изменений».
- `cd registry && gofmt -l .` — пусто; `go vet ./...`, `go build -mod=readonly ./...`
  — OK; `go test -race -count=1 ./...` — все пакеты ok (control 47.6 с, server 84.2 с,
  provision 16.7 с).
- `console.log` в изменённых `ui/`-файлах — нет; unused imports — нет.
- Бандл подтверждает: `has_vps` в `cabinet.html`, `data-vreveal` и
  `input[type=password]`/`.vars-row .input-group` — в обоих бандлах.
- Незакоммиченная работа других задач не тронута; коммит/пуш не выполнялись.

## Раунд — UI fix: широкий редактор профиля

Редактор профиля (админ `profiles.ts`, кабинет `main.ts`) открывался в
модалке 400px — шаги и таблица переменных были тесными. Только раскладка,
без новых цветов, семантики и API не тронуты.

### Файлы

| Файл | Изменение |
|---|---|
| `ui/components/modal.css` | `.modal-box.modal-wide { max-width: min(1100px, 96vw) }`; `.profile-editor textarea` — `min-height:260px`, `line-height:1.5`, `white-space:pre` + `overflow:auto` (горизонтальная прокрутка), `resize:vertical` |
| `ui/components/vars.css` | `.vars-row` — третий столбец `minmax(140px,1.6fr)` (описание шире); входы `width:100%`; брейкпоинт 480px — одна колонка (стек) |
| `ui/pages/panel/views/profiles.ts` | Тело редактора → `class="field profile-editor"`; `<textarea wrap="off">`; `openModal(..., true)` — широкая модалка |
| `ui/pages/cabinet/main.ts` | То же для `profileEditor`; `openModal` принимает `wide` и переключает `.modal-wide` на `.modal-box` |
| `ui/pages/panel/views/users.ts` | `fillModal`/`openModal` принимают `wide = false` и переключают класс; обратно совместимо |
| `registry/internal/webassets/{panel,cabinet,links}.html` | regenerated (`build_ui.sh`), не закоммичены |

### Что изменилось визуально

- Модалка редактора: `min(1100px, 96vw)` вместо 400px; высота по-прежнему
  ограничена viewport, длинное тело прокручивается, подвал с кнопками виден.
- Шаги: textarea выше (260px), моноширинный, длинные команды переносятся
  горизонтальной прокруткой (`wrap="off"`), а не ломаются по словам.
- Переменные: имя/значение/описание/секрет выровнены по колонкам, поля на всю
  ширину ячейки; ≤720px — 2 колонки, ≤480px (в т.ч. 320px) — в один столбец,
  без горизонтальной прокрутки страницы.
- Escape/Tab-ловушка и inert-фон не менялись (логика фокуса не тронута).

### Верификация раунда

- `bash scripts/build_ui.sh` → `panel.html` (218 КБ), `cabinet.html` (114 КБ),
  `links.html` (48 КБ, общий `modal.css`); `build_ui.sh -check` → exit 0.
- `cd registry && gofmt -l .` — пусто; `go vet ./...`, `go build -mod=readonly ./...`
  — OK; `go test -race -count=1 ./...` — все пакеты ok (server 219 с, control 94 с,
  provision 43 с).
- Бандлы содержат `modal-wide`, `profile-editor`, `wrap="off"`; `console.*` — нет.
- Непроверено без браузера: фактическая ширина/прокрутка в живом DOM, порядок
  Tab-ловушки внутри расширенного редактора и отсутствие горизонтального скролла
  страницы на 320px.



## Фикс CSRF-бага кабинета (Beget, «csrf token mismatch»)

Симптом: подключение Beget из кабинета падало с 403 `csrf_invalid`. Причина —
рассинхрон имени cookie: сервер ставит читаемую CSRF-cookie `__Host-sharedd_csrf`
(`cabinet_api.go`: `csrfCookieName`), а фронт читал несуществующий
`sharedd_csrf`, слал пустой `X-CSRF-Token` → `requireCSRF` отклонял.

### Файлы

| Файл | Изменение |
|---|---|
| `ui/pages/cabinet/api.ts` | `csrf()` читает `__Host-sharedd_csrf` (точное совпадение с сервером), значение `decodeURIComponent(...).trim()`. Добавлен `csrfError()`: нет cookie → не шлём пустой заголовок, сразу понятная ошибка; ответ `csrf_invalid` (403) маппится в то же сообщение «Сессия устарела или истекла. Войдите заново.» вместо непрозрачного `csrf token mismatch` |
| `registry/internal/webassets/cabinet.html` | regenerated (`build_ui.sh`), не закоммичен |

### Консистентность

`grep -rn "csrf\|X-CSRF\|__Host" ui/` — CSRF-заголовок/cookie трогает только
`cabinet/api.ts`. Панель, signin и общие компоненты CSRF не читают/не пишут.
Имя заголовка `X-CSRF-Token` совпадает с `csrfHeaderName`; имя cookie — с
`csrfCookieName`. Старое имя `sharedd_csrf` сервер не ставит и не принимает —
фолбэк не нужен (точное совпадение).

### Поток входа (sanity)

`login`/`register` — POST без сессии, `requireCSRF` для них не применяется
(голые маршруты в `cabinet_api.go`). `handleAuthLogin` вызывает
`issueAuthCookies` (строка ~404), которая ставит и сессионную, и CSRF-cookie в
одном ответе — после входа cookie есть для последующих мутаций.

**Caveat http vs https:** CSRF-cookie ставится `Secure: true` и с префиксом
`__Host-` (требует Secure + Path=/ без Domain). Браузер не сохраняет/не отдаёт
её на `http://`-origin — на голом HTTP `document.cookie` её не увидит, и
`csrf()` вернёт `""` → теперь это даёт понятную подсказку о пере-входе, а не
пустой заголовок. Прод за Caddy/https — работает; локальная отладка должна идти
по https (или с TLS-терминацией), иначе все мутации отклонятся.

### Верификация

- `bash scripts/build_ui.sh` → `cabinet.html` пересобран; `build_ui.sh -check`
  → exit 0.
- `grep` по собранному бандлу: `__Host-sharedd_csrf` присутствует (1),
  старое `sharedd_csrf=` — 0.
- `gofmt -l .` пусто; `go vet ./...`, `go build -mod=readonly ./...` — OK;
  `go test -race -count=1 ./...` — все пакеты ok (server 182 с, control 75 с,
  provision 39 с).
- Непроверено без браузера: реальный round-trip cookie на https-origin.

---

## Phase 2: «Ключ доступа» + «Свои серверы» (addendum-manual-servers.md §1/§3)

Ветка `dev`. Реализация UI к бэкенду из
`implementation-summary-manual-servers.md`. Только `ui/`; новых цветов/токенов и
зависимостей нет — переиспользованы `.card/.field/.btn/.badge/.node-card/.seg`,
`openModal` (вкл. wide) и `api.ts`.

### Файлы

| Файл | Изменение |
|---|---|
| `ui/pages/cabinet/api.ts` | Типы `ManualServer`, `ManualServerPayload`, `ManualServerCheck`; методы `rotateAuthkey`, `revokeAuthkey`, `servers`, `createServer`, `checkServer`, `installServer`, `deleteServer` |
| `ui/pages/cabinet/index.html` | Пункт меню «Свои серверы» (topbar + mobile-nav) и раздел `#view-servers`: форма подключения + список |
| `ui/pages/cabinet/main.ts` | Раздел «Ключ доступа» (rotate/revoke/copy), список/форма/действия своих серверов, маршрут `#/servers`, делегирование событий |
| `ui/pages/cabinet/page.css` | Раскладка `.srv-grid`, `.seg` (радиогруппа способа входа), `.srv-more`, `.srv-out` — поверх существующих компонентов |
| `registry/internal/webassets/cabinet.html` | Пересобран (`build_ui.sh`), не закоммичен |

### Wiring по фичам

- **Ключ доступа** (§1): `GET /api/me/authkey` → prefix/created/active.
  «Выпустить/перевыпустить» → `POST /api/me/authkey/rotate` → wide-модалка
  `.secret-reveal` (полный ключ, tabindex, копирование). «Отозвать» → confirm →
  `DELETE /api/me/authkey` (204). Явно: один ключ на ВСЕ VPS; перевыпуск
  немедленно отзывает старый и требует новый для незарегистрированных нод; отзыв
  не ломает уже работающие ноды.
- **Свои серверы** (§3): `GET /api/me/servers` → карточки (.node-card) с label,
  host:port, ssh_user, способом входа, состоянием (новый/проверен/устанавливается/
  установлен/ошибка), `last_error`, `node_id`, `operation`. Действия:
  «Проверить доступ» (`POST .../{id}/check`), «Установить»/«Повторить установку»
  (`POST .../{id}/install`, повтор — та же нода, второй сервер не создаётся),
  «Удалить» (confirm → `DELETE`, место освобождается).
- **Форма подключения**: label, host (публичный IPv4), port (опц., дефолт на
  бэкенде 22), ssh_user (опц., дефолт root), радиогруппа «пароль | приватный
  ключ», поле пароля ИЛИ textarea ключа + нативный file-picker (FileReader).
  После submit секретное поле и file-input очищаются. Ошибки валидации бэкенда
  (400 `validation_failed`) показываются дословно.
- **Install-модалка**: выбор профиля (стандартный `0` + published + свои),
  `profile_id` уходит в `POST .../install`; переменные профиля применяются со
  значениями по умолчанию (бэкенд `MergeProfileVars`; редактор vars намеренно не
  дублируется из визарда). Коды отказа (`quota_exceeded`, `ssh_unavailable`,
  `already_done`, `in_progress`, `not_found`) — понятным текстом.
- **Beget** не тронут: rate-limit-баннер, поле «Код (2FA)», CSRF-fix (в `api.ts`)
  работают как прежде.

### Инварианты / a11y

- Ни один контрол не называет установку покупкой/созданием сервера;
  «Повторить установку» использует тот же хост.
- Секреты: полный authkey — только в одноразовой модалке; пароль/ключ после
  отправки очищаются; в списке секретов нет (бэкенд их не отдаёт).
- Состояния: skeleton → список/empty-state/error (`#servers-body`), pending-тексты
  на кнопках, `role="alert"` для ошибок, `role="radiogroup"`/`aria-checked`
  для способа входа, Escape/фокус-трап/`inert` — из общего `openModal`.

### Верификация

- `bash scripts/build_ui.sh` — `cabinet.html` пересобран; `build_ui.sh -check` → exit 0.
- `gofmt -l .` пусто; `go vet ./...`, `go build -mod=readonly ./...` — OK;
  `go test -race -count=1 ./...` — все пакеты ok (server ~113 с).
- Нет `console.log`, нет неиспользуемых импортов; все `$("id")` статической формы
  присутствуют в `index.html` (проверено скриптом).
- Отдельный TS-`typecheck` для `ui/` в репозитории не настроен (сборка — Go-esbuild
  в `tools/uigen`); проверка ограничена успешной сборкой + статическим обзором.

### Ограничения / degraded

- Операции `manual_install` не отражаются в разделе «Операции» (`GET /api/me/operations`)
  отдельной строкой сверх уже существующего общего списка; прогресс показан в
  карточке сервера через `operation` из `GET /api/me/servers`.
- ~~Переопределение переменных профиля при ручной установке не вынесено в UI~~
  закрыто в «Раунде 8» ниже.

## Раунд 7 — fix B1/B2 + L1–L5 (QA design-qa-report.md, Round 7)

Источник: `docs/specs/design-qa-report.md` §Round 7. Изменён только кабинет.

- **B2 (UI-гард):** `submitServerForm` до запроса проверяет `SERVERS` на тот же
  `host`+`port` (дефолт порта 22) и показывает «Этот адрес уже подключён. Откройте
  его в списке или удалите прежнюю запись.». Серверный 409 `duplicate_host`
  подстраховывает (сообщение приходит уже на русском через `#sv-err`).
- **B1:** сообщения валидации бэкенда локализованы на сервере; UI показывает
  `e.message` как прежде — английских строк больше нет.
- **L1:** `#sv-authtype` — полноценный APG radiogroup: roving tabindex
  (`setAuthType` ставит `0`/`-1`), стрелки Left/Right/Up/Down перемещают выбор и
  фокус (`initServerForm`). В `index.html` задан начальный `tabindex="0"/"-1"`.
- **L2:** `renderServers` и `renderAuthkey` сохраняют фокус через перерисовку:
  запоминают `data-*` активного контрола и возвращают фокус на него, иначе — на
  стабильную кнопку (`#servers-reload` или `#ak-rotate`).
- **L3:** `.check` получил `min-height: 24px` (сам бокс остаётся 17px) — цель
  нажатия ≥ 24px (WCAG 2.2 2.5.8).
- **L4:** `serverState` маппит состояние операции `deleted` в нейтральную метку
  «удалено» (без retry-кнопки), а не «ошибка установки».
- **L5:** `setAuthType` очищает секрет неактивного способа; `route()` вызывает
  `clearServerSecret()` при уходе со страницы «Свои серверы».

Верификация: `bash scripts/build_ui.sh` → `build_ui.sh -check` exit 0; в собранном
`cabinet.html` присутствуют «Этот адрес уже подключён…», `min-height: 24px`,
`ArrowRight`, «удалено».

## Раунд 8 — переменные профиля при ручной установке (свои серверы)

Задача: владелец не мог задать `ad_tag`/переменные профиля при установке на
уже существующий сервер. Бэкенд `POST /api/me/servers/{id}/install` уже принимал
`{profile_id, vars}` через тот же `MergeProfileVars`/`ValidateExtraVars`, что и
визард Beget, — не хватало только UI. Изменён только кабинет.

Файлы: `ui/pages/cabinet/main.ts` (+`api.ts`, тип `ProfileVar` из `ui/lib/vars.ts`).
`ui/components/vars.css` не менялся.

- В модалке установки/повтора появился раздел «Переменные установки»: редактор
  `renderVarsEditor`/`collectVarsEditor` (`ui/lib/vars.ts`) с объявленными
  переменными выбранного профиля, дефолты предзаполнены, значения переопределяемы;
  кнопка «Добавить переменную» добавляет свои сверху. Секретные — `password` с
  «глазом»; имена — те же клиентские подсказки (`varNameHint`).
- Смена профиля в `<select id="sv-install-prof">` перерисовывает редактор
  (`installProfileDecls`: published + свои). Профиль без переменных → компактное
  «Переменных нет» (`renderVarsEditor` empty) и Add доступен (свои переменные).
- При отправке `doInstall` собирает `vars` (непустые значения по именам) и шлёт
  их в `installServer(id, {profile_id, vars})`. Прямая правка профиля здесь
  невозможна — это только значения для конкретной установки.
- Подсказка: значения можно менять здесь для этой установки; дефолты профиля
  задаёт администратор в панели (профили). Ошибки валидации бэкенда
  (`validation_failed`) показываются `e.message` дословно, как и раньше.
- Свежий корень редактора на каждый рендер (`wrap.replaceChildren()`), иначе
  делегированные слушатели `renderVarsEditor` копились бы на повторном вызове.
- Инвариант «отказ ≠ пересоздание» соблюдён: новых контролов создания/покупки
  сервера не добавлено — только переменные.

Ограничения: отдельного TS-`typecheck` для `ui/` нет (Go-esbuild); проверка —
успешная сборка + статический обзор. `firstVarNameError` даёт мгновенную
подсказку по именам, авторитет — серверная проверка (дословно).

Верификация: `bash scripts/build_ui.sh` → `build_ui.sh -check` exit 0 (webassets
незакоммичены); `cd registry && gofmt -l . && go vet ./... && go build
-mod=readonly ./... && go test -race -count=1 ./...` — всё зелёное. В собранном
`cabinet.html` присутствуют `sv-install-vars` и текст подсказки.

## Раунд 9 — встроенные переменные прокси + устойчивость разделов к рестарту

Задача (владелец): (1) профиль должен сам поднимать прокси и вытягивать
клиентский креденшл — добавить встроенные переменные из `[shared_proxy]`;
(2) при рестарте регистратора раздел «Профили» прятал «Создать профиль»
(ошибка `control_unavailable`), из-за чего казалось, что редактора нет.

### Встроенные переменные профиля (бэкенд)

`registry/internal/server/cabinet_beget.go` (`provisionVars`) дополнительно
инъектит из `[shared_proxy]`: `proxy_port` (`port`), `proxy_sni` (`tls_domain`),
`proxy_secret` (client secret из `users`). Правило выбора секрета: ровно один
пользователь → его секрет; несколько → секрет пользователя с алфавитно-первым
именем; ноль → пустая строка (детерминизм нужен для retry).
`node_id`/`node_secret`/`registry_url` не изменены.

`proxy_secret` — СЕКРЕТ: добавлен в конвенциональный список
`provision.secretValues` (`registry/internal/provision/provision.go`) рядом с
`node_secret`/`node_token`/`authkey`/`password`, поэтому маскируется (`***`) в
`operations.output`/`last_error` и в маскируемом выводе прогона профиля; в
логи не попадает (не логируется). На клиенте `proxy_secret` добавлен в
набор секретов сводки визарда кабинета (`varsSummaryHTML`). Помощь редактора
профилей (панель и кабинет) перечисляет встроенные переменные.

### Устойчивость разделов (фронтенд)

Файлы: `ui/pages/panel/views/profiles.ts` (+`users.ts` helper
`isRetryableLoad`), `ui/pages/cabinet/main.ts`.

- Сбой загрузки раздела (5xx/`control_unavailable`/сеть) БОЛЬШЕ НЕ скрывает
  контролы: «Создать профиль» остаётся доступной, ошибка — инлайн с
  «Повторить».
- Одноразовый self-heal: повтор загрузки через ~2с при временном сбое
  (окно рестарта регистратора проходит само).
- Применено к панели «Профили» и к разделам кабинета: свои серверы, профили,
  ключ доступа (authkey грузится независимо от остального обзора — его сбой
  не роняет ноды/операции и не затирает уже отрисованные контролы).
- A11y-паттерны сохранены (роли `alert`/текст баннера, кнопка «Повторить»).

Верификация раунда: `bash scripts/build_ui.sh && bash scripts/build_ui.sh -check`
exit 0 (webassets незакоммичены); `gofmt -l` по изменённым пакетам чисто,
`cd registry && go vet ./internal/server ./internal/provision && go build
-mod=readonly ./...` зелёные. Тест-сьюты НЕ запускались (политика владельца).

## Раунд — «Сделать копию» опубликованного профиля (свой черновик)

Источник: `docs/api-contract.md` (POST /api/me/profiles/{id}/copy), бэкенд —
`registry/internal/server/profiles_user_api.go` (`handleMeCopyProfile`).
Копия видимого профиля (опубликованного админом ИЛИ своего; чужой
неопубликованный → 404 `not_found`) создаётся как локальный черновик
(`published=false`, `created_by=пользователь`) с именем «`<оригинал> (моя копия)»`,
при коллизии — суффиксы «-2», «-3»… (потолок 100 попыток → 409
`duplicate_name`). Шаги и `vars_schema` копируются дословно из последней
видимой версии; секретных ЗНАЧЕНИЙ копия не содержит — `vars_schema` хранит
объявления (флаги/дефолты), как их и так видит зритель профиля. Лимит
`max_user_profiles` → 403 `profile_limit_reached`, выключенный флаг → 403
`user_profiles_disabled`.

UI (`ui/pages/cabinet/`):

- `api.ts` — `copyProfile(id)` (POST, CSRF-заголовок ставится слоем `req`).
- Раздел «Мои профили» (`main.ts` `renderProfiles`/`renderPublishedProfiles`,
  `index.html` `#profiles-published`) — ниже списка своих профилей появился
  блок «Опубликованные администратором» с карточками и кнопкой «Сделать
  копию»; кнопка выключена при `enabled=false`. После копии — тост и
  открытие копии в обычном редакторе профиля (шаги/переменные доступны
  сразу); ошибки (лимит/отключено) — человекочитаемые, остальные — дословно
  от сервера (включая `duplicate_name`).
- Модал установки (`openInstall`) — под селектом профиля кнопка «Сделать
  копию»: активна только когда выбран опубликованный профиль (не «Стандартный
  профиль» value=0 — его копируют выбором «sharedd-node» из списка), помощь
  под кнопкой объясняет состояние. Копия открывается в том же редакторе
  профиля (модал установки заменяется); клик обрабатывается общим
  делегированием `[data-pcopy]`.

Подтверждение не требуется (копия — дешёвое обратимое действие, удаляется
обычным «Удалить» профиля). A11y: обычные кнопки, `title`/текст помощи
описывают выключенные состояния, регистр русский, новых цветов нет.

Верификация раунда: `bash scripts/build_ui.sh && bash scripts/build_ui.sh
-check` exit 0 (cabinet.html перегенерирован, остальные — без изменений);
`gofmt -l ./internal/server` чисто, `cd registry && go vet ./internal/server/
&& go build -mod=readonly ./...` зелёные. Тест-сьюты НЕ запускались
(политика владельца), живой сервер не поднимался.

## Fix round — SPA UX-дефекты: скролл-прыжки, «застрявший» старый интерфейс, скрытые действия

Ветка `dev`. Только UI (ванильный TS + esbuild, `tools/uigen`), без новых
зависимостей/цветов/API. Эталон — `mtplane` (серверный рендер, без опроса и
перерисовки у пользователя); кабинет и панель — SPA с опросом раз в 5 с, из-за
чего теряли прокрутку/фокус и держали старый бандл в открытой вкладке. Три фикса.

### Файлы

| Файл | Изменение |
|---|---|
| `ui/lib/dom.ts` | += `modalOpen()`, `renderSection(section, poll, render)` — единый помощник «не тревожить пользователя» (см. ниже) |
| `ui/lib/update.ts` | **новый**: слежение за свежестью бандла (маркер `sharedd-build` + полоска «Обновить»), восстановление hash/скролла |
| `tools/uigen/main.go` | В `<head>` каждой страницы запекается `<meta name="sharedd-build" content="<sha256[0:16]>">` — хэш каркас+CSS+JS (детерминированный, `-check` не протухает) |
| `ui/components/misc.css` | Стили `#update-bar` (плавающая полоска, `[hidden]` уважается) |
| `ui/pages/cabinet/main.ts` | `renderSection` во всех путях опроса; профильные карточки — действия в `.node-side` (всегда видны); `initBuildWatch()` |
| `ui/pages/cabinet/page.css` | — (профильные действия переиспользуют `.srv-actions`/`.node-side`) |
| `ui/pages/panel/main.ts` | `renderAll(poll)` оборачивает рендер вида в `renderSection`; `node.load(id, poll)`; `initBuildWatch()` |
| `ui/pages/panel/views/node.ts` | `load(id, poll)` — перерисовка детали через `renderSection` |
| `ui/pages/panel/views/users.ts` | `render(poll)`, `renderInvites(poll)`, `applyLegacy` — через `renderSection` |
| `ui/pages/panel/views/profiles.ts` | `render()` через `renderSection` |
| `registry/internal/webassets/*.html` | regenerated (`build_ui.sh`), **не закоммичены** |

### (1) Опрос не сбивает пользователя

`renderSection(section, poll, render)` в `ui/lib/dom.ts`:
- при `poll=true` (фоновое обновление раз в 5 с) **полностью пропускает**
  отрисовку, если открыта модалка (`#modal.show`) или фокус внутри `section` —
  ввод и каретка не теряются;
- иначе снимает `window.scrollY` и стабильную идентичность фокуса (id или первый
  `data-*` атрибут), выполняет `render()`, затем возвращает фокус
  (`focus({preventScroll:true})`) и прокрутку (`window.scrollTo`).

Точки применения (каждый — отдельный `renderSection`):
- кабинет: ноды (`#nodes-body`), метрики/квота, операции (`#ops-body`), свои
  серверы (`#servers-body` + интервальный `loadServers(true)`), ключ доступа
  (`#authkey-body`), Beget (`#beget-status`), визард (`#view-create`, шаги
  «Регион/Тариф/Профиль/Переменные»), профили (`#view-profiles`).
- панель: обзор, ноды, Мастера/DNS, СРМД, журнал (по контейнеру вида),
  деталь ноды, пользователи, приглашения, «совместимость агентов», профили.

Ручные (не-poll) вызовы по-прежнему перерисовываются всегда — локально-инициированные
изменения (правка квоты, удаление и т.п.) не «залипают».

### (2) Самообновляющийся интерфейс

`tools/uigen` печёт в каждую страницу `sharedd-build` (хэш исходников). Обе
страницы при старте вызывают `initBuildWatch()` (`ui/lib/update.ts`): на
`focus`, `visibilitychange` и раз в 60 с запрашивают собственную HTML
(`no-store`), сравнивают маркер и при расхождении показывают ненавязчивую
полоску «Доступно обновление интерфейса — Обновить». Автоперезагрузки нет —
только по клику (открытая модалка/форма данные не теряет); перед `reload()`
скролл сохраняется в `sessionStorage` и восстанавливается на загрузке, hash
сохраняет сам браузер.

### (3) Действия всегда видны

- «Мои профили» (кабинет): кнопки вынесены из сворачиваемого `.node-more` в
  видимую колонку `.node-side` — свои: «Изменить», «Удалить»; опубликованные:
  «Сделать копию».
- «Свои серверы» (кабинет): действия («Проверить доступ», «Установить»/
  «Повторить установку», «Удалить») и раньше лежали в `.node-side` (не в
  `.node-more`) — подтверждено в собранном бандле; описание операции остаётся
  отдельной строкой `.srv-more`, но действий не прячет.

### Верификация

- `bash scripts/build_ui.sh` → собраны все 6 страниц; `bash scripts/build_ui.sh -check`
  → exit 0.
- В бандлах: `name="sharedd-build"` во всех 6 страницах (например,
  `cabinet = 79e92d91c65b0b93`); «Доступно обновление интерфейса» и `#update-bar`
  — в `cabinet.html`/`panel.html`; профильные действия — в `node-side srv-actions`.
- `gofmt -l tools/uigen/` и `cd registry && gofmt -l .` — пусто;
  `go vet ./internal/server` — чисто; `go build -mod=readonly ./...` — ok.
  Тест-сью́ты **не запускались** (политика: только gofmt/vet/build + build_ui).
- `console.log` в `ui/` — нет; unused imports — нет.

### Не проверено без браузера

- Реальное отсутствие прыжка прокрутки/потери каретки при опросе 5 с (логика
  покрыта помощником, но живой DOM/браузер не поднимались).
- Что `fetch(location.pathname)` собственной HTML проходит авторизацию
  (кабинет — cookie `same-origin`; панель — HTML публичен, токен только к API)
  и что полоска обновления показывается ровно при смене маркера.
- Поведение `.node-side srv-actions` на узких экранах (ожидается обёртка/стек).

## Раунд — plan-ui-batch.md Фаза 1, пп. 1, 3, 4, 5

Ветка `dev`, без коммита/пуша. Ванильный TS + esbuild (`tools/uigen`), без новых
зависимостей/цветов, русский регистр, a11y-паттерны прежних раундов сохранены.
Тест-сьюты НЕ запускались (политика владельца).

### (1) Кабинет: редактируемое «Имя» своей копии профиля

`ui/pages/cabinet/main.ts` (`profileEditor`, `saveProfile`), `ui/pages/cabinet/api.ts`:

- Поле «Имя» в редакторе своего профиля (`#view-profiles` → «Мои профили»)
  больше не `readonly`; «Имя» уходит в `PUT /api/me/profiles/{id}`.
- Переименование — метаданные: **версию не создаёт**. `saveProfile` шлёт
  `steps`/`vars_schema` только когда шаги/переменные реально изменились
  (иначе серверный `AddProfileVersion` создал бы лишнюю версию), и `name` —
  только при изменении.
- `ProfilePayload.steps` стал опциональным (type-only правка `api.ts`).
- `duplicate_name` (409) показывается инлайн в `#mp-err`: «Профиль с таким
  именем уже существует — выберите другое имя.» Прочая валидация — дословно.
- Тост различает: создание / сохранение новой версии / переименование.
- Поведение шагов и переменных не менялось.

### (3) Панель: «Дефолтная квота» = `[users] default_max_nodes`

`ui/pages/panel/index.html` (карточка `#quota-card` в `#view-users`),
`ui/pages/panel/views/users.ts` (`loadUserQuota`/`saveUserQuota`, вызов из `load()`).

- Поле `#udq-max` (number, 0..1000000) + «Сохранить квоту»; значение читается из
  `GET /panel/api/config` (`config.users.default_max_nodes`) и пишется через
  `saveSection({users:{default_max_nodes:n}})` (`PUT /panel/api/config`, merge).
- Инлайн-валидация диапазона 0..1000000 и инлайн-показ серверной ошибки (`#udq-err`).
- **Backend-дополнение (gap):** редактор конфига не принимал секцию `users` —
  `editableConfig` получил `*editableUsers{DefaultMaxNodes}` (`GET` отдаёт
  значение, `PUT` валидирует 0..1000000 и hot-apply+персистит; enabled/legacy/
  allow_shared_secret остаются вне редактора). Без этого поле было бы мёртвым
  (`DisallowUnknownFields` → 400). `registry/internal/server/config_editor.go`.

### (4) Панель: удаление пользователя целиком

`ui/pages/panel/views/users.ts` (`actionsCell` + `deleteUser`).

- В «Действия» добавлена danger-кнопка «Удалить» (`data-udel`).
- Confirm-модалка объясняет: ноды **остаются в пуле** и перепривязываются к
  системному `legacy`, место в квоте освобождается; физически ноды не
  удаляются; ключи/сессии/профили/серверы/операции удаляются.
- `DELETE /panel/api/admin/users/{id}`; 400-отказы (`legacy_protected`,
  `last_admin`) показываются дословно (`e.message`) в баннере модалки.
- 204 → тост «Пользователь удалён. Его ноды остались в пуле.» + перезагрузка списка.

### (5) Кабинет: слияние «Создать сервер» + «Свои серверы»; «Beget» → «Хостинги»

`ui/pages/cabinet/index.html`, `ui/pages/cabinet/main.ts`.

- Верхнее и мобильное меню: удалён пункт «Создать сервер»; «Свои серверы» →
  **«Серверы»**; «Beget» → **«Хостинги»** (label only, маршрут `#/beget` и весь
  Beget-контент не тронуты).
- Раздел `#view-servers` теперь «Серверы»: список серверов + кнопка
  **«Добавить сервер»** (`#srv-add`, `aria-expanded`/`aria-controls`), которая
  открывает существующую форму подключения (`#server-form`, теперь `hidden`).
  «Отмена» (`#sv-cancel`) скрывает форму; успешное подключение возвращает к списку.
  Все действия серверов («Проверить доступ», «Установить/Повторить установку»,
  «Удалить») без изменений.
- **Визард «Создать сервер»** остаётся (`#view-create`, `#/create`) и достижим:
  ссылка в `card-note` раздела «Серверы» и в пустом состоянии «Мои ноды»;
  `route()` по-прежнему вызывает `wizard.enter()` для `#/create`.
- В карточку Beget добавлена пометка: пока поддерживается только Beget, другие
  хостинги появятся позже.

### Файлы

| Файл | Изменение |
|---|---|
| `registry/internal/server/config_editor.go` | секция `[users] default_max_nodes`: `editableUsers`, GET-выдача, валидация 0..1000000, apply+persist |
| `ui/pages/cabinet/api.ts` | `ProfilePayload.steps` → опциональный |
| `ui/pages/cabinet/main.ts` | редактируемое имя + rename-PUT + inline `duplicate_name`; слияние раздела «Серверы», toggle формы, метки меню, ссылка на визард |
| `ui/pages/cabinet/index.html` | навигация (Серверы/Хостинги, без «Создать сервер»), «Добавить сервер», скрытая форма + «Отмена», пометка про хостинги |
| `ui/pages/panel/index.html` | карточка `#quota-card` (дефолтная квота) |
| `ui/pages/panel/views/users.ts` | `loadUserQuota`/`saveUserQuota`, `deleteUser` + кнопка «Удалить» |
| `registry/internal/webassets/{cabinet,panel}.html` | regenerated (`build_ui.sh`), не закоммичены |

### Верификация

- `bash scripts/build_ui.sh` → `cabinet.html` (165 КБ), `panel.html` (238 КБ);
  `bash scripts/build_ui.sh -check` → exit 0 (webassets незакоммичены).
- `cd registry && gofmt -l .` — пусто; `go vet ./internal/server` — ok;
  `go build -mod=readonly ./...` — ok. Тест-сьюты НЕ запускались.
- `console.log` в изменённых `ui/`-файлах — нет; в бандлах присутствуют
  `Хостинги`, `Профиль переименован`, `srv-add`; `default_max_nodes`, `data-udel`,
  `udq-max` в `panel.html`.
- Незакоммиченная работа других задач не тронута; коммит/пуш не выполнялись.

### Не проверено без браузера

- Реальное поведение модалки удаления (фокус/inert), рост формы подключения и
  переключение «Добавить сервер» на узких экранах.
- Round-trip `PUT /panel/api/config {users:...}` с живым регистратором (в дереве
  персист и hot-apply покрыты кодом, но сервер не поднимался).

## Фаза 2 UI (slice 1..3) — детальная карточка ноды в кабинете

Кабинет, «Мои ноды»: клик по карточке (или Enter/Space — карточка `role="link"`
`tabindex="0"`) открывает вид `#/nodes/<id>` (`#view-node`, контейнер
`#node-detail`). Данные — `GET /api/me/nodes/{id}/detail` (владелец; чужая/legacy
→ 404 → пустое состояние с «← К нодам»). Опрос 5 с идёт через `renderSection`,
поэтому список нод и прокрутка не «дёргаются». Разделы без источника (`absent[]`)
рисуются строкой «нет данных», значения не выдумываются.

Отрендеренные разделы (из `node_detail_api.go`): Параметры (IP — маскирован тем
же правилом, что `maskPublicIP`: первый и последний октет; порт, регион, аптайм,
heartbeat/отчёт, мастерство, домены, слоты, TTL, карантин), Доступность (общая %,
GP % + столбики по 7 дням), История IP (таблица, IP маскированы), Последние
проверки, Хостинг (сеть — счётчики ingress/egress; CPU/RAM — «нет данных»),
Соединения 24ч (текущие gauge + спарклайн writers + подпись «series bounded by
report ring»), Уникальные IP по пользователям (трафик по пользователям — «нет
данных»), Статистика telemt (сводка снимка + сворачиваемая таблица метрик),
DC и endpoints / Качество и здоровье / Действия — «нет данных», События ноды
(лента `.feed`).

## Фаза 3 UI — пошаговый журнал операций

Раздел «Операции»: у операции с `steps[]` появляется кнопка «Шаги (N)»; строка
раскрывается в список шагов (индекс, имя, бейдж статуса running/ok/error,
длительность мс). Вывод каждого шага (`output`, секреты уже замаскированы
сервером) — в сворачиваемом `<details>`. Открытые журналы/логи запоминаются в
`OPEN_OPS`/`OPEN_LOGS` и восстанавливаются при перерисовке опроса. Финальная
строка ошибки операции (`last_error`) сохранена без изменений.

## Файлы (Фаза 2/3 UI)

| Файл | Изменение |
|---|---|
| `ui/pages/cabinet/api.ts` | типы `NodeDetail`/`NodeParam`/`NodeAvailability`/`NodeDaily`/`NodeCheck`/`NodeHosting`/`NodeConnections`/`NodeUsers`/`NodeEvent`/`IPHistoryRow`/`OperationStep`; `api.nodeDetail(id)`; `Operation.steps` |
| `ui/pages/cabinet/node-detail.ts` | **new** — загрузка/отрисовка `#node-detail`, маскировка IP, состояния loading/error/empty, все секции |
| `ui/pages/cabinet/main.ts` | маршрут `#/nodes/<id>`, клик/клавиатура по карточке ноды (карточка стала ссылкой, `OPEN_NODES` удалён), кнопка «Шаги» + рендер шагов и логов, опрос детали |
| `ui/pages/cabinet/index.html` | секция `#view-node` + контейнер `#node-detail` |
| `ui/pages/cabinet/page.css` | импорт `feed.css`, `.nd-*`, `.day-bars`, `.op-step*` (только токены, без новых цветов) |
| `registry/internal/webassets/cabinet.html` | regenerated (`build_ui.sh`), не закоммичен |

## Верификация

- `bash scripts/build_ui.sh` → `cabinet.html` (192 КБ); `-check` → exit 0
  (webassets незакоммичены).
- `cd registry && gofmt -l . && go vet ./internal/server && go build -mod=readonly ./...` — ok.
- Тест-сьюты НЕ запускались (по условию задачи); TypeScript-компилятора в дереве нет
  (сборка через esbuild/uigen), поэтому tsc-проверка не выполнялась.
- `console.*` в изменённых `ui/` — нет.

## Не проверено без браузера/сервера

- Живой рендер детали, состояния 404 и опрос не поднимались (регистратор не
  запускался). Проверка — статическая (сборка/бандл-строки).
