/* Экран «Ноды пула» + таблица карантина.

   Вычисления (что считается «нездоровой» причиной, деление на карантин/
   основной список, формат GP-текста, класс полосы доступности) перенесены
   из прежней версии ДОСЛОВНО — renderNodes/chip/availColor/nodeTypeBadge.
   Меняется только разметка строки (tr[data-id] и клик по ней — сохранены,
   на них завязан переход на карточку ноды). */

import { $, esc } from "../../../lib/dom";
import { fmtDur } from "../../../lib/format";

function chip(flag: boolean | null | undefined, okText?: string, noText?: string): string {
  if (flag === null || flag === undefined) return `<span class="badge">—</span>`;
  return flag ? `<span class="badge ok">${okText || "ok"}</span>` : `<span class="badge bad">${noText || "fail"}</span>`;
}

function availColor(p: number): "ok" | "warn" | "bad" {
  return p >= 99 ? "ok" : p >= 95 ? "warn" : "bad";
}

// тип ноды (менеджер прокси), присылается агентом в /register
const NODE_TYPE_META: Record<string, { label: string; cls: string; title: string }> = {
  classic:  { label: "Classic",  cls: "",         title: "классический telemt" },
  mtproxyl: { label: "MTProxyL", cls: "special",  title: "форк MTProxyL (/opt/mtproxyl/)" },
  meko:     { label: "MEKO",     cls: "info",     title: "фикс MEKO (/opt/mtpr-simple/)" },
};
function nodeTypeBadge(t: string): string {
  const m = NODE_TYPE_META[t];
  if (!m) return ""; // старый агент тип не присылает — бейджа нет
  return `<span class="badge ${m.cls}" title="${esc(m.title)}">${esc(m.label)}</span>`;
}

function quarRowHTML(n: any): string {
  const age = Math.max(0, Math.round((Date.now() - new Date(n.quarantine.entered_at).getTime()) / 1000));
  return `<tr class="clickable" data-id="${esc(n.node_id)}">
    <td class="mono" data-label="Нода" title="${esc(n.node_id)}">${esc(n.node_id)}</td>
    <td class="mono" data-label="Адрес">${esc(n.ip)}${n.port ? ":" + n.port : ""}</td>
    <td class="mono" data-label="В карантине с">${fmtDur(age)} назад</td>
    <td data-label="Попытка"><span class="badge bad">${n.quarantine.attempt}/${n.quarantine.max}</span></td>
    <td class="dim wrap" data-label="Решение">${n.quarantine.reverify
      ? "перепроверка старого ip — ok вернёт в пул, fail — повторный бан"
      : "ждём вердикт GP — восстановление или бан по IP"}</td>
  </tr>`;
}

