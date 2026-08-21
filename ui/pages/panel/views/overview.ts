/* Экран «Обзор»: KPI-стрип, топология пула, статистика пула по журналу,
   история банов Globalping.

   Вычисления (пороги, условия, тексты) для KPI и статистики перенесены
   из прежней версии ДОСЛОВНО — renderKpis/renderGPStats/fmtInterval/
   banStatusBadge. «Мини-лента» последних событий рисуется main.ts через
   events.renderInto, сюда не входит.

   Топология — новый элемент экрана: то же о.masters/о.nodes, без новых
   запросов. Координаты связей между карточками не подбираются вручную —
   после вставки разметки измеряются реальные позиции DOM-узлов
   (getBoundingClientRect), поэтому линии остаются точными при любом
   числе доменов/нод и любой ширине окна, а не только в «типовом» случае
   из макета. */

import { $, esc, icon, ICONS } from "../../../lib/dom";
import { fmtDur, fmtClock, fmtInterval, fmtTtlMin, plural, fmtNum } from "../../../lib/format";

/* ── KPI-стрип ─────────────────────────────────────────────────────── */

/* В /panel/api/overview истории клиентов нет — только текущее значение.
   Копим её на клиенте между опросами (раз в 5 с), максимум 72 точки ≈ 6 мин.
   Новых запросов не появляется, при перезагрузке буфер просто пуст. */
const CLIENTS_HISTORY_MAX = 72;
const clientsHistory: number[] = [];
function pushClients(v: number | null | undefined): void {
  if (v === null || v === undefined) return;
  clientsHistory.push(v);
  if (clientsHistory.length > CLIENTS_HISTORY_MAX) clientsHistory.shift();
}

/* Мини-график в плитке метрики. Берёт историю из того же снимка, новых
   запросов нет: клиенты — из report_hist всех нод, баны — из статистики. */
function sparkArea(values: number[]): string {
  if (values.length < 2) return "";
  // Шкала по размаху, а не от нуля: у счётчика клиентов колебания малы
  // относительно самого числа, и от нуля график вырождался в полосу.
  const w = 140, h = 26;
  const min = Math.min(...values), max = Math.max(...values), span = max - min;
  const step = w / (values.length - 1);
  const y = (v: number) => span === 0 ? h / 2 : h - 4 - ((v - min) / span) * (h - 8);
  const pts = values.map((v, i) => `${(i * step).toFixed(1)},${y(v).toFixed(1)}`);
  const last = pts[pts.length - 1].split(",");
  return `<svg class="metric-spark" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">
    <defs><linearGradient id="mSpark" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="var(--acc)" stop-opacity=".34"/>
      <stop offset="1" stop-color="var(--acc)" stop-opacity="0"/></linearGradient></defs>
    <polygon points="0,${h} ${pts.join(" ")} ${w},${h}" fill="url(#mSpark)"/>
    <polyline points="${pts.join(" ")}" fill="none" stroke="var(--acc)" stroke-width="1.6"
      stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/>
    <circle cx="${last[0]}" cy="${last[1]}" r="2.2" fill="var(--acc)" vector-effect="non-scaling-stroke"/>
  </svg>`;
}

function sparkBars(values: number[]): string {
  if (!values.length) return "";
  const w = 140, h = 26, max = Math.max(...values, 1), bw = Math.max(3, w / values.length - 3);
  const step = w / values.length;
  return `<svg class="metric-spark" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">
    ${values.map((v, i) => {
      const bh = Math.max(2, (v / max) * (h - 4));
      return `<rect x="${(i * step).toFixed(1)}" y="${(h - bh).toFixed(1)}" width="${bw.toFixed(1)}"
        height="${bh.toFixed(1)}" rx="1.5" fill="var(--bad)" opacity=".8"/>`;
    }).join("")}
  </svg>`;
}

