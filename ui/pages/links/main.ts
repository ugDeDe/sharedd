/* Публичная страница прокси-ссылок.

   Данные берутся из СУЩЕСТВУЮЩЕГО эндпоинта GET /proxylinks — того самого,
   что и раньше отдавал JSON. Ни формат ответа, ни сам эндпоинт не меняются:
   на него могут быть завязаны чужие скрипты и боты-раздатчики. */

import { $ } from "../../lib/dom";
import { plural } from "../../lib/format";
import { initTheme } from "../../lib/theme";
import { encode as qrEncode } from "./qr";

interface ProxyLink { domain: string; port: number; user: string; url: string; }

/* ── QR ──────────────────────────────────────────────────────────────
   Матрица рисуется в canvas один модуль = один пиксель, растягивает CSS
   (image-rendering: pixelated) — так код остаётся резким на любом экране
   и не зависит от плотности дисплея. */
function drawQR(canvas: HTMLCanvasElement, text: string): void {
  const m = qrEncode(text), n = m.length, q = 4, size = n + q * 2;
  canvas.width = size; canvas.height = size;
  const ctx = canvas.getContext("2d")!;
  ctx.fillStyle = "#fff"; ctx.fillRect(0, 0, size, size);
  ctx.fillStyle = "#000";
  for (let r = 0; r < n; r++)
    for (let c = 0; c < n; c++)
      if (m[r][c]) ctx.fillRect(c + q, r + q, 1, 1);
}

function showQR(domain: string, url: string): void {
  $("qr-domain").textContent = domain;
  drawQR($("qr-canvas") as HTMLCanvasElement, url);
  $("qr-modal").classList.add("show");
}

function closeQR(): void { $("qr-modal").classList.remove("show"); }

/* ── копирование ─────────────────────────────────────────────────────── */
let flashTimer: number | undefined;
function flash(btn: HTMLElement, text: string): void {
  const old = btn.dataset.label || btn.textContent || "";
  btn.dataset.label = old;
  btn.textContent = text;
  clearTimeout(flashTimer);
  flashTimer = window.setTimeout(() => { btn.textContent = btn.dataset.label!; }, 1600);
}

function copy(btn: HTMLElement, url: string): void {
  const done = () => flash(btn, "скопировано");
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(url).then(done, () => flash(btn, "не вышло"));
    return;
  }
  // http без TLS: clipboard API недоступен — старый путь через textarea
  const ta = document.createElement("textarea");
  ta.value = url; ta.style.position = "fixed"; ta.style.opacity = "0";
  document.body.appendChild(ta); ta.select();
  try { document.execCommand("copy"); done(); } catch { flash(btn, "не вышло"); }
  document.body.removeChild(ta);
}

/* ── отрисовка ───────────────────────────────────────────────────────── */
function render(links: ProxyLink[]): void {
  const el = $("list");
  el.replaceChildren();
  if (!links.length) {
    const card = document.createElement("div"), empty = document.createElement("div");
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("viewBox", "0 0 24 24"); svg.setAttribute("fill", "none"); svg.setAttribute("stroke", "currentColor"); svg.setAttribute("stroke-width", "1.8"); svg.setAttribute("stroke-linecap", "round"); svg.setAttribute("stroke-linejoin", "round");
    for (const d of ["M10 13a5 5 0 0 0 7.5.5l3-3a5 5 0 0 0-7-7l-1.7 1.7", "M14 11a5 5 0 0 0-7.5-.5l-3 3a5 5 0 0 0 7 7l1.7-1.7"]) {
      const path = document.createElementNS("http://www.w3.org/2000/svg", "path"); path.setAttribute("d", d); svg.appendChild(path);
    }
    const title = document.createElement("span"), text = document.createElement("span");
    card.className = "card"; empty.className = "empty-state";
    title.className = "t"; title.textContent = "Сейчас ни одна ссылка не доступна";
    text.className = "s"; text.textContent = "У доменов нет назначенных мастеров — либо пул пуст, либо все ноды в блокировке. Ссылки появятся, как только регистратор назначит домену живую ноду.";
    empty.append(svg, title, text); card.appendChild(empty); el.appendChild(card);
    return;
  }
  const byUser: Record<string, ProxyLink[]> = {};
  for (const l of links) (byUser[l.user] = byUser[l.user] || []).push(l);

  for (const user of Object.keys(byUser).sort()) {
    const n = byUser[user].length;
    const section = document.createElement("section"), head = document.createElement("div");
    const name = document.createElement("span"), count = document.createElement("span");
    section.className = "card"; section.style.marginBottom = "14px"; head.className = "user-head";
    name.className = "user-name"; name.textContent = user; count.className = "user-count";
    count.textContent = `${n} ${plural(n, "домен", "домена", "доменов")}`; head.append(name, count); section.appendChild(head);
    for (const l of byUser[user]) {
      const row = document.createElement("div"), main = document.createElement("div"), domain = document.createElement("div"), port = document.createElement("span"), url = document.createElement("div"), acts = document.createElement("div");
      row.className = "link-row"; main.className = "link-main"; domain.className = "link-domain"; port.className = "dim"; port.style.fontWeight = "400";
      domain.textContent = l.domain; port.textContent = `:${l.port}`; domain.appendChild(port); url.className = "link-url"; url.textContent = l.url; main.append(domain, url); acts.className = "link-acts";
      const open = document.createElement("a"), copyBtn = document.createElement("button"), qrBtn = document.createElement("button");
      open.className = "btn primary sm"; open.href = l.url; open.textContent = "Открыть";
      copyBtn.className = "btn sm"; copyBtn.dataset.copy = l.url; copyBtn.textContent = "копировать";
      qrBtn.className = "btn sm"; qrBtn.dataset.qr = l.url; qrBtn.dataset.domain = l.domain; qrBtn.textContent = "QR";
      acts.append(open, copyBtn, qrBtn); row.append(main, acts); section.appendChild(row);
    }
    el.appendChild(section);
  }
}

async function refresh(): Promise<void> {
  const pill = $("live-pill");
  try {
    const resp = await fetch("/proxylinks", { cache: "no-store" });
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    const data = await resp.json();
    render(data.links || []);
    pill.className = "status-pill live";
    $("live-text").textContent = "обновлено " + new Date().toLocaleTimeString("ru-RU");
  } catch {
    pill.className = "status-pill off";
    $("live-text").textContent = "нет связи";
  }
}

/* ── события ─────────────────────────────────────────────────────────── */
document.addEventListener("click", (e) => {
  const t = e.target as HTMLElement;
  const c = t.closest<HTMLElement>("button[data-copy]");
  if (c) { copy(c, c.dataset.copy!); return; }
  const q = t.closest<HTMLElement>("button[data-qr]");
  if (q) { showQR(q.dataset.domain!, q.dataset.qr!); return; }
});
$("qr-close").onclick = closeQR;
$("qr-modal").addEventListener("click", (e) => {
  if ((e.target as HTMLElement).id === "qr-modal") closeQR();
});
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeQR(); });

initTheme();
refresh();
setInterval(refresh, 30000);
