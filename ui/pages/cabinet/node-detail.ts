/* Детальная карточка ноды в кабинете (plan-ui-batch.md Фаза 2, slice 1).
   Данные — `GET /api/me/nodes/{id}/detail` (node_detail_api.go): владелец
   видит свои ноды, чужая/legacy → 404. Разделы без источника приходят null и
   перечислены в absent[] — рисуем «нет данных», не выдумываем значения.
   Разметка — существующие .card/.table-cards/.badge/.metrics/.kv/.feed.
   Опрос каждые 5 с идёт через renderSection: не сбивает фокус и прокрутку. */

import { $, esc, renderSection } from "../../lib/dom";
import { fmtAgo, fmtClock, fmtDur, fmtNum, plural } from "../../lib/format";
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

let DETAIL_ID = "";
let DETAIL: NodeDetail | null = null;
let DETAIL_ERR: { msg: string; status?: number } | null = null;
let DETAIL_RETRY = false;

export function nodeDetailId(): string { return DETAIL_ID; }

/** Открыть детальную карточку ноды: сброс предыдущей, затем загрузка. */
export function openNodeDetail(id: string): void {
  DETAIL_ID = id;
  DETAIL = null;
  DETAIL_ERR = null;
  DETAIL_RETRY = false;
  const el = $("node-detail");
  if (el) el.innerHTML = `<div class="skeleton skel-block"></div>`;
  load(false);
}

export function closeNodeDetail(): void {
  DETAIL_ID = "";
  DETAIL = null;
  DETAIL_ERR = null;
}

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