function renderKpis(o: any): void {
  pushClients(o.clients_unique_ips_total);
  const c = o.counters || {};
  const masters = o.masters || [];
  // свёрнутые СРМД в CNAME домены мастера не требуют — в «живых назначениях»
  // считаются только активные домены
  const active = masters.filter((m: any) => !m.cname_target);
  const folded = masters.length - active.length;
  const aliveMs = active.filter((m: any) => m.node_id && !m.dead).length;
  const dnsErr = c.dns_errors_total ?? 0;

  const card = (label: string, iconPath: string, valueHtml: string, valueCls: string,
                foot: string, chart = ""): string => `
    <div class="card metric">
      <div class="metric-label">${icon(iconPath)}<span>${label}</span></div>
      <div class="metric-value ${valueCls}">${valueHtml}</div>
      ${chart}
      <div class="metric-foot">${foot}</div>
    </div>`;

  $("cards").innerHTML =
    card("Мастера", ICONS.masters,
      `${aliveMs}<span class="sep">/</span>${active.length}`,
      active.length > 0 && aliveMs === active.length ? "ok" : "bad",
      `живых назначений${folded ? " · свёрнуто СРМД: " + folded : ""}`) +
    card("Нод в пуле", ICONS.nodes,
      `${o.nodes_total}`, "",
      `fully healthy: ${o.nodes_fully_healthy} · очередь: ${o.queue_size}`) +
    card("TCP / GP / Metrics OK", ICONS.health,
      `${o.nodes_tcp_ok}<span class="sep">/</span>${o.nodes_gp_verified}<span class="sep">/</span>${o.nodes_metrics_ok}`, "",
      `из ${o.nodes_total} нод`) +
    card("Активные клиенты", ICONS.users,
      `${fmtNum(o.clients_unique_ips_total)}`, "",
      o.clients_unique_ips_total == null ? "нет данных" : `me_writers: ${o.writers_active_total ?? 0}`,
      sparkArea(clientsHistory)) +
    card("Смен мастера", ICONS.swap,
      `${fmtNum(c.master_switches_total ?? 0)}`, "",
      `блокировок GP: ${c.gp_blocked_total ?? 0}`) +
    card("DNS-обновления", ICONS.dns,
      `${fmtNum(c.dns_updates_total ?? 0)}<span class="unit">ok</span><span class="sep">/</span>` +
      `<span style="color:${dnsErr > 0 ? "var(--bad)" : "inherit"}">${dnsErr}</span><span class="unit">err</span>`, "",
      `heartbeats: ${fmtNum(c.heartbeats_total ?? 0)} · отчёты: ${fmtNum(c.health_reports_total ?? 0)}`);
}

/* ── статистика пула по журналу + история банов ───────────────────── */

function banStatusBadge(b: any): string {
  if (b.active) return `<span class="badge bad">активен</span>`;
  const by = b.closed_by === "node_expired" ? "нода отвалилась"
    : b.closed_by === "node_replaced" ? "вытеснена новым ID"
    : b.closed_by === "node_pruned" ? "удалена (неактивна)"
    : b.closed_by === "node_terminated" ? "завершена навсегда"
    : (b.closed_by || "закрыт");
  return `<span class="badge">${esc(by)}</span>`;
}

function renderGPStats(o: any): void {
  const st = o.stats || {};
  $("stats-hint").textContent = `окно анализа: ${st.events_window ?? 0} событий журнала`;

  const card = (label: string, iconPath: string, value: string, foot: string, cls = ""): string => `
    <div class="card metric">
      <div class="metric-label">${icon(iconPath)}<span>${label}</span></div>
      <div class="metric-value ${cls}">${value}</div>
      <div class="metric-foot">${foot}</div>
    </div>`;

  $("pool-stats").innerHTML =
    // цвет числа банов был и в прежней версии: есть баны — жёлтый, нет — зелёный
    card("Баны GP", ICONS.ban,
      `${st.gp_bans_total ?? 0}`,
      `активных: ${st.gp_bans_active ?? 0} · уникальных нод: ${st.gp_bans_nodes ?? 0}`,
      (st.gp_bans_total ?? 0) > 0 ? "warn" : "ok") +
    card("Периодичность банов GP", ICONS.clock,
      fmtInterval(st.gp_ban_interval_sec), "средний промежуток между банами") +
    card("Периодичность смен DNS", ICONS.dns,
      fmtInterval(st.dns_switch_interval_sec), "средний промежуток между переключениями") +
    card("Среднее время мастерства", ICONS.masters,
      fmtInterval(st.master_avg_sec), "от избрания до потери домена");

  const bans = st.gp_ban_history || [];
  const body = $("bans-body");
  if (bans.length === 0) {
    body.innerHTML = `<tr><td colspan="6" class="table-empty">банов за окно журнала не было 🎉</td></tr>`;
    return;
  }
  body.innerHTML = bans.map((b: any) => `<tr>
    <td class="mono" data-label="Начало" title="${esc(b.started_at)}">${fmtClock(b.started_at)}</td>
    <td class="mono" data-label="Нода" title="${esc(b.node_id)}">${esc(b.node_id)}</td>
    <td class="mono" data-label="IP">${esc(b.ip || "—")}</td>
    <td class="mono" data-label="Длительность">${fmtDur(b.duration_sec || 0)}${b.active ? " · идёт" : ""}</td>
    <td data-label="Статус">${banStatusBadge(b)}</td>
    <td class="dim wrap" data-label="Причина">${esc(b.cause || "—")}</td>
  </tr>`).join("");
}

