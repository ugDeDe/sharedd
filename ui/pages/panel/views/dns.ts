/* Экран «Мастера и DNS»: назначения доменов.

   Вычисления (что считается «живым» назначением, чип TTL, текст при
   отсутствии мастера) перенесены из прежней версии ДОСЛОВНО —
   renderMastersInto/masterRowHTML/fmtTtlMin. Изменено только представление:
   вместо таблицы — связанные пары «домен ↔ мастер», как в топологии.

   Кнопка «Перезаписать DNS сейчас» — тот же POST /panel/api/dns-push,
   что и в «Настройках»; обработчик навешивается в render() (вызывается
   каждые 5 с, переприсвоение .onclick идемпотентно — в отличие от
   addEventListener дублей не плодит), т.к. у этого экрана нет init(). */

import { $, esc } from "../../../lib/dom";
import { fmtTtlMin } from "../../../lib/format";
import { getToken, showToast } from "../state";

const NODE_TYPE_META: Record<string, { label: string; cls: string; title: string }> = {
  classic:  { label: "Classic",  cls: "",         title: "классический telemt" },
  mtproxyl: { label: "MTProxyL", cls: "special",  title: "форк MTProxyL (/opt/mtproxyl/)" },
  meko:     { label: "MEKO",     cls: "info",     title: "фикс MEKO (/opt/mtpr-simple/)" },
  mtdd:     { label: "mtdd",     cls: "info",     title: "прокси mtdd (/etc/mtdd/)" },
};
function nodeTypeBadge(t: string): string {
  const m = NODE_TYPE_META[t];
  if (!m) return "";
  return `<span class="badge ${m.cls}" title="${esc(m.title)}">${esc(m.label)}</span>`;
}

/* Кольцо обратного отсчёта мастерства: наглядно, сколько домен ещё
   пробудет на этой ноде. Раньше это была строка «21м / 30м». */
function ttlRing(m: any): string {
  if (!(m.ttl_sec > 0)) return `<span class="pair-dot"></span>`;
  const expired = m.ttl_remaining_sec < 0;
  const left = Math.max(0, m.ttl_remaining_sec);
  const frac = expired ? 0 : Math.min(1, left / m.ttl_sec);
  const r = 15, c = 2 * Math.PI * r;
  const label = expired ? "!" : fmtTtlMin(Math.round(left / 60));
  const cls = expired ? "bad" : frac < 0.2 ? "warn" : "ok";
  return `<span class="ttl-ring ${cls}" title="осталось мастерства: ${expired ? "лимит истёк" : label} из ${fmtTtlMin(m.ttl_sec / 60)}">
    <svg width="36" height="36" viewBox="0 0 36 36">
      <circle cx="18" cy="18" r="${r}" fill="none" stroke="var(--surf-3)" stroke-width="2.5"/>
      <circle cx="18" cy="18" r="${r}" fill="none" stroke="currentColor" stroke-width="2.5"
        stroke-linecap="round" transform="rotate(-90 18 18)"
        stroke-dasharray="${c.toFixed(1)}" stroke-dashoffset="${(c * (1 - frac)).toFixed(1)}"/>
    </svg>
    <b>${esc(label)}</b>
  </span>`;
}

