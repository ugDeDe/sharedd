/* Дашборд блокировок. Математика графика (бакеты, метрики bans/gap/life,
   диапазоны day/week/month), наведение/тултип и расчёты KPI перенесены из
   прежней версии дословно. Меняется разметка, стили и то, что цвета берутся
   из токенов темы вместо чисел — с перерисовкой при смене темы. */

import { $ } from "../../lib/dom";
import { initTheme, token } from "../../lib/theme";

interface Bucket { start_ts: number; bans: number; avg_gap_sec: number | null; avg_lifetime_sec: number | null; }
interface DashData {
  buckets: Bucket[];
  kpi: { bans: number; avg_gap_sec: number | null; avg_lifetime_sec: number | null };
  history_ok: boolean;
  by_reason?: Record<string, number>;
  recent?: any[];
}

type Metric = "bans" | "gap" | "life";
type Range = "day" | "week" | "month";

const state: { range: Range; metric: Metric; data: DashData | null } = { range: "day", metric: "bans", data: null };
const RANGE_LABEL: Record<Range, string> = { day: "за сегодня", week: "за 7 дней", month: "за 30 дней" };
const METRIC_TITLE: Record<Metric, string> = {
  bans: "Баны GP",
  gap: "Периодичность банов (интервал между блокировками)",
  life: "Среднее время жизни ноды",
};

/* Локальный компактный fmtDur — НЕ путать с lib/format.fmtDur (тот
   разбивает на д/ч/м/с). Этот выбирает одну старшую единицу — так было
   в прежней версии дашборда для осей графика и KPI, оставляем как есть. */
function fmtDur(sec: number | null | undefined): string {
  if (sec == null || sec < 0) return "—";
  if (sec >= 86400) return (sec / 86400).toFixed(1) + " дн";
  if (sec >= 3600) return (sec / 3600).toFixed(1) + " ч";
  if (sec >= 60) return Math.round(sec / 60) + " мин";
  return Math.round(sec) + " с";
}
function fmtDurLong(sec: number | null | undefined): string {
  if (sec == null || sec < 0) return "—";
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d > 0) return d + " дн " + h + " ч";
  if (h > 0) return h + " ч " + m + " мин";
  if (m > 0) return m + " мин";
  return Math.round(sec) + " с";
}
function fmtTs(sec: number, range: Range): string {
  const d = new Date(sec * 1000);
  const p = (v: number) => (v < 10 ? "0" : "") + v;
  const hm = p(d.getHours()) + ":" + p(d.getMinutes());
  if (range === "day") return hm;
  return p(d.getDate()) + "." + p(d.getMonth() + 1) + (range === "month" ? "" : " " + hm);
}
function bucketVal(b: Bucket, m: Metric): number | null {
  if (m === "bans") return b.bans;
  if (m === "gap") return b.avg_gap_sec != null ? b.avg_gap_sec : null;
  return b.avg_lifetime_sec != null ? b.avg_lifetime_sec : null;
}

const NS = "http://www.w3.org/2000/svg";
function el(name: string, attrs: Record<string, string | number>): SVGElement {
  const e = document.createElementNS(NS, name);
  for (const k in attrs) e.setAttribute(k, String(attrs[k]));
  return e;
}

function metricColor(m: Metric): { stroke: string; soft: string } {
  if (m === "bans") return { stroke: token("--bad"), soft: token("--bad-soft") };
  if (m === "gap") return { stroke: token("--info"), soft: token("--info-soft") };
  return { stroke: token("--warn"), soft: token("--warn-soft") };
}

