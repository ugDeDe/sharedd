/* Детальная карточка ноды в кабинете (plan-ui-batch.md Фаза 2, slice 1).
   Данные — `GET /api/me/nodes/{id}/detail` (node_detail_api.go): владелец
   видит свои ноды, чужая/legacy → 404. Разделы без источника приходят null и
   перечислены в absent[] — рисуем «нет данных», не выдумываем значения.
   Статистика разбита по КАТЕГОРИЯМ-ТАБАМ (порт nav.c-tabs из mtplane):
   Обзор · Метрики · Проверки · Журнал · Действия. Активный таб — в хэше
   `#/nodes/<id>?tab=<key>`. Разметка — .card/.table-cards/.badge/.metrics/.kv/.feed.
   Опрос каждые 5 с идёт через renderSection: не сбивает фокус, прокрутку
   и активный таб. */

import { $, esc, renderSection } from "../../lib/dom";
import { fmtAgo, fmtClock, fmtDur, fmtNum, plural } from "../../lib/format";
import { barChart } from "../../lib/chart";
import {
  connectionsCardHTML, fmtPct, hostingCardHTML, installLiveCharts, srTable, userIPsBarsHTML, usersTableHTML,
} from "../../lib/node-load";
import { api } from "./api";
import type { CloudResource, ManagementPolicy, NodeDetail, NodeSettings, Operation } from "./api";
import { mountBlueprintManagement } from "./blueprint-management";

const EMPTY_SVG =
  `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="3" width="20" height="7" rx="2"/><rect x="2" y="14" width="20" height="7" rx="2"/></svg>`;

const STATUS_META: Record<string, { label: string; cls: string }> = {
  healthy:     { label: "здорова", cls: "ok" },
  unhealthy:   { label: "нездорова", cls: "bad" },
  quarantined: { label: "карантин Globalping", cls: "warn" },
  deleting:    { label: "удаляется", cls: "warn" },
  offline:     { label: "нет данных", cls: "" },
};

const NODE_TYPE_META: Record<string, { label: string; cls: string; title: string }> = {
  classic:  { label: "Classic",  cls: "",        title: "классический telemt" },
  mtproxyl: { label: "MTProxyL", cls: "special", title: "форк MTProxyL (/opt/mtproxyl/)" },
  meko:     { label: "MEKO",     cls: "info",    title: "фикс MEKO (/opt/mtpr-simple/)" },
  mtdd:     { label: "mtdd",     cls: "info",     title: "прокси mtdd (/etc/mtdd/)" },
};

// Подписи типов событий ноды (как в панели, core set). Неизвестный тип
// показывается как есть — не выдумываем ярлык.
const EV_LABEL: Record<string, { label: string; dot: string }> = {
  node_registered:      { label: "Регистрация ноды",      dot: "info" },
  node_replaced:        { label: "Нода вытеснена",        dot: "warn" },
  node_expired:         { label: "Нода отвалилась",       dot: "warn" },
  node_pruned:          { label: "Удалена (неактивна)",   dot: "warn" },
  tcp_down:             { label: "TCP недоступен",        dot: "bad" },
  tcp_up:               { label: "TCP восстановлен",      dot: "ok" },
  metrics_down:         { label: "Метрики в отказе",      dot: "bad" },
  metrics_up:           { label: "Метрики восстановлены", dot: "ok" },
  globalping_blocked:   { label: "Блокировка (GP)",       dot: "bad" },
  globalping_recovered: { label: "GP восстановлен",       dot: "ok" },
  queue_joined:         { label: "В очереди мастеров",    dot: "info" },
  queue_left:           { label: "Вышла из очереди",      dot: "warn" },
  master_elected:       { label: "МАСТЕР ИЗБРАН",         dot: "ok" },
  master_lost:          { label: "Мастер потерян",        dot: "bad" },
  dns_updated:          { label: "DNS обновлён",          dot: "info" },
  dns_error:            { label: "Ошибка DNS",            dot: "bad" },
  dns_deleted:          { label: "DNS запись удалена",    dot: "warn" },
  node_quarantined:     { label: "Карантин (GP)",         dot: "warn" },
  quarantine_recovered: { label: "Вышла из карантина",    dot: "ok" },
  node_terminated:      { label: "Нода завершена (бан)",  dot: "bad" },
  ban_lifted:           { label: "Бан снят (смена IP)",   dot: "info" },
  ip_blocked:           { label: "IP заблокирован",       dot: "bad" },
};

// Разделы-табы: статистика разбита по категориям, как в mtplane (nav.c-tabs),
// а не свалена в один вертикальный скролл. Порядок табов = порядок категорий.
const TABS: { key: string; label: string }[] = [
  { key: "overview", label: "Обзор" },
  { key: "metrics",  label: "Метрики" },
  { key: "checks",   label: "Проверки" },
  { key: "management", label: "Управление" },
  { key: "journal",  label: "Журнал" },
  { key: "actions",  label: "Действия" },
];
const TAB_KEYS = TABS.map((t) => t.key);

let DETAIL_ID = "";
let DETAIL: NodeDetail | null = null;
let DETAIL_ERR: { msg: string; status?: number } | null = null;
let DETAIL_RETRY = false;
// Управление нодой (cabinet_cloud.go §5) + связь с облачным ресурсом (§18).
let MGMT: ManagementPolicy | null = null;
let MGMT_ERR: string | null = null;
let NODE_RES: CloudResource | null = null;
let NODE_OP: Operation | null = null;
let SETTINGS: NodeSettings | null = null;
let SETTINGS_ERR: string | null = null;
let SETTINGS_DRAFT: string | null = null;
let SETTINGS_AD_DRAFT: string | null = null;
// Активный таб. Живёт между опросами (5 с); сбрасывается в openNodeDetail.
let DETAIL_TAB = "overview";