/* ── топология пула ────────────────────────────────────────────────
   Слева — домены (живой мастер / свёрнутый СРМД в CNAME), справа —
   ноды (мастера сверху, ниже очередь здоровья). Показываем максимум
   2 домена со связями и 3 ноды в очереди — остальное сводится в строку
   «ещё N…», иначе схема расползается на большом пуле. */

const NODE_TYPE_LABEL: Record<string, string> = { classic: "Classic", mtproxyl: "MTProxyL", meko: "MEKO" };

function domainSubtitle(m: any): string {
  if (m.node_id) {
    let ttl = "";
    if (m.ttl_sec > 0) {
      ttl = m.ttl_remaining_sec < 0 ? " · TTL истёк"
        : ` · TTL ${fmtTtlMin(Math.max(0, Math.round(m.ttl_remaining_sec / 60)))}`;
    }
    return `A → ${esc(m.ip || "?")}${ttl}${m.dead ? " · нода нездорова" : ""}`;
  }
  return `<span style="color:var(--bad)">мастер не назначен — DNS-записи нет</span>`;
}

function domainItemHTML(m: any, id: string, pair?: number): string {
  const cls = m.node_id ? (m.dead ? "sick" : "live") : "sick";
  return `<div class="topo-item ${cls}" id="${id}"${pair != null ? ` data-pair="${pair}"` : ""}>
    <div class="topo-b"><div class="topo-t">${esc(m.domain)}</div><div class="topo-s">${domainSubtitle(m)}</div></div>
    <div class="topo-r"><div class="topo-n">${m.clients != null ? m.clients : "—"}</div><div class="topo-u">клиентов</div></div>
  </div>`;
}

function ghostItemHTML(m: any, id: string): string {
  return `<div class="topo-item ghost" id="${id}">
    <div class="topo-b"><div class="topo-t dim">${esc(m.domain)}</div>
      <div class="topo-s">CNAME → ${esc(m.cname_target)} · свёрнут СРМД${m.clients != null ? " · клиентов: " + m.clients : ""}</div></div>
    <span class="badge">CNAME</span>
  </div>`;
}

function masterNodeItemHTML(n: any, id: string, dead?: boolean, pair?: number): string {
  const type = NODE_TYPE_LABEL[n.node_type as string] || "";
  const avail = (n.availability_pct ?? 0).toFixed(1);
  return `<div class="topo-item ${dead ? "sick" : "live"}" id="${id}"${pair != null ? ` data-pair="${pair}"` : ""}>
    <span class="topo-star">${icon(ICONS.masters, 14)}</span>
    <div class="topo-b"><div class="topo-t">${esc(n.node_id)}</div>
      <div class="topo-s">${esc(n.ip)}${type ? " · " + type : ""} · ${avail}%</div></div>
    <span class="badge ${dead ? "warn" : "ok"}">мастер</span>
  </div>`;
}

function queuedItemHTML(n: any, first: boolean): string {
  const avail = (n.availability_pct ?? 0).toFixed(1);
  return `<div class="topo-item queued">
    <span class="qnum${first ? " first" : ""}">${n.queue_position}</span>
    <div class="topo-b"><div class="topo-t">${esc(n.node_id)}</div></div>
    <span class="dim">${avail}%</span>
  </div>`;
}

/* Наведение на домен подсвечивает его ноду и связь между ними, остальное
   гаснет — ради этого схему и делали: видно, кто на ком сидит.
   Обработчик вешается один раз на стабильный контейнер, перерисовка
   раз в 5 секунд его не сбрасывает. */
let hoverBound = false;
/* Какая пара подсвечена сейчас. Схема перерисовывается раз в 5 секунд,
   и без этого подсветка слетала бы прямо под курсором. */
let hlPair: string | null = null;