function renderChart(): void {
  const svg = $("chart"), tip = $("tip");
  svg.innerHTML = "";
  const d = state.data;
  if (!d) return;
  const W = 860, H = 280, padL = 46, padR = 12, padT = 14, padB = 30;
  const cw = W - padL - padR, ch = H - padT - padB;
  const buckets = d.buckets, m = state.metric;
  const vals = buckets.map((b) => bucketVal(b, m));
  let maxV = 0;
  for (const v of vals) if (v != null && v > maxV) maxV = v;
  if (m === "bans") maxV = Math.max(maxV, 2);
  const lineColor = token("--line"), fg3 = token("--fg-3"), surf = token("--surf");
  if (maxV === 0) {
    $("chart-note").textContent = d.history_ok ? "За период банов нет." : "История недоступна (БД не открыта).";
    const em = el("text", { x: W / 2, y: H / 2, "text-anchor": "middle", fill: fg3, "font-size": 13 });
    em.textContent = d.history_ok ? "нет данных за период" : "история недоступна";
    svg.appendChild(em);
    return;
  }
  maxV = maxV * 1.15;
  const n = buckets.length;
  const stepX = cw / n;
  const y = (v: number) => padT + ch - (v / maxV) * ch;

  // сетка + подписи Y — подпись не рисуем, если она совпала с предыдущей
  // (на малых значениях соседние деления иначе дают одинаковую цифру: "3, 3")
  const ticks = 4;
  let prevLbl: string | null = null;
  for (let i = 0; i <= ticks; i++) {
    const v = maxV * i / ticks, yy = y(v);
    svg.appendChild(el("line", { x1: padL, y1: yy, x2: W - padR, y2: yy, stroke: lineColor, "stroke-width": 1 }));
    const text = m === "bans" ? String(Math.round(v)) : fmtDur(v);
    if (text !== prevLbl) {
      const lbl = el("text", { x: padL - 8, y: yy + 4, "text-anchor": "end", fill: fg3, "font-size": 10 });
      lbl.textContent = text;
      svg.appendChild(lbl);
      prevLbl = text;
    }
  }
  // подписи X (редкие)
  const labEvery = Math.max(1, Math.ceil(n / 8));
  for (let i = 0; i < n; i += labEvery) {
    const lx = padL + i * stepX + stepX / 2;
    const lt = el("text", { x: lx, y: H - 10, "text-anchor": "middle", fill: fg3, "font-size": 10 });
    lt.textContent = fmtTs(buckets[i].start_ts, state.range);
    svg.appendChild(lt);
  }

  const { stroke: color, soft: color2 } = metricColor(m);
  const pts: ({ x: number; y: number; v: number; ts: number } | null)[] = [];
  if (m === "bans") {
    const bw = Math.max(4, Math.min(26, stepX * 0.62));
    for (let i = 0; i < n; i++) {
      const v = vals[i];
      if (v == null || v === 0) { pts.push(null); continue; }
      const x = padL + i * stepX + (stepX - bw) / 2;
      const bar = el("rect", { x, y: y(v), width: bw, height: padT + ch - y(v), rx: 3, fill: color2, stroke: color, "stroke-width": 1.2 });
      svg.appendChild(bar);
      pts.push({ x: x + bw / 2, y: y(v), v, ts: buckets[i].start_ts });
    }
  } else {
    // график цельный: точки соединяются и через пустые бакеты («нет данных»
    // за час/сутки); разрыв остаётся только в тултипе.
    let pathD = "", areaD = "";
    for (let i = 0; i < n; i++) {
      const v = vals[i];
      if (v == null) { pts.push(null); continue; }
      const px = padL + i * stepX + stepX / 2, py = y(v);
      pathD += (pathD ? " L" : "M") + px.toFixed(1) + " " + py.toFixed(1);
      areaD += (areaD ? " L" : "M" + px.toFixed(1) + " " + (padT + ch) + " L") + px.toFixed(1) + " " + py.toFixed(1);
      pts.push({ x: px, y: py, v, ts: buckets[i].start_ts });
    }
    if (pathD) {
      let lastX = 0;
      for (let i = n - 1; i >= 0; i--) { const p = pts[i]; if (p) { lastX = p.x; break; } }
      let firstX = 0;
      for (let i = 0; i < n; i++) { const p = pts[i]; if (p) { firstX = p.x; break; } }
      const area = el("path", { d: areaD + " L" + lastX.toFixed(1) + " " + (padT + ch) + " L" + firstX.toFixed(1) + " " + (padT + ch) + " Z", fill: color2, stroke: "none" });
      svg.appendChild(area);
      svg.appendChild(el("path", { d: pathD, fill: "none", stroke: color, "stroke-width": 2, "stroke-linejoin": "round", "stroke-linecap": "round" }));
      for (let i = 0; i < n; i++) {
        const p = pts[i];
        if (p && p.v != null) svg.appendChild(el("circle", { cx: p.x, cy: p.y, r: 3, fill: surf, stroke: color, "stroke-width": 1.6 }));
      }
    }
  }

  // hover: зоны наведения + перекрестие + тултип
  const cur = el("line", { x1: 0, y1: padT, x2: 0, y2: padT + ch, stroke: fg3, "stroke-width": 1, "stroke-dasharray": "3 3", visibility: "hidden" });
  svg.appendChild(cur);
  const hov = el("circle", { r: 4.5, fill: color, visibility: "hidden" });
  svg.appendChild(hov);
  const showTip = (i: number, clientX: number, clientY: number) => {
    const b = buckets[i], v = vals[i];
    const midX = padL + i * stepX + stepX / 2;
    cur.setAttribute("x1", String(midX)); cur.setAttribute("x2", String(midX));
    cur.setAttribute("visibility", "visible");
    const p = pts[i];
    if (p) { hov.setAttribute("cx", String(p.x)); hov.setAttribute("cy", String(p.y)); hov.setAttribute("visibility", "visible"); }
    else hov.setAttribute("visibility", "hidden");
    let t1 = fmtTs(b.start_ts, state.range);
    if (state.range !== "day") t1 = new Date(b.start_ts * 1000).toLocaleDateString("ru-RU");
    tip.querySelector(".t1")!.textContent = t1;
    tip.querySelector(".t2")!.textContent = v == null ? "нет данных" : (m === "bans" ? (v + " бан" + (v === 1 ? "" : "ов")) : fmtDurLong(v));
    const box = $("chart-box").getBoundingClientRect();
    let x = clientX - box.left + 14; const yy = clientY - box.top - 10;
    tip.style.display = "block";
    if (x + tip.offsetWidth > box.width - 8) x = clientX - box.left - tip.offsetWidth - 14;
    tip.style.left = x + "px"; tip.style.top = Math.max(0, yy) + "px";
  };
  for (let i = 0; i < n; i++) {
    const zone = el("rect", { x: padL + i * stepX, y: padT, width: stepX, height: ch, fill: "transparent" });
    zone.addEventListener("mousemove", (ev) => showTip(i, (ev as MouseEvent).clientX, (ev as MouseEvent).clientY));
    zone.addEventListener("mouseleave", () => { cur.setAttribute("visibility", "hidden"); hov.setAttribute("visibility", "hidden"); tip.style.display = "none"; });
    svg.appendChild(zone);
  }
  tip.innerHTML = '<div class="t1"></div><div class="t2"></div>';
}