function spark(pts: number[]): string {
  if (pts.length < 2) return `<span class="dim">накапливается — пока мало точек</span>`;
  const max = Math.max(...pts, 1);
  const step = 100 / (pts.length - 1);
  const d = pts.map((v, i) => `${(i * step).toFixed(1)},${(26 - 3 - (v / max) * 20).toFixed(1)}`).join(" ");
  return `<svg class="metric-spark" viewBox="0 0 100 26" preserveAspectRatio="none">
    <polyline points="${d}" fill="none" stroke="var(--acc)" stroke-width="1.6"
      stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/></svg>`;
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

  return `
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
  </div>

  <section class="card">
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
  </section>

  ${availabilityHTML(d)}

  <section class="card">
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
  </section>

  <section class="card">
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
  </section>

  <section class="card">
    <div class="card-head"><h2>Хостинг</h2><span class="hint">CPU / RAM / сеть</span></div>
    <div class="nd-grid">
      <div class="kv"><span class="kv-k">CPU</span><span class="kv-v mono">${esc(fmtPct(d.absent.includes("hosting.cpu") ? null : d.hosting?.cpu_percent))}</span></div>
      <div class="kv"><span class="kv-k">ОЗУ (RAM)</span><span class="kv-v mono">${esc(fmtPct(d.absent.includes("hosting.ram") ? null : d.hosting?.ram_percent))}</span></div>
      <div class="kv"><span class="kv-k">Сеть — принято</span><span class="kv-v mono">${esc(fmtBytes(d.hosting?.net_ingress_bytes_total))}</span></div>
      <div class="kv"><span class="kv-k">Сеть — отдано</span><span class="kv-v mono">${esc(fmtBytes(d.hosting?.net_egress_bytes_total))}</span></div>
    </div>
  </section>

  ${connectionsHTML(d)}

  <section class="card">
    <div class="card-head"><h2>Уникальные IP по пользователям</h2><span class="hint">из снимка telemt</span></div>
    ${(d.users?.unique_ips_by_user?.length)
      ? `<div class="table-wrap"><table class="table-cards">
          <thead><tr><th>Пользователь</th><th class="num">Уникальных IP</th></tr></thead>
          <tbody>${d.users!.unique_ips_by_user.map((u) => `<tr>
            <td class="mono" data-label="Пользователь">${esc(u.user)}</td>
            <td class="num" data-label="Уникальных IP">${fmtNum(u.unique_ips)}</td>
          </tr>`).join("")}</tbody></table></div>`
      : `<div class="table-empty">${d.users === null ? "нет данных" : "пользователей нет"}</div>`}
    <div class="card-head"><h2>Трафик по пользователям</h2><span class="hint">за 24 часа</span></div>
    ${(d.users?.traffic_by_user?.length && !d.absent.includes("users.traffic_by_user"))
      ? `<div class="table-wrap"><table class="table-cards">
          <thead><tr><th>Пользователь</th><th class="num">Принято</th><th class="num">Отдано</th></tr></thead>
          <tbody>${d.users!.traffic_by_user!.map((u) => `<tr>
            <td class="mono" data-label="Пользователь">${esc(u.user)}</td>
            <td class="num" data-label="Принято">${esc(fmtBytes(u.ingress_bytes))}</td>
            <td class="num" data-label="Отдано">${esc(fmtBytes(u.egress_bytes))}</td>
          </tr>`).join("")}</tbody></table></div>`
      : `<div class="table-empty">${d.users === null ? "нет данных" : "пользователей нет"}</div>`}
  </section>

  ${telemtHTML(d)}

  <section class="card">
    <div class="card-head"><h2>DC и endpoints</h2></div>
    <div class="nd-grid card-body">${nd("DC и endpoints")}</div>
  </section>

  <section class="card">
    <div class="card-head"><h2>Качество и здоровье</h2></div>
    <div class="nd-grid card-body">${nd("Качество и здоровье")}</div>
  </section>

  <section class="card">
    <div class="card-head"><h2>События ноды</h2><span class="hint">последние ${d.events.length}</span></div>
    <ul class="feed" id="nd-events" style="max-height:none">
      ${d.events.length ? d.events.map(eventRow).join("") : `<li class="empty-state"><span class="t">Событий пока нет</span></li>`}
    </ul>
  </section>

  <section class="card">
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

function availabilityHTML(d: NodeDetail): string {
  const a = d.availability;
  if (!a) {
    return `<section class="card"><div class="card-head"><h2>Доступность</h2></div>
      <div class="table-empty">нет данных</div></section>`;
  }
  const bars = a.daily.map((day) => {
    const pct = Math.max(0, Math.min(100, day.ok_pct));
    const cls = pct >= 99 ? "ok" : pct >= 95 ? "warn" : "bad";
    const [, m, dd] = day.date.split("-");
    const title = `${day.date}: ok ${day.ok_pct.toFixed(1)}% (tcp ${day.tcp_ok}/${day.tcp_total}, отчёты ${day.reports_ok}/${day.reports_total})`;
    return `<div class="day-bar" title="${esc(title)}">
      <div class="day-bar-track"><div class="day-bar-fill ${cls}" style="height:${pct}%"></div></div>
      <span class="dim">${esc(dd)}.${esc(m)}</span></div>`;
  }).join("");
  const overallCls = availColor(a.overall_pct);
  return `<section class="card">
    <div class="card-head"><h2>Доступность</h2><span class="hint">окно ${a.window_days} дней</span></div>
    <div class="card-body">
      <div class="metrics" style="margin-bottom:14px">
        <div class="card metric">
          <div class="metric-label">Общая доступность</div>
          <div class="metric-value ${overallCls}" style="font-size:24px">${a.overall_pct.toFixed(1)}%</div>
          <div class="metric-foot">отчётов ok: ${a.reports_ok}/${a.reports_total}</div>
        </div>
        <div class="card metric">
          <div class="metric-label">GP верификации</div>
          <div class="metric-value ${availColor(a.gp_verified_pct)}" style="font-size:24px">${a.gp_verified_pct.toFixed(1)}%</div>
          <div class="metric-foot">проверок: ${a.gp_checks_total}</div>
        </div>
      </div>
      <div class="chart-cap">По дням <span class="hint">${a.daily.length} ${plural(a.daily.length, "день", "дня", "дней")}</span></div>
      ${a.daily.length ? `<div class="day-bars">${bars}</div>` : `<span class="dim">пока нет точек за окно</span>`}
    </div>
  </section>`;
}

function availColor(p: number): string { return p >= 99 ? "ok" : p >= 95 ? "warn" : "bad"; }

function connectionsHTML(d: NodeDetail): string {
  const c = d.connections_24h;
  if (!c) {
    return `<section class="card"><div class="card-head"><h2>Соединения 24ч</h2></div>
      <div class="table-empty">нет данных</div></section>`;
  }
  const series = c.series.map((p) => p.writers);
  const kv = (label: string, v: number | undefined): string =>
    `<div class="kv"><span class="kv-k">${esc(label)}</span><span class="kv-v">${v === undefined || v === null ? "нет данных" : fmtNum(v)}</span></div>`;
  return `<section class="card">
    <div class="card-head"><h2>Соединения 24ч</h2></div>
    <div class="card-body">
      <div class="nd-grid" style="padding:0;margin-bottom:12px">
        ${kv("Писатели (сейчас)", c.current_writers)}
        ${kv("Уникальные IP (сейчас)", c.current_unique_ips)}
        ${kv("Соединения пользователей", c.current_user_connections)}
      </div>
      <div class="chart-cap">Writers по отчётам <span class="hint">${c.series.length} ${plural(c.series.length, "точка", "точки", "точек")}</span></div>
      ${spark(series)}
      ${c.note ? `<div class="card-note" style="padding:10px 0 0">${esc(c.note)}</div>` : ""}
    </div>
  </section>`;
}

function telemtHTML(d: NodeDetail): string {
  const t = d.telemt;
  const snap = t?.metrics_snapshot;
  const keys = snap ? Object.keys(snap).sort() : [];
  if (!t || !snap || !keys.length) {
    return `<section class="card"><div class="card-head"><h2>Статистика telemt</h2></div>
      <div class="table-empty">нет данных</div></section>`;
  }
  return `<section class="card">
    <div class="card-head"><h2>Статистика telemt</h2><span class="hint">снимок метрик</span></div>
    <div class="card-body" style="padding-bottom:0">
      <div class="nd-grid" style="padding:0">
        <div class="kv"><span class="kv-k">Метрик в снимке</span><span class="kv-v">${keys.length}</span></div>
        <div class="kv"><span class="kv-k">Собрано</span><span class="kv-v">${t.collected_at ? esc(fmtAgo(t.collected_at)) : "—"}</span></div>
      </div>
    </div>
    <details class="op-step-log" style="margin:12px var(--pad) var(--pad)">
      <summary>Показать снимок (${keys.length})</summary>
      <div class="table-wrap"><table class="table-cards">
        <thead><tr><th>Метрика</th><th class="num">Значение</th></tr></thead>
        <tbody>${keys.map((k) => `<tr>
          <td class="mono wrap" data-label="Метрика">${esc(k)}</td>
          <td class="num mono" data-label="Значение">${fmtNum(snap![k])}</td>
        </tr>`).join("")}</tbody></table></div>
    </details>
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