export function nodeDetailId(): string { return DETAIL_ID; }

/** Открыть детальную карточку ноды: сброс предыдущей, затем загрузка. */
export function openNodeDetail(id: string): void {
  DETAIL_ID = id;
  DETAIL = null;
  DETAIL_ERR = null;
  DETAIL_RETRY = false;
  MGMT = null;
  MGMT_ERR = null;
  NODE_RES = null;
  NODE_OP = null;
  SETTINGS = null;
  SETTINGS_ERR = null;
  SETTINGS_DRAFT = null;
  SETTINGS_AD_DRAFT = null;
  DETAIL_TAB = parseTabHash(); // дефолт «Обзор»; валидный #?tab= восстанавливаем
  const el = $("node-detail");
  if (el) el.innerHTML = `<div class="skeleton skel-block"></div>`;
  load(false);
}

export function closeNodeDetail(): void {
  DETAIL_ID = "";
  DETAIL = null;
  DETAIL_ERR = null;
  MGMT = null;
  NODE_RES = null;
  NODE_OP = null;
  SETTINGS = null;
  SETTINGS_ERR = null;
  SETTINGS_DRAFT = null;
  SETTINGS_AD_DRAFT = null;
}

/* ── табы: hash-навигация и переключение панелей ──────────────────────
   Активный таб отражается в хэше как `?tab=<key>` внутри того же маршрута
   `#/nodes/<id>` (SPA владеет хэшем). writeTabHash идёт через replaceState —
   hashchange не срабатывает и маршрут не перезапускается. */
function parseTabHash(): string {
  const m = /[?&]tab=([a-z]+)/.exec(location.hash || "");
  return m && TAB_KEYS.includes(m[1]) ? m[1] : "overview";
}

function writeTabHash(name: string): void {
  if (!DETAIL_ID) return;
  const clean = (location.hash || "").replace(/[?&]tab=[a-z]+/, "");
  const base = clean || "#/nodes/" + encodeURIComponent(DETAIL_ID);
  const next = base + (base.includes("?") ? "&" : "?") + "tab=" + name;
  history.replaceState(null, "", location.pathname + location.search + next);
}

/** Переключить таб без полной перерисовки: скрыть/показать панели, обновить
 *  aria-selected + roving tabindex. focusPanel — клик фокусирует панель,
 *  стрелки оставляют фокус на табе (APG). */
function activateTab(name: string, focusPanel: boolean): void {
  if (!TAB_KEYS.includes(name)) name = "overview";
  DETAIL_TAB = name;
  document.querySelectorAll<HTMLButtonElement>("#node-detail .nd-tabs [role='tab']").forEach((b) => {
    const on = b.dataset.tab === name;
    b.setAttribute("aria-selected", String(on));
    b.tabIndex = on ? 0 : -1;
  });
  document.querySelectorAll<HTMLElement>("#node-detail [data-tab-panel]").forEach((p) => {
    p.hidden = p.dataset.tabPanel !== name;
  });
  writeTabHash(name);
  // Панели «Управление»/«Действия» опираются на политику и ресурс — тянем
  // их, если ещё не загружены (ленивая загрузка не блокирует деталь).
  if ((name === "management" || name === "actions") && !MGMT && !MGMT_ERR) refreshMgmt(false);
  if (focusPanel) document.getElementById("nd-panel-" + name)?.focus({ preventScroll: true });
}

// Клик и стрелки — делегированы на document: innerHTML-перерисовка опроса
// (5 с) сносит слушатели, привязанные к nav.
document.addEventListener("click", (e) => {
  const el = e.target as HTMLElement;
  if (!DETAIL_ID) return;
  if (el.closest("[data-mgmt-retry]")) { refreshMgmt(false); return; }
  const tab = el?.closest?.("#node-detail .nd-tabs [role='tab']") as HTMLElement | null;
  if (!tab) return;
  activateTab(tab.dataset.tab || "overview", true);
});
document.addEventListener("keydown", (e) => {
  const tabs = Array.from(document.querySelectorAll<HTMLButtonElement>("#node-detail .nd-tabs [role='tab']"));
  const idx = tabs.indexOf(document.activeElement as HTMLButtonElement);
  if (idx < 0) return;
  let next = -1;
  if (e.key === "ArrowRight") next = (idx + 1) % tabs.length;
  else if (e.key === "ArrowLeft") next = (idx - 1 + tabs.length) % tabs.length;
  else if (e.key === "Home") next = 0;
  else if (e.key === "End") next = tabs.length - 1;
  if (next < 0) return;
  e.preventDefault();
  tabs[next].focus();
  activateTab(tabs[next].dataset.tab || "overview", false);
});