function nodeRowHTML(n: any): string {
  const avail = n.availability_pct ?? 0;
  // GP: значок = текущий статус; первый % — доля успешных РФ-проб в ПОСЛЕДНЕЙ
  // независимой верификации (ratio measurement'а); «вериф. N%» — доля успешных
  // верификаций за всё время (GPChecksOK/GPChecksTotal жизни node_id).
  const gpTxt = n.gp_checks_total > 0
    ? `${chip(n.globalping_ok)} <span class="dim mono" title="последняя верификация · % успешных за всё время">${(n.globalping_ratio * 100).toFixed(0)}% · вериф. ${n.gp_verified_pct.toFixed(0)}%</span>`
    : chip(n.globalping_ok);
  // «Клиенты» = уникальные активные IP из метрик ноды (НЕ me_writers —
  // writers это апстрим-подключения к Telegram ME, их сумма и была багом).
  const clients = n.clients_unique_ips ?? null;
  const clientsCell = clients === null ? `<span class="dim">—</span>` : `<span class="num">${clients}</span>`;
  const status = n.is_master && n.queue_position === 0
    ? `<span style="color:var(--bad)">мастер, но вне очереди!</span>`
    : (n.unhealthy_reason ? `<span style="color:var(--bad)">${esc(n.unhealthy_reason)}</span>`
      : (n.report_error ? `<span style="color:var(--bad)">${esc(n.report_error)}</span>` : `<span class="dim">—</span>`));
  const mdoms = n.master_domains || [];
  const role = n.is_master
    ? `<span class="badge gold" title="${esc(mdoms.join(", "))}">★ МАСТЕР${mdoms.length > 1 ? " ×" + mdoms.length : ""}</span>`
    : (n.queue_position > 0 ? `<span class="badge info">#${n.queue_position}</span>` : `<span class="badge">вне очереди</span>`);
  const typeBadge = nodeTypeBadge(n.node_type);
  return `<tr class="clickable${n.is_master ? " master" : ""}" data-id="${esc(n.node_id)}">
    <td class="mono" data-label="Нода" title="${esc(n.node_id)}">${esc(n.node_id)}</td>
    <td class="mono" data-label="Адрес">${esc(n.ip)}${n.port ? ":" + n.port : ""}</td>
    <td data-label="Роль">${role}</td>
    <td data-label="TCP">${chip(n.healthy)}</td>
    <td class="nowrap" data-label="Globalping">${gpTxt}</td>
    <td data-label="Metrics"><span title="серия падений ${n.metrics_fail_streak || 0} · последний отчёт: ${n.metrics_ok ? "ok" : "FAIL"}">${chip(n.metrics_healthy ?? n.metrics_ok, "metrics ok", "metrics fail")}</span></td>
    <td data-label="Доступность"><div class="meter"><div class="meter-track"><div class="meter-fill ${availColor(avail)}" style="width:${avail}%"></div></div><span class="meter-value">${avail.toFixed(1)}%</span></div></td>
    <td class="num" data-label="Клиенты">${clientsCell}</td>
    <td class="mono" data-label="Heartbeat" style="${n.heartbeat_age_sec > 45 ? "color:var(--bad)" : ""}">${fmtDur(n.heartbeat_age_sec)} назад</td>
    <td class="mono" data-label="Отчёт">${n.report_age_sec < 0 ? "—" : fmtDur(n.report_age_sec) + " назад"}</td>
    <td class="mono" data-label="Мастерство">${n.master_stints > 0 ? fmtDur(n.master_time_sec) + " · ×" + n.master_stints : "—"}</td>
    <td data-label="Статус / ошибка">${status}${typeBadge ? " " + typeBadge : ""}</td>
  </tr>`;
}

export function render(o: any): void {
  const domMap = (o.masters || []).map((m: any) => `${m.domain}→${m.ip || "—"}`).join(", ");
  const all: any[] = o.nodes || [];
  // карантин — отдельная таблица сверху (в пул не входит, бан близко)
  const quar = all.filter((n) => n.quarantine);
  const main = all.filter((n) => !n.quarantine);

  $("nodes-hint").textContent = `${o.nodes_total} нод${quar.length ? " · карантин: " + quar.length : ""} · DNS: ${domMap || o.domains.join(", ")}`;
  $("quar-block").hidden = quar.length === 0;
  if (quar.length) $("quar-body").innerHTML = quar.map(quarRowHTML).join("");

  const body = $("nodes-body");
  if (main.length === 0) {
    body.innerHTML = `<tr><td colspan="12" class="table-empty">${quar.length ? "Активных нод нет — все в карантине" : "Пул пуст — ноды регистрируются через POST /register"}</td></tr>`;
    return;
  }
  body.innerHTML = main.map(nodeRowHTML).join("");
}

export function init(): void {
  $("nodes-body").addEventListener("click", (e) => {
    const tr = (e.target as HTMLElement).closest<HTMLElement>("tr[data-id]");
    if (tr) location.hash = "#/nodes/" + encodeURIComponent(tr.dataset.id!);
  });
  $("quar-body").addEventListener("click", (e) => {
    const tr = (e.target as HTMLElement).closest<HTMLElement>("tr[data-id]");
    if (tr) location.hash = "#/nodes/" + encodeURIComponent(tr.dataset.id!);
  });
}
