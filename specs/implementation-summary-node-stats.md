# implementation-summary-node-stats.md — Фаза 2 (@frontend)

Спека: `specs/node-stats.md`, токены/a11y: `DESIGN.md` §13. Ветка `dev`.

## Что сделано

### Категории — ТАБАМИ (порт `nav.c-tabs` из mtplane; не единый scroll)

Шапка ноды (статус-чипы + «Удалить ноду») — над таб-баром, всегда видна. Табы
(`role="tablist"` → `<button role="tab">` → `[role="tabpanel"][data-tab-panel]`):

1. **Обзор** (`overview`, дефолт) — Параметры + **Доступность, 7 дней** (график A) + История IP
2. **Метрики** (`metrics`) — **Уникальные IP, 24 ч** (график C, только `clients`) + Хостинг + **Статистика telemt** (график E + таблица «Пользователи» + снимок `<details>`) + telemt Control API
3. **Проверки** (`checks`) — Последние проверки (таблица)
4. **Журнал** (`journal`) — События ноды (`feed`)
5. **Действия** (`actions`) — placeholder `nd("Действия ноды")`

Поведение/a11y табов:
- `hidden` на неактивных панелях — видна ровно одна, страница больше не один скролл.
- `aria-selected` на активном; roving tabindex; стрелки ←/→/Home/End; клик фокусирует панель.
- активный таб в хэше `#/nodes/<id>?tab=<key>` (`replaceState` — hashchange не будит
  маршрут); restore из хэша при `openNodeDetail`, неизвестное → **Обзор**.
- переключение НЕ перерисовывает innerHTML: графики A/C/E продолжают рендериться
  (SVG с `preserveAspectRatio="none"` масштабируется при показе панели).
- `routeNodeId()` в `main.ts` срезает хэш-параметр `?tab=` из id ноды.

### Графики (ванильный inline-SVG, без зависимостей)
| График | Тип | Источник | Данные |
|---|---|---|---|
| A «Доступность, 7 дней» | столбики | `availability.daily[].ok_pct` | data-ready |
| C «Уникальные IP, 24 ч» | линия **интерактивная** | `connections_24h.series[].clients` (= `telemt_user_unique_ips_current`) | data-ready |
| D «Хостинг CPU/RAM/сеть» | 2 линии **интерактивные** | `hosting.cpu_ram_series[]`, `hosting.net_series[]` | data-ready |
| E «Уникальные IP (сейчас)» | горизонтальные столбики (hover) | `users.unique_ips_by_user[]` | снимок |
| ~~F «Трафик по пользователям, 24 ч»~~ | удалён по требованию владельца | `users.traffic_by_user[]` | итоги |

- **me-writers удалён**: поле `writers` не участвует ни в одном графике/плитке. `current_writers`
  убран из видимой части. Раздел не удалён — заменён линией `clients` (§5).
- График A: цвет столбика = здоровье дня (`availColor`: ok≥99 / warn≥95 / bad<95); ось Y 0–100%.
- График C: разрыв линии на пропуске сэмпла (`null`); подпись «Уникальные IP по отчётам»;
  наведение/тап → кроссхэйр + точка + тултип «время · уникальные IP».
- График D: две панели (CPU/RAM %, сеть байт); наведение как у C; пустые серии →
  «данные копятся — пока мало точек», не холст.
- График F УДАЛЁН: столбики-итоги по `traffic_by_user` больше не рисуются;
  `.legend-btn`/`data-series`/`chartFHTML`/`F_HIDDEN` убраны. Таблица
  «Пользователи» продолжает показывать трафик по имени.
- График B — оставлен таблицей «Последние проверки» (backend-blocked §3 G4), без пустого холста.

## A11y (DESIGN §13.4)
- Каждый график A/C/E: контейнер `role="img"` + русский `aria-label`.
- `<table class="sr-only">` с caption/thead/tbody — текстовый эквивалент теми же форматтерами
  (`fmtPct`/`fmtBytes`/`fmtClock`/`fmtNum`).
