/* Экран «Журнал событий» + мини-лента на «Обзоре».

   Логика (справочник EV_META, группы фильтров, поиск по словам, сортировка)
   перенесена из прежней версии ДОСЛОВНО — meняется только разметка отдельной
   записи (вместо .events li теперь .feed .feed-item) и стили. */

import { $, esc } from "../../../lib/dom";
import { fmtClock } from "../../../lib/format";
import { state } from "../state";

/* группа (чипы): all/master/blocks/nodes/system */
let evFilter = "all";
/* тег: конкретный тип события */
let evTypeFilter = "";
/* ключевые слова (все слова обязаны найтись) */
let evSearch = "";

const EV_META: Record<string, { label: string; cls: string; grp: string }> = {
  registry_started:      { label: "Запуск регистратора",  cls: "ev-neutral", grp: "system" },
  config_changed:        { label: "Конфиг изменён",       cls: "ev-info",    grp: "system" },
  node_registered:       { label: "Регистрация ноды",     cls: "ev-info",    grp: "nodes" },
  node_replaced:         { label: "Нода вытеснена",       cls: "ev-warn",    grp: "nodes" },
  node_expired:          { label: "Нода отвалилась",      cls: "ev-warn",    grp: "nodes" },
  node_pruned:           { label: "Удалена (неактивна)",  cls: "ev-warn",    grp: "nodes" },
  tcp_down:              { label: "TCP недоступен",       cls: "ev-bad",     grp: "blocks" },
  tcp_up:                { label: "TCP восстановлен",     cls: "ev-good",    grp: "blocks" },
  metrics_down:          { label: "Метрики в отказе",     cls: "ev-bad",     grp: "blocks" },
  metrics_up:            { label: "Метрики восстановлены", cls: "ev-good",   grp: "blocks" },
  globalping_blocked:    { label: "Блокировка (GP)",      cls: "ev-bad",     grp: "blocks" },
  globalping_recovered:  { label: "GP восстановлен",      cls: "ev-good",    grp: "blocks" },
  queue_joined:          { label: "В очереди мастеров",   cls: "ev-info",    grp: "nodes" },
  queue_left:            { label: "Вышла из очереди",     cls: "ev-warn",    grp: "nodes" },
  master_elected:        { label: "МАСТЕР ИЗБРАН",        cls: "ev-good",    grp: "master" },
  master_lost:           { label: "Мастер потерян",       cls: "ev-bad",     grp: "master" },
  dns_updated:           { label: "DNS обновлён",         cls: "ev-info",    grp: "master" },
  dns_error:             { label: "Ошибка DNS",           cls: "ev-bad",     grp: "master" },
  dns_deleted:           { label: "DNS запись удалена",   cls: "ev-warn",    grp: "master" },
  node_quarantined:      { label: "Карантин (GP)",        cls: "ev-warn",    grp: "blocks" },
  quarantine_recovered:  { label: "Вышла из карантина",   cls: "ev-good",    grp: "blocks" },
  node_terminated:       { label: "Нода завершена (бан)", cls: "ev-bad",     grp: "nodes" },
  ban_lifted:            { label: "Бан снят (смена IP)",  cls: "ev-info",    grp: "nodes" },
  ip_blocked:            { label: "IP заблокирован (смена в карантине)", cls: "ev-bad", grp: "blocks" },
  srmd_domain_created:   { label: "СРМД: домен создан",   cls: "ev-info",    grp: "master" },
  srmd_domain_folded:    { label: "СРМД: домен свёрнут",  cls: "ev-warn",    grp: "master" },
  srmd_domain_unfolded:  { label: "СРМД: домен развёрнут", cls: "ev-good",   grp: "master" },
  srmd_domain_taken:     { label: "СРМД: домен взят под контроль", cls: "ev-info", grp: "master" },
  srmd_domain_released:  { label: "СРМД: домен в ручной режим", cls: "ev-warn", grp: "master" },
};

/* старый cls (ev-good/ev-bad/ev-warn/ev-info/ev-neutral) → цвет точки в новой ленте */
function dotClass(cls: string): string {
  switch (cls) {
    case "ev-good": return "ok";
    case "ev-bad": return "bad";
    case "ev-warn": return "warn";
    case "ev-info": return "info";
    default: return "";
  }
}

