/* Экран «Ноды пула» + карантин.

   Логика перенесена из прежней версии дословно: пороги, тексты, признаки
   здоровья, состав данных. Изменено только представление — вместо таблицы
   на двенадцать колонок карточка-строка: главное видно сразу, редкое
   раскрывается по клику. */

import { $, esc } from "../../../lib/dom";
import { fmtDur, fmtNum } from "../../../lib/format";

function availColor(p: number): string {
  return p >= 99 ? "ok" : p >= 95 ? "warn" : "bad";
}

// тип ноды (менеджер прокси), присылается агентом в /register
const NODE_TYPE_META: Record<string, { label: string; cls: string; title: string }> = {
  classic:  { label: "Classic",  cls: "",         title: "классический telemt" },
  mtproxyl: { label: "MTProxyL", cls: "special",  title: "форк MTProxyL (/opt/mtproxyl/)" },
  meko:     { label: "MEKO",     cls: "info",     title: "фикс MEKO (/opt/mtpr-simple/)" },
};
function nodeTypeLabel(t: string): string {
  const m = NODE_TYPE_META[t];
  return m ? m.label : "";
}

/* Три проверки одной группой вместо трёх колонок: подпись плюс
   подчёркивание цветом состояния. */
function checksHTML(n: any): string {
  const tag = (label: string, state: string, title: string) =>
    `<span class="check-tag ${state}" title="${esc(title)}">${label}</span>`;
  const st = (flag: boolean | null | undefined) =>
    flag === null || flag === undefined ? "unknown" : flag ? "ok" : "bad";

  const gpTitle = n.gp_checks_total > 0
    ? `последняя верификация ${(n.globalping_ratio * 100).toFixed(0)}% · успешных за всё время ${n.gp_verified_pct.toFixed(0)}%`
    : "верификаций ещё не было";
  const mStreak = n.metrics_fail_streak || 0;
  const mTitle = `серия падений ${mStreak} · последний отчёт: ${n.metrics_ok ? "ok" : "FAIL"}`;

  return `<span class="checks">
    ${tag("TCP", st(n.healthy), n.healthy ? "порт отвечает" : "порт не отвечает")}
    ${tag("GP", st(n.globalping_ok), gpTitle)}
    ${tag("METRICS", st(n.metrics_healthy ?? n.metrics_ok), mTitle)}
  </span>`;
}

function roleHTML(n: any): string {
  const mdoms = n.master_domains || [];
  if (n.is_master) {
    return `<span class="badge gold" title="${esc(mdoms.join(", "))}">★ МАСТЕР${mdoms.length > 1 ? " ×" + mdoms.length : ""}</span>`;
  }
  return n.queue_position > 0
    ? `<span class="badge info">#${n.queue_position}</span>`
    : `<span class="badge">вне очереди</span>`;
}

function problemHTML(n: any): string {
  if (n.is_master && n.queue_position === 0) {
    return `<span class="node-problem">мастер, но вне очереди!</span>`;
  }
  const text = n.unhealthy_reason || n.report_error || "";
  return text ? `<span class="node-problem" title="${esc(text)}">${esc(text)}</span>` : "";
}

/* ── карточка ноды ──────────────────────────────────────────────────── */
function nodeCardHTML(n: any): string {
  const avail = n.availability_pct ?? 0;
  const clients = n.clients_unique_ips ?? null;
  const type = nodeTypeLabel(n.node_type);
  const hbLate = n.heartbeat_age_sec > 45;

  return `<div class="node-card${n.is_master ? " master" : ""}" data-id="${esc(n.node_id)}">
    <div class="node-main">
      <div class="node-top">
        <span class="node-id mono" title="${esc(n.node_id)}">${esc(n.node_id)}</span>
        ${roleHTML(n)}
      </div>
      <div class="node-sub">
        <span class="mono">${esc(n.ip)}${n.port ? ":" + n.port : ""}</span>
        ${type ? `<span class="dim">${esc(type)}</span>` : ""}
        ${checksHTML(n)}
        ${problemHTML(n)}
      </div>
    </div>

    <div class="node-side">
      <div class="node-clients">
        <span class="node-clients-v">${clients === null ? "—" : fmtNum(clients)}</span>
        <span class="node-clients-l">клиентов</span>
      </div>
      <div class="meter node-meter">
        <div class="meter-track"><div class="meter-fill ${availColor(avail)}" style="width:${avail}%"></div></div>
        <span class="meter-value">${avail.toFixed(1)}%</span>
      </div>
    </div>

    <div class="node-more">
      <div class="kv"><span class="kv-k">Мастерство</span><span class="kv-v">${
        n.master_stints > 0 ? fmtDur(n.master_time_sec) + " · ×" + n.master_stints : "—"}</span></div>
      <div class="kv"><span class="kv-k">Heartbeat</span><span class="kv-v${hbLate ? " late" : ""}">${
        fmtDur(n.heartbeat_age_sec)} назад</span></div>
      <div class="kv"><span class="kv-k">Отчёт</span><span class="kv-v">${
        n.report_age_sec < 0 ? "—" : fmtDur(n.report_age_sec) + " назад"}</span></div>
      <div class="kv"><span class="kv-k">Отчётов ok</span><span class="kv-v">${
        fmtNum(n.reports_ok ?? 0)} / ${fmtNum(n.reports_total ?? 0)}</span></div>
      <div class="kv"><span class="kv-k">Верификаций GP</span><span class="kv-v">${
        n.gp_checks_total ?? 0}${n.gp_checks_total ? ` · успешных ${n.gp_verified_pct.toFixed(0)}%` : ""}</span></div>
      <div class="kv"><span class="kv-k">Зарегистрирована</span><span class="kv-v">${esc(n.registered_at ? String(n.registered_at).slice(0, 16).replace("T", " ") : "—")}</span></div>
      <a class="btn sm node-open" href="#/nodes/${encodeURIComponent(n.node_id)}">Статистика ноды →</a>
    </div>
  </div>`;
}

