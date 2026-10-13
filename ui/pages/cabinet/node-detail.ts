/* Детальная карточка ноды в кабинете (plan-ui-batch.md Фаза 2, slice 1).
   Данные — `GET /api/me/nodes/{id}/detail` (node_detail_api.go): владелец
   видит свои ноды, чужая/legacy → 404. Разделы без источника приходят null и
   перечислены в absent[] — рисуем «нет данных», не выдумываем значения.
   Статистика разбита по КАТЕГОРИЯМ-ТАБАМ (порт nav.c-tabs из mtplane):
   Обзор · Метрики · Проверки · Журнал · Действия. Активный таб — в хэше
   `#/nodes/<id>?tab=<key>`. Разметка — .card/.table-cards/.badge/.metrics/.kv/.feed.
   Опрос каждые 5 с идёт через renderSection: не сбивает фокус, прокрутку
   и активный таб. */

import { $, esc, renderSection } from "../../lib/dom";
import { fmtAgo, fmtClock, fmtDur, fmtNum, plural } from "../../lib/format";
import { barChart, chartTop, donutChart, liveLineChart } from "../../lib/chart";
import type { LiveSeries } from "../../lib/chart";
import { api } from "./api";
import type { NodeDetail } from "./api";

const EMPTY_SVG =
  `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="3" width="20" height="7" rx="2"/><rect x="2" y="14" width="20" height="7" rx="2"/></svg>`;

const STATUS_META: Record<string, { label: string; cls: string }> = {
  healthy:     { label: "здорова", cls: "ok" },
  unhealthy:   { label: "нездорова", cls: "bad" },
  quarantined: { label: "карантин Globalping", cls: "warn" },
  deleting:    { label: "удаляется", cls: "warn" },
  offline:     { label: "нет данных", cls: "" },
};

const NODE_TYPE_META: Record<string, { label: string; cls: string; title: string }> = {
  classic:  { label: "Classic",  cls: "",        title: "классический telemt" },
  mtproxyl: { label: "MTProxyL", cls: "special", title: "форк MTProxyL (/opt/mtproxyl/)" },
  meko:     { label: "MEKO",     cls: "info",    title: "фикс MEKO (/opt/mtpr-simple/)" },
};

// Подписи типов событий ноды (как в панели, core set). Неизвестный тип
// показывается как есть — не выдумываем ярлык.
const EV_LABEL: Record<string, { label: string; dot: string }> = {
  node_registered:      { label: "Регистрация ноды",      dot: "info" },
  node_replaced:        { label: "Нода вытеснена",        dot: "warn" },
  node_expired:         { label: "Нода отвалилась",       dot: "warn" },
  node_pruned:          { label: "Удалена (неактивна)",   dot: "warn" },
  tcp_down:             { label: "TCP недоступен",        dot: "bad" },
  tcp_up:               { label: "TCP восстановлен",      dot: "ok" },
  metrics_down:         { label: "Метрики в отказе",      dot: "bad" },
  metrics_up:           { label: "Метрики восстановлены", dot: "ok" },
  globalping_blocked:   { label: "Блокировка (GP)",       dot: "bad" },
  globalping_recovered: { label: "GP восстановлен",       dot: "ok" },
  queue_joined:         { label: "В очереди мастеров",    dot: "info" },
  queue_left:           { label: "Вышла из очереди",      dot: "warn" },
  master_elected:       { label: "МАСТЕР ИЗБРАН",         dot: "ok" },
  master_lost:          { label: "Мастер потерян",        dot: "bad" },
  dns_updated:          { label: "DNS обновлён",          dot: "info" },
  dns_error:            { label: "Ошибка DNS",            dot: "bad" },
  dns_deleted:          { label: "DNS запись удалена",    dot: "warn" },
  node_quarantined:     { label: "Карантин (GP)",         dot: "warn" },
  quarantine_recovered: { label: "Вышла из карантина",    dot: "ok" },
  node_terminated:      { label: "Нода завершена (бан)",  dot: "bad" },
  ban_lifted:           { label: "Бан снят (смена IP)",   dot: "info" },
  ip_blocked:           { label: "IP заблокирован",       dot: "bad" },
};

// Разделы-табы: статистика разбита по категориям, как в mtplane (nav.c-tabs),
// а не свалена в один вертикальный скролл. Порядок табов = порядок категорий.
const TABS: { key: string; label: string }[] = [
  { key: "overview", label: "Обзор" },
  { key: "metrics",  label: "Метрики" },
  { key: "checks",   label: "Проверки" },
  { key: "journal",  label: "Журнал" },
  { key: "actions",  label: "Действия" },
];
const TAB_KEYS = TABS.map((t) => t.key);

let DETAIL_ID = "";
let DETAIL: NodeDetail | null = null;
let DETAIL_ERR: { msg: string; status?: number } | null = null;
let DETAIL_RETRY = false;
// Активный таб. Живёт между опросами (5 с); сбрасывается в openNodeDetail.
let DETAIL_TAB = "overview";
// Скрытые серии графика F (легенда-кнопки). Живёт между опросами (5 с).
let F_HIDDEN = new Set<string>();

export function nodeDetailId(): string { return DETAIL_ID; }

/** Открыть детальную карточку ноды: сброс предыдущей, затем загрузка. */
export function openNodeDetail(id: string): void {
  DETAIL_ID = id;
  DETAIL = null;
  DETAIL_ERR = null;
  DETAIL_RETRY = false;
  DETAIL_TAB = parseTabHash(); // дефолт «Обзор»; валидный #?tab= восстанавливаем
  F_HIDDEN = new Set();
  const el = $("node-detail");
  if (el) el.innerHTML = `<div class="skeleton skel-block"></div>`;
  load(false);
}

export function closeNodeDetail(): void {
  DETAIL_ID = "";
  DETAIL = null;
  DETAIL_ERR = null;
}

