/* Экран отдельной ноды: карточка, метрики, Globalping, история проверок.

   Вычисления (выбор роли-чипа, чипа metrics по защёлке, пороги good/warn/bad,
   маппинг измерений в спарклайн, ховер по спарклайну) перенесены из прежней
   версии ДОСЛОВНО — renderNodeDetail/clientsSparkInteractive/attachSparkHover.
   Меняется только разметка. */

import { $, esc } from "../../../lib/dom";
import { fmtDur, fmtAgo, fmtClock } from "../../../lib/format";
import { api, state } from "../state";
import { renderInto as renderEventsInto } from "./events";

function chip(flag: boolean | null | undefined, okText?: string, noText?: string): string {
  if (flag === null || flag === undefined) return `<span class="badge">—</span>`;
  return flag ? `<span class="badge ok">${okText || "ok"}</span>` : `<span class="badge bad">${noText || "fail"}</span>`;
}

const NODE_TYPE_META: Record<string, { label: string; cls: string; title: string }> = {
  classic:  { label: "Classic",  cls: "",         title: "классический telemt" },
  mtproxyl: { label: "MTProxyL", cls: "special",  title: "форк MTProxyL (/opt/mtproxyl/)" },
  meko:     { label: "MEKO",     cls: "info",     title: "фикс MEKO (/opt/mtpr-simple/)" },
};
function nodeTypeBadge(t: string): string {
  const m = NODE_TYPE_META[t];
  if (!m) return "";
  return `<span class="badge ${m.cls}" title="${esc(m.title)}">${esc(m.label)}</span>`;
}

/* ── спарклайн клиентов с ховером ──────────────────────────────────
   наводишь курсор — подсказка со временем и числом подключений в этой
   точке + вертикальная направляющая и точка. pts: [{at, v}]. */
