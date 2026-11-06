/* Нагрузка ноды: клиенты и соединения, хостинг (CPU/RAM/сеть) и разбивка по
   MTProto-пользователям. Общие для кабинета и публичной статистики: данные
   приходят в одной форме (registry nodeLoad — connections_24h/hosting/users),
   разметка и графики — отсюда, стили — components/charts.css. Логика и
   разметка перенесены из кабинета (node-detail.ts) без изменений. */

import { esc } from "./dom";
import { fmtClock, fmtNum, plural } from "./format";
import { chartTop, donutChart, liveLineChart } from "./chart";
import type { LiveSeries } from "./chart";

// Серии хостинга (24 ч, по возрастанию времени). Пустые массивы = «данные
// копятся»; cpu_percent/ram_percent внутри точки могут быть null (пропуск).
export interface NodeHostingCpuRam { at: string; cpu_percent: number | null; ram_percent: number | null; }
export interface NodeHostingNet { at: string; ingress_bytes: number; egress_bytes: number; }
export interface NodeHosting {
  cpu_percent: number | null;
  ram_percent: number | null;
  net_ingress_bytes_total?: number;
  net_egress_bytes_total?: number;
  cpu_ram_series?: NodeHostingCpuRam[];
  net_series?: NodeHostingNet[];
}
export interface NodeConnections {
  current_writers?: number; current_unique_ips?: number; current_user_connections?: number;
  series: { at: string; clients: number; writers: number }[];
  note?: string;
}
export interface NodeUserUniqueIPs { user: string; unique_ips: number; }
// Агрегат трафика пользователя за 24ч. null, когда данных нет; имя раздела в
// absent[] равнозначно «нет данных».
export interface NodeUserTraffic { user: string; ingress_bytes: number; egress_bytes: number; }
export interface NodeUsers {
  unique_ips_by_user: NodeUserUniqueIPs[];
  traffic_by_user: NodeUserTraffic[] | null;
}
/** Нагрузка ноды. absent — имена разделов без источника (кабинет); публичная
 *  статистика его не шлёт: отсутствие значения = null. */
export interface NodeLoad {
  hosting: NodeHosting | null;
  connections_24h: NodeConnections | null;
  users: NodeUsers | null;
  absent?: string[];
}