function load(poll: boolean): void {
  if (!DETAIL_ID) return;
  const id = DETAIL_ID;
  // Политику/ресурс опрашиваем всегда при первом входе и далее — только на
  // сегментах, которые их показывают (не множим запросы на каждом такте).
  if (!poll || DETAIL_TAB === "management" || DETAIL_TAB === "actions") refreshMgmt(poll);
  if (!poll || DETAIL_TAB === "management") refreshSettings();
  api.nodeDetail(id)
    .then((d) => {
      if (id !== DETAIL_ID) return; // ушли на другую ноду
      DETAIL = d;
      DETAIL_ERR = null;
      DETAIL_RETRY = false;
      render(poll);
    })
    .catch((e: Error & { status?: number; code?: string }) => {
      if (id !== DETAIL_ID) return;
      if (e.message === "unauthorized") return;
      // 404: нода чужая, удалена или не существует — не подтверждаем наличие.
      DETAIL_ERR = { msg: e.status === 404 ? "Нода не найдена или недоступна." : e.message, status: e.status };
      DETAIL = null;
      render(poll);
      if (poll && e.status !== 404 && !DETAIL_RETRY && (e.status ?? 0) >= 500) {
        DETAIL_RETRY = true;
        setTimeout(() => load(true), 2000);
      }
    });
}

/** Перезагрузить открытую карточку (опрос из общего цикла кабинета). */
export function pollNodeDetail(): void {
  if (DETAIL_ID) load(true);
}

/* Управление нодой + связь с облачным ресурсом (§16/§18). Отдельный запрос,
   не блокирует деталь; нода без строки policy получает виртуальный manual. */
function refreshMgmt(poll: boolean): void {
  const id = DETAIL_ID;
  if (!id) return;
  Promise.all([api.nodeManagement(id), api.cloudResources(), api.operations()])
    .then(([m, res, ops]) => {
      if (id !== DETAIL_ID) return;
      MGMT = m; MGMT_ERR = null;
      NODE_RES = (res.resources || []).find((r) => r.node_id === id) || null;
      const active = new Set(["pending", "sent", "confirmed", "installing", "waiting_vps", "deleting"]);
      NODE_OP = NODE_RES
        ? (ops.operations || []).find((o) => o.node_id === id && active.has(o.state)) || null
        : null;
      render(poll);
    })
    .catch((e: Error & { status?: number }) => {
      if (id !== DETAIL_ID) return;
      if (e.message === "unauthorized") return;
      MGMT_ERR = e.status === 404 ? "Нода не найдена." : e.message;
      render(poll);
    });
}

/* Параметры логической ноды: имя/номер слота. Runtime-ad_tag пока
   unsupported; не показываем ложное «применено». */
function refreshSettings(): void {
  const id = DETAIL_ID;
  if (!id) return;
  api.nodeSettings(id)
    .then((s) => {
      if (id !== DETAIL_ID) return;
      SETTINGS = s;
      SETTINGS_ERR = null;
      render(false);
    })
    .catch((e: Error & { status?: number }) => {
      if (id !== DETAIL_ID) return;
      if (e.message === "unauthorized") return;
      SETTINGS_ERR = e.status === 404 ? "У ноды нет логического слота." : e.message;
      render(false);
    });
}

function render(poll: boolean): void {
  const el = $("node-detail");
  if (!el) return;
  renderSection(el, poll, () => {
    if (DETAIL_ERR) {
      el.innerHTML = `<div class="empty-state">${EMPTY_SVG}
        <span class="t">Не удалось открыть ноду</span>
        <span class="s">${esc(DETAIL_ERR.msg)}</span>
        <a class="btn sm" href="#/nodes" style="margin-top:6px">← К нодам</a></div>`;
      return;
    }
    if (!DETAIL) { el.innerHTML = `<div class="skeleton skel-block"></div>`; return; }
    const management = document.getElementById("blueprint-management");
    el.innerHTML = detailHTML(DETAIL);
    const renameBtn = el.querySelector<HTMLButtonElement>("[data-slot-rename]");
    const nameInp = el.querySelector<HTMLInputElement>("#nd-slot-name");
    if (nameInp) nameInp.oninput = () => { SETTINGS_DRAFT = nameInp.value; };
    if (renameBtn && SETTINGS) {
      renameBtn.onclick = async () => {
        const inp = el.querySelector<HTMLInputElement>("#nd-slot-name");
        const msg = el.querySelector<HTMLElement>("[data-slot-msg]");
        const name = (inp?.value || "").trim();
        if (!name || !msg) return;
        renameBtn.disabled = true;
        msg.textContent = "Сохранение…";
        try {
          SETTINGS = await api.patchNodeSettings(DETAIL_ID, {
            expected_revision: SETTINGS!.revision,
            name,
          });
          SETTINGS_DRAFT = null;
          if (inp) inp.value = SETTINGS.name;
          msg.textContent = "Название сохранено. Технический ID не изменился.";
        } catch (e) {
          msg.textContent = (e as Error & { code?: string }).code === "revision_conflict"
            ? "Настройки уже изменились — откройте карточку заново."
            : `Не удалось сохранить: ${(e as Error).message}`;
        } finally {
          renameBtn.disabled = false;
        }
      };
    }
    const adBtn = el.querySelector<HTMLButtonElement>("[data-ad-save]");
    const adInp = el.querySelector<HTMLInputElement>("#nd-ad-tag");
    if (adInp) adInp.oninput = () => { SETTINGS_AD_DRAFT = adInp.value; };
    if (adBtn && adInp && SETTINGS) {
      adBtn.onclick = async () => {
        const msg = el.querySelector<HTMLElement>("[data-ad-msg]");
        if (!msg) return;
        adBtn.disabled = true;
        msg.textContent = "Сохранение…";
        try {
          SETTINGS = await api.patchNodeSettings(DETAIL_ID, {
            expected_revision: SETTINGS!.revision,
            runtime: { ad_tag: adInp.value.trim() },
          });
          SETTINGS_AD_DRAFT = null;
          const st = SETTINGS.runtime.ad_tag;
          msg.textContent = st?.message || "Сохранено. Ожидается подтверждение агента.";
          if (st?.state === "pending") msg.textContent = "Сохранено. Ожидается подтверждение агента.";
          if (st?.state === "failed") msg.textContent = `Не применено: ${st.message || "ошибка агента"}`;
        } catch (e) {
          const err = e as Error & { code?: string };
          msg.textContent = err.code === "unsupported_setting"
            ? "Эта нода не поддерживает изменение ad_tag на живой ноде. Задайте его при переустановке."
            : `Не удалось сохранить: ${err.message}`;
        } finally {
          adBtn.disabled = false;
        }
      };
    }
    const target = document.getElementById("blueprint-management");
    // Preserve the complete policy form/input snapshot across statistics polls.
    if (target && management?.dataset.mounted === DETAIL_ID) target.replaceWith(management);
    else if (target && MGMT) mountBlueprintManagement(target, DETAIL_ID, MGMT, NODE_RES, (m) => { MGMT = m; });
  });
}

