/* Публичная статистика пула (список нод и страница одной ноды).
   Логика — эндпоинты, расчёты, интерактивный спарклайн клиентов —
   перенесена из прежней версии дословно. Меняется только разметка и стили. */

import { $, esc, icon, ICONS } from "../../lib/dom";
import { fmtDur, fmtAgo, fmtClock, plural } from "../../lib/format";
import { initTheme } from "../../lib/theme";
import {
  connectionsCardHTML, fmtBytes, fmtPct, hostingCardHTML, installLiveCharts, userIPsBarsHTML, usersTableHTML,
} from "../../lib/node-load";
import type { NodeLoad } from "../../lib/node-load";

function chip(flag: boolean | null | undefined, okText?: string, noText?: string): string {
  if (flag === null || flag === undefined) return `<span class="badge">—</span>`;
  return flag ? `<span class="badge ok">${okText || "ok"}</span>` : `<span class="badge bad">${noText || "fail"}</span>`;
}
function availClass(p: number): string { return p >= 99 ? "ok" : p >= 95 ? "warn" : "bad"; }

/* Публичная подпись приходит с сервера: для пользовательской ноды —
   "login · Нода №N", для legacy/общей регистрации — прежний node_id. */
function nodeName(n: any): string { return String(n?.display_name || n?.node_id || "—"); }

const NODE_TYPE_META: Record<string, { label: string; cls: string; title: string }> = {
  classic:  { label: "Classic",  cls: "",        title: "классический telemt" },
  mtproxyl: { label: "MTProxyL", cls: "special",  title: "форк MTProxyL (/opt/mtproxyl/)" },
  meko:     { label: "MEKO",     cls: "info",     title: "фикс MEKO (/opt/mtpr-simple/)" },
  mtdd:     { label: "mtdd",     cls: "info",     title: "прокси mtdd (/etc/mtdd/)" },
};
function nodeTypeBadge(t: string): string {
  const m = NODE_TYPE_META[t];
  if (!m) return "";
  return `<span class="badge ${m.cls}" title="${esc(m.title)}">${esc(m.label)}</span>`;
}

const EV_META: Record<string, { label: string; cls: string }> = {
  registry_started:      { label: "Запуск регистратора",  cls: "" },
  config_changed:        { label: "Конфиг изменён",       cls: "info" },
  node_registered:       { label: "Регистрация ноды",     cls: "info" },
  node_replaced:         { label: "Нода вытеснена",       cls: "warn" },
  node_expired:          { label: "Нода отвалилась",      cls: "warn" },
  node_pruned:           { label: "Удалена (неактивна)",  cls: "warn" },
  tcp_down:              { label: "TCP недоступен",       cls: "bad" },
  tcp_up:                { label: "TCP восстановлен",     cls: "ok" },
  metrics_down:          { label: "Метрики в отказе",     cls: "bad" },
  metrics_up:            { label: "Метрики восстановлены", cls: "ok" },
  globalping_blocked:    { label: "Блокировка (GP)",      cls: "bad" },
  globalping_recovered:  { label: "GP восстановлен",      cls: "ok" },
  queue_joined:          { label: "В очереди мастеров",   cls: "info" },
  ip_blocked:            { label: "IP заблокирован (смена в карантине)", cls: "bad" },
  queue_left:            { label: "Вышла из очереди",     cls: "warn" },
  master_elected:        { label: "Мастер избран",        cls: "ok" },
  master_lost:           { label: "Мастер потерян",       cls: "bad" },
  dns_updated:           { label: "DNS обновлён",         cls: "info" },
  dns_error:             { label: "Ошибка DNS",           cls: "bad" },
  dns_deleted:           { label: "DNS запись удалена",   cls: "warn" },
};

async function pubApi(path: string): Promise<any> {
  const resp = await fetch(path, { cache: "no-store" });
  if (!resp.ok) { const e: any = new Error("HTTP " + resp.status); e.status = resp.status; throw e; }
  return resp.json();
}

/* ---------------- список нод (/statistics) ---------------- */

/* Три проверки одной группой вместо трёх колонок: подпись плюс
   подчёркивание цветом состояния. Значения не теряются — процент и
   защёлка metrics уезжают в подсказку. */
/* MTProto-проверка: блокировка — только RKN_BLOCK (прочие статусы сервиса
   не значат, что нода закрыта из РФ). */
function mtpState(status: string): string {
  if (status === "RKN_BLOCK") return "bad";
  if (status === "UNKNOWN" || status === "error") return "unknown";
  return "ok";
}

function mtpTitle(n: any): string {
  return `MTProto-проверка: ${n.mtproto_summary || n.mtproto_status}` +
    (n.mtproto_rescued ? " · Globalping провалена, MTProto не видит блокировки — без карантина" : "");
}

