/* Экран «СРМД»: таблица доменов и настройки.

   Вычисления (что считается «основным»/«СРМД»/«ручным» доменом, текст
   подсказки, тело запроса srmd-domain, разбор ответа) перенесены из
   прежней версии ДОСЛОВНО — renderSRMD/loadSRMDSettings/srmdDomainAction.
   Меняется только разметка строки таблицы. */

import { $, esc } from "../../../lib/dom";
import { api, state, getToken, showToast, saveSection, refreshAll } from "../state";

/* Полоса вместимости: сколько нод в очереди против того, сколько их
   вмещают домены. Именно по этой разнице СРМД и решает — создать новый
   домен или свернуть лишний. Раньше эти числа лежали строкой в подписи,
   и логика решения из них не читалась. */
function capacityHTML(s: any): string {
  const perDomain = Math.max(1, s.max_nodes_per_domain || 1);
  const activeDomains = Math.max(0, (s.total_domains || 0) - (s.folded_domains || 0));
  const capacity = activeDomains * perDomain;
  const used = s.queue_size || 0;
  const pct = capacity > 0 ? Math.min(100, (used / capacity) * 100) : 0;
  const over = capacity > 0 && used > capacity;
  const need = Math.max(0, (s.required_domains || 0) - activeDomains);

  const verdict = over || need > 0
    ? `<span class="cap-verdict warn">не хватает доменов: ${need || 1} — ${
        s.enabled ? "СРМД создаст недостающие" : "создание выключено, нужно включить"}</span>`
    : (activeDomains > (s.required_domains || 0)
      ? `<span class="cap-verdict">доменов больше, чем нужно — лишние будут свёрнуты в CNAME</span>`
      : `<span class="cap-verdict ok">вместимости хватает, менять ничего не нужно</span>`);

  return `<div class="capacity">
    <div class="cap-head">
      <span class="cap-title">Вместимость пула</span>
      <span class="cap-nums"><b>${used}</b> нод в очереди из <b>${capacity}</b>
        <span class="dim">(${activeDomains} × ${perDomain} на домен)</span></span>
    </div>
    <div class="cap-bar"><div class="cap-fill ${over ? "over" : pct > 80 ? "warn" : "ok"}" style="width:${pct.toFixed(1)}%"></div></div>
    <div class="cap-foot">
      ${verdict}
      <span class="cap-state ${s.enabled ? "on" : "off"}">${s.enabled ? "создание разрешено" : "создание выключено"}</span>
    </div>
  </div>`;
}

function domainCardHTML(d: any): string {
  const origin = d.base
    ? `<span class="badge gold" title="основной домен СРМД">основной</span>`
    : (d.created ? `<span class="badge info" title="создан СРМД с инкрементом">СРМД</span>`
                 : `<span class="badge" title="добавлен оператором вручную">ручной</span>`);
  const rec = d.cname
    ? `<span class="mono dim">CNAME → ${esc(d.cname)}</span>`
    : (d.ip ? `<span class="mono dim">A → ${esc(d.ip)}</span>` : `<span class="badge bad">нет записи</span>`);
  const master = d.cname
    ? `<span class="dim">свёрнут</span>`
    : (d.node_id ? `<span class="mono dim" title="${esc(d.node_id)}">${esc(d.node_id)}</span>`
                 : `<span style="color:var(--bad)">нет мастера</span>`);
  const stale = d.clients != null && !d.fresh;
  const clients = d.clients == null ? "—" : String(d.clients);
  // ручной ⇄ СРМД: ручной домен можно насильно отдать СРМД (тогда он сможет
  // сворачиваться в CNAME), домен СРМД — вернуть в ручной режим
  const control = d.created
    ? `<button class="btn sm danger" data-srmd-release="${esc(d.domain)}" title="СРМД перестанет сворачивать этот домен${d.cname ? "; домен будет развёрнут" : ""}">в ручной</button>`
    : `<button class="btn sm" data-srmd-take="${esc(d.domain)}" title="Отдать домен под контроль СРМД: сможет сворачиваться в CNAME при сжатии пула">под СРМД</button>`;

  return `<div class="srmd-card${d.cname ? " folded" : ""}">
    <div class="srmd-main">
      <div class="srmd-top"><span class="mono srmd-name">${esc(d.domain)}</span>${origin}</div>
      <div class="srmd-sub">${rec}<span class="dim">·</span>${master}</div>
    </div>
    <div class="srmd-clients" title="${stale ? "последнее известное значение — живой мастер сейчас не присылает метрику" : "активные клиенты по общему секрету"}">
      <b>${clients}${stale ? "<i>*</i>" : ""}</b><span>клиентов</span>
    </div>
    <div class="srmd-ctl">${control}</div>
  </div>`;
}