function chip(ok: boolean, okText: string, badText: string): string {
  return ok ? `<span class="badge ok">${esc(okText)}</span>` : `<span class="badge bad">${esc(badText)}</span>`;
}

function detailHTML(d: NodeDetail): string {
  const n = d.node;
  if (!n) {
    return `<div class="empty-state">${EMPTY_SVG}<span class="t">Нода вне пула</span>
      <span class="s">Параметры недоступны — нода не зарегистрирована в пуле.</span></div>`;
  }
  const meta = STATUS_META[n.status] || STATUS_META.offline;
  const nt = n.node_type ? NODE_TYPE_META[n.node_type] : undefined;
  const q = n.quarantine;
  const slot = SETTINGS && SETTINGS.node_id === n.node_id ? SETTINGS : null;

  // Шапка со статусом — всегда над табами.
  const head = `
  <div class="card nd-head">
    <div>
      <div class="nd-ip-lead"><span class="mono">${esc(n.ip || "—")}${n.port ? ":" + n.port : ""}</span>
        · зарегистрирована ${esc(fmtAgo(n.registered_at))}</div>
      <div class="nd-ip">${slot ? `<b>${esc(slot.name)} · Нода №${slot.number}</b> · ` : ""}нода <span class="mono">${esc(n.node_id)}</span></div>
    </div>
    <div class="nd-chips">
      <span class="badge ${meta.cls}"><i></i>${esc(meta.label)}</span>
      ${n.is_master ? `<span class="badge gold">★ мастер</span>` : ""}
      ${nt ? `<span class="badge ${nt.cls}" title="${esc(nt.title)}">${esc(nt.label)}</span>` : ""}
      ${q ? `<span class="badge warn"><i></i>карантин</span>` : ""}
    </div>
    ${n.unhealthy_reason ? `<div class="nd-reason">${esc(n.unhealthy_reason)}</div>` : ""}
    <button class="btn danger sm" data-del="${esc(n.node_id)}" style="margin-left:auto">Удалить ноду</button>
  </div>`;

  const tabs = `<nav class="nd-tabs" role="tablist" aria-label="Разделы ноды">${TABS.map((t) => {
    const on = t.key === DETAIL_TAB;
    return `<button type="button" role="tab" id="nd-tab-${t.key}" data-tab="${t.key}" aria-controls="nd-panel-${t.key}" aria-selected="${on}" tabindex="${on ? 0 : -1}">${esc(t.label)}</button>`;
  }).join("")}</nav>`;

  const panels = TABS.map((t) => {
    const on = t.key === DETAIL_TAB;
    return `<div class="nd-panel" id="nd-panel-${t.key}" role="tabpanel" aria-labelledby="nd-tab-${t.key}" tabindex="-1" data-tab-panel="${t.key}"${on ? "" : " hidden"}>${panelHTML(t.key, d)}</div>`;
  }).join("");

  return head + tabs + panels;
}

/** Содержимое панели по ключу таба. Категории сохраняют исходный порядок. */
function panelHTML(key: string, d: NodeDetail): string {
  switch (key) {
    case "metrics": return chartCHTML(d) + hostingHTML(d) + telemtHTML(d) + controlApiHTML(d);
    case "checks":  return checksHTML(d);
    case "management": return settingsHTML() + mgmtHTML() + resLinkageHTML();
    case "journal": return eventsHTML(d);
    case "actions": return actionsHTML(d);
    default:        return paramsHTML(d) + chartAHTML(d) + ipHistoryHTML(d);
  }
}