function checksHTML(n: any): string {
  const st = (flag: boolean | null | undefined) =>
    flag === null || flag === undefined ? "unknown" : flag ? "ok" : "bad";
  const tag = (label: string, state: string, title: string) =>
    `<span class="check-tag ${state}" title="${esc(title)}">${label}</span>`;
  return `<span class="checks">
    ${tag("TCP", st(n.healthy), n.healthy ? "порт отвечает" : "порт не отвечает")}
    ${tag("GP", st(n.globalping_ok), n.globalping_ok ? "верификация пройдена" : "верификация провалена")}
    ${n.mtproto_status ? tag("MTP", mtpState(n.mtproto_status), mtpTitle(n)) : ""}
    ${tag("METRICS", st(n.metrics_healthy), "защёлка metrics-здоровья: в отказ только после серии подряд плохих отчётов")}
  </span>`;
}

function trafficCell(inB: number | undefined, outB: number | undefined): string {
  const total = (inB ?? 0) + (outB ?? 0);
  return total > 0 ? `${esc(fmtBytes(total))}` : "—";
}

/* Сводка пула: итоги по всем нодам (клиенты — сумма по нодам: клиент,
   подключённый к двум нодам, считается дважды). */
function summaryHTML(s: any): string {
  const tile = (l: string, v: string, e: string, icn: string, t?: string): string =>
    `<div class="card metric"${t ? ` title="${esc(t)}"` : ""}>
      <div class="metric-label">${icon(icn, 13)}${esc(l)}</div>
      <div class="metric-value" style="font-size:20px">${v}</div>
      <div class="metric-foot">${esc(e)}</div></div>`;
  return `<div class="metrics" style="margin-bottom:16px">
    ${tile("Ноды", `${s.healthy}/${s.nodes}`, `в очереди мастерства · карантин ${s.quarantine}`, ICONS.health)}
    ${tile("Мастера", String(s.masters), "держат домены сейчас", ICONS.masters)}
    ${tile("Клиенты", fmtNum0(s.clients_sum), `соединений ${fmtNum0(s.user_connections)}`, ICONS.users,
      "сумма уникальных IP по нодам: клиент на двух нодах считается дважды")}
    ${tile("Трафик 24 ч", esc(fmtBytes((s.traffic_24h_in ?? 0) + (s.traffic_24h_out ?? 0))),
      `принято ${fmtBytes(s.traffic_24h_in ?? 0)} · отдано ${fmtBytes(s.traffic_24h_out ?? 0)}`, ICONS.swap)}
    ${tile("Нагрузка", `${esc(fmtPct(s.avg_cpu ?? null))}`, `CPU в среднем · RAM ${fmtPct(s.avg_ram ?? null)}`, ICONS.clock)}
  </div>`;
}

function fmtNum0(n: number | null | undefined): string {
  return n === null || n === undefined ? "—" : Math.round(n).toLocaleString("ru-RU");
}

function listRowHTML(n: any): string {
  const avail = n.availability_pct ?? 0;
  const role = n.is_master
    ? `<span class="badge gold" title="${esc((n.master_domains || []).join(", "))}">★ МАСТЕР</span>`
    : (n.queue_position > 0 ? `<span class="badge info mono">#${n.queue_position}</span>` : `<span class="badge">вне очереди</span>`);
  // когда мастера переключит таймер (принудительная ротация)
  const ttlRem = n.is_master && n.master_ttl_remaining_sec != null
    ? `<div class="dim" style="font-size:11px;margin-top:3px" title="принудительная ротация мастерства по master_ttl_minutes">⏱ ${
        n.master_ttl_remaining_sec <= 0 ? "переключение сейчас" : "переключение через " + fmtDur(n.master_ttl_remaining_sec)}</div>`
    : "";
  const clients = n.clients_unique_ips ?? null;
  return `<tr class="clickable${n.is_master ? " master" : ""}" data-id="${esc(n.node_id)}" title="открыть публичную статистику ноды">
    <td data-label="Нода" title="${esc(n.node_id)}">${esc(nodeName(n))}</td>
    <td class="mono" data-label="IP (маска)">${esc(n.ip)}${n.port ? ":" + n.port : ""}</td>
    <td data-label="Роль">${role}${ttlRem}</td>
    <td data-label="Проверки">${checksHTML(n)}</td>
    <td data-label="Доступность"><div class="meter"><div class="meter-track"><span class="meter-fill ${availClass(avail)}" style="width:${avail}%"></span></div><span class="meter-value mono">${avail.toFixed(1)}%</span></div></td>
    <td class="mono num" data-label="Клиенты">${clients === null ? "—" : clients}</td>
    <td class="mono num" data-label="CPU / RAM">${esc(fmtPct(n.cpu_percent ?? null))} / ${esc(fmtPct(n.ram_percent ?? null))}</td>
    <td class="mono num" data-label="Трафик 24 ч" title="принято / отдано">${trafficCell(n.traffic_24h_in, n.traffic_24h_out)}</td>
    <td class="mono" data-label="Отчёт">${n.report_age_sec < 0 ? "—" : fmtDur(n.report_age_sec) + " назад"}</td>
    <td data-label="Тип">${nodeTypeBadge(n.node_type)}</td>
  </tr>`;
}