function renderSRMD(o: any): void {
  const s = o.srmd;
  if (!s) return;
  $("srmd-hint").textContent =
    `очередь: ${s.queue_size} нод · нужно доменов: ${s.required_domains} · лимит: ${s.max_nodes_per_domain} нод/домен · ` +
    `доменов: ${s.total_domains}${s.folded_domains ? " (свёрнуто: " + s.folded_domains + ")" : ""} · ` +
    (s.enabled ? "создание доменов разрешено" : "создание доменов выключено");
  const created = (s.domains || []).filter((d: any) => d.created).map((d: any) => d.domain);
  $("srmd-created-list").textContent = created.length ? created.join(", ") : "нет";

  const body = $("srmd-body");
  if (!s.domains || s.domains.length === 0) {
    body.innerHTML = capacityHTML(s) +
      `<div class="empty-state"><span class="t">Домены не настроены</span>` +
      `<span class="s">Задайте их в «Настройки → Cloudflare DNS».</span></div>`;
    return;
  }
  body.innerHTML = capacityHTML(s) + `<div class="srmd-list">${s.domains.map(domainCardHTML).join("")}</div>`;
}

// перевод домена ручной ⇄ под контролем СРМД
async function srmdDomainAction(domain: string, action: string): Promise<any> {
  const resp = await fetch("/panel/api/srmd-domain", {
    method: "POST",
    headers: { "Authorization": "Bearer " + getToken(), "Content-Type": "application/json" },
    body: JSON.stringify({ domain, action }),
  });
  const data = await resp.json().catch(() => ({}));
  if (resp.status === 401) throw new Error("unauthorized");
  if (!resp.ok) throw new Error(data.error || ("HTTP " + resp.status));
  return data;
}

export async function loadSettings(): Promise<void> {
  const data = await api("/panel/api/config");
  const s = (data.config && data.config.srmd) || {};
  ($("s-srmd-base") as HTMLInputElement).value = s.base_domain || "";
  ($("s-srmd-base") as HTMLInputElement).placeholder = "shared.ddproxy.xyz";
  ($("s-srmd-max") as HTMLInputElement).value = s.max_nodes_per_domain ?? 5;
  ($("s-srmd-enabled") as HTMLInputElement).checked = !!s.enabled;
}

export function render(o: any): void {
  renderSRMD(o);
}

export function init(): void {
  $("srmd-body").addEventListener("click", (e) => {
    const target = e.target as HTMLElement;
    const take = target.closest<HTMLElement>("button[data-srmd-take]");
    const rel = target.closest<HTMLElement>("button[data-srmd-release]");
    if (!take && !rel) return;
    const domain = take ? take.dataset.srmdTake! : rel!.dataset.srmdRelease!;
    const action = take ? "take" : "release";
    srmdDomainAction(domain, action)
      .then(() => showToast(take ? `${domain} — под контролем СРМД` : `${domain} — снова в ручном режиме`))
      // как в прежней версии — не ждём общий 5-секундный поллинг, сразу
      // подтягиваем свежий /panel/api/overview и перерисовываем таблицу
      // Прежняя версия после take/release дёргала полное обновление —
      // не только таблицу СРМД, но и шапку, баннер тревог и журнал.
      // Делаем ровно то же, чтобы поведение не разошлось.
      .then(() => refreshAll())
      .catch((e) => showToast((e as Error).message, true));
  });

  $("srmd-reload").onclick = () => loadSettings().then(() => showToast("Настройки СРМД перечитаны")).catch(() => {});
  $("s-srmd-save").onclick = () =>
    saveSection({ srmd: {
      enabled: ($("s-srmd-enabled") as HTMLInputElement).checked,
      base_domain: ($("s-srmd-base") as HTMLInputElement).value.trim(),
      max_nodes_per_domain: Math.max(1, parseInt(($("s-srmd-max") as HTMLInputElement).value, 10) || 5),
    } }, "Настройки СРМД сохранены").catch((e) => showToast(e.message, true));
}