/* ── табы: hash-навигация и переключение панелей ──────────────────────
   Активный таб отражается в хэше как `?tab=<key>` внутри того же маршрута
   `#/nodes/<id>` (SPA владеет хэшем). writeTabHash идёт через replaceState —
   hashchange не срабатывает и маршрут не перезапускается. */
function parseTabHash(): string {
  const m = /[?&]tab=([a-z]+)/.exec(location.hash || "");
  return m && TAB_KEYS.includes(m[1]) ? m[1] : "overview";
}

function writeTabHash(name: string): void {
  if (!DETAIL_ID) return;
  const clean = (location.hash || "").replace(/[?&]tab=[a-z]+/, "");
  const base = clean || "#/nodes/" + encodeURIComponent(DETAIL_ID);
  const next = base + (base.includes("?") ? "&" : "?") + "tab=" + name;
  history.replaceState(null, "", location.pathname + location.search + next);
}

/** Переключить таб без полной перерисовки: скрыть/показать панели, обновить
 *  aria-selected + roving tabindex. focusPanel — клик фокусирует панель,
 *  стрелки оставляют фокус на табе (APG). */
function activateTab(name: string, focusPanel: boolean): void {
  if (!TAB_KEYS.includes(name)) name = "overview";
  DETAIL_TAB = name;
  document.querySelectorAll<HTMLButtonElement>("#node-detail .nd-tabs [role='tab']").forEach((b) => {
    const on = b.dataset.tab === name;
    b.setAttribute("aria-selected", String(on));
    b.tabIndex = on ? 0 : -1;
  });
  document.querySelectorAll<HTMLElement>("#node-detail [data-tab-panel]").forEach((p) => {
    p.hidden = p.dataset.tabPanel !== name;
  });
  writeTabHash(name);
  if (focusPanel) document.getElementById("nd-panel-" + name)?.focus({ preventScroll: true });
}

// Клик и стрелки — делегированы на document: innerHTML-перерисовка опроса
// (5 с) сносит слушатели, привязанные к nav.
document.addEventListener("click", (e) => {
  const tab = (e.target as HTMLElement)?.closest?.("#node-detail .nd-tabs [role='tab']") as HTMLElement | null;
  if (!tab || !DETAIL_ID) return;
  activateTab(tab.dataset.tab || "overview", true);
});
document.addEventListener("keydown", (e) => {
  const tabs = Array.from(document.querySelectorAll<HTMLButtonElement>("#node-detail .nd-tabs [role='tab']"));
  const idx = tabs.indexOf(document.activeElement as HTMLButtonElement);
  if (idx < 0) return;
  let next = -1;
  if (e.key === "ArrowRight") next = (idx + 1) % tabs.length;
  else if (e.key === "ArrowLeft") next = (idx - 1 + tabs.length) % tabs.length;
  else if (e.key === "Home") next = 0;
  else if (e.key === "End") next = tabs.length - 1;
  if (next < 0) return;
  e.preventDefault();
  tabs[next].focus();
  activateTab(tabs[next].dataset.tab || "overview", false);
});

function load(poll: boolean): void {
  if (!DETAIL_ID) return;
  const id = DETAIL_ID;
  api.nodeDetail(id)
    .then((d) => {
      if (id !== DETAIL_ID) return; // ушли на другую ноду
      DETAIL = d;
      DETAIL_ERR = null;
      DETAIL_RETRY = false;
      render(poll);
    })
    .catch((e: Error & { status?: number; code?: string }) => {
      if (id !== DETAIL_ID) return;
      if (e.message === "unauthorized") return;
      // 404: нода чужая, удалена или не существует — не подтверждаем наличие.
      DETAIL_ERR = { msg: e.status === 404 ? "Нода не найдена или недоступна." : e.message, status: e.status };
      DETAIL = null;
      render(poll);
      if (poll && e.status !== 404 && !DETAIL_RETRY && (e.status ?? 0) >= 500) {
        DETAIL_RETRY = true;
        setTimeout(() => load(true), 2000);
      }
    });
}

/** Перезагрузить открытую карточку (опрос из общего цикла кабинета). */
export function pollNodeDetail(): void {
  if (DETAIL_ID) load(true);
}

function render(poll: boolean): void {
  const el = $("node-detail");
  if (!el) return;
  renderSection(el, poll, () => {
    if (DETAIL_ERR) {
      el.innerHTML = `<div class="empty-state">${EMPTY_SVG}
        <span class="t">Не удалось открыть ноду</span>
        <span class="s">${esc(DETAIL_ERR.msg)}</span>
        <a class="btn sm" href="#/nodes" style="margin-top:6px">← К нодам</a></div>`;
      return;
    }
    if (!DETAIL) { el.innerHTML = `<div class="skeleton skel-block"></div>`; return; }
    el.innerHTML = detailHTML(DETAIL);
  });
}