function clientsSparkInteractive(pts: { at: string; v: number }[]): string {
  if (pts.length < 2) {
    return `<div class="dim" style="padding:6px 0">пока мало данных — накапливается раз в metrics_ms</div>`;
  }
  const w = 600, hgt = 56, max = Math.max(...pts.map((p) => p.v), 1);
  const step = w / (pts.length - 1);
  const xy = pts.map((p, i) => [i * step, hgt - 5 - (p.v / max) * (hgt - 12)]);
  const s = xy.map((p) => `${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(" ");
  return `<div class="sparkwrap" id="clients-sparkwrap">
    <svg class="spark" id="clients-spark" viewBox="0 0 ${w} ${hgt}" preserveAspectRatio="none">
      <defs><linearGradient id="sparkGrad" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0" stop-color="var(--acc)" stop-opacity=".32"/>
        <stop offset="1" stop-color="var(--acc)" stop-opacity="0"/>
      </linearGradient></defs>
      <polygon points="0,${hgt} ${s} ${w},${hgt}" fill="url(#sparkGrad)"/>
      <polyline points="${s}" fill="none" stroke="var(--acc)" stroke-width="2"
        stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/>
      <circle cx="${xy[xy.length - 1][0].toFixed(1)}" cy="${xy[xy.length - 1][1].toFixed(1)}" r="2.6"
        fill="var(--acc)" vector-effect="non-scaling-stroke"/>
    </svg>
    <div class="spark-guide" id="clients-guide"></div>
    <div class="spark-dot" id="clients-dot"></div>
    <div class="spark-tip" id="clients-tip"></div>
  </div>`;
}

function attachSparkHover(pts: { at: string; v: number }[]): void {
  const wrap = $("clients-sparkwrap"), svg = $("clients-spark") as unknown as SVGSVGElement;
  if (!wrap || !svg || pts.length < 2 || !svg.getBoundingClientRect) return;
  const guide = $("clients-guide"), dot = $("clients-dot"), tip = $("clients-tip");
  const hgt = 56, max = Math.max(...pts.map((p) => p.v), 1);
  wrap.addEventListener("mousemove", (e) => {
    const rect = svg.getBoundingClientRect();
    if (!rect.width || !rect.height) return;
    let frac = (e.clientX - rect.left) / rect.width;
    frac = Math.min(1, Math.max(0, frac));
    const i = Math.min(pts.length - 1, Math.round(frac * (pts.length - 1)));
    const xFrac = i / (pts.length - 1);
    const xPx = xFrac * rect.width;
    const yUnit = hgt - 5 - (pts[i].v / max) * (hgt - 12);
    const yPx = (yUnit / hgt) * rect.height;
    guide.style.left = xPx.toFixed(1) + "px";
    dot.style.left = xPx.toFixed(1) + "px";
    dot.style.top = yPx.toFixed(1) + "px";
    tip.textContent = `${fmtClock(pts[i].at)} · ${pts[i].v} подкл.`;
    tip.style.left = Math.min(Math.max(xPx, 62), rect.width - 62).toFixed(1) + "px";
    wrap.classList.add("hover");
  });
  wrap.addEventListener("mouseleave", () => wrap.classList.remove("hover"));
}

function renderNodeDetail(d: any): void {
  const n = d.node || {};
  const mdoms = n.master_domains || [];
  const clients = n.clients_unique_ips ?? null;
  // Роль — ОДНА взаимоисключающая плашка (мастер / позиция в очереди / вне
  // очереди). Отдельный fully-healthy-чип был дублем: queue_position===0 и
  // fully_healthy===false — одно и то же состояние, показывалось дважды.
  const roleChip = n.is_master
    ? `<span class="badge gold" title="${esc(mdoms.join(", "))}">★ МАСТЕР${mdoms.length > 1 ? " ×" + mdoms.length : ""}</span>`
    : (n.queue_position > 0 ? `<span class="badge info">очередь #${n.queue_position}</span>` : `<span class="badge">вне очереди</span>`);
  // metrics-чип — по ЗАЩЁЛКЕ: именно она решает судьбу мастерства.
  // При идущей серии падений показываем «серия N/F» рядом с чипом.
  const hcFail = (d.hc && d.hc.fail_threshold) || "?";
  const mStreak = n.metrics_fail_streak || 0;
  const mChip = chip(n.metrics_healthy ?? n.metrics_ok, "metrics ok", "metrics fail")
    + (mStreak > 0 ? ` <span class="dim" style="font-size:11px">серия ${mStreak}/${hcFail}</span>` : "");
  const chips = [chip(n.healthy, "TCP ok", "TCP fail"), chip(n.globalping_ok, "GP ok", "GP fail"), mChip, roleChip, nodeTypeBadge(n.node_type)].join(" ");
  const reason = n.unhealthy_reason ? `<div class="nd-reason">${esc(n.unhealthy_reason)}</div>` : "";

  const statDefs = [
    { l: "Доступность", v: `${(n.availability_pct ?? 0).toFixed(1)}%`, e: `отчётов ok: ${n.reports_ok ?? 0}/${n.reports_total ?? 0}`,
      cls: (n.availability_pct ?? 0) >= 99 ? "ok" : (n.availability_pct ?? 0) >= 95 ? "warn" : "bad" },
    { l: "GP верификации", v: `${(n.gp_verified_pct ?? 0).toFixed(1)}%`, e: `верификаций всего: ${n.gp_checks_total ?? 0}`,
      cls: (n.gp_verified_pct ?? 0) >= 99 ? "ok" : (n.gp_verified_pct ?? 0) >= 90 ? "warn" : "bad" },
    { l: "Клиенты", v: clients === null ? "—" : String(clients), e: clients === null ? "нет данных" : "уникальные активные IP", cls: "" },
    { l: "Мастерство", v: (n.master_stints ?? 0) > 0 ? fmtDur(n.master_time_sec) : "—", e: `stint'ов: ${n.master_stints ?? 0}`, cls: "" },
    { l: "Heartbeat", v: `${fmtDur(n.heartbeat_age_sec)} назад`, e: `всего: ${n.heartbeats_total ?? 0}`, cls: n.heartbeat_age_sec > 45 ? "bad" : "" },
    { l: "Отчёт", v: n.report_age_sec < 0 ? "—" : `${fmtDur(n.report_age_sec)} назад`, e: n.report_error ? `ошибка: ${n.report_error}` : "metrics / globalping", cls: "" },
  ];
  const stats = statDefs.map((s) => `<div class="card metric">
      <div class="metric-label">${esc(s.l)}</div>
      <div class="metric-value ${s.cls}" style="font-size:20px">${s.v}</div>
      <div class="metric-foot">${esc(s.e)}</div></div>`).join("");

  // последняя верификация Globalping по площадкам (регистратор хранит деталь)
  const gp = d.gp_last || null;
  const gpHist = d.gp_hist || [];
  // Сводка идёт СТРОКОЙ над таблицей, а не колонкой сбоку: в колонке
  // содержимого на пять строк, а таблица площадок — на двадцать, и под
  // сводкой оставалось полэкрана пустоты.
  let gpSide: string;
  if (gp) {
    gpSide = `<div class="gp-summary">
      <div class="gp-big ${gp.ok ? "ok" : "bad"}">${Math.round((gp.ratio || 0) * 100)}%</div>
      <div class="gp-facts">
        <div>${chip(gp.ok, "верификация пройдена", "верификация провалена")}</div>
        <div class="kv"><span class="kv-k">проб успешно</span><span class="kv-v">${gp.probes_ok}/${gp.probes_total}</span></div>
        <div class="kv"><span class="kv-k">проверено</span><span class="kv-v">${esc(fmtAgo(gp.at))}</span></div>
        <div class="kv"><span class="kv-k">measurement</span><span class="kv-v mono">${esc(gp.measurement_id)}</span></div>
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
  const clientPts = repHist.filter((p: any) => p.clients >= 0).map((p: any) => ({ at: p.at, v: p.clients }));
  const spark = clientsSparkInteractive(clientPts);

  $("node-detail").innerHTML = `
    <div class="card nd-head">
      <div>
        
        <div class="nd-ip nd-ip-lead"><span class="mono">${esc(n.ip)}${n.port ? ":" + n.port : ""}</span> · зарегистрирована ${esc(fmtAgo(n.registered_at))}</div>
        <a class="back-link" style="margin:6px 0 0" href="/statistics/${encodeURIComponent(n.node_id)}" target="_blank" rel="noopener">публичная страница ↗</a>
      </div>
      <div class="nd-chips">${chips}</div>
      ${reason}
    </div>
    <div class="metrics">${stats}</div>
    <section class="card">
      <div class="card-head"><h2>Globalping — независимая верификация</h2></div>
      ${gpSide}${probesTbl}
    </section>
    <section class="card">
      <div class="card-head"><h2>История проверок</h2></div>
      <div class="card-body">
        <div class="chart-cap">Globalping — ratio по верификациям <span class="hint">всего ${gpHist.length}</span></div>
        <div class="bars">${gpBars || '<span class="dim">пока нет верификаций</span>'}</div>
        <div class="chart-cap">TCP-probe регистратора <span class="hint">последние ${Math.min(tcpHist.length, 96)}</span></div>
        <div class="ticks">${tcpTicks || '<span class="dim">пока нет данных</span>'}</div>
        <div class="chart-cap">Отчёты metrics <span class="hint">последние ${Math.min(repHist.length, 96)}</span></div>
        <div class="ticks">${repTicks || '<span class="dim">пока нет данных</span>'}</div>
        <div class="chart-cap">Активные клиенты (unique IP)</div>
        ${spark}
      </div>
    </section>
    <section class="card">
      <div class="card-head"><h2>События ноды</h2><span class="hint">последние 100</span></div>
      <ul class="feed" id="nd-events" style="max-height:none"></ul>
    </section>`;
  renderEventsInto($("nd-events"), d.events || []);
  attachSparkHover(clientPts);
}

export async function load(id: string): Promise<void> {
  const d = await api("/panel/api/node?id=" + encodeURIComponent(id));
  if (state.nodeId === id) {
    state.node = d;
    renderNodeDetail(d);
  }
}

export function render(d: any): void {
  renderNodeDetail(d);
}
