/* Публичная статистика пула (список нод и страница одной ноды).
   Логика — эндпоинты, расчёты, интерактивный спарклайн клиентов —
   перенесена из прежней версии дословно. Меняется только разметка и стили. */

import { $, esc, icon, ICONS } from "../../lib/dom";
import { fmtDur, fmtAgo, fmtClock } from "../../lib/format";
import { initTheme } from "../../lib/theme";

function chip(flag: boolean | null | undefined, okText?: string, noText?: string): string {
  if (flag === null || flag === undefined) return `<span class="badge">—</span>`;
  return flag ? `<span class="badge ok">${okText || "ok"}</span>` : `<span class="badge bad">${noText || "fail"}</span>`;
}
function availClass(p: number): string { return p >= 99 ? "ok" : p >= 95 ? "warn" : "bad"; }

const NODE_TYPE_META: Record<string, { label: string; cls: string; title: string }> = {
  classic:  { label: "Classic",  cls: "",        title: "классический telemt" },
  mtproxyl: { label: "MTProxyL", cls: "special",  title: "форк MTProxyL (/opt/mtproxyl/)" },
  meko:     { label: "MEKO",     cls: "info",     title: "фикс MEKO (/opt/mtpr-simple/)" },
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
    <td class="mono" data-label="Нода" title="${esc(n.node_id)}">${esc(n.node_id)}</td>
    <td class="mono" data-label="IP (маска)">${esc(n.ip)}${n.port ? ":" + n.port : ""}</td>
    <td data-label="Роль">${role}${ttlRem}</td>
    <td data-label="TCP">${chip(n.healthy)}</td>
    <td data-label="GP">${chip(n.globalping_ok)}</td>
    <td data-label="Metrics"><span title="защёлка metrics-здоровья: в отказ только после серии подряд плохих отчётов">${chip(n.metrics_healthy, "metrics ok", "metrics fail")}</span></td>
    <td data-label="Доступность"><div class="meter"><div class="meter-track"><span class="meter-fill ${availClass(avail)}" style="width:${avail}%"></span></div><span class="meter-value mono">${avail.toFixed(1)}%</span></div></td>
    <td class="mono num" data-label="Клиенты">${clients === null ? "—" : clients}</td>
    <td class="mono" data-label="Отчёт">${n.report_age_sec < 0 ? "—" : fmtDur(n.report_age_sec) + " назад"}</td>
    <td data-label="Тип">${nodeTypeBadge(n.node_type)}</td>
  </tr>`;
}

async function renderListView(): Promise<void> {
  const d = await pubApi("/statistics/api/list");
  $("view-title").textContent = "Публичная статистика пула";
  const rows = (d.nodes || []).map(listRowHTML).join("");
  $("root").innerHTML = `
    <div class="banner info">${icon(ICONS.info, 16)}<span>Открытая сводка пула: <b>IP-адреса замаскированы</b>. Клик по строке — подробная страница ноды.</span></div>
    <section class="card">
      <div class="card-head"><h2>Ноды пула</h2><span class="hint">${(d.nodes || []).length} шт</span></div>
      <div class="table-wrap"><table class="table-cards">
        <thead><tr><th>Нода</th><th>IP (маска)</th><th>Роль</th><th>TCP</th><th>GP</th><th>Metrics</th><th>Доступность</th><th>Клиенты</th><th>Отчёт</th><th>Тип</th></tr></thead>
        <tbody id="pub-nodes-body">${rows || `<tr><td colspan="10" class="table-empty">пул пуст</td></tr>`}</tbody>
      </table></div>
    </section>`;
  const body = $("pub-nodes-body");
  if (body) body.addEventListener("click", (e) => {
    const tr = (e.target as HTMLElement).closest<HTMLElement>("tr[data-id]");
    if (tr) location.href = "/statistics/" + encodeURIComponent(tr.dataset.id!);
  });
}

/* ---------------- страница ноды (/statistics/<id>) ---------------- */

interface ClientPt { at: string; v: number; }

function clientsSparkInteractive(pts: ClientPt[]): string {
  if (pts.length < 2) {
    return `<div class="dim" style="padding:6px 0">пока мало данных</div>`;
  }
  const w = 600, hgt = 56, max = Math.max(...pts.map((p) => p.v), 1);
  const step = w / (pts.length - 1);
  const xy = pts.map((p, i) => [i * step, hgt - 5 - (p.v / max) * (hgt - 12)]);
  const s = xy.map((p) => `${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(" ");
  return `<div class="sparkwrap" id="clients-sparkwrap">
    <svg class="spark" id="clients-spark" viewBox="0 0 ${w} ${hgt}" preserveAspectRatio="none">
      <polygon points="0,${hgt} ${s} ${w},${hgt}" fill="var(--acc)" fill-opacity=".13"/>
      <polyline points="${s}" fill="none" stroke="var(--acc)" stroke-width="2" vector-effect="non-scaling-stroke"/>
    </svg>
    <div class="spark-guide" id="clients-guide"></div>
    <div class="spark-dot" id="clients-dot"></div>
    <div class="spark-tip" id="clients-tip"></div>
  </div>`;
}

function attachSparkHover(pts: ClientPt[]): void {
  const wrap = $("clients-sparkwrap");
  const svg = $("clients-spark");
  if (!wrap || !svg || pts.length < 2 || !svg.getBoundingClientRect) return;
  const guide = $("clients-guide"), dot = $("clients-dot"), tip = $("clients-tip");
  const hgt = 56, max = Math.max(...pts.map((p) => p.v), 1);
  wrap.addEventListener("mousemove", (e: MouseEvent) => {
    const rect = svg.getBoundingClientRect();
    if (!rect.width || !rect.height) return;
    let frac = (e.clientX - rect.left) / rect.width;
    frac = Math.min(1, Math.max(0, frac));
    const i = Math.min(pts.length - 1, Math.round(frac * (pts.length - 1)));
    const xFraction = i / (pts.length - 1);
    const xPx = xFraction * rect.width;
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
  $("view-title").textContent = "Нода · " + (n.node_id || id);
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
  const clientPts: ClientPt[] = repHist.filter((p: any) => p.clients >= 0).map((p: any) => ({ at: p.at, v: p.clients }));

  $("root").innerHTML = `
    <div class="banner info">${icon(ICONS.info, 16)}<span><a href="/statistics">← все ноды пула</a> · IP-адрес замаскирован.</span></div>
    <div class="card node-head">
      <div>
        <div class="node-id">${esc(n.node_id)}</div>
        <div class="node-ip">${esc(n.ip)}${n.port ? ":" + n.port : ""} · зарегистрирована ${esc(fmtAgo(n.registered_at))}</div>
      </div>
      <div class="node-chips">${chips}</div>
      ${reason}
    </div>
    <div class="metrics" style="margin-bottom:16px">${stats}</div>
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
        <div class="chart-cap">Активные клиенты (unique IP)</div>
        ${clientsSparkInteractive(clientPts)}
      </div>
    </section>
    <section class="card">
      <div class="card-head"><h2>События ноды</h2><span class="hint">последние 100</span></div>
      <ul class="feed" style="max-height:none">${renderPubEvents(d.events || [])}</ul>
    </section>`;
  attachSparkHover(clientPts);
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
refreshCurrent();
setInterval(refreshCurrent, 15000);