/* ---------------- карточка «Ссылки (домены)» ----------------
   Поле `domains` появляется в /statistics/api/list только на новом
   бэкенде. Нет ключа — карточки нет вообще (страница байт-в-байт
   прежняя). Пустой массив — карточка с пустым состоянием. */

function masterChip(m: any): string {
  const uid = esc(nodeName(m));
  const live = m.healthy !== false; // отсутствие поля трактуем как «жив»
  const ip = m.ip_masked ? `<span class="mono dim">${esc(m.ip_masked)}</span>` : "";
  const st = live ? `<span class="badge ok">жив</span>` : `<span class="badge warn">мёртв</span>`;
  let ttl = "", ttlTitle = "";
  if (m.ttl_remaining_sec != null) {
    if (m.ttl_remaining_sec > 0) { ttl = `<span class="dim">⏱ ${fmtDur(m.ttl_remaining_sec)}</span>`; ttlTitle = " · TTL " + fmtDur(m.ttl_remaining_sec); }
    else { ttl = `<span class="dim">переключение сейчас</span>`; ttlTitle = " · TTL истёк"; }
  }
  const title = `${m.node_id ?? ""} · ${live ? "жив" : "мёртв"}${ttlTitle}`;
  return `<span class="chip link-master" title="${esc(title)}"><span class="mono">${uid}</span>${ip}${st}${ttl}</span>`;
}

function masterSort(a: any, b: any): number {
  const al = a.healthy !== false ? 0 : 1, bl = b.healthy !== false ? 0 : 1;
  if (al !== bl) return al - bl;
  return String(a.node_id ?? "").localeCompare(String(b.node_id ?? ""));
}

/* «N/M» → занятое/всего. Возвращает null, если строку разобрать нельзя. */
function parseSlots(sf: unknown): { used: number; total: number } | null {
  if (typeof sf !== "string" || !sf.includes("/")) return null;
  const [u, t] = sf.split("/").map((x) => parseInt(x, 10));
  if (!isFinite(u) || !isFinite(t)) return null;
  return { used: u, total: t };
}

function slotsCell(sf: unknown): string {
  const p = parseSlots(sf);
  if (!p) return sf == null ? `<span class="dim">—</span>` : `<span class="mono">${esc(sf)}</span>`;
  const full = p.total > 0 && p.used >= p.total;
  const pct = p.total > 0 ? Math.max(0, Math.min(100, (p.used / p.total) * 100)) : 0;
  return `<div class="meter"><div class="meter-track"><span class="meter-fill ${full ? "ok" : "warn"}" style="width:${pct.toFixed(1)}%"></span></div><span class="meter-value mono">${esc(sf)}</span></div>`;
}

function domainRowHTML(dm: any): string {
  const masters = [...(dm.masters || [])].sort(masterSort);
  const chips = masters.length
    ? `<span class="chips link-masters">${masters.map(masterChip).join("")}</span>`
    : "";
  const count = masters.length
    ? `<span class="badge">${masters.length} ${plural(masters.length, "мастер", "мастера", "мастеров")}</span>`
    : "";
  return `<tr>
    <td class="mono" data-label="Домен" title="${esc(dm.domain)}" style="white-space:nowrap">${esc(dm.domain)}</td>
    <td class="mono num" data-label="Порт">${dm.port != null ? esc(dm.port) : "—"}</td>
    <td data-label="Мастера">${chips ? chips + count : `<span class="dim">—</span>`}</td>
    <td data-label="Заполнено слотов">${slotsCell(dm.slots_filled)}</td>
  </tr>`;
}

function domainsCardHTML(domains: any[]): string {
  let masters = 0, filled = 0, total = 0, hasSlots = false;
  for (const dm of domains) {
    masters += (dm.masters || []).length;
    const p = parseSlots(dm.slots_filled);
    if (p) { filled += p.used; total += p.total; hasSlots = true; }
  }
  const summary = `ссылок: ${domains.length} · мастеров: ${masters}` + (hasSlots ? ` · слоты: ${filled}/${total}` : "");
  const body = domains.length
    ? `<div class="table-wrap"><table class="table-cards">
        <thead><tr><th>Домен</th><th>Порт</th><th>Мастера</th><th>Заполнено слотов</th></tr></thead>
        <tbody>${domains.map(domainRowHTML).join("")}</tbody></table></div>`
    : `<div class="empty-state">${icon(ICONS.info, 26)}<span class="t">Доменов-ссылок нет</span><span class="s">Managed-домены появятся здесь после настройки Cloudflare DNS.</span></div>`;
  return `<section class="card domains-card">
    <div class="card-head"><h2>Ссылки (домены)</h2><span class="hint">${esc(summary)}</span></div>
    ${body}
  </section>`;
}