function paintTopology(pair: string | null): void {
  hlPair = pair;
  const topo = $("topology").querySelector<HTMLElement>(".topo");
  if (!topo) return;
  topo.classList.toggle("has-hl", pair !== null);
  topo.querySelectorAll<HTMLElement>("[data-pair]").forEach((el) => {
    el.classList.toggle("hl", pair !== null && el.dataset.pair === pair);
  });
}

function bindTopologyHover(): void {
  if (hoverBound) return;
  hoverBound = true;
  const root = $("topology");
  root.addEventListener("mouseover", (e) => {
    const item = (e.target as HTMLElement).closest<HTMLElement>(".topo-item[data-pair]");
    if (item) paintTopology(item.dataset.pair!);
  });
  root.addEventListener("mouseout", (e) => {
    const to = (e as MouseEvent).relatedTarget as HTMLElement | null;
    if (!to || !to.closest(".topo-item[data-pair]")) paintTopology(null);
  });
}

/* Под колонкой доменов оставалось пусто — там место сводке по пулу.
   Всё берётся из того же снимка, новых запросов нет. */
function poolSummary(o: any): string {
  const nodes: any[] = o.nodes || [];
  const quarantined = nodes.filter((n) => n.quarantine).length;
  const queued = nodes.filter((n) => !n.quarantine && n.queue_position > 0).length;
  const idle = nodes.filter((n) => !n.quarantine && !(n.queue_position > 0) && !n.is_master).length;
  const cell = (v: number, label: string, cls = "") =>
    `<div class="pool-cell"><div class="pool-v ${cls}">${v}</div><div class="pool-l">${label}</div></div>`;
  return `<div class="pool-summary">
    ${cell(queued, "в очереди")}
    ${cell(quarantined, "в карантине", quarantined > 0 ? "warn" : "")}
    ${cell(idle, "вне очереди", idle > 0 ? "dim" : "")}
  </div>`;
}

function renderTopology(o: any): void {
  const el = $("topology");
  const masters: any[] = o.masters || [];
  const nodes: any[] = o.nodes || [];

  if (masters.length === 0 && nodes.length === 0) {
    el.innerHTML = `<div class="card">
      <div class="empty-state">
        <div class="t">Пул пуст</div>
        <div class="s">Ноды регистрируются через POST /register, домены — в «Настройки → Cloudflare DNS».</div>
      </div>
    </div>`;
    return;
  }

  const active = masters.filter((m) => !m.cname_target);
  const folded = masters.filter((m) => m.cname_target);
  const shownActive = active.slice(0, 2);
  const shownGhost = folded.slice(0, 1);
  const moreDomains = (active.length - shownActive.length) + (folded.length - shownGhost.length);

  const wired = shownActive
    .map((m, i) => ({ m, i, node: m.node_id ? nodes.find((n) => n.node_id === m.node_id) : undefined }))
    .filter((x) => x.node);

  const ghostTargetIdx = shownGhost.length
    ? shownActive.findIndex((m) => m.domain === shownGhost[0].cname_target)
    : -1;

  const nonMaster = nodes.filter((n) => !n.is_master);
  const queued = nonMaster.filter((n) => n.queue_position > 0).sort((a, b) => a.queue_position - b.queue_position);
  const outOfQueue = nonMaster.filter((n) => !(n.queue_position > 0));
  const shownQueued = queued.slice(0, 3);
  const moreNodes = (queued.length - shownQueued.length) + outOfQueue.length;

  const wiredSet = new Set(wired.map((w: any) => w.i));
  const domainsHTML = shownActive.map((m, i) => domainItemHTML(m, `topo-d${i}`, wiredSet.has(i) ? i : undefined)).join("")
    + shownGhost.map((m) => ghostItemHTML(m, "topo-ghost")).join("")
    + (moreDomains > 0 ? `<div class="topo-more">ещё ${moreDomains} ${plural(moreDomains, "домен", "домена", "доменов")}</div>` : "");

  const nodesHTML = wired.map(({ node, i, m }: any) => masterNodeItemHTML(node, `topo-n${i}`, !!m.dead, i)).join("")
    + shownQueued.map((n, i) => queuedItemHTML(n, i === 0)).join("")
    + (moreNodes > 0
      ? `<div class="topo-more">ещё ${moreNodes} ${plural(moreNodes, "нода", "ноды", "нод")}${outOfQueue.length ? " · вне очереди: " + outOfQueue.length : ""}</div>`
      : "");

  el.innerHTML = `<div class="card glass">
    <div class="card-head"><h2>Топология пула</h2><span class="hint">домен → мастер → очередь здоровья</span></div>
    <div class="topo">
      <div class="topo-gutter" id="topo-gutter"><svg viewBox="0 0 34 236" preserveAspectRatio="none" aria-hidden="true"></svg></div>
      <div class="topo-col"><div class="topo-cap">Домены</div>${domainsHTML}${poolSummary(o)}</div>
      <div class="topo-wires" id="topo-wires"><svg viewBox="0 0 92 236" preserveAspectRatio="none" aria-hidden="true"></svg></div>
      <div class="topo-col"><div class="topo-cap">Ноды пула</div>${nodesHTML}</div>
    </div>
  </div>`;

  bindTopologyHover();
  if (hlPair !== null) paintTopology(hlPair);   // пережить перерисовку
  drawTopologyLinks(wired.map((w) => w.i), ghostTargetIdx >= 0 ? ghostTargetIdx : null,
    new Set(wired.filter((w: any) => w.m.dead).map((w) => w.i)));
}