function paramsHTML(_d: NodeDetail): string {
  const n = _d.node!;
  const q = n.quarantine;
  return `<section class="card">
    <div class="card-head"><h2>Параметры</h2><span class="hint">${esc(n.node_id)}</span></div>
    <div class="nd-grid">
      <div class="kv"><span class="kv-k">IP</span><span class="kv-v mono">${esc(n.ip || "—")}</span></div>
      <div class="kv"><span class="kv-k">Порт</span><span class="kv-v mono">${n.port || "—"}</span></div>
      <div class="kv"><span class="kv-k">Регион</span><span class="kv-v">${esc(n.region || "—")}</span></div>
      <div class="kv"><span class="kv-k">Аптайм</span><span class="kv-v">${esc(fmtDur(n.uptime_sec))}</span></div>
      <div class="kv"><span class="kv-k">Heartbeat</span><span class="kv-v">${esc(fmtDur(n.heartbeat_age_sec))} назад</span></div>
      <div class="kv"><span class="kv-k">Отчёт</span><span class="kv-v">${n.report_age_sec < 0 ? "—" : esc(fmtDur(n.report_age_sec)) + " назад"}</span></div>
      <div class="kv"><span class="kv-k">Роль</span><span class="kv-v">${n.is_master ? "мастер" : n.queue_position > 0 ? `очередь #${n.queue_position}` : "вне очереди"}</span></div>
      <div class="kv"><span class="kv-k">Домены</span><span class="kv-v mono">${esc((n.master.domains || []).join(", ") || "—")}</span></div>
      <div class="kv"><span class="kv-k">Слоты</span><span class="kv-v">${n.master.slots_filled} / ${n.master.slots_total}</span></div>
      <div class="kv"><span class="kv-k">TTL мастерства</span><span class="kv-v">${n.master.ttl_remaining_sec != null ? esc(fmtDur(n.master.ttl_remaining_sec)) : "—"}</span></div>
      <div class="kv"><span class="kv-k">Мастерство</span><span class="kv-v">${n.master.master_stints > 0 ? esc(fmtDur(n.master.master_time_sec)) + ` · ${n.master.master_stints} stint` : "—"}</span></div>
      <div class="kv"><span class="kv-k">Quarantine</span><span class="kv-v">${q ? `${q.attempts} попыток · с ${esc(fmtClock(new Date(q.entered_at * 1000).toISOString()))}` : "—"}</span></div>
    </div>
  </section>`;
}

function ipHistoryHTML(d: NodeDetail): string {
  return `<section class="card">
    <div class="card-head"><h2>История IP</h2><span class="hint">по журналу событий</span></div>
    ${(d.ip_history && d.ip_history.length)
      ? `<div class="table-wrap"><table class="table-cards">
          <thead><tr><th>IP</th><th>Впервые</th><th>Последний раз</th><th class="num">Событий</th></tr></thead>
          <tbody>${d.ip_history.map((r) => `<tr>
            <td class="mono" data-label="IP">${esc(r.ip || "—")}</td>
            <td class="mono" data-label="Впервые">${esc(fmtClock(r.first_seen))}</td>
            <td class="mono" data-label="Последний раз">${esc(fmtClock(r.last_seen))}</td>
            <td class="num" data-label="Событий">${fmtNum(r.events)}</td>
          </tr>`).join("")}</tbody></table></div>`
      : `<div class="table-empty">${d.ip_history === null ? "нет данных" : "записей нет"}</div>`}
  </section>`;
}

function checksHTML(d: NodeDetail): string {
  return `<section class="card">
    <div class="card-head"><h2>Последние проверки</h2><span class="hint">tcp · globalping · отчёт</span></div>
    ${d.last_checks.length
      ? `<div class="table-wrap"><table class="table-cards">
          <thead><tr><th>Время</th><th>Источник</th><th>Итог</th><th>Детали</th></tr></thead>
          <tbody>${d.last_checks.map((c) => `<tr>
            <td class="mono" data-label="Время">${esc(fmtClock(c.at))}</td>
            <td data-label="Источник">${esc(sourceLabel(c.source))}</td>
            <td data-label="Итог">${chip(c.ok, "ок", "провал")}</td>
            <td class="wrap" data-label="Детали">${esc(c.detail || "—")}</td>
          </tr>`).join("")}</tbody></table></div>`
      : `<div class="table-empty">проверок пока нет</div>`}
  </section>`;
}

function eventsHTML(d: NodeDetail): string {
  return `<section class="card">
    <div class="card-head"><h2>События ноды</h2><span class="hint">последние ${d.events.length}</span></div>
    <ul class="feed" id="nd-events" style="max-height:none">
      ${d.events.length ? d.events.map(eventRow).join("") : `<li class="empty-state"><span class="t">Событий пока нет</span></li>`}
    </ul>
  </section>`;
}

/* ── «Управление» и «Действия»: политика ноды и связь с ресурсом ────── */

const CR_STATE_META: Record<string, { label: string; cls: string }> = {
  planned:  { label: "запланирован", cls: "info" },
  creating: { label: "создаётся…", cls: "info" },
  ready:    { label: "готов", cls: "ok" },
  deleting: { label: "удаляется…", cls: "warn" },
  deleted:  { label: "удалён", cls: "" },
  error:    { label: "ошибка", cls: "bad" },
};
function crState(state: string): { label: string; cls: string } {
  return CR_STATE_META[state] || { label: state, cls: "info" };
}

const OP_STATE_META: Record<string, { label: string; cls: string }> = {
  pending:    { label: "ожидает", cls: "info" },
  sent:       { label: "отправлено провайдеру", cls: "info" },
  confirmed:  { label: "подтверждено", cls: "ok" },
  installing: { label: "установка профиля", cls: "info" },
  waiting_vps:{ label: "ждём готовности VPS", cls: "warn" },
  deleting:   { label: "удаляется", cls: "warn" },
};

/* Бан/диспозиция — информационно, без намёка на автопокупку (design doc §5). */
function banNoteHTML(d: NodeDetail): string {
  const banned = d.events.some((e) => e.type === "ip_blocked" || e.type === "node_terminated");
  const quarantined = !!d.node?.quarantine || d.node?.status === "quarantined";
  if (!banned && !quarantined) return "";
  if (MGMT?.mode === "profile") return `<div class="banner warn" role="note"><span>IP в блокировке. Включённый профильный режим разрешает действия закреплённого профиля, включая платные действия. Настройки и текущий эпизод — в разделе «Управление».</span></div>`;
  return `<div class="banner info" role="note"><span>IP в блокировке (ip_ban) — это вердикт регистратора, показан информационно. Он <b>не</b> запускает покупку или замену сервера: автопокупка при бане не выполняется. Замену можно инициировать только вручную.</span></div>`;
}

function settingsHTML(): string {
  const head = `<div class="card-head"><h2>Название и параметры</h2><span class="hint">логическая нода</span></div>`;
  if (!SETTINGS) {
    return `<section class="card">${head}<div class="card-body"><div class="skeleton skel-block"></div>${SETTINGS_ERR ? `<div class="banner warn" role="status">${esc(SETTINGS_ERR)}</div>` : ""}</div></section>`;
  }
  const ad = SETTINGS.runtime.ad_tag;
  const adValue = ad?.value || "";
  const adDisabled = ad?.state === "unsupported";
  return `<section class="card">${head}
    <div class="card-body">
      <div class="srv-grid">
        <div class="field">
          <label for="nd-slot-name">Название</label>
          <input id="nd-slot-name" type="text" maxlength="10" value="${esc(SETTINGS_DRAFT ?? SETTINGS.name)}" autocomplete="off" spellcheck="false" aria-describedby="nd-slot-help">
          <span class="help" id="nd-slot-help">1–10 символов — латиница, цифры, . _ -. Меняется только название в кабинете и имя будущих установок; технический ID, секрет и история остаются прежними.</span>
        </div>
        <div class="field">
          <span class="static-head">Публичный номер</span>
          <div class="kv-v">Нода №${SETTINGS.number}</div>
          <span class="help">Постоянный номер логической ноды. Сохраняется при замене VPS.</span>
        </div>
      </div>
      <div class="blueprint-toolbar">
        <button type="button" class="btn primary" data-slot-rename>Сохранить название</button>
        <span class="hint" data-slot-msg aria-live="polite"></span>
      </div>
      <div class="srv-grid" style="margin-top:14px">
        <div class="field">
          <label for="nd-ad-tag">Рекламный тег (ad_tag)</label>
          <input id="nd-ad-tag" class="mono" type="text" maxlength="32" value="${esc(SETTINGS_AD_DRAFT ?? adValue)}" autocomplete="off" spellcheck="false" aria-describedby="nd-ad-help" ${adDisabled ? "disabled" : ""}>
          <span class="help" id="nd-ad-help">32 hex-символа. Пусто — снять тег. Применяется только на MTProxyL и подтверждается агентом; профиль не меняется.</span>
        </div>
      </div>
      <div class="blueprint-toolbar">
        <button type="button" class="btn" data-ad-save ${adDisabled ? "disabled" : ""}>Применить ad_tag</button>
        <span class="hint" data-ad-msg aria-live="polite">${esc(ad?.message || "")}</span>
      </div>
      ${ad?.state === "unsupported" ? `<div class="banner info" role="note"><span>${esc(ad.message || "Изменение ad_tag на этой ноде недоступно.")}</span></div>` : ""}
    </div>
  </section>`;
}

function mgmtHTML(): string {  const head = `<div class="card-head"><h2>Управление</h2><span class="hint">режим обслуживания ноды</span></div>`;
  if (MGMT_ERR) {
    return `<section class="card">${head}<div class="banner bad" role="alert">${esc(MGMT_ERR)} · Последние полученные настройки не считаются обновлёнными. <button type="button" class="btn sm" data-mgmt-retry>Повторить GET</button></div>${MGMT ? '<div class="card-body" id="blueprint-management"></div>' : ""}</section>`;
  }
  if (!MGMT) {
    return `<section class="card">${head}<div class="card-body"><div class="skeleton skel-block"></div></div></section>`;
  }
  return `<section class="card">${head}<div class="card-body" id="blueprint-management"></div></section>`;
}

/* §18: цепочка нода → ресурс → подключение и активная операция. */
function resLinkageHTML(): string {
  if (!NODE_RES) return "";
  const m = crState(NODE_RES.state);
  const op = NODE_OP;
  const opMeta = op ? (OP_STATE_META[op.state] || { label: op.state, cls: "" }) : null;
  return `<section class="card">
    <div class="card-head"><h2>Облачный ресурс</h2><span class="hint">связь ноды и ресурса</span></div>
    <div class="nd-grid card-body">
      <div class="kv"><span class="kv-k">Ресурс</span><span class="kv-v"><a class="mono" href="#/resources/${NODE_RES.id}">#${NODE_RES.id}</a> · ${esc(NODE_RES.label)}</span></div>
      <div class="kv"><span class="kv-k">Состояние</span><span class="kv-v"><span class="badge ${m.cls}"><i></i>${esc(m.label)}</span></span></div>
      <div class="kv"><span class="kv-k">Подключение</span><span class="kv-v">${esc(NODE_RES.provider)} · #${NODE_RES.connection_id}</span></div>
      ${op ? `<div class="kv"><span class="kv-k">Активная операция</span><span class="kv-v">#${op.id} <span class="badge ${opMeta!.cls}"><i></i>${esc(opMeta!.label)}</span> · <a href="#/operations">К операциям</a></span></div>` : ""}
    </div>
  </section>`;
}

function actionsHTML(d: NodeDetail): string {
  const m = MGMT;
  const res = NODE_RES;
  const resLink = m?.current_resource_id
    ? `<a class="node-open mono" href="#/resources/${m.current_resource_id}">#${m.current_resource_id}</a>`
    : "—";
  const conn = res ? `${esc(res.provider)} · #${res.connection_id}` : "—";
  const ready = res?.state === "ready";
  const canReplace = !res || res.state === "ready" || res.state === "error";
  return `<section class="card">
    <div class="card-head"><h2>Действия</h2><span class="hint">ручное управление ресурсом</span></div>
    <div class="card-body nd-grid" style="padding-bottom:0">
      <div class="kv"><span class="kv-k">Текущий ресурс</span><span class="kv-v">${resLink}</span></div>
      <div class="kv"><span class="kv-k">Подключение</span><span class="kv-v">${conn}</span></div>
    </div>
    <div class="card-body srv-actions">
      <a class="btn" href="#/beget?tab=resources">Создать сервер…</a>
      <button type="button" class="btn" data-res-action="install"${ready ? "" : " disabled"}>Установить профиль</button>
      <button type="button" class="btn" data-res-action="change-ip"${ready ? "" : " disabled"}>Сменить IP</button>
      <button type="button" class="btn" data-res-action="replace"${canReplace ? "" : " disabled"}>Заменить</button>
      <button type="button" class="btn danger" data-res-action="delete"${res ? "" : " disabled"}>Удалить ресурс</button>
    </div>
    ${banNoteHTML(d)}
  </section>`;
}