async function renderListView(): Promise<void> {
  const d = await pubApi("/statistics/api/list");
  $("view-title").textContent = "Публичная статистика пула";
  const domainsCard = Array.isArray(d.domains) ? domainsCardHTML(d.domains) : "";
  const rows = (d.nodes || []).map(listRowHTML).join("");
  const quarantineRows = (d.quarantine || []).map((n: any) => `<tr>
    <td data-label="Нода" title="${esc(n.node_id)}">${esc(nodeName(n))}</td>
    <td class="mono" data-label="IP (маска)">${esc(n.ip)}</td>
    <td data-label="Причина"><span class="badge warn">${n.stale ? "ожидает свежий GP" : n.reverify ? "перепроверка IP" : "GP карантин"}</span></td>
    <td class="mono" data-label="Попытки">${n.attempts}/${n.max_attempts}</td>
    <td class="mono" data-label="Последний ratio">${Math.round((n.last_ratio || 0) * 100)}%</td>
    <td class="mono" data-label="В карантине">${fmtDur(n.age_sec)}</td>
  </tr>`).join("");
  $("root").innerHTML = `
    <div class="banner info">${icon(ICONS.info, 16)}<span>Открытая сводка пула: <b>IP-адреса замаскированы</b>. Клик по строке — подробная страница ноды.</span></div>
    ${d.summary ? summaryHTML(d.summary) : ""}
    ${Array.isArray(d.conn_cost_by_type) ? costByTypeHTML(d.conn_cost_by_type) : ""}
    ${domainsCard}
    <section class="card">
      <div class="card-head"><h2>Ноды пула</h2><span class="hint">${(d.nodes || []).length} шт</span></div>
      <div class="table-wrap"><table class="table-cards">
        <thead><tr><th>Нода</th><th>IP (маска)</th><th>Роль</th><th>Проверки</th><th>Доступность</th><th>Клиенты</th><th>CPU / RAM</th><th>Трафик 24 ч</th><th>Отчёт</th><th>Тип</th></tr></thead>
        <tbody id="pub-nodes-body">${rows || `<tr><td colspan="10" class="table-empty">пул пуст</td></tr>`}</tbody>
      </table></div>
    </section>
    <section class="card quarantine-card">
      <div class="card-head"><h2>Карантин</h2><span class="hint">${(d.quarantine || []).length} шт</span></div>
      <div class="table-wrap"><table class="table-cards">
        <thead><tr><th>Нода</th><th>IP (маска)</th><th>Причина</th><th>Попытки</th><th>Последний ratio</th><th>В карантине</th></tr></thead>
        <tbody>${quarantineRows || `<tr><td colspan="6" class="table-empty">карантин пуст</td></tr>`}</tbody>
      </table></div>
    </section>`;
  const body = $("pub-nodes-body");
  if (body) body.addEventListener("click", (e) => {
    const tr = (e.target as HTMLElement).closest<HTMLElement>("tr[data-id]");
    if (tr) location.href = "/statistics/" + encodeURIComponent(tr.dataset.id!);
  });
}

/* ---------------- страница ноды (/statistics/<id>) ---------------- */

function renderPubEvents(list: any[]): string {
  if (!list || list.length === 0) return `<li class="feed-item"><div class="feed-body dim">Событий пока нет</div></li>`;
  return list.map((ev) => {
    const m = EV_META[ev.type] || { label: ev.type, cls: "" };
    return `<li class="feed-item">
      <span class="feed-dot ${m.cls}"></span>
      <div class="feed-body">
        <div class="feed-head">
          <span class="feed-kind">${esc(m.label)}</span>
          <span class="feed-time" title="${esc(ev.at)}">${fmtClock(ev.at)}</span>
        </div>
        <div class="feed-detail">${ev.domain ? `<span class="feed-domain">${esc(ev.domain)}</span> ` : ""}${ev.detail ? esc(ev.detail) : ""}</div>
      </div>
    </li>`;
  }).join("");
}

