/* Экран «СРМД»: таблица доменов и настройки.

   Вычисления (что считается «основным»/«СРМД»/«ручным» доменом, текст
   подсказки, тело запроса srmd-domain, разбор ответа) перенесены из
   прежней версии ДОСЛОВНО — renderSRMD/loadSRMDSettings/srmdDomainAction.
   Меняется только разметка строки таблицы. */

import { $, esc } from "../../../lib/dom";
import { api, state, getToken, showToast, saveSection } from "../state";

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
    body.innerHTML = `<tr><td colspan="5" class="table-empty">Домены не настроены — задайте в «Настройки → Cloudflare DNS»</td></tr>`;
    return;
  }
  body.innerHTML = s.domains.map((d: any) => {
    const origin = d.base
      ? `<span class="badge gold" title="основной домен СРМД">основной</span>`
      : (d.created ? `<span class="badge info" title="создан СРМД с инкрементом">СРМД</span>`
                   : `<span class="badge" title="добавлен оператором вручную">ручной</span>`);
    const rec = d.cname
      ? `<span class="badge" title="домен свёрнут СРМД">CNAME → ${esc(d.cname)}</span>`
      : (d.ip ? `<span class="mono">A → ${esc(d.ip)}</span>` : `<span class="badge bad">нет записи</span>`);
    const master = d.cname
      ? `<span class="dim">— свёрнут</span>`
      : (d.node_id ? `<span class="mono" title="${esc(d.node_id)}">${esc(d.node_id)}</span>` : `<span class="dim">нет мастера</span>`);
    const clients = d.clients == null ? `<span class="dim">—</span>`
      : `${d.clients}${d.fresh ? "" : ` <span class="dim" title="последнее известное значение — живой мастер сейчас не присылает метрику">*</span>`}`;
    // ручной ⇄ СРМД: ручной домен можно насильно отдать СРМД (тогда он сможет
    // сворачиваться в CNAME), домен СРМД — вернуть в ручной режим
    const control = d.created
      ? `<button class="btn sm danger" data-srmd-release="${esc(d.domain)}" title="СРМД перестанет сворачивать этот домен${d.cname ? "; домен будет развёрнут" : ""}">в ручной</button>`
      : `<button class="btn sm" data-srmd-take="${esc(d.domain)}" title="Отдать домен под контроль СРМД: сможет сворачиваться в CNAME при сжатии пула">под СРМД</button>`;
    return `<tr>
      <td class="mono" data-label="Домен">${esc(d.domain)} ${origin}</td>
      <td class="mono num" data-label="Активные клиенты">${clients}</td>
      <td data-label="DNS-запись">${rec}</td>
      <td data-label="Мастер">${master}</td>
      <td data-label="Управление">${control}</td>
    </tr>`;
  }).join("");
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
      .then(() => api("/panel/api/overview"))
      .then((ov) => { state.ov = ov; renderSRMD(ov); })
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