function sourceLabel(s: string): string {
  switch (s) {
    case "tcp": return "TCP-проба";
    case "globalping": return "Globalping";
    case "health_report": return "Отчёт metrics";
    default: return s;
  }
}

// §3 «Доступность, 7 дней»: плитки + график A (столбики по дням, цвет — здоровье дня).
function chartAHTML(d: NodeDetail): string {
  const a = d.availability;
  const head = `<div class="card-head"><h2>Доступность, 7 дней</h2>` +
    (a ? `<span class="hint">окно ${a.window_days} ${plural(a.window_days, "день", "дня", "дней")}</span>` : "") + `</div>`;
  if (!a) return `<section class="card">${head}<div class="table-empty">нет данных</div></section>`;

  const days = a.daily;
  const tiles = `<div class="metrics" style="margin-bottom:14px">
      <div class="card metric">
        <div class="metric-label">Общая доступность</div>
        <div class="metric-value ${availColor(a.overall_pct)}" style="font-size:24px">${a.overall_pct.toFixed(1)}%</div>
        <div class="metric-foot">отчётов ok: ${a.reports_ok}/${a.reports_total}</div>
      </div>
      <div class="card metric">
        <div class="metric-label">GP верификации</div>
        <div class="metric-value ${availColor(a.gp_verified_pct)}" style="font-size:24px">${a.gp_verified_pct.toFixed(1)}%</div>
        <div class="metric-foot">проверок: ${a.gp_checks_total}</div>
      </div>
    </div>`;

  let chart: string;
  if (days.length < 2) {
    chart = `<span class="dim">накапливается — пока мало точек</span>`;
  } else {
    const rows = days.map((day) => {
      const pct = Math.max(0, Math.min(100, day.ok_pct));
      return { bars: [{ value: pct, cls: availColor(pct) }] };
    });
    chart = `<div class="chart-wrap" role="img" aria-label="Доступность за ${days.length} дней, средняя ${a.overall_pct.toFixed(1)} процента">
        <div class="chart-plot">
          <div class="chart-y" aria-hidden="true"><span>100%</span><span>0%</span></div>
          ${barChart(rows, { max: 100 })}
        </div>
        <div class="chart-x" aria-hidden="true">${days.map((day) => {
          const [, m, dd] = day.date.split("-");
          return `<span>${esc(dd)}.${esc(m)}</span>`;
        }).join("")}</div>
      </div>
      ${srTable("Доступность по дням", ["День", "OK %", "TCP ok/total", "Отчёты ok/total"],
        days.map((day) => [day.date, fmtPct(day.ok_pct), `${day.tcp_ok}/${day.tcp_total}`, `${day.reports_ok}/${day.reports_total}`]))}
      ${days.length < a.window_days ? `<p class="card-note">данные за ${days.length} из ${a.window_days} ${plural(a.window_days, "дня", "дней", "дней")}</p>` : ""}`;
  }

  return `<section class="card">${head}
    <div class="card-body">${tiles}
      <div class="chart-cap">По дням <span class="hint">${days.length} ${plural(days.length, "день", "дня", "дней")}</span></div>
      ${chart}
    </div>
  </section>`;
}