async function renderNodeView(id: string): Promise<void> {
  const d = await pubApi("/statistics/api/node?id=" + encodeURIComponent(id));
  const n = d.node || {};
  $("view-title").textContent = "Нода · " + nodeName(n);
  const mdoms = n.master_domains || [];
  const clients = n.clients_unique_ips ?? null;
  const roleChip = n.is_master
    ? `<span class="badge gold" title="${esc(mdoms.join(", "))}">★ МАСТЕР${mdoms.length > 1 ? " ×" + mdoms.length : ""}</span>`
    : (n.queue_position > 0
        ? `<span class="badge info mono" title="fully healthy — ждёт назначения домена">очередь #${n.queue_position}</span>`
        : `<span class="badge">вне очереди</span>`);
  const hcFail = (d.hc && d.hc.fail_threshold) || "?";
  const mStreak = n.metrics_fail_streak || 0;
  const mChip = chip(n.metrics_healthy, "metrics ok", "metrics fail")
    + (mStreak > 0
      ? ` <span class="dim" style="font-size:11px">серия ${mStreak}/${hcFail}</span>`
      : "");
  const ttlRem = n.is_master && n.master_ttl_remaining_sec != null
    ? `<span class="dim" title="принудительная ротация мастерства по master_ttl_minutes"> · ⏱ ${
        n.master_ttl_remaining_sec <= 0 ? "переключение сейчас" : "переключение через " + fmtDur(n.master_ttl_remaining_sec)}</span>`
    : "";
  const chips = [
    chip(n.healthy, "TCP ok", "TCP fail"),
    chip(n.globalping_ok, "GP ok", "GP fail"),
    n.mtproto_status
      ? `<span title="${esc(mtpTitle(n))}">${chip(mtpState(n.mtproto_status) !== "bad", "MTP " + n.mtproto_status, "MTP " + n.mtproto_status)}</span>`
      : "",
    mChip,
    roleChip,
    nodeTypeBadge(n.node_type),
  ].join(" ") + ttlRem;
  const reason = n.unhealthy_reason
    ? `<div class="node-reason"><div class="banner warn">${icon(ICONS.info, 16)}<span>${esc(n.unhealthy_reason)}</span></div></div>` : "";

  const statDefs = [
    { l: "Доступность", v: `${(n.availability_pct ?? 0).toFixed(1)}%`, e: `отчётов ok: ${n.reports_ok ?? 0}/${n.reports_total ?? 0}`,
      cls: (n.availability_pct ?? 0) >= 99 ? "ok" : (n.availability_pct ?? 0) >= 95 ? "warn" : "bad",
      icn: ICONS.health,
      t: "ReportsOK/ReportsTotal × 100% за всё время жизни node_id" },
    { l: "GP верификации", v: `${(n.gp_verified_pct ?? 0).toFixed(1)}%`, e: `верификаций всего: ${n.gp_checks_total ?? 0}`,
      cls: (n.gp_verified_pct ?? 0) >= 99 ? "ok" : (n.gp_verified_pct ?? 0) >= 90 ? "warn" : "bad",
      icn: ICONS.check,
      t: "Доля успешных НЕЗАВИСИМЫХ верификаций Globalping" },
    { l: "Клиенты", v: clients === null ? "—" : String(clients),
      e: clients === null ? "нет данных" : "уникальные активные IP",
      icn: ICONS.users,
      t: "telemt_user_unique_ips_current (агрегат по пользователям)" },
    { l: "Мастерство", v: (n.master_stints ?? 0) > 0 ? fmtDur(n.master_time_sec) : "—", e: `stint'ов: ${n.master_stints ?? 0}`,
      icn: ICONS.masters,
      t: "Суммарное время в роли мастера домена" },
    { l: "Heartbeat", v: `${fmtDur(n.heartbeat_age_sec)} назад`, e: `всего: ${n.heartbeats_total ?? 0}`, cls: n.heartbeat_age_sec > 45 ? "bad" : "",
      icn: ICONS.clock },
    { l: "Отчёт", v: n.report_age_sec < 0 ? "—" : `${fmtDur(n.report_age_sec)} назад`, e: "metrics / globalping",
      icn: ICONS.info },
  ];
  const stats = statDefs.map((s) => `<div class="card metric"${s.t ? ` title="${esc(s.t)}"` : ""}>
      <div class="metric-label">${icon(s.icn, 13)}${esc(s.l)}</div>
      <div class="metric-value ${s.cls || ""}" style="font-size:20px">${s.v}</div>
      <div class="metric-foot">${esc(s.e)}</div></div>`).join("");

  const gp = d.gp_last || null;
  const gpHist = d.gp_hist || [];
  // Сводка строкой над таблицей: у колонки сбоку содержимого на четыре
  // строки, а у таблицы площадок — на двадцать, и снизу зияла пустота.
  let gpSide: string;
  if (gp) {
    gpSide = `<div class="gp-summary">
      <div class="gp-big ${gp.ok ? "ok" : "bad"}">${Math.round((gp.ratio || 0) * 100)}%</div>
      <div class="gp-facts">
        <div>${chip(gp.ok, "верификация пройдена", "верификация провалена")}</div>
        <div class="kv"><span class="kv-k">проб успешно</span><span class="kv-v">${gp.probes_ok}/${gp.probes_total}</span></div>
        <div class="kv"><span class="kv-k">проверено</span><span class="kv-v">${esc(fmtAgo(gp.at))}</span></div>
      </div>
    </div>`;
  } else {
    gpSide = `<div class="gp-summary">
      <div class="gp-facts">
        <div class="kv"><span class="kv-k">текущий статус</span><span class="kv-v">${chip(n.globalping_ok)}</span></div>
        ${n.gp_checks_total ? `<div class="kv"><span class="kv-k">ratio последней</span><span class="kv-v">${(n.globalping_ratio * 100).toFixed(0)}%</span></div>` : ""}
        <div class="dim">Детали по площадкам появятся после ближайшей верификации.</div>
      </div>
    </div>`;
  }

  const probesTbl = gp && (gp.probes || []).length
    ? `<div class="table-wrap"><table class="table-cards">
        <thead><tr><th>Площадка</th><th>Сеть</th><th>Статус пробы</th><th>HTTP</th><th>Итог</th></tr></thead>
        <tbody>${gp.probes.map((p: any) => `<tr>
          <td class="mono" data-label="Площадка">${esc(p.country || "?")}${p.city ? " · " + esc(p.city) : ""}</td>
          <td class="mono dim" data-label="Сеть">${esc(p.network || "—")}${p.asn ? " (AS" + p.asn + ")" : ""}</td>
          <td class="mono" data-label="Статус пробы">${esc(p.status || "—")}</td>
          <td class="mono" data-label="HTTP">${p.http_code || "—"}</td>
          <td data-label="Итог">${chip(p.ok)}</td>
        </tr>`).join("")}</tbody></table></div>`
    : `<div class="table-empty">строк проб нет${gp ? " (measurement ещё выполнялся, когда его скачали)" : ""}</div>`;

  const tcpHist = d.tcp_hist || [];
  const tcpTicks = tcpHist.slice(-96).map((p: any) =>
    `<div class="tick ${p.ok ? "" : "fail"}" title="${esc(fmtClock(p.at))} — ${p.ok ? "ok" : "FAIL"}"></div>`).join("");
  const gpBars = gpHist.map((p: any) => {
    const h = 6 + Math.round((p.ratio || 0) * 50);
    const cls = p.ok ? "" : (p.ratio >= 0.35 ? "mid" : "fail");
    return `<div class="gbar ${cls}" style="height:${h}px" title="${esc(fmtClock(p.at))} — ratio ${(p.ratio * 100).toFixed(0)}% (${p.probes_ok}/${p.probes_total})"></div>`;
  }).join("");
  const repHist = d.report_hist || [];
  const repTicks = repHist.slice(-96).map((p: any) =>
    `<div class="tick ${p.metrics_ok ? "" : "fail"}" title="${esc(fmtClock(p.at))} — metrics ${p.metrics_ok ? "ok" : "FAIL"}"></div>`).join("");

  $("root").innerHTML = `
    <div class="banner info">${icon(ICONS.info, 16)}<span><a href="/statistics">← все ноды пула</a> · IP-адрес замаскирован.</span></div>
    <div class="card node-head">
      <div>
        <div class="node-id" title="${esc(n.node_id)}">${esc(nodeName(n))}</div>
        <div class="node-ip">${n.display_name && n.display_name !== n.node_id ? `<span class="mono">${esc(n.node_id)}</span> · ` : ""}${esc(n.ip)}${n.port ? ":" + n.port : ""} · зарегистрирована ${esc(fmtAgo(n.registered_at))}</div>
      </div>
      <div class="node-chips">${chips}</div>
      ${reason}
    </div>
    <div class="metrics" style="margin-bottom:16px">${stats}</div>
    ${loadHTML(d.load ?? null)}
    ${connCostHTML(d.conn_cost ?? null, d.proxy_host ?? null)}
    <section class="card" style="margin-bottom:16px">
      <div class="card-head"><h2>Globalping — независимая верификация</h2></div>
      ${gpSide}${probesTbl}
    </section>
    <section class="card" style="margin-bottom:16px">
      <div class="card-head"><h2>История проверок</h2></div>
      <div class="chart-body">
        <div class="chart-cap">Globalping — ratio по верификациям <span class="hint">всего ${gpHist.length}</span></div>
        <div class="bars">${gpBars || '<span class="dim">пока нет верификаций</span>'}</div>
        <div class="chart-cap">TCP-probe регистратора <span class="hint">последние ${Math.min(tcpHist.length, 96)}</span></div>
        <div class="ticks">${tcpTicks || '<span class="dim">пока нет данных</span>'}</div>
        <div class="chart-cap">Отчёты metrics <span class="hint">последние ${Math.min(repHist.length, 96)}</span></div>
        <div class="ticks">${repTicks || '<span class="dim">пока нет данных</span>'}</div>
      </div>
    </section>
    <section class="card">
      <div class="card-head"><h2>События ноды</h2><span class="hint">последние 100</span></div>
      <ul class="feed" style="max-height:none">${renderPubEvents(d.events || [])}</ul>
    </section>`;
}