/* Связи рисуются ПОСЛЕ вставки разметки: координаты — это измеренные
   центры реальных карточек (getBoundingClientRect), а не подобранные
   вручную числа. Так линии остаются точными при любом составе пула и
   любой ширине окна. */
function drawTopologyLinks(wiredIdx: number[], ghostTargetIdx: number | null, sick: Set<number> = new Set()): void {
  const wiresDiv = document.getElementById("topo-wires");
  const gutterDiv = document.getElementById("topo-gutter");
  if (!wiresDiv || !gutterDiv) return;
  const originTop = wiresDiv.getBoundingClientRect().top;
  const centerY = (elId: string): number | null => {
    const e = document.getElementById(elId);
    if (!e) return null;
    const r = e.getBoundingClientRect();
    return r.top - originTop + r.height / 2;
  };

  let wiresSvg = "";
  for (const i of wiredIdx) {
    const yD = centerY(`topo-d${i}`), yN = centerY(`topo-n${i}`);
    if (yD == null || yN == null) continue;
    const col = sick.has(i) ? "var(--warn)" : "var(--ok)";
    wiresSvg += `<path class="wire" data-pair="${i}" d="M0,${yD.toFixed(1)} C40,${yD.toFixed(1)} 52,${yN.toFixed(1)} 88,${yN.toFixed(1)}" ` +
      `fill="none" stroke="${col}" stroke-width="1.6" opacity=".85"/>` +
      `<circle cx="3" cy="${yD.toFixed(1)}" r="2.8" fill="${col}"/><circle cx="89" cy="${yN.toFixed(1)}" r="2.8" fill="${col}"/>`;
  }
  wiresDiv.querySelector("svg")!.innerHTML = wiresSvg;

  let gutterSvg = "";
  if (ghostTargetIdx != null) {
    const yG = centerY("topo-ghost"), yT = centerY(`topo-d${ghostTargetIdx}`);
    if (yG != null && yT != null) {
      const sign = yT < yG ? -1 : 1;
      const b1 = (yG + sign * 6).toFixed(1), b2 = (yT - sign * 6).toFixed(1);
      gutterSvg =
        `<path d="M34,${yG.toFixed(1)} H14 Q8,${yG.toFixed(1)} 8,${b1} V${b2} Q8,${yT.toFixed(1)} 14,${yT.toFixed(1)} H27" ` +
        `fill="none" stroke="var(--link)" stroke-width="1.3" stroke-dasharray="3.5 4" opacity=".85"/>` +
        `<path d="M23,${(yT - 3.6).toFixed(1)} L29.4,${yT.toFixed(1)} L23,${(yT + 3.6).toFixed(1)} Z" fill="var(--link)" opacity=".85"/>` +
        `<circle cx="34" cy="${yG.toFixed(1)}" r="2.2" fill="var(--link)" opacity=".85"/>` +
        `<text class="cname-label" transform="translate(6,${((yG + yT) / 2).toFixed(1)}) rotate(-90)">CNAME</text>`;
    }
  }
  gutterDiv.querySelector("svg")!.innerHTML = gutterSvg;
}

/* ── точка входа ───────────────────────────────────────────────────── */

export function render(o: any): void {
  renderKpis(o);
  renderTopology(o);
  renderGPStats(o);
}