function availColor(p: number): string { return p >= 99 ? "ok" : p >= 95 ? "warn" : "bad"; }

// §6 «Уникальные IP, 24 ч»: плитки + график C (линия clients). clients —
// это telemt_user_unique_ips_current (уникальные IP, не соединения), поэтому
// заголовок честно назван «Уникальные IP». me-writers удалён по требованию
// владельца — поле writers нигде не рисуется (specs §5).
// Нагрузка ноды — общие блоки с публичной статистикой (ui/lib/node-load.ts).
const chartCHTML = (d: NodeDetail): string => connectionsCardHTML(d);
const hostingHTML = (d: NodeDetail): string => hostingCardHTML(d);
const chartEHTML = (d: NodeDetail): string => userIPsBarsHTML(d);
const usersTable = (d: NodeDetail): string => usersTableHTML(d);
installLiveCharts(() => !!DETAIL_ID);

function telemtHTML(d: NodeDetail): string {
  const t = d.telemt;
  const snap = t?.metrics_snapshot;
  const keys = snap ? Object.keys(snap).sort() : [];
  const ca = t?.control_api ?? null;
  const dcs = ca?.dcs ?? [];
  const endpoints = dcs.reduce((s, x) => s + x.available_endpoints, 0);
  const tile = (label: string, v: string): string =>
    `<div class="kv"><span class="kv-k">${esc(label)}</span><span class="kv-v">${v}</span></div>`;

  const summary = `<div class="nd-grid" style="padding:0">
      ${tile("Метрик в снимке", snap ? String(keys.length) : "нет данных")}
      ${tile("Собрано", t?.collected_at ? esc(fmtAgo(t.collected_at)) : "—")}
      ${tile("ДЦ", ca ? String(dcs.length) : "нет данных")}
      ${tile("Endpoints (доступно)", ca ? fmtNum(endpoints) : "нет данных")}
    </div>`;

  const snapshot = keys.length ? `<details class="op-step-log" style="margin:12px var(--pad) var(--pad)">
      <summary>Показать снимок (${keys.length})</summary>
      <div class="table-wrap"><table class="table-cards">
        <thead><tr><th>Метрика</th><th class="num">Значение</th></tr></thead>
        <tbody>${keys.map((k) => `<tr>
          <td class="mono wrap" data-label="Метрика">${esc(k)}</td>
          <td class="num mono" data-label="Значение">${fmtNum(snap![k])}</td>
        </tr>`).join("")}</tbody></table></div>
    </details>` : "";

  return `<section class="card">
    <div class="card-head"><h2>Статистика telemt</h2><span class="hint">снимок метрик · пользователи</span></div>
    <div class="card-body" style="padding-bottom:0">
      ${summary}
      <div class="chart-cap">Уникальные IP по пользователям <span class="hint">сейчас</span></div>
      ${chartEHTML(d)}
      <div class="chart-cap">Пользователи <span class="hint">IP · трафик 24 ч</span></div>
      ${usersTable(d)}
    </div>
    ${snapshot}
  </section>`;
}