- `.sr-only` добавлена в `ui/components/misc.css` (+ `table.sr-only { table-layout: fixed }`).
- Цвет-носитель состояния дублируется текстом в sr-only-таблице.
- Легенда-кнопки F удалены вместе с графиком F (нет `aria-pressed`-кнопок серий).
- Интерактив линий (C, D) — только указатель/тап; `role="img"`+`aria-label`+`sr-only`-таблица
  сохранены, клавиатурный путь = таблица (обоснование — §6.7 спеки).

## Состояния
- A/C: `<2` точек → `dim` «накапливается — пока мало точек»; `null`-раздел → `.table-empty`.
- A: дней < `window_days` → note «данные за N из M дней».
- E/F: раздел `null`/пусто → `.table-empty` «нет данных» / «пользователей нет».
- F: обе серии скрыты → текст «серии скрыты» (не пустой холст).
- loading — существующий `skel-block` через `openNodeDetail`/`render`.

## Файлы
- `ui/lib/chart.ts` — `lineChart()` / `barChart()` / **`liveLineChart()`** (inline-SVG,
  `preserveAspectRatio="none"`, цвета — только токены).
- `ui/pages/cabinet/node-detail.ts` — табы разделов + графики A/C/D/E/F, таблица
  «Пользователи», обработчики легенды F и наведения на линии; панели `panelHTML()`/…
- `ui/pages/cabinet/page.css` — `.nd-tabs`/`.nd-panel`, `.chart-*`, `.h-bar*`, адаптив
  ≤640px; добавлены `.chart-canvas`/`.chart-cross`/`.chart-dots`/`.chart-tip`/`.legend-static`.
- `ui/pages/cabinet/main.ts` — `routeNodeId()` срезает `?tab=` из id ноды.
- `ui/components/misc.css` — утилита `.sr-only`.
- `ui/pages/cabinet/api.ts` — добавлены `NodeHostingCpuRam`/`NodeHostingNet` и поля
  `cpu_ram_series`/`net_series` в `NodeHosting`.
- `registry/internal/webassets/*.html` — регенерированы `build_ui.sh` (не правились руками).

## Оставлено бэкенду (не фейкуем)
- График D разблокирован: `cpu_ram_series`/`net_series` приходят в `/detail` (G1/G5).
- E/F как «24 ч тренд» — нет временного ряда (G2/G3): показан текущий снимок/итоги, заголовки честные.
- График B с переключателем периода — окно проверок фиксировано бэкендом (G4): таблица.

## Интерактив линий (C, D) — `liveLineChart`
- `ui/lib/chart.ts`: `liveLineChart(xLabels, series, opts)` → `.chart-canvas[data-live]`
  (SVG + пустые `.chart-cross`/`.chart-dots`/`.chart-tip`); данные тултипа — JSON в `data-live`.
- Обработчик — делегирован на `document` (`pointermove`/`pointerdown`): переживает
  innerHTML-перерисовку опроса (5 с). Индекс — по доле ширины, точка — по доле высоты
  (та же шкала `pad=3`/`h`, что у растянутого SVG). Тузлтип — время + строки серий.
- **Клавиатура:** точки не фокусируются — данные даёт `sr-only`-таблица (см. §6.7 спеки).
- E: наведение красит трек + `title`; F: кап ширины (см. выше).

## Верификация
- `bash scripts/build_ui.sh` — собирается без npm (esbuild Go-embedded), 6 страниц.
- `bash scripts/build_ui.sh -check` — **все шесть «без изменений»**.
- В `cabinet.html` присутствуют `.chart-canvas[data-live]`, `.chart-tip` (графики C/D/E).
- Формулы обработчика проверены standalone-ассертами (y-доля точки, кап ширины F).
- `git grep writers ui/pages/cabinet/node-detail.ts` — только `alive_writers`/`fresh_alive_writers`
  в таблице Control API (метрики ДЦ, не me-writers-график) + комментарий; в графиках/плитках `writers` нет.
- Нет `tsconfig.json`/`package.json` → `nuxi typecheck` невозможен; типы проверены только
  парсером esbuild. Статус: **partially_verified**.
- Хэш-логика табов (`parseTabHash`/`writeTabHash` + срезание `?tab=` в `routeNodeId`)
  проверена standalone-ассертами (дефолт/restore/unknown/replace) — OK.