/* Нагрузка ноды — те же блоки, что в кабинете (ui/lib/node-load.ts). */
function loadHTML(load: NodeLoad | null): string {
  if (!load) return "";
  const users = `<section class="card">
      <div class="card-head"><h2>Пользователи пула</h2><span class="hint">уникальные IP · трафик 24 ч</span></div>
      <div class="card-body">
        <div class="chart-cap">Уникальные IP по пользователям <span class="hint">сейчас</span></div>
        ${userIPsBarsHTML(load)}
        <div class="chart-cap">Пользователи <span class="hint">IP · трафик 24 ч</span></div>
        ${usersTableHTML(load)}
      </div>
    </section>`;
  return `<div class="load-stack">${hostingCardHTML(load)}${connectionsCardHTML(load)}${users}</div>`;
}

/* ── «Цена 1000 соединений» ────────────────────────────────────────────
   CPU и память — самого процесса прокси (не всего хоста), сеть — трафик
   прокси. «В среднем» = средний ресурс / среднее число соединений × 1000
   (вместе с фоном процесса); «предельная» = наклон регрессии × 1000 —
   сколько добавляет тысяча соединений сверх фона. */

type CostKey = "host_cpu_pct" | "host_ram_pct" | "cpu_cores" | "ram_mb" | "net_mbps"
  | "host_cpu_pct_per_100mbps" | "cpu_cores_per_100mbps";