function dnsOpsHTML(dns: any): string {
  const items: any[] = dns?.items || [];
  if (!items.length) return "";
  const fmtTargets = (t: unknown): string => Array.isArray(t) && t.length ? t.join(", ") : "—";
  const when = (v: unknown): string => {
    if (!v) return "—";
    const d = new Date(String(v));
    return Number.isNaN(d.getTime()) ? "—" : d.toLocaleString("ru-RU");
  };
  return `<section class="card">
    <div class="card-head"><h2>DNS-синхронизация</h2><span class="hint">${dns.drifted || 0} ожидает · ${dns.failed || 0} с ошибкой</span></div>
    <div class="table-wrap"><table class="table-cards">
      <thead><tr><th>Домен</th><th>Нужно</th><th>В Cloudflare</th><th>Состояние</th><th>Повтор</th></tr></thead>
      <tbody>${items.map((it) => {
        const err = it.last_error ? `<span class="badge bad" title="${esc(it.last_error)}">ошибка</span>` : "";
        const drift = it.drifted ? `<span class="badge warn">ожидает</span>` : `<span class="badge ok">синхронизировано</span>`;
        return `<tr>
          <td class="mono" data-label="Домен">${esc(it.domain)}</td>
          <td class="mono wrap" data-label="Нужно">${esc(fmtTargets(it.desired_targets))}</td>
          <td class="mono wrap" data-label="В Cloudflare">${esc(fmtTargets(it.applied_targets))}</td>
          <td data-label="Состояние">${drift}${err ? " " + err : ""}</td>
          <td class="mono" data-label="Повтор">${it.next_attempt ? esc(when(it.next_attempt)) : "—"}</td>
        </tr>`;
      }).join("")}</tbody>
    </table></div>
  </section>`;
}
function pairHTML(m: any): string {
  // свёрнутый СРМД домен: мастера нет по определению, показываем приёмник
  if (m.cname_target) {
    return `<div class="pair-card ghost">
      <div class="pair-domain">
        <div class="pair-name mono dim">${esc(m.domain)}</div>
        <div class="pair-sub">свёрнут СРМД${m.clients != null ? " · клиентов: " + m.clients : ""}</div>
      </div>
      <div class="pair-link"><span class="pair-cname">CNAME →</span></div>
      <div class="pair-node">
        <div class="pair-name mono">${esc(m.cname_target)}</div>
        <div class="pair-sub">домен-приёмник</div>
      </div>
      <div class="pair-meta"><span class="badge">свёрнут</span></div>
    </div>`;
  }

  if (!m.node_id) {
    return `<div class="pair-card nodns">
      <div class="pair-domain">
        <div class="pair-name mono">${esc(m.domain)}</div>
        <div class="pair-sub" style="color:var(--bad)">A-записи нет</div>
      </div>
      <div class="pair-link"><span class="pair-broken">✕</span></div>
      <div class="pair-node">
        <div class="pair-name dim">мастер не назначен</div>
        <div class="pair-sub">ждёт здоровую ноду из очереди</div>
      </div>
      <div class="pair-meta"><span class="badge bad">нет DNS</span></div>
    </div>`;
  }

  const dead = !!m.dead;
  return `<div class="pair-card ${dead ? "sick" : "live"}">
    <div class="pair-domain">
      <div class="pair-name mono">${esc(m.domain)}</div>
      <div class="pair-sub mono">A → ${esc(m.ip || "?")}</div>
    </div>
    <div class="pair-link">${ttlRing(m)}</div>
    <div class="pair-node">
      <div class="pair-name mono">${esc(m.node_id)}</div>
      <div class="pair-sub">${nodeTypeBadge(m.node_type)}</div>
    </div>
    <div class="pair-meta">
      <div class="pair-clients"><b>${m.clients != null ? m.clients : "—"}</b><span>клиентов</span></div>
      <span class="badge ${dead ? "warn" : "ok"}">${dead ? "нода нездорова" : "live"}</span>
    </div>
  </div>`;
}

export function render(o: any): void {
  const masters: any[] = o.masters || [];
  // свёрнутые СРМД в CNAME домены не требуют мастера — счёт «живых» только
  // по активным доменам; число свёрнутых показываем отдельно
  const active = masters.filter((m) => !m.cname_target);
  const folded = masters.length - active.length;
  const aliveMs = active.filter((m) => m.node_id && !m.dead).length;
  const ttlNote = (o.master_ttl_sec || 0) > 0
    ? " · лимит мастера: " + fmtTtlMin(o.master_ttl_sec / 60)
    : " · лимит мастера: выкл";
  $("dns-hint").textContent = masters.length
    ? `${aliveMs}/${active.length} живых${folded ? " · свёрнуто СРМД: " + folded : ""}${ttlNote}`
    : "cloudflare.domains пуст";

  $("masters-full").innerHTML = masters.length === 0
    ? `<div class="empty-state"><div class="t">Домены не настроены</div><div class="s">Задайте их в «Настройки → Cloudflare DNS».</div></div>`
    : `<div class="pair-list">${masters.map(pairHTML).join("")}</div>`;

  $("dns-sync").innerHTML = dnsOpsHTML(o.dns);
  $("s-dns-push2").onclick = dnsPush;
}