export function fmtBytes(n: number | null | undefined): string {
  if (n === null || n === undefined) return "нет данных";
  const u = ["Б", "КБ", "МБ", "ГБ", "ТБ"];
  let v = n, i = 0;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${u[i]}`;
}

export function fmtPct(n: number | null | undefined): string {
  return n === null || n === undefined ? "—" : `${n.toFixed(1)}%`;
}

/** Текстовый эквивалент графика (DESIGN.md §13.4): тот же ряд значений теми
 *  же форматтерами, что в видимой части. Показывается скринридеру. */
export function srTable(caption: string, headers: string[], rows: string[][]): string {
  return `<table class="sr-only"><caption>${esc(caption)}</caption>
    <thead><tr>${headers.map((h) => `<th>${esc(h)}</th>`).join("")}</tr></thead>
    <tbody>${rows.map((r) => `<tr>${r.map((c) => `<td>${esc(c)}</td>`).join("")}</tr>`).join("")}</tbody></table>`;
}

/** Статичная легенда «цвет + текст» (DESIGN §13.4 — <span>, не только цвет). */
export function legend(items: [string, string][]): string {
  return `<div class="chart-legend">${items.map(([name, color]) =>
    `<span class="legend-static"><span class="legend-dot" style="background:${color}"></span>${esc(name)}</span>`).join("")}</div>`;
}

const absentIn = (d: NodeLoad, key: string): boolean => (d.absent ?? []).includes(key);

/** «Уникальные IP, 24 ч»: текущие соединения/IP и график по отчётам. */
export function connectionsCardHTML(d: NodeLoad): string {
  const c = d.connections_24h;
  const head = `<div class="card-head"><h2>Уникальные IP, 24 ч</h2><span class="hint">по отчётам</span></div>`;
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
    const live = liveLineChart(x, [{ name: "Уникальные IP", values: clients, texts: clients.map((v) => fmtNum(v)) }]);
    chart = `<div class="chart-wrap" role="img" aria-label="Уникальные IP за 24 часа, максимум ${fmtNum(top)}">
        <div class="chart-plot">
          <div class="chart-y" aria-hidden="true"><span>${fmtNum(top)}</span><span>0</span></div>
          ${live}
        </div>
      </div>
      ${srTable("Уникальные IP за 24 часа", ["Время", "Уникальные IP"], series.map((p) => [fmtClock(p.at), fmtNum(p.clients)]))}`;
  }

  return `<section class="card">${head}
    <div class="card-body">
      <div class="load-grid">
        ${kv("Соединения пользователей", c.current_user_connections)}
        ${kv("Уникальные IP (сейчас)", c.current_unique_ips)}
      </div>
      <div class="chart-cap">Уникальные IP по отчётам <span class="hint">${series.length} ${plural(series.length, "точка", "точки", "точек")}</span></div>
      ${chart}
      ${c.note ? `<div class="card-note" style="padding:10px 0 0">${esc(c.note)}</div>` : ""}
    </div>
  </section>`;
}

/** «Хостинг»: плитки текущих значений + графики CPU/RAM и сети из
 *  cpu_ram_series/net_series. Пустые серии → честное «данные копятся». */
export function hostingCardHTML(d: NodeLoad): string {
  const h = d.hosting;
  const head = `<div class="card-head"><h2>Хостинг: CPU / RAM / сеть</h2><span class="hint">текущие значения · 24 ч</span></div>`;
  if (!h) return `<section class="card">${head}<div class="table-empty">нет данных</div></section>`;

  // Суммы за 24 ч нужны и плиткам, и пончику: живые *_total могут отсутствовать
  // (свежий отчёт без счётчиков), поэтому плитки падают на историю.
  const net = h.net_series ?? [];
  const sum = (k: "ingress_bytes" | "egress_bytes"): number =>
    net.length ? net.reduce((s, p) => s + (p[k] ?? 0), 0) : (h[k === "ingress_bytes" ? "net_ingress_bytes_total" : "net_egress_bytes_total"] ?? 0);
  const ing = sum("ingress_bytes");
  const egr = sum("egress_bytes");
  const netTotal = ing + egr;
  const ingShown = net.length ? ing : h.net_ingress_bytes_total;
  const egrShown = net.length ? egr : h.net_egress_bytes_total;

  const tiles = `<div class="load-grid">
      <div class="kv"><span class="kv-k">CPU</span><span class="kv-v mono">${esc(fmtPct(absentIn(d, "hosting.cpu") ? null : h.cpu_percent))}</span></div>
      <div class="kv"><span class="kv-k">ОЗУ (RAM)</span><span class="kv-v mono">${esc(fmtPct(absentIn(d, "hosting.ram") ? null : h.ram_percent))}</span></div>
      <div class="kv"><span class="kv-k">Сеть — принято</span><span class="kv-v mono">${esc(fmtBytes(ingShown))}</span></div>
      <div class="kv"><span class="kv-k">Сеть — отдано</span><span class="kv-v mono">${esc(fmtBytes(egrShown))}</span></div>
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

  // Сеть: пончик принято/отдано с долями. Данные — сумма net_series за 24 ч
  // (см. выше), fallback на накопительные *_total уже учтён в ing/egr.
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

/** Горизонтальные столбики — уникальные IP по пользователям (текущий снимок,
 *  не временной ряд: заголовок честный). */
export function userIPsBarsHTML(d: NodeLoad): string {
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

/** Таблица «Пользователи»: объединяет уникальные IP и 24-ч трафик по имени. */
export function usersTableHTML(d: NodeLoad): string {
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

/* ── Наведение на линейные графики ──────────────────────────────────────
   Делегировано на document: innerHTML-перерисовка (поллинг) сносит слушатели,
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

let liveInstalled = false;

/** Подключает наведение на линейные графики (один раз на страницу).
 *  active — когда графики на экране (кабинет: открыта карточка ноды). */
export function installLiveCharts(active: () => boolean = () => true): void {
  if (liveInstalled) return;
  liveInstalled = true;
  const onPointer = (e: PointerEvent): void => {
    const canvas = (e.target as HTMLElement)?.closest?.(".chart-canvas[data-live]") as HTMLElement | null;
    if (!canvas || !active()) { liveHide(); return; }
    liveShow(canvas, e.clientX);
  };
  document.addEventListener("pointermove", onPointer);
  document.addEventListener("pointerdown", onPointer);
  document.addEventListener("mouseleave", liveHide);
  // L2: тач не шлёт mouseleave — по отпусканию/отмене жеста прячем оверлей.
  document.addEventListener("pointerup", (e: PointerEvent) => { if (e.pointerType === "touch") liveHide(); });
  document.addEventListener("touchcancel", liveHide);
}