/* Сервер (%) — по данным любых агентов; процесс прокси (ядра, МБ) — только
   у агентов с телеметрией процесса: без данных строка не показывается. */
const COST_ROWS: { key: CostKey; label: string; unit: string; digits: number; optional?: boolean }[] = [
  { key: "host_cpu_pct", label: "CPU сервера", unit: "%", digits: 1 },
  { key: "host_ram_pct", label: "RAM сервера", unit: "%", digits: 1 },
  { key: "net_mbps", label: "Сеть", unit: "Мбит/с", digits: 2 },
  { key: "cpu_cores", label: "CPU процесса прокси", unit: "ядра", digits: 3, optional: true },
  { key: "ram_mb", label: "Память процесса прокси", unit: "МБ", digits: 1, optional: true },
];

/* CPU на 100 Мбит/с трафика: убирает разницу в том, сколько качают
   клиенты, — остаётся цена байта (в основном шифрование). */
const NET_COST_ROWS: typeof COST_ROWS = [
  { key: "host_cpu_pct_per_100mbps", label: "CPU сервера", unit: "%", digits: 2 },
  { key: "cpu_cores_per_100mbps", label: "CPU процесса прокси", unit: "ядра", digits: 4, optional: true },
];

function costVal(v: number | null | undefined, digits: number, unit: string): string {
  return v === null || v === undefined ? "—" : `${v.toFixed(digits)} ${unit}`;
}

function connCostHTML(c: any, host: any): string {
  const head = `<div class="card-head"><h2>Цена 1000 соединений</h2><span class="hint">${c ? c.window_hours : 24} ч</span></div>`;
  if (!c) {
    return `<section class="card" style="margin-bottom:16px">${head}<div class="table-empty">данные копятся — первая точка через минуту после отчёта ноды</div></section>`;
  }
  const costRows = (defs: typeof COST_ROWS): string => defs.filter((r) => !r.optional || (c[r.key]?.points ?? 0) > 0).map((r) => {
    const f = c[r.key] || {};
    const r2 = f.r2 == null ? "—" : f.r2.toFixed(2);
    const base = f.baseline == null ? "—" : `${f.baseline.toFixed(r.digits)} ${r.unit}`;
    return `<tr>
      <td data-label="Ресурс">${esc(r.label)}</td>
      <td class="num mono" data-label="В среднем">${costVal(f.avg, r.digits, r.unit)}</td>
      <td class="num mono" data-label="Предельная">${costVal(f.marginal, r.digits, r.unit)}</td>
      <td class="num mono" data-label="Фон">${base}</td>
      <td class="num mono" data-label="R²">${r2}</td>
    </tr>`;
  }).join("");
  const rows = costRows(COST_ROWS);
  const netRows = costRows(NET_COST_ROWS);
  const hostBits: string[] = [];
  if (host?.cpu_count != null) hostBits.push(`${host.cpu_count} ${plural(host.cpu_count, "ядро", "ядра", "ядер")}`);
  if (host?.ram_total_bytes != null) hostBits.push(`RAM ${fmtBytes(host.ram_total_bytes)}`);
  if (host?.proxy_processes != null) hostBits.push(`процессов прокси ${host.proxy_processes}`);
  return `<section class="card" style="margin-bottom:16px">${head}
    <div class="table-wrap"><table class="table-cards">
      <thead><tr><th>Ресурс</th><th class="num" title="средний ресурс / среднее число соединений × 1000, вместе с фоном">В среднем</th>
        <th class="num" title="наклон регрессии × 1000: сколько добавляет тысяча соединений сверх фона">Предельная</th>
        <th class="num" title="ресурс процесса при нуле соединений (оценка)">Фон</th><th class="num" title="насколько линейная модель объясняет данные (1 — идеально)">R²</th></tr></thead>
      <tbody>${rows}</tbody></table></div>
    <div class="chart-cap" style="margin:14px var(--pad) 7px">CPU на 100 Мбит/с трафика <span class="hint">цена байта, без влияния того, сколько качают клиенты</span></div>
    <div class="table-wrap"><table class="table-cards">
      <thead><tr><th>Ресурс</th><th class="num">В среднем</th><th class="num">Предельная</th><th class="num">Фон</th><th class="num">R²</th></tr></thead>
      <tbody>${netRows || `<tr><td colspan="5" class="table-empty">нет данных — нода не присылает счётчики трафика</td></tr>`}</tbody></table></div>
    <div class="card-note">По ${c.points} ${plural(c.points, "точке", "точкам", "точкам")} (точка в минуту): соединений от ${fmtNum0(c.conns_min)} до ${fmtNum0(c.conns_max)}, в среднем ${fmtNum0(c.conns_avg)}.
      Предельная цена появляется, когда соединений было заметно разное число. Проценты — от всего сервера: сравнимы только между серверами одного размера.${hostBits.length ? " Хост: " + esc(hostBits.join(" · ")) + "." : ""}</div>
  </section>`;
}

