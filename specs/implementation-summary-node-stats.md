# implementation-summary-node-stats.md — Фаза 2 (@frontend)

Спека: `specs/node-stats.md`, токены/a11y: `DESIGN.md` §13. Ветка `dev`.

## Что сделано

### Категории (единый scroll, порядок §1)
1. Шапка ноды (без изменений)
2. Параметры (без изменений)
3. **Доступность, 7 дней** — плитки «Общая доступность» / «GP верификации» + **график A**
4. История IP (без изменений)
5. Последние проверки (без изменений)
6. **Соединения, 24 ч** — плитки «Соединения пользователей» / «Уникальные IP (сейчас)» + **график C**
7. **Хостинг: CPU / RAM / сеть** — плитки текущих значений + note (график D заблокирован)
8. **Статистика telemt** — плитки-сводка (метрики, собрано, ДЦ, endpoints) + **график E** + **график F** + таблица «Пользователи» + снимок метрик (`<details>`)
9. telemt Control API (без изменений)
10. События ноды (без изменений)
11. Действия (без изменений)

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
- `ui/pages/cabinet/node-detail.ts` — категории, графики A/C/E/F, таблица «Пользователи»,
  обработчик легенды; удалён `spark()`, `writers`/`current_writers` из видимой части.
- `ui/pages/cabinet/page.css` — раскладка `.chart-wrap/.chart-plot/.chart-y/.chart-svg/.chart-x/.chart-legend/.h-bar*`
  (старый `.day-bar*` удалён), адаптив ≤640px.
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