function controlApiHTML(d: NodeDetail): string {
  const ca = d.telemt?.control_api;
  const gone = d.absent.includes("telemt_control_api") || d.absent.includes("dc_endpoints") || d.absent.includes("quality_health");
  if (!ca || !ca.ok || gone) {
    return `<section class="card"><div class="card-head"><h2>telemt Control API</h2></div>
      <div class="table-empty">нет данных</div></section>`;
  }
  const at = new Date(ca.generated_at_epoch_secs * 1000).toISOString();
  return `<section class="card">
    <div class="card-head"><h2>telemt Control API</h2><span class="hint">снимок DC</span></div>
    <div class="nd-grid card-body" style="padding-bottom:0">
      <div class="kv"><span class="kv-k">Middle-proxy</span><span class="kv-v">${chip(ca.middle_proxy_enabled, "включён", "выключен")}</span></div>
      <div class="kv"><span class="kv-k">Снимок</span><span class="kv-v">${esc(fmtAgo(at))}</span></div>
    </div>
    ${ca.dcs.length
      ? `<div class="table-wrap"><table class="table-cards">
          <thead><tr><th>ДЦ</th><th class="num">Endpoints</th><th class="num">Alive</th><th class="num">Fresh</th><th class="num">RTT</th></tr></thead>
          <tbody>${ca.dcs.map((x) => `<tr>
            <td data-label="ДЦ">${fmtNum(x.dc)}</td>
            <td class="num" data-label="Endpoints">${fmtNum(x.available_endpoints)}</td>
            <td class="num" data-label="Alive">${fmtNum(x.alive_writers)}</td>
            <td class="num" data-label="Fresh">${fmtNum(x.fresh_alive_writers)}</td>
            <td class="num" data-label="RTT">${x.rtt_ms.toFixed(1)} ms</td>
          </tr>`).join("")}</tbody></table></div>`
      : `<div class="table-empty">ДЦ нет</div>`}
  </section>`;
}

function eventRow(ev: { at: string; type: string; domain?: string; detail?: string }): string {
  const m = EV_LABEL[ev.type] || { label: ev.type, dot: "" };
  const parts = [ev.domain].filter(Boolean).map((v) => `<code>${esc(v)}</code>`);
  const detail = ev.detail ? esc(ev.detail) : "";
  const line = [parts.join(" "), detail].filter(Boolean).join(parts.length && detail ? " — " : "");
  return `<li class="feed-item">
    <span class="feed-dot ${m.dot}"></span>
    <div class="feed-body">
      <div class="feed-head"><span class="feed-kind">${esc(m.label)}</span>
        <span class="feed-time" title="${esc(ev.at)}">${esc(fmtClock(ev.at))}</span></div>
      ${line ? `<div class="feed-detail">${line}</div>` : ""}
    </div></li>`;
}