const TYPE_LABEL: Record<string, string> = { classic: "Classic", mtproxyl: "MTProxyL", meko: "MEKO", mtdd: "mtdd", unknown: "неизвестно" };

function costByTypeHTML(list: any[]): string {
  const head = `<div class="card-head"><h2>Цена 1000 соединений по типам прокси</h2><span class="hint">медиана по нодам · 24 ч</span></div>`;
  if (!list.length) {
    return `<section class="card" style="margin-bottom:16px">${head}<div class="table-empty">данные копятся — первые точки появятся через минуту после отчётов нод</div></section>`;
  }
  const cell = (s: any, digits: number, unit: string): string => {
    const avg = costVal(s?.avg, digits, unit);
    const marg = s?.marginal == null ? "" : `<div class="dim" style="font-size:11px">предельная ${costVal(s.marginal, digits, unit)} (${s.marginal_nodes})</div>`;
    return `${avg}${marg}`;
  };
  // Колонки процесса прокси — только когда хоть один тип их прислал.
  const proc = list.some((t) => (t.cpu_cores?.avg_nodes ?? 0) > 0 || (t.ram_mb?.avg_nodes ?? 0) > 0);
  const rows = list.map((t) => `<tr>
      <td data-label="Тип">${esc(TYPE_LABEL[t.node_type] || t.node_type)}</td>
      <td class="num mono" data-label="Нод">${t.nodes}</td>
      <td class="num mono" data-label="CPU сервера">${cell(t.host_cpu_pct, 1, "%")}</td>
      <td class="num mono" data-label="RAM сервера">${cell(t.host_ram_pct, 1, "%")}</td>
      <td class="num mono" data-label="Сеть">${cell(t.net_mbps, 2, "Мбит/с")}</td>
      <td class="num mono" data-label="CPU на 100 Мбит/с">${cell(t.host_cpu_pct_per_100mbps, 2, "%")}</td>
      ${proc ? `<td class="num mono" data-label="CPU прокси">${cell(t.cpu_cores, 3, "ядра")}</td>
      <td class="num mono" data-label="Память прокси">${cell(t.ram_mb, 1, "МБ")}</td>` : ""}
    </tr>`).join("");
  return `<section class="card" style="margin-bottom:16px">${head}
    <div class="table-wrap"><table class="table-cards">
      <thead><tr><th>Тип</th><th class="num">Нод</th><th class="num">CPU сервера</th><th class="num">RAM сервера</th><th class="num">Сеть</th><th class="num" title="CPU сервера на 100 Мбит/с трафика прокси">CPU на 100 Мбит/с</th>${
        proc ? `<th class="num">CPU прокси</th><th class="num">Память прокси</th>` : ""}</tr></thead>
      <tbody>${rows}</tbody></table></div>
    <div class="card-note">Проценты — от всего сервера (вместе с ОС): сравнение типов честное, когда серверы одного размера.${
      proc ? " CPU и память прокси — самого процесса прокси, сравнимы между любыми серверами." : ""} Сеть зависит от того, что делают клиенты, а не только от прокси.</div>
  </section>`;
}

/* ---------------- каркас ---------------- */

const pathId = decodeURIComponent(location.pathname.replace(/^\/statistics\/?/, "").replace(/\/+$/, ""));

async function refreshCurrent(): Promise<void> {
  const y = window.scrollY;
  const pill = $("live-pill");
  try {
    if (pathId) { await renderNodeView(pathId); } else { await renderListView(); }
    pill.className = "status-pill live";
    $("upd").textContent = "обновлено " + new Date().toLocaleTimeString("ru-RU");
  } catch (e: any) {
    if (e && e.status === 404 && pathId) {
      $("root").innerHTML = `<div class="card"><div class="empty-state">${icon(ICONS.info, 26)}<span class="t">нода не найдена</span><span class="s">Нода <b>${esc(pathId)}</b> не найдена в пуле (или уже удалена).</span></div><div class="card-foot"><a class="btn sm" href="/statistics">← к списку нод</a></div></div>`;
    } else {
      $("root").innerHTML = `<div class="card"><div class="empty-state">${icon(ICONS.info, 26)}<span class="t">не удалось загрузить данные</span><span class="s">${esc(e.message || e)}</span></div></div>`;
    }
    pill.className = "status-pill off";
    $("upd").textContent = "ошибка обновления";
  }
  window.scrollTo(0, y);
}

initTheme();
installLiveCharts();
refreshCurrent();
setInterval(refreshCurrent, 15000);
