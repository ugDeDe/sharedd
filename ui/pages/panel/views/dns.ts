/* Экран «Мастера и DNS»: назначения доменов.

   Вычисления (что считается «живым» назначением, чип TTL, текст при
   отсутствии мастера) перенесены из прежней версии ДОСЛОВНО —
   renderMastersInto/masterRowHTML/fmtTtlMin. Разметка строк — таблица
   вместо самодельных .mrow-блоков, но данные и формулировки те же.

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
};
function nodeTypeBadge(t: string): string {
  const m = NODE_TYPE_META[t];
  if (!m) return "";
  return `<span class="badge ${m.cls}" title="${esc(m.title)}">${esc(m.label)}</span>`;
}

function masterRowHTML(m: any): string {
  // домен, свёрнутый СРМД в CNAME — мастера нет по определению
  if (m.cname_target) {
    return `<tr>
      <td class="mono" data-label="Домен">${esc(m.domain)}</td>
      <td class="mono" data-label="Назначение">CNAME → ${esc(m.cname_target)}${m.clients != null ? " · клиентов: " + m.clients : ""}</td>
      <td class="num" data-label="Клиенты">${m.clients != null ? m.clients : "—"}</td>
      <td data-label="TTL"><span class="dim">—</span></td>
      <td data-label="Статус"><span class="badge">свёрнут СРМД</span></td>
    </tr>`;
  }
  if (!m.node_id) {
    return `<tr>
      <td class="mono" data-label="Домен">${esc(m.domain)}</td>
      <td data-label="Назначение"><span style="color:var(--bad)">мастер не назначен — DNS-записи нет</span></td>
      <td class="num" data-label="Клиенты"><span class="dim">—</span></td>
      <td data-label="TTL"><span class="dim">—</span></td>
      <td data-label="Статус"><span class="badge bad">нет DNS</span></td>
    </tr>`;
  }
  const dead = !!m.dead;
  // чип принудительной ротации по лимиту времени мастерства
  let ttlChip = `<span class="dim">—</span>`;
  if (m.ttl_sec > 0) {
    if (m.ttl_remaining_sec < 0) {
      ttlChip = `<span class="badge bad">TTL истёк</span>`;
    } else {
      const usedMin = Math.max(0, Math.round((m.ttl_sec - m.ttl_remaining_sec) / 60));
      ttlChip = `<span class="badge">${fmtTtlMin(usedMin)} / ${fmtTtlMin(m.ttl_sec / 60)}</span>`;
    }
  }
  return `<tr>
    <td class="mono" data-label="Домен">${esc(m.domain)}</td>
    <td class="mono" data-label="Назначение">A → ${esc(m.ip || "?")} · ${esc(m.node_id)} ${nodeTypeBadge(m.node_type)}</td>
    <td class="num" data-label="Клиенты">${m.clients != null ? m.clients : "—"}</td>
    <td data-label="TTL">${ttlChip}</td>
    <td data-label="Статус"><span class="badge ${dead ? "bad" : "ok"}">${dead ? "нода нездорова" : "live"}</span></td>
  </tr>`;
}

async function dnsPush(): Promise<void> {
  try {
    const resp = await fetch("/panel/api/dns-push", { method: "POST", headers: { "Authorization": "Bearer " + getToken() } });
    const data = await resp.json();
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    showToast(
      `DNS-push: ${(data.updated || []).join(", ") || "—"}${(data.errors || []).length ? " · ошибки: " + data.errors.join("; ") : ""}`,
      (data.errors || []).length > 0 && (data.updated || []).length === 0,
    );
  } catch (e) {
    showToast((e as Error).message, true);
  }
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
    : `<div class="table-wrap"><table class="table-cards">
        <thead><tr><th>Домен</th><th>Назначение</th><th class="num">Клиенты</th><th>TTL</th><th>Статус</th></tr></thead>
        <tbody>${masters.map(masterRowHTML).join("")}</tbody>
      </table></div>`;

  $("s-dns-push2").onclick = dnsPush;
}

