# implementation-summary-node-stats.md — Фаза 2 (@frontend)

Спека: `specs/node-stats.md`, токены/a11y: `DESIGN.md` §13. Ветка `dev`.

## Что сделано

### Категории — ТАБАМИ (порт `nav.c-tabs` из mtplane; не единый scroll)

Шапка ноды (статус-чипы + «Удалить ноду») — над таб-баром, всегда видна. Табы
(`role="tablist"` → `<button role="tab">` → `[role="tabpanel"][data-tab-panel]`):

1. **Обзор** (`overview`, дефолт) — Параметры + **Доступность, 7 дней** (график A) + История IP
2. **Метрики** (`metrics`) — **Соединения, 24 ч** (график C, только `clients`) + Хостинг + **Статистика telemt** (график E + график F + таблица «Пользователи» + снимок `<details>`) + telemt Control API
3. **Проверки** (`checks`) — Последние проверки (таблица)
4. **Журнал** (`journal`) — События ноды (`feed`)
5. **Действия** (`actions`) — placeholder `nd("Действия ноды")`

Поведение/a11y табов:
- `hidden` на неактивных панелях — видна ровно одна, страница больше не один скролл.
- `aria-selected` на активном; roving tabindex; стрелки ←/→/Home/End; клик фокусирует панель.
- активный таб в хэше `#/nodes/<id>?tab=<key>` (`replaceState` — hashchange не будит
  маршрут); restore из хэша при `openNodeDetail`, неизвестное → **Обзор**.
- переключение НЕ перерисовывает innerHTML: графики A/C/E/F продолжают рендериться
  (SVG с `preserveAspectRatio="none"` масштабируется при показе панели).
- `routeNodeId()` в `main.ts` срезает хэш-параметр `?tab=` из id ноды.

### Графики (ванильный inline-SVG, без зависимостей)
| График | Тип | Источник | Данные |
|---|---|---|---|
| A «Доступность, 7 дней» | столбики | `availability.daily[].ok_pct` | data-ready |
| C «Соединения, 24 ч» | линия | `connections_24h.series[].clients` | data-ready |
| E «Уникальные IP (сейчас)» | горизонтальные столбики | `users.unique_ips_by_user[]` | снимок |
| F «Трафик по пользователям, 24 ч» | сгруппированные столбики | `users.traffic_by_user[]` | итоги |

- **me-writers удалён**: поле `writers` не участвует ни в одном графике/плитке. `current_writers`
  убран из видимой части. Раздел не удалён — заменён линией `clients` (§5).
- График A: цвет столбика = здоровье дня (`availColor`: ok≥99 / warn≥95 / bad<95); ось Y 0–100%.
- График C: разрыв линии на пропуске сэмпла (`null`); подпись «Соединения по отчётам».
- График F: легенда-кнопки «Принято» (`--info`) / «Отдано» (`--acc`), `aria-pressed`,
  переключение видимости серий; состояние живёт между опросами (`F_HIDDEN`).
- График B — оставлен таблицей «Последние проверки» (backend-blocked §3 G4), без пустого холста.
- График D — оставлен плитками + note «история появится после включения сбора» (backend G1/G5).

## A11y (DESIGN §13.4)
- Каждый график A/C/E/F: контейнер `role="img"` + русский `aria-label`.
- `<table class="sr-only">` с caption/thead/tbody — текстовый эквивалент теми же форматтерами
  (`fmtPct`/`fmtBytes`/`fmtClock`/`fmtNum`).
- `.sr-only` добавлена в `ui/components/misc.css` (+ `table.sr-only { table-layout: fixed }`).
- Цвет-носитель состояния дублируется текстом в sr-only-таблице.
- Легенда F — `<button>` с `aria-pressed` (нативная клавиатура).

## Состояния
- A/C: `<2` точек → `dim` «накапливается — пока мало точек»; `null`-раздел → `.table-empty`.
- A: дней < `window_days` → note «данные за N из M дней».
- E/F: раздел `null`/пусто → `.table-empty` «нет данных» / «пользователей нет».
- F: обе серии скрыты → текст «серии скрыты» (не пустой холст).
- loading — существующий `skel-block` через `openNodeDetail`/`render`.

## Файлы
- `ui/lib/chart.ts` — **новый**: `lineChart()` / `barChart()` (inline-SVG, `preserveAspectRatio="none"`,
  цвета — только токены), поглощает идею прежнего `spark()`.
- `ui/pages/cabinet/node-detail.ts` — табы разделов + графики A/C/E/F, таблица «Пользователи»,
  обработчик легенды; панели `panelHTML()`/`paramsHTML()`/`ipHistoryHTML()`/`checksHTML()`/
  `eventsHTML()`/`actionsHTML()`; hash-хелперы `parseTabHash`/`writeTabHash`/`activateTab`.
- `ui/pages/cabinet/page.css` — раскладка `.nd-tabs`/`.nd-panel` (порт `.c-tabs`) +
  `.chart-wrap/.chart-plot/.chart-y/.chart-svg/.chart-x/.chart-legend/.h-bar*`, адаптив ≤640px.
- `ui/pages/cabinet/main.ts` — `routeNodeId()` срезает `?tab=` из id ноды (иначе хэш-параметр
  ломал бы маршрут).
- `ui/components/misc.css` — утилита `.sr-only`.
- `ui/pages/cabinet/api.ts` — **не менялся**: формы ответа уже совпадали с реальным API
  (`clients`/`writers`, `unique_ips_by_user`, `traffic_by_user`).
- `registry/internal/webassets/*.html` — регенерированы `build_ui.sh` (не правились руками).

## Оставлено бэкенду (не фейкуем)
- График D — нет серии истории хостинга (G1/G5): плитки + note.
- E/F как «24 ч тренд» — нет временного ряда (G2/G3): показан текущий снимок/итоги, заголовки честные.
- График B с переключателем периода — окно проверок фиксировано бэкендом (G4): таблица.

## Верификация
- `bash scripts/build_ui.sh` — собирается без npm (esbuild Go-embedded), 6 страниц.
- `bash scripts/build_ui.sh -check` — **все шесть «без изменений»**.
- `git grep writers ui/pages/cabinet/node-detail.ts` — только `alive_writers`/`fresh_alive_writers`
  в таблице Control API (метрики ДЦ, не me-writers-график) + комментарий; в графиках/плитках `writers` нет.
- Нет `tsconfig.json`/`package.json` → `nuxi typecheck` невозможен; типы проверены только
  парсером esbuild. Статус: **partially_verified**.
- Хэш-логика табов (`parseTabHash`/`writeTabHash` + срезание `?tab=` в `routeNodeId`)
  проверена standalone-ассертами (дефолт/restore/unknown/replace) — OK.