/* ── карточка ноды в карантине ──────────────────────────────────────── */
function quarCardHTML(n: any): string {
  const age = Math.max(0, Math.round((Date.now() - new Date(n.quarantine.entered_at).getTime()) / 1000));
  const verdict = n.quarantine.reverify
    ? "перепроверка старого ip — ok вернёт в пул, fail — повторный бан"
    : "ждём вердикт GP — восстановление или бан по IP";
  return `<div class="node-card quar" data-id="${esc(n.node_id)}">
    <div class="node-main">
      <div class="node-top">
        <span class="node-id mono" title="${esc(n.node_id)}">${esc(n.node_id)}</span>
        <span class="badge bad">попытка ${n.quarantine.attempt}/${n.quarantine.max}</span>
      </div>
      <div class="node-sub">
        <span class="mono">${esc(n.ip)}${n.port ? ":" + n.port : ""}</span>
        <span class="dim">в карантине ${fmtDur(age)}</span>
        <span class="dim">${verdict}</span>
      </div>
    </div>
  </div>`;
}

/* ── обработчики ────────────────────────────────────────────────────── */
export function init(): void {
  // Клик по карточке раскрывает подробности, клик по ссылке внутри —
  // уводит на страницу ноды. Делегирование на стабильный контейнер,
  // перерисовка раз в 5 секунд обработчик не сбрасывает.
  const onClick = (e: Event) => {
    const t = e.target as HTMLElement;
    if (t.closest("a")) return;
    const card = t.closest<HTMLElement>(".node-card[data-id]");
    if (!card) return;
    const id = card.dataset.id!;
    if (card.classList.contains("quar")) { location.hash = "#/nodes/" + encodeURIComponent(id); return; }
    card.classList.toggle("open");
    openIds.has(id) ? openIds.delete(id) : openIds.add(id);
  };
  $("nodes-body").addEventListener("click", onClick);
  $("quar-body").addEventListener("click", onClick);
  $("nodes-body").addEventListener("dblclick", (e) => {
    const card = (e.target as HTMLElement).closest<HTMLElement>(".node-card[data-id]");
    if (card) location.hash = "#/nodes/" + encodeURIComponent(card.dataset.id!);
  });
}

/* Какие карточки раскрыты — чтобы перерисовка раз в 5 секунд их не
   схлопывала прямо под курсором. */
const openIds = new Set<string>();

export function render(o: any): void {
  const domMap = (o.masters || []).map((m: any) => `${m.domain}→${m.ip || "—"}`).join(", ");
  const all = o.nodes || [];
  // карантин — отдельный блок сверху (в пул не входит, бан близко)
  const quar = all.filter((n: any) => n.quarantine);
  const main = all.filter((n: any) => !n.quarantine);

  $("nodes-hint").textContent =
    `${o.nodes_total} нод${quar.length ? " · карантин: " + quar.length : ""} · DNS: ${domMap || o.domains.join(", ")}`;
  $("quar-block").hidden = quar.length === 0;
  if (quar.length) $("quar-body").innerHTML = quar.map(quarCardHTML).join("");

  const body = $("nodes-body");
  if (main.length === 0) {
    body.innerHTML = `<div class="empty-state"><span class="t">${
      quar.length ? "Активных нод нет — все в карантине" : "Пул пуст"}</span><span class="s">${
      quar.length ? "" : "Ноды регистрируются через POST /register."}</span></div>`;
    return;
  }

  // Регистратор отдаёт ноды по алфавиту node_id, поэтому мастер мог
  // оказаться в середине списка. Порядок меняем только на отображении.
  const masters = main.filter((n: any) => n.is_master);
  const queued = main.filter((n: any) => !n.is_master && n.queue_position > 0)
    .sort((a: any, b: any) => a.queue_position - b.queue_position);
  const rest = main.filter((n: any) => !n.is_master && !(n.queue_position > 0));

  const group = (title: string, note: string) =>
    `<div class="node-group"><span>${title}</span><span class="dim">${note}</span></div>`;

  let html = "";
  if (masters.length) {
    html += group("Мастера доменов", `${masters.length} · держат A-записи`);
    html += masters.map(nodeCardHTML).join("");
  }
  if (queued.length) {
    html += group("Очередь здоровья", `${queued.length} · следующие кандидаты в мастера`);
    html += queued.map(nodeCardHTML).join("");
  }
  if (rest.length) {
    html += group("Вне очереди", `${rest.length} · не участвуют в выборе мастера`);
    html += rest.map(nodeCardHTML).join("");
  }
  body.innerHTML = html;

  // вернуть раскрытые карточки в раскрытое состояние после перерисовки
  openIds.forEach((id) => {
    const el = body.querySelector<HTMLElement>(`.node-card[data-id="${CSS.escape(id)}"]`);
    if (el) el.classList.add("open");
  });
}
