/* Публичная страница прокси-ссылок.

   Данные берутся из СУЩЕСТВУЮЩЕГО эндпоинта GET /proxylinks — того самого,
   что и раньше отдавал JSON. Ни формат ответа, ни сам эндпоинт не меняются:
   на него могут быть завязаны чужие скрипты и боты-раздатчики. */

import { $, esc } from "../../lib/dom";
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
  if (!links.length) {
    el.innerHTML = `<div class="card"><div class="empty-state">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M10 13a5 5 0 0 0 7.5.5l3-3a5 5 0 0 0-7-7l-1.7 1.7"/><path d="M14 11a5 5 0 0 0-7.5-.5l-3 3a5 5 0 0 0 7 7l1.7-1.7"/></svg>
      <span class="t">Сейчас ни одна ссылка не доступна</span>
      <span class="s">У доменов нет назначенных мастеров — либо пул пуст, либо все ноды в блокировке.
        Ссылки появятся, как только регистратор назначит домену живую ноду.</span>
    </div></div>`;
    return;
  }
  const byUser: Record<string, ProxyLink[]> = {};
  for (const l of links) (byUser[l.user] = byUser[l.user] || []).push(l);

  el.innerHTML = Object.keys(byUser).sort().map((user) => {
    const n = byUser[user].length;
    return `<section class="card" style="margin-bottom:14px">
      <div class="user-head">
        <span class="user-name">${esc(user)}</span>
        <span class="user-count">${n} ${plural(n, "домен", "домена", "доменов")}</span>
      </div>
      ${byUser[user].map((l) => `
        <div class="link-row">
          <div class="link-main">
            <div class="link-domain">${esc(l.domain)}<span class="dim" style="font-weight:400">:${l.port}</span></div>
            <div class="link-url">${esc(l.url)}</div>
          </div>
          <div class="link-acts">
            <a class="btn primary sm" href="${esc(l.url)}">Открыть</a>
            <button class="btn sm" data-copy="${esc(l.url)}">копировать</button>
            <button class="btn sm" data-qr="${esc(l.url)}" data-domain="${esc(l.domain)}">QR</button>
          </div>
        </div>`).join("")}
    </section>`;
  }).join("");
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