function renderEventRow(ev: any): string {
  const m = EV_META[ev.type] || { label: ev.type, cls: "ev-neutral", grp: "" };
  const idParts = [ev.node_id, ev.ip, ev.domain]
    .filter(Boolean)
    .map((v) => `<code>${esc(v)}</code>`)
    .join(" ");
  const detailText = ev.detail ? esc(ev.detail) : "";
  const line = [idParts, detailText].filter(Boolean).join(idParts && detailText ? " — " : "");
  return `<li class="feed-item">
    <span class="feed-dot ${dotClass(m.cls)}"></span>
    <div class="feed-body">
      <div class="feed-head">
        <span class="feed-kind">${esc(m.label)}</span>
        <span class="feed-time" title="${esc(ev.at)}">${fmtClock(ev.at)}</span>
      </div>
      ${line ? `<div class="feed-detail">${line}</div>` : ""}
    </div>
  </li>`;
}

/* Разделитель дня: «Сегодня» / «Вчера» / дата. */
function dayLabel(iso: string): string {
  const d = new Date(iso);
  const today = new Date();
  const same = (a: Date, b: Date) =>
    a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
  if (same(d, today)) return "Сегодня";
  const y = new Date(today); y.setDate(y.getDate() - 1);
  if (same(d, y)) return "Вчера";
  return d.toLocaleDateString("ru-RU", { day: "2-digit", month: "long" });
}

/* Плоский список превратился в ленту с осью времени: события одного дня
   собраны под липким разделителем, слева вертикальная линия с точками по
   семантике события. Данные и порядок прежние. */
export function renderInto(el: HTMLElement, list: any[]): void {
  if (!list || list.length === 0) {
    el.innerHTML = `<li class="empty-state"><span class="t">Событий пока нет</span></li>`;
    return;
  }
  let html = "";
  let day = "";
  for (const ev of list) {
    const d = dayLabel(ev.at);
    if (d !== day) {
      day = d;
      html += `<li class="feed-day"><span>${esc(day)}</span></li>`;
    }
    html += renderEventRow(ev);
  }
  el.innerHTML = html;
}

/* поиск по ключевым словам: каждое слово (через пробел) должно встретиться
   в типе/теге события, ноде, ip, домене или детали (регистр не важен) */
function eventMatchesSearch(ev: any, q: string): boolean {
  if (!q) return true;
  const meta = EV_META[ev.type] || ({} as any);
  const hay = [ev.type, meta.label || "", ev.node_id || "", ev.ip || "", ev.domain || "", ev.detail || ""]
    .join(" ").toLowerCase();
  return q.split(/\s+/).every((w) => hay.includes(w));
}

function filteredEvents(): any[] {
  return state.events.filter((ev: any) => {
    if (evFilter !== "all") {
      const meta = EV_META[ev.type];
      if (!meta || meta.grp !== evFilter) return false;
    }
    if (evTypeFilter && ev.type !== evTypeFilter) return false;
    return eventMatchesSearch(ev, evSearch);
  });
}

export function renderView(): void {
  const list = filteredEvents();
  renderInto($("events-list"), list);
  const filtered = evFilter !== "all" || evTypeFilter !== "" || evSearch !== "";
  $("ev-found").textContent = filtered
    ? `найдено: ${list.length} из ${state.events.length}`
    : `всего: ${state.events.length}`;
}

/* select тегов (типов событий) — из EV_META, по алфавиту */
function initEvTypeFilter(): void {
  const opts = Object.entries(EV_META).sort((a, b) => a[1].label.localeCompare(b[1].label, "ru"));
  $("ev-type-filter").innerHTML = `<option value="">Все события (тег)</option>` +
    opts.map(([code, m]) => `<option value="${esc(code)}" title="${esc(code)}">${esc(m.label)}</option>`).join("");
}

export function init(): void {
  initEvTypeFilter();

  $("ev-filters").addEventListener("click", (e) => {
    const b = (e.target as HTMLElement).closest<HTMLElement>("button[data-f]");
    if (!b) return;
    evFilter = b.dataset.f!;
    document.querySelectorAll("#ev-filters button").forEach((x) => x.classList.toggle("active", x === b));
    renderView();
  });
  $("ev-type-filter").addEventListener("change", (e) => {
    evTypeFilter = (e.target as HTMLSelectElement).value;
    renderView();
  });
  $("ev-search").addEventListener("input", (e) => {
    evSearch = (e.target as HTMLInputElement).value.trim().toLowerCase();
    renderView();
  });
}