function renderKPI(): void {
  const d = state.data; if (!d) return;
  // «за сегодня» — единица измерения рядом с числом, а не мелкий текст
  // впритык: у .unit есть отступ и своя высота строки.
  $("k1").innerHTML = String(d.kpi.bans) + '<span class="unit">' + RANGE_LABEL[state.range] + '</span>';
  $("k2").textContent = d.kpi.avg_gap_sec != null ? fmtDur(d.kpi.avg_gap_sec) : "—";
  $("k3").textContent = d.kpi.avg_lifetime_sec != null ? fmtDur(d.kpi.avg_lifetime_sec) : "—";
  $("k1-cap").textContent = d.history_ok ? "Баны GP (без восстановления)" : "Баны GP — история недоступна (БД не открыта)";
  const dead = (d.by_reason && d.by_reason.dead) ? d.by_reason.dead : 0;
  $("chart-note").textContent = dead > 0
    ? "За период также терминально завершено по dead (порт/метрики не отвечали): " + dead + " — в метриках банов GP не участвуют."
    : "";
}

function renderRecent(): void {
  const d = state.data; if (!d) return;
  const tb = $("recent");
  if (!d.recent || d.recent.length === 0) {
    tb.innerHTML = '<tr><td colspan="5" class="dim">за период банов нет</td></tr>';
    return;
  }
  let html = "";
  for (const b of d.recent) {
    html += "<tr><td class='dim' data-label='Время'>" + new Date(b.ts).toLocaleString("ru-RU") + "</td>" +
      "<td class='mono' data-label='Нода'>" + b.node_id + "</td>" +
      "<td class='mono dim' data-label='IP'>" + (b.ip_masked || "—") + "</td>" +
      "<td data-label='Время жизни'>" + (b.lifetime_sec >= 0 ? fmtDurLong(b.lifetime_sec) : "—") + "</td>" +
      "<td data-label='Интервал с прошлого'>" + (b.gap_sec >= 0 ? fmtDurLong(b.gap_sec) : "—") + "</td></tr>";
  }
  tb.innerHTML = html;
}

function load(): void {
  fetch("/dashboard/api?range=" + state.range, { cache: "no-store" })
    .then((r) => { if (!r.ok) throw new Error("http " + r.status); return r.json(); })
    .then((d: DashData) => {
      state.data = d;
      $("chart-title").textContent = METRIC_TITLE[state.metric] + " · " + RANGE_LABEL[state.range];
      renderKPI(); renderChart(); renderRecent();
      const now = new Date();
      $("live-text").textContent = "обновлено " + now.toLocaleTimeString("ru-RU");
      $("live-pill").className = "status-pill live";
    })
    .catch(() => {
      $("live-text").textContent = "нет связи";
      $("live-pill").className = "status-pill off";
    });
}

$("range-seg").addEventListener("click", (ev) => {
  const b = (ev.target as HTMLElement).closest<HTMLButtonElement>("button"); if (!b) return;
  state.range = b.dataset.r as Range;
  $("range-seg").querySelectorAll("button").forEach((x) => { x.className = x === b ? "active" : ""; });
  load();
});
$("metric-seg").addEventListener("click", (ev) => {
  const b = (ev.target as HTMLElement).closest<HTMLButtonElement>("button"); if (!b) return;
  state.metric = b.dataset.m as Metric;
  $("metric-seg").querySelectorAll("button").forEach((x) => { x.className = x === b ? "active" : ""; });
  if (state.data) {
    $("chart-title").textContent = METRIC_TITLE[state.metric] + " · " + RANGE_LABEL[state.range];
    renderChart();
  }
});
document.addEventListener("visibilitychange", () => { if (!document.hidden) load(); });
document.addEventListener("themechange", () => renderChart());

initTheme();
setInterval(load, 60000);
load();