function fmtBytes(n: number | null | undefined): string {
  if (n === null || n === undefined) return "нет данных";
  const u = ["Б", "КБ", "МБ", "ГБ", "ТБ"];
  let v = n, i = 0;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${u[i]}`;
}

function fmtPct(n: number | null | undefined): string {
  return n === null || n === undefined ? "—" : `${n.toFixed(1)}%`;
}

function nd(label: string): string {
  return `<div class="kv"><span class="kv-k">${esc(label)}</span><span class="kv-v dim">нет данных</span></div>`;
}

function chip(ok: boolean, okText: string, badText: string): string {
  return ok ? `<span class="badge ok">${esc(okText)}</span>` : `<span class="badge bad">${esc(badText)}</span>`;
}

/** Текстовый эквивалент графика (DESIGN.md §13.4): тот же ряд значений теми
 *  же форматтерами, что в видимой части. Показывается скринридеру. */
function srTable(caption: string, headers: string[], rows: string[][]): string {
  return `<table class="sr-only"><caption>${esc(caption)}</caption>
    <thead><tr>${headers.map((h) => `<th>${esc(h)}</th>`).join("")}</tr></thead>
    <tbody>${rows.map((r) => `<tr>${r.map((c) => `<td>${esc(c)}</td>`).join("")}</tr>`).join("")}</tbody></table>`;
}

/** Статичная легенда «цвет + текст» (DESIGN §13.4 — <span>, не только цвет). */
function legend(items: [string, string][]): string {
  return `<div class="chart-legend">${items.map(([name, color]) =>
    `<span class="legend-static"><span class="legend-dot" style="background:${color}"></span>${esc(name)}</span>`).join("")}</div>`;
}

function detailHTML(d: NodeDetail): string {
  const n = d.node;
  if (!n) {
    return `<div class="empty-state">${EMPTY_SVG}<span class="t">Нода вне пула</span>
      <span class="s">Параметры недоступны — нода не зарегистрирована в пуле.</span></div>`;
  }
  const meta = STATUS_META[n.status] || STATUS_META.offline;
  const nt = n.node_type ? NODE_TYPE_META[n.node_type] : undefined;
  const q = n.quarantine;

  // Шапка со статусом — всегда над табами.
  const head = `
  <div class="card nd-head">
    <div>
      <div class="nd-ip-lead"><span class="mono">${esc(n.ip || "—")}${n.port ? ":" + n.port : ""}</span>
        · зарегистрирована ${esc(fmtAgo(n.registered_at))}</div>
      <div class="nd-ip">нода <span class="mono">${esc(n.node_id)}</span></div>
    </div>
    <div class="nd-chips">
      <span class="badge ${meta.cls}"><i></i>${esc(meta.label)}</span>
      ${n.is_master ? `<span class="badge gold">★ мастер</span>` : ""}
      ${nt ? `<span class="badge ${nt.cls}" title="${esc(nt.title)}">${esc(nt.label)}</span>` : ""}
      ${q ? `<span class="badge warn"><i></i>карантин</span>` : ""}
    </div>
    ${n.unhealthy_reason ? `<div class="nd-reason">${esc(n.unhealthy_reason)}</div>` : ""}
    <button class="btn danger sm" data-del="${esc(n.node_id)}" style="margin-left:auto">Удалить ноду</button>
  </div>`;

  const tabs = `<nav class="nd-tabs" role="tablist" aria-label="Разделы ноды">${TABS.map((t) => {
    const on = t.key === DETAIL_TAB;
    return `<button type="button" role="tab" id="nd-tab-${t.key}" data-tab="${t.key}" aria-controls="nd-panel-${t.key}" aria-selected="${on}" tabindex="${on ? 0 : -1}">${esc(t.label)}</button>`;
  }).join("")}</nav>`;

  const panels = TABS.map((t) => {
    const on = t.key === DETAIL_TAB;
    return `<div class="nd-panel" id="nd-panel-${t.key}" role="tabpanel" aria-labelledby="nd-tab-${t.key}" tabindex="-1" data-tab-panel="${t.key}"${on ? "" : " hidden"}>${panelHTML(t.key, d)}</div>`;
  }).join("");

  return head + tabs + panels;
}

/** Содержимое панели по ключу таба. Категории сохраняют исходный порядок. */
function panelHTML(key: string, d: NodeDetail): string {
  switch (key) {
    case "metrics": return chartCHTML(d) + hostingHTML(d) + telemtHTML(d) + controlApiHTML(d);
    case "checks":  return checksHTML(d);
    case "journal": return eventsHTML(d);
    case "actions": return actionsHTML(d);
    default:        return paramsHTML(d) + chartAHTML(d) + ipHistoryHTML(d);
  }
}

function paramsHTML(_d: NodeDetail): string {
  const n = _d.node!;
  const q = n.quarantine;
  return `<section class="card">
    <div class="card-head"><h2>Параметры</h2><span class="hint">${esc(n.node_id)}</span></div>
    <div class="nd-grid">
      <div class="kv"><span class="kv-k">IP</span><span class="kv-v mono">${esc(n.ip || "—")}</span></div>
      <div class="kv"><span class="kv-k">Порт</span><span class="kv-v mono">${n.port || "—"}</span></div>
      <div class="kv"><span class="kv-k">Регион</span><span class="kv-v">${esc(n.region || "—")}</span></div>
      <div class="kv"><span class="kv-k">Аптайм</span><span class="kv-v">${esc(fmtDur(n.uptime_sec))}</span></div>
      <div class="kv"><span class="kv-k">Heartbeat</span><span class="kv-v">${esc(fmtDur(n.heartbeat_age_sec))} назад</span></div>
      <div class="kv"><span class="kv-k">Отчёт</span><span class="kv-v">${n.report_age_sec < 0 ? "—" : esc(fmtDur(n.report_age_sec)) + " назад"}</span></div>
      <div class="kv"><span class="kv-k">Роль</span><span class="kv-v">${n.is_master ? "мастер" : n.queue_position > 0 ? `очередь #${n.queue_position}` : "вне очереди"}</span></div>
      <div class="kv"><span class="kv-k">Домены</span><span class="kv-v mono">${esc((n.master.domains || []).join(", ") || "—")}</span></div>
      <div class="kv"><span class="kv-k">Слоты</span><span class="kv-v">${n.master.slots_filled} / ${n.master.slots_total}</span></div>
      <div class="kv"><span class="kv-k">TTL мастерства</span><span class="kv-v">${n.master.ttl_remaining_sec != null ? esc(fmtDur(n.master.ttl_remaining_sec)) : "—"}</span></div>
      <div class="kv"><span class="kv-k">Мастерство</span><span class="kv-v">${n.master.master_stints > 0 ? esc(fmtDur(n.master.master_time_sec)) + ` · ${n.master.master_stints} stint` : "—"}</span></div>
      <div class="kv"><span class="kv-k">Quarantine</span><span class="kv-v">${q ? `${q.attempts} попыток · с ${esc(fmtClock(new Date(q.entered_at * 1000).toISOString()))}` : "—"}</span></div>
    </div>
  </section>`;
}

function ipHistoryHTML(d: NodeDetail): string {
  return `<section class="card">
    <div class="card-head"><h2>История IP</h2><span class="hint">по журналу событий</span></div>
    ${(d.ip_history && d.ip_history.length)
      ? `<div class="table-wrap"><table class="table-cards">
          <thead><tr><th>IP</th><th>Впервые</th><th>Последний раз</th><th class="num">Событий</th></tr></thead>
          <tbody>${d.ip_history.map((r) => `<tr>
            <td class="mono" data-label="IP">${esc(r.ip || "—")}</td>
            <td class="mono" data-label="Впервые">${esc(fmtClock(r.first_seen))}</td>
            <td class="mono" data-label="Последний раз">${esc(fmtClock(r.last_seen))}</td>
            <td class="num" data-label="Событий">${fmtNum(r.events)}</td>
          </tr>`).join("")}</tbody></table></div>`
      : `<div class="table-empty">${d.ip_history === null ? "нет данных" : "записей нет"}</div>`}
  </section>`;
}

function checksHTML(d: NodeDetail): string {
  return `<section class="card">
    <div class="card-head"><h2>Последние проверки</h2><span class="hint">tcp · globalping · отчёт</span></div>
    ${d.last_checks.length
      ? `<div class="table-wrap"><table class="table-cards">
          <thead><tr><th>Время</th><th>Источник</th><th>Итог</th><th>Детали</th></tr></thead>
          <tbody>${d.last_checks.map((c) => `<tr>
            <td class="mono" data-label="Время">${esc(fmtClock(c.at))}</td>
            <td data-label="Источник">${esc(sourceLabel(c.source))}</td>
            <td data-label="Итог">${chip(c.ok, "ок", "провал")}</td>
            <td class="wrap" data-label="Детали">${esc(c.detail || "—")}</td>
          </tr>`).join("")}</tbody></table></div>`
      : `<div class="table-empty">проверок пока нет</div>`}
  </section>`;
}

function eventsHTML(d: NodeDetail): string {
  return `<section class="card">
    <div class="card-head"><h2>События ноды</h2><span class="hint">последние ${d.events.length}</span></div>
    <ul class="feed" id="nd-events" style="max-height:none">
      ${d.events.length ? d.events.map(eventRow).join("") : `<li class="empty-state"><span class="t">Событий пока нет</span></li>`}
    </ul>
  </section>`;
}

function actionsHTML(_d: NodeDetail): string {
  return `<section class="card">
    <div class="card-head"><h2>Действия</h2><span class="hint">проверка · конфиг · переустановка</span></div>
    <div class="nd-grid card-body">${nd("Действия ноды")}</div>
  </section>`;
}

function sourceLabel(s: string): string {
  switch (s) {
    case "tcp": return "TCP-проба";
    case "globalping": return "Globalping";
    case "health_report": return "Отчёт metrics";
    default: return s;
  }
}

// §3 «Доступность, 7 дней»: плитки + график A (столбики по дням, цвет — здоровье дня).
function chartAHTML(d: NodeDetail): string {
  const a = d.availability;
  const head = `<div class="card-head"><h2>Доступность, 7 дней</h2>` +
    (a ? `<span class="hint">окно ${a.window_days} ${plural(a.window_days, "день", "дня", "дней")}</span>` : "") + `</div>`;
  if (!a) return `<section class="card">${head}<div class="table-empty">нет данных</div></section>`;

  const days = a.daily;
  const tiles = `<div class="metrics" style="margin-bottom:14px">
      <div class="card metric">
        <div class="metric-label">Общая доступность</div>
        <div class="metric-value ${availColor(a.overall_pct)}" style="font-size:24px">${a.overall_pct.toFixed(1)}%</div>
        <div class="metric-foot">отчётов ok: ${a.reports_ok}/${a.reports_total}</div>
      </div>
      <div class="card metric">
        <div class="metric-label">GP верификации</div>
        <div class="metric-value ${availColor(a.gp_verified_pct)}" style="font-size:24px">${a.gp_verified_pct.toFixed(1)}%</div>
        <div class="metric-foot">проверок: ${a.gp_checks_total}</div>
      </div>
    </div>`;

  let chart: string;
  if (days.length < 2) {
    chart = `<span class="dim">накапливается — пока мало точек</span>`;
  } else {
    const rows = days.map((day) => {
      const pct = Math.max(0, Math.min(100, day.ok_pct));
      return { bars: [{ value: pct, cls: availColor(pct) }] };
    });
    chart = `<div class="chart-wrap" role="img" aria-label="Доступность за ${days.length} дней, средняя ${a.overall_pct.toFixed(1)} процента">
        <div class="chart-plot">
          <div class="chart-y" aria-hidden="true"><span>100%</span><span>0%</span></div>
          ${barChart(rows, { max: 100 })}
        </div>
        <div class="chart-x" aria-hidden="true">${days.map((day) => {
          const [, m, dd] = day.date.split("-");
          return `<span>${esc(dd)}.${esc(m)}</span>`;
        }).join("")}</div>
      </div>
      ${srTable("Доступность по дням", ["День", "OK %", "TCP ok/total", "Отчёты ok/total"],
        days.map((day) => [day.date, fmtPct(day.ok_pct), `${day.tcp_ok}/${day.tcp_total}`, `${day.reports_ok}/${day.reports_total}`]))}
      ${days.length < a.window_days ? `<p class="card-note">данные за ${days.length} из ${a.window_days} ${plural(a.window_days, "дня", "дней", "дней")}</p>` : ""}`;
  }

  return `<section class="card">${head}
    <div class="card-body">${tiles}
      <div class="chart-cap">По дням <span class="hint">${days.length} ${plural(days.length, "день", "дня", "дней")}</span></div>
      ${chart}
    </div>
  </section>`;
}

function availColor(p: number): string { return p >= 99 ? "ok" : p >= 95 ? "warn" : "bad"; }

// §6 «Соединения, 24 ч»: плитки + график C (линия clients). me-writers удалён
// по требованию владельца — поле writers нигде не рисуется (specs §5).
function chartCHTML(d: NodeDetail): string {
  const c = d.connections_24h;
  const head = `<div class="card-head"><h2>Соединения, 24 ч</h2><span class="hint">соединения · уникальные IP</span></div>`;
  if (!c) return `<section class="card">${head}<div class="table-empty">нет данных</div></section>`;

  const series = c.series;
  const clients = series.map((p) => p.clients);
  // Верх оси — эффективная шкала линии (dataMax + запас), не сырой максимум (L1).
  const top = chartTop(clients);
  const kv = (label: string, v: number | undefined): string =>
    `<div class="kv"><span class="kv-k">${esc(label)}</span><span class="kv-v">${v == null ? "нет данных" : fmtNum(v)}</span></div>`;

  let chart: string;
  if (clients.length < 2) {
    chart = `<span class="dim">накапливается — пока мало точек</span>`;
  } else {
    const x = series.map((p) => fmtClock(p.at));
    const live = liveLineChart(x, [{ name: "Соединения", values: clients, texts: clients.map((v) => fmtNum(v)) }]);
    chart = `<div class="chart-wrap" role="img" aria-label="Соединения за 24 часа, максимум ${fmtNum(top)}">
        <div class="chart-plot">
          <div class="chart-y" aria-hidden="true"><span>${fmtNum(top)}</span><span>0</span></div>
          ${live}
        </div>
      </div>
      ${srTable("Соединения за 24 часа", ["Время", "Соединения"], series.map((p) => [fmtClock(p.at), fmtNum(p.clients)]))}`;
  }

  return `<section class="card">${head}
    <div class="card-body">
      <div class="nd-grid" style="padding:0;margin-bottom:12px">
        ${kv("Соединения пользователей", c.current_user_connections)}
        ${kv("Уникальные IP (сейчас)", c.current_unique_ips)}
      </div>
      <div class="chart-cap">Соединения по отчётам <span class="hint">${series.length} ${plural(series.length, "точка", "точки", "точек")}</span></div>
      ${chart}
      ${c.note ? `<div class="card-note" style="padding:10px 0 0">${esc(c.note)}</div>` : ""}
    </div>
  </section>`;
}

// §7 «Хостинг»: плитки текущих значений + графики D (CPU/RAM и сеть) из
// cpu_ram_series/net_series. Пустые серии → честное «данные копятся», не холст.
function hostingHTML(d: NodeDetail): string {
  const h = d.hosting;
  const head = `<div class="card-head"><h2>Хостинг: CPU / RAM / сеть</h2><span class="hint">текущие значения · 24 ч</span></div>`;
  if (!h) return `<section class="card">${head}<div class="table-empty">нет данных</div></section>`;

  const tiles = `<div class="nd-grid" style="padding:0 0 2px">
      <div class="kv"><span class="kv-k">CPU</span><span class="kv-v mono">${esc(fmtPct(d.absent.includes("hosting.cpu") ? null : h.cpu_percent))}</span></div>
      <div class="kv"><span class="kv-k">ОЗУ (RAM)</span><span class="kv-v mono">${esc(fmtPct(d.absent.includes("hosting.ram") ? null : h.ram_percent))}</span></div>
      <div class="kv"><span class="kv-k">Сеть — принято</span><span class="kv-v mono">${esc(fmtBytes(h.net_ingress_bytes_total))}</span></div>
      <div class="kv"><span class="kv-k">Сеть — отдано</span><span class="kv-v mono">${esc(fmtBytes(h.net_egress_bytes_total))}</span></div>
    </div>`;

  const cr = h.cpu_ram_series ?? [];
  let cpuRam: string;
  if (cr.length < 2) {
    cpuRam = `<div class="chart-cap">CPU и RAM <span class="hint">%</span></div>
      <span class="dim">данные копятся — пока мало точек</span>`;
  } else {
    const x = cr.map((p) => fmtClock(p.at));
    const series: LiveSeries[] = [
      { name: "CPU", color: "var(--acc)", values: cr.map((p) => p.cpu_percent), texts: cr.map((p) => fmtPct(p.cpu_percent)) },
      { name: "RAM", color: "var(--info)", values: cr.map((p) => p.ram_percent), texts: cr.map((p) => fmtPct(p.ram_percent)) },
    ];
    cpuRam = `<div class="chart-cap">CPU и RAM <span class="hint">%</span></div>
      ${legend([["CPU", "var(--acc)"], ["RAM", "var(--info)"]])}
      <div class="chart-wrap" role="img" aria-label="CPU и RAM за 24 часа, в процентах, точек ${cr.length}">
        <div class="chart-plot">
          <div class="chart-y" aria-hidden="true"><span>100%</span><span>0%</span></div>
          ${liveLineChart(x, series, { max: 100 })}
        </div>
      </div>
      ${srTable("Хостинг — CPU и RAM", ["Время", "CPU %", "RAM %"],
        cr.map((p) => [fmtClock(p.at), fmtPct(p.cpu_percent), fmtPct(p.ram_percent)]))}`;
  }

  // §7 Сеть: пончик принято/отдано с долями. Данные — сумма net_series за 24 ч,
  // иначе fallback на накопительные *_total. Сумма 0 → честное «нет данных».
  const net = h.net_series ?? [];
  const sum = (k: "ingress_bytes" | "egress_bytes"): number =>
    net.length ? net.reduce((s, p) => s + (p[k] ?? 0), 0) : (h[k === "ingress_bytes" ? "net_ingress_bytes_total" : "net_egress_bytes_total"] ?? 0);
  const ing = sum("ingress_bytes");
  const egr = sum("egress_bytes");
  const netTotal = ing + egr;
  let netHTML: string;
  if (netTotal <= 0) {
    netHTML = `<div class="chart-cap">Сеть <span class="hint">принято / отдано</span></div>
      <span class="dim">нет данных — трафик не зафиксирован</span>`;
  } else {
    const ingPct = (ing / netTotal) * 100;
    const egrPct = 100 - ingPct;
    const ingText = `${ingPct.toFixed(1)}%`;
    const egrText = `${egrPct.toFixed(1)}%`;
    netHTML = `<div class="chart-cap">Сеть <span class="hint">принято / отдано, за 24 ч</span></div>
      <div class="donut-wrap" role="img" aria-label="Сеть за 24 часа: принято ${ingText} (${esc(fmtBytes(ing))}), отдано ${egrText} (${esc(fmtBytes(egr))})">
        <div class="donut-plot">${donutChart([
          { name: "Принято", value: ing, color: "var(--info)" },
          { name: "Отдано", value: egr, color: "var(--acc)" },
        ])}</div>
        <div class="chart-legend donut-legend">
          <span class="legend-static"><span class="legend-dot" style="background:var(--info)"></span>Принято <b class="mono">${ingText}</b> · ${esc(fmtBytes(ing))}</span>
          <span class="legend-static"><span class="legend-dot" style="background:var(--acc)"></span>Отдано <b class="mono">${egrText}</b> · ${esc(fmtBytes(egr))}</span>
        </div>
      </div>
      ${srTable("Хостинг — сеть, доли за 24 часа", ["Принято", "Отдано", "доля %"],
        [[fmtBytes(ing), fmtBytes(egr), `${ingText} / ${egrText}`]])}`;
  }

  return `<section class="card">${head}
    <div class="card-body">${tiles}${cpuRam}${netHTML}</div>
  </section>`;
}

// §8 график E: горизонтальные столбики — уникальные IP по пользователям
// (текущий снимок, не временной ряд: заголовок честный).
function chartEHTML(d: NodeDetail): string {
  const list = d.users?.unique_ips_by_user ?? null;
  if (!list || !list.length) {
    return `<div class="table-empty">${d.users === null ? "нет данных" : "пользователей нет"}</div>`;
  }
  const max = Math.max(...list.map((x) => x.unique_ips), 1);
  const bars = list.map((x) => `<div class="h-bar" title="${esc(x.user)} — ${fmtNum(x.unique_ips)} IP" aria-hidden="true">
      <span class="h-bar-k mono">${esc(x.user)}</span>
      <span class="h-bar-track"><span class="h-bar-fill" style="width:${Math.max(0, Math.min(100, (x.unique_ips / max) * 100)).toFixed(1)}%"></span></span>
      <span class="h-bar-v">${fmtNum(x.unique_ips)}</span>
    </div>`).join("");
  return `<div class="chart-wrap h-bars" role="img" aria-label="Уникальные IP по пользователям, текущий снимок, максимум ${fmtNum(max)}">
      ${bars}
    </div>
    ${srTable("Уникальные IP по пользователям (сейчас)", ["Пользователь", "Уникальных IP"],
      list.map((x) => [x.user, fmtNum(x.unique_ips)]))}`;
}

// §8 график F: сгруппированные столбики — трафик по пользователям за 24 ч
// (итоги, не временной ряд). Легенда — кнопки видимости серий (specs §6.3).
const F_SERIES = [
  { key: "ingress", label: "Принято", color: "var(--info)" },
  { key: "egress", label: "Отдано", color: "var(--acc)" },
] as const;

function chartFHTML(d: NodeDetail): string {
  const list = d.users?.traffic_by_user ?? null;
  if (!list || !list.length) {
    return `<div class="table-empty">${d.users === null ? "нет данных" : "пользователей нет"}</div>`;
  }
  const series = F_SERIES.map((s) => ({ ...s, visible: !F_HIDDEN.has(s.key) }));
  const rows = list.map((u) => ({
    bars: series.filter((s) => s.visible).map((s) => ({
      value: s.key === "ingress" ? u.ingress_bytes : u.egress_bytes,
      color: s.color,
    })),
  }));
  const max = Math.max(...list.flatMap((u) => [u.ingress_bytes, u.egress_bytes]), 1);
  const legend = `<div class="chart-legend">${series.map((s) =>
    `<button type="button" class="legend-btn" data-series="${s.key}" aria-pressed="${s.visible}">
      <span class="legend-dot" style="background:${s.color}"></span>${esc(s.label)}
    </button>`).join("")}</div>`;
  // Потолок ширины: 1–2 пользователя не должны растягивать столбики в плиты
  // (SVG preserveAspectRatio="none" иначе тянет их на всю карточку). CSS:
  // min(100%, Npx) — на узком экране всё равно 100%, на широком — потолок.
  const capW = list.length * 92 + 44;
  const chart = series.some((s) => s.visible)
    ? `<div class="chart-wrap" role="img" aria-label="Трафик по пользователям за 24 часа, принято и отдано, максимум ${fmtBytes(max)}"
          style="max-width:min(100%,${capW}px);margin-inline:auto">
        <div class="chart-plot">
          <div class="chart-y" aria-hidden="true"><span>${esc(fmtBytes(max))}</span><span>0</span></div>
          ${barChart(rows, { height: 32 })}
        </div>
        <div class="chart-x" aria-hidden="true">${list.map((u) => `<span class="mono">${esc(u.user)}</span>`).join("")}</div>
      </div>`
    : `<span class="dim">серии скрыты</span>`;
  return `${legend}${chart}
    ${srTable("Трафик по пользователям за 24 часа", ["Пользователь", "Принято", "Отдано"],
      list.map((u) => [u.user, fmtBytes(u.ingress_bytes), fmtBytes(u.egress_bytes)]))}`;
}

// §8 таблица «Пользователи»: объединяет уникальные IP и 24-ч трафик по имени.
function usersTable(d: NodeDetail): string {
  const uniq = d.users?.unique_ips_by_user ?? [];
  const traffic = d.users?.traffic_by_user ?? null;
  const names: string[] = [];
  const seen = new Set<string>();
  for (const x of [...uniq, ...(traffic ?? [])]) {
    if (!seen.has(x.user)) { seen.add(x.user); names.push(x.user); }
  }
  if (!names.length) return `<div class="table-empty">${d.users === null ? "нет данных" : "пользователей нет"}</div>`;
  const uMap = new Map(uniq.map((x) => [x.user, x.unique_ips]));
  const tMap = new Map((traffic ?? []).map((x) => [x.user, x]));
  const trafficCols = traffic
    ? `<th class="num">Принято</th><th class="num">Отдано</th>`
    : "";
  const body = names.map((n) => {
    const t = tMap.get(n);
    const trafficCells = traffic
      ? `<td class="num" data-label="Принято">${t ? esc(fmtBytes(t.ingress_bytes)) : "—"}</td>
        <td class="num" data-label="Отдано">${t ? esc(fmtBytes(t.egress_bytes)) : "—"}</td>`
      : "";
    return `<tr>
      <td class="mono" data-label="Пользователь">${esc(n)}</td>
      <td class="num" data-label="Уникальных IP">${uMap.has(n) ? fmtNum(uMap.get(n)) : "—"}</td>
      ${trafficCells}
    </tr>`;
  }).join("");
  return `<div class="table-wrap"><table class="table-cards">
      <thead><tr><th>Пользователь</th><th class="num">Уникальных IP</th>${trafficCols}</tr></thead>
      <tbody>${body}</tbody></table></div>`;
}

// Переключение серий графика F по кнопкам легенды (клавиатура — нативная).
document.addEventListener("click", (e) => {
  const btn = (e.target as HTMLElement)?.closest?.("[data-series]") as HTMLElement | null;
  if (!btn || !DETAIL_ID) return;
  const key = btn.dataset.series || "";
  if (F_HIDDEN.has(key)) F_HIDDEN.delete(key); else F_HIDDEN.add(key);
  render(false);
});

/* ── Наведение на линейные графики (C, хостинг) ───────────────────────
   Делегировано на document: innerHTML-перерисовка (5 с) сносит слушатели,
   привязанные к SVG. Всё из data-live (JSON из chart.ts): индекс — по доле
   ширины, точка — по доле высоты (шкала та же, что в SVG: pad 3 к h). Клавиатура:
   точки не фокусируем (role="img" сворачивает график в один образ, а данные
   даёт sr-only-таблица) — путь для клавиатуры/скринридера = таблица. */
let LIVE_EL: HTMLElement | null = null;

interface LivePayload {
  h: number; pad: number; max: number; x: string[];
  series: { name: string; color: string; v: (number | null)[]; t: (string | null)[] | null }[];
}

function liveHide(): void {
  const el = LIVE_EL;
  LIVE_EL = null;
  if (!el || !el.isConnected) return;
  (el.querySelector(".chart-cross") as HTMLElement | null)?.setAttribute("hidden", "");
  (el.querySelector(".chart-tip") as HTMLElement | null)?.setAttribute("hidden", "");
  const dots = el.querySelector(".chart-dots");
  if (dots) dots.innerHTML = "";
}

function liveShow(canvas: HTMLElement, clientX: number): void {
  let p: LivePayload;
  try { p = JSON.parse(canvas.dataset.live || ""); } catch { return; }
  const n = p.x.length;
  if (n < 2) return;
  if (LIVE_EL && LIVE_EL !== canvas) liveHide();
  LIVE_EL = canvas;

  const rect = canvas.getBoundingClientRect();
  if (rect.width < 1) return;
  const frac = Math.max(0, Math.min(1, (clientX - rect.left) / rect.width));
  const idx = Math.round(frac * (n - 1));
  const leftPct = (idx / (n - 1)) * 100;
  const plotH = p.h - p.pad * 2;

  const cross = canvas.querySelector(".chart-cross") as HTMLElement | null;
  if (cross) { cross.hidden = false; cross.style.left = leftPct + "%"; }

  const dots = canvas.querySelector(".chart-dots") as HTMLElement | null;
  if (dots) {
    dots.innerHTML = "";
    for (const s of p.series) {
      const v = s.v[idx];
      if (typeof v !== "number" || !isFinite(v)) continue;
      const yPct = ((p.pad + (1 - v / p.max) * plotH) / p.h) * 100;
      const dot = document.createElement("span");
      dot.className = "chart-dot";
      dot.style.left = leftPct + "%";
      dot.style.top = yPct + "%";
      dot.style.background = s.color;
      dots.appendChild(dot);
    }
  }

  const tip = canvas.querySelector(".chart-tip") as HTMLElement | null;
  if (!tip) return;
  tip.hidden = false;
  tip.textContent = "";
  const cap = document.createElement("div");
  cap.className = "tip-t";
  cap.textContent = p.x[idx] || "";
  tip.appendChild(cap);
  for (const s of p.series) {
    const v = s.v[idx];
    const row = document.createElement("div");
    row.className = "tip-r";
    const dot = document.createElement("span");
    dot.className = "tip-dot";
    dot.style.background = s.color;
    const name = document.createElement("span");
    name.textContent = s.name;
    const val = document.createElement("b");
    val.textContent = typeof v === "number" && isFinite(v)
      ? (s.t && s.t[idx] != null ? String(s.t[idx]) : String(v)) : "—";
    row.append(dot, name, val);
    tip.appendChild(row);
  }
  // M1: клампим по РЕАЛЬНОЙ ширине тултипа (translateX(-50%)), иначе на ≤640px
  // широкий многосерийный тултип вылезает за карточку. CSS max-width:100% держит
  // halfW ≤ canvasWidth/2, поэтому обе границы не выворачиваются.
  const halfW = tip.offsetWidth / 2;
  const cx = (leftPct / 100) * rect.width;
  tip.style.left = Math.min(Math.max(cx, halfW), Math.max(rect.width - halfW, halfW)) + "px";
}

function onChartPointer(e: PointerEvent): void {
  const canvas = (e.target as HTMLElement)?.closest?.(".chart-canvas[data-live]") as HTMLElement | null;
  if (!canvas || !DETAIL_ID) { liveHide(); return; }
  liveShow(canvas, e.clientX);
}
document.addEventListener("pointermove", onChartPointer);
document.addEventListener("pointerdown", onChartPointer);
document.addEventListener("mouseleave", liveHide);
// L2: тач не шлёт mouseleave — по отпусканию/отмене жеста прячем оверлей.
function onChartUp(e: PointerEvent): void {
  if (e.pointerType === "touch") liveHide();
}
document.addEventListener("pointerup", onChartUp);
document.addEventListener("touchcancel", liveHide);

function telemtHTML(d: NodeDetail): string {
  const t = d.telemt;
  const snap = t?.metrics_snapshot;
  const keys = snap ? Object.keys(snap).sort() : [];
  const ca = t?.control_api ?? null;
  const dcs = ca?.dcs ?? [];
  const endpoints = dcs.reduce((s, x) => s + x.available_endpoints, 0);
  const tile = (label: string, v: string): string =>
    `<div class="kv"><span class="kv-k">${esc(label)}</span><span class="kv-v">${v}</span></div>`;

  const summary = `<div class="nd-grid" style="padding:0">
      ${tile("Метрик в снимке", snap ? String(keys.length) : "нет данных")}
      ${tile("Собрано", t?.collected_at ? esc(fmtAgo(t.collected_at)) : "—")}
      ${tile("ДЦ", ca ? String(dcs.length) : "нет данных")}
      ${tile("Endpoints (доступно)", ca ? fmtNum(endpoints) : "нет данных")}
    </div>`;

  const snapshot = keys.length ? `<details class="op-step-log" style="margin:12px var(--pad) var(--pad)">
      <summary>Показать снимок (${keys.length})</summary>
      <div class="table-wrap"><table class="table-cards">
        <thead><tr><th>Метрика</th><th class="num">Значение</th></tr></thead>
        <tbody>${keys.map((k) => `<tr>
          <td class="mono wrap" data-label="Метрика">${esc(k)}</td>
          <td class="num mono" data-label="Значение">${fmtNum(snap![k])}</td>
        </tr>`).join("")}</tbody></table></div>
    </details>` : "";

  return `<section class="card">
    <div class="card-head"><h2>Статистика telemt</h2><span class="hint">снимок метрик · пользователи</span></div>
    <div class="card-body" style="padding-bottom:0">
      ${summary}
      <div class="chart-cap">Уникальные IP по пользователям <span class="hint">сейчас</span></div>
      ${chartEHTML(d)}
      <div class="chart-cap">Трафик по пользователям <span class="hint">за 24 часа</span></div>
      ${chartFHTML(d)}
      <div class="chart-cap">Пользователи <span class="hint">IP · трафик 24 ч</span></div>
      ${usersTable(d)}
    </div>
    ${snapshot}
  </section>`;
}

function controlApiHTML(d: NodeDetail): string {
  const ca = d.telemt?.control_api;
  const gone = d.absent.includes("telemt_control_api") || d.absent.includes("dc_endpoints") || d.absent.includes("quality_health");
  if (!ca || !ca.ok || gone) {
    return `<section class="card"><div class="card-head"><h2>telemt Control API</h2></div>
      <div class="table-empty">нет данных</div></section>`;
  }
  const at = new Date(ca.generated_at_epoch_secs * 1000).toISOString();
  return `<section class="card">
    <div class="card-head"><h2>telemt Control API</h2><span class="hint">снимок DC</span></div>
    <div class="nd-grid card-body" style="padding-bottom:0">
      <div class="kv"><span class="kv-k">Middle-proxy</span><span class="kv-v">${chip(ca.middle_proxy_enabled, "включён", "выключен")}</span></div>
      <div class="kv"><span class="kv-k">Снимок</span><span class="kv-v">${esc(fmtAgo(at))}</span></div>
    </div>
    ${ca.dcs.length
      ? `<div class="table-wrap"><table class="table-cards">
          <thead><tr><th>ДЦ</th><th class="num">Endpoints</th><th class="num">Alive</th><th class="num">Fresh</th><th class="num">RTT</th></tr></thead>
          <tbody>${ca.dcs.map((x) => `<tr>
            <td data-label="ДЦ">${fmtNum(x.dc)}</td>
            <td class="num" data-label="Endpoints">${fmtNum(x.available_endpoints)}</td>
            <td class="num" data-label="Alive">${fmtNum(x.alive_writers)}</td>
            <td class="num" data-label="Fresh">${fmtNum(x.fresh_alive_writers)}</td>
            <td class="num" data-label="RTT">${x.rtt_ms.toFixed(1)} ms</td>
          </tr>`).join("")}</tbody></table></div>`
      : `<div class="table-empty">ДЦ нет</div>`}
  </section>`;
}

function eventRow(ev: { at: string; type: string; domain?: string; detail?: string }): string {
  const m = EV_LABEL[ev.type] || { label: ev.type, dot: "" };
  const parts = [ev.domain].filter(Boolean).map((v) => `<code>${esc(v)}</code>`);
  const detail = ev.detail ? esc(ev.detail) : "";
  const line = [parts.join(" "), detail].filter(Boolean).join(parts.length && detail ? " — " : "");
  return `<li class="feed-item">
    <span class="feed-dot ${m.dot}"></span>
    <div class="feed-body">
      <div class="feed-head"><span class="feed-kind">${esc(m.label)}</span>
        <span class="feed-time" title="${esc(ev.at)}">${esc(fmtClock(ev.at))}</span></div>
      ${line ? `<div class="feed-detail">${line}</div>` : ""}
    </div></li>`;
}
