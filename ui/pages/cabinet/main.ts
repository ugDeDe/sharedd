/* Кабинет пользователя (docs/specs/cabinet.md §2).

   Основной сценарий — Пул · Хостинги · Журнал. Остальные разделы
   (Серверы, Профили) открываются из меню «Дополнительно».
   Данные и мутации — через ./api (cookie-сессия + CSRF). Инвариант
   «отказ ≠ пересоздание» показан на нодах, в операциях и в визарде. */

import { $, esc, renderSection } from "../../lib/dom";
import { initBuildWatch } from "../../lib/update";
import { fmtClock, fmtAgo, fmtNum, plural } from "../../lib/format";
import { initTheme } from "../../lib/theme";
import { renderVarsEditor, collectVarsEditor, collectInstallVars, parseVarsSchema, serializeVarsSchema, validateVarsEditor, isSecretVar, PROFILE_VARS_HELP } from "../../lib/vars";
import type { ProfileVar, InstallDefaults } from "../../lib/vars";
import { api, newIdemKey } from "./api";
import type { Authkey, BegetLink, CloudReplaceBody, CloudResource, Configuration, EstimateResult, Fleet, FleetCreateBody, FleetItem, FleetState, HostingBalance, HostingCatalog, HostingConnection, HostingConnectionInput, ManualServer, ManualServerPayload, Me, MyProfiles, NodehostConfiguration, NodeInfo, Operation, OperationStep, OwnProfile, Profile, ProfilePayload, Provider, Region } from "./api";
import { openNodeDetail, closeNodeDetail, pollNodeDetail } from "./node-detail";
import { canLeaveBlueprint, isBlueprintRoute, leaveBlueprintEditor, openBlueprintEditor } from "./blueprint-editor";
import { closeBlueprintRun, leaveRunReview, openBlueprintRun } from "./blueprint-runs";
import { positiveID, textProfiles } from "./blueprint-model";

/* ── состояние ───────────────────────────────────────────────────────── */
let ME: Me | null = null;
let NODES: NodeInfo[] = [];
let SERVERS: ManualServer[] = [];
let OPS: Operation[] = [];
let OPEN_OPS = new Set<number>(); // операции с раскрытым журналом шагов
let OPEN_LOGS = new Set<string>(); // `${opId}:${idx}` — раскрытый лог шага
let HIGHLIGHT_OP = 0;

const STATUS_META: Record<string, { label: string; cls: string }> = {
  healthy:     { label: "здорова", cls: "ok" },
  unhealthy:   { label: "нездорова", cls: "bad" },
  quarantined: { label: "карантин Globalping", cls: "warn" },
  deleting:    { label: "удаляется", cls: "warn" },
  offline:     { label: "нет данных", cls: "" },
};

const OP_META: Record<string, { label: string; cls: string }> = {
  pending:    { label: "ожидает", cls: "info" },
  sent:       { label: "отправлено провайдеру", cls: "info" },
  confirmed:  { label: "подтверждено", cls: "ok" },
  installing: { label: "установка профиля", cls: "info" },
  waiting_vps:{ label: "ждём готовности VPS", cls: "warn" },
  done:       { label: "готово", cls: "ok" },
  unknown:    { label: "неизвестно · ручной разбор", cls: "special" },
  failed:     { label: "ошибка", cls: "bad" },
  deleting:   { label: "удаляется", cls: "warn" },
  deleted:    { label: "удалено", cls: "" },
};

const EMPTY_SVG =
  `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="3" width="20" height="7" rx="2"/><rect x="2" y="14" width="20" height="7" rx="2"/></svg>`;

/* Временный сбой загрузки раздела: 5xx, control_unavailable (рестарт
   регистратора) или сетевой сбой (status не проставлен). Такие ошибки
   self-heal-ятся повтором; 4xx (права/валидация) повторять бессмысленно. */
function isRetryableLoad(e: Error & { status?: number; code?: string }): boolean {
  if (e.code === "control_unavailable") return true;
  const s = e.status ?? 0;
  return s >= 500 || s === 0;
}

/* ── тост ────────────────────────────────────────────────────────────── */
let toastTimer: number | undefined;
function showToast(text: string, isErr = false): void {
  const t = $("toast");
  t.textContent = text;
  t.classList.toggle("err", isErr);
  t.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => t.classList.remove("show"), 5000);
}

/* ── модалка: подтверждения и одноразовый ключ ───────────────────────── */
let lastFocused: HTMLElement | null = null;
/* Пока модалка открыта, фон уводим в inert — aria-modal обещает, что за
   пределы .modal-box фокус не уходит. Тосты не глушим: они должны звучать. */
function setPageInert(on: boolean): void {
  Array.from(document.body.children).forEach((el) => {
    if (el.id === "modal" || el.id === "toast") return;
    if (on) el.setAttribute("inert", ""); else el.removeAttribute("inert");
  });
}
function openModal(title: string, body: string, actions: { label: string; cls?: string; onClick: (ev: MouseEvent) => void }[], wide = false): void {
  lastFocused = document.activeElement as HTMLElement;
  $("modal-title").textContent = title;
  $("modal-body").innerHTML = body;
  $("modal").querySelector<HTMLElement>(".modal-box")?.classList.toggle("modal-wide", wide);
  const foot = $("modal-foot");
  foot.replaceChildren();
  actions.forEach((a) => {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "btn" + (a.cls ? " " + a.cls : "");
    b.textContent = a.label;
    b.addEventListener("click", a.onClick);
    foot.appendChild(b);
  });
  const m = $("modal");
  m.classList.add("show");
  setPageInert(true);
  (foot.querySelector("button") as HTMLButtonElement | null)?.focus();
}
function closeModal(): void {
  $("modal").classList.remove("show");
  setPageInert(false);
  $("modal-body").replaceChildren();
  $("modal-foot").replaceChildren();
  if (lastFocused) { try { lastFocused.focus(); } catch { /* detached */ } }
  lastFocused = null;
}
$("modal").addEventListener("click", (e) => { if ((e.target as HTMLElement).id === "modal") closeModal(); });
document.addEventListener("keydown", (e) => {
  const m = $("modal");
  if (!m.classList.contains("show")) return;
  if (e.key === "Escape") { closeModal(); return; }
  if (e.key !== "Tab") return;
  const box = m.querySelector<HTMLElement>(".modal-box");
  if (!box) return;
  const f = box.querySelectorAll<HTMLElement>(
    'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])');
  if (!f.length) return;
  const first = f[0], last = f[f.length - 1];
  const active = document.activeElement as HTMLElement;
  if (e.shiftKey && (active === first || !box.contains(active))) { e.preventDefault(); last.focus(); }
  else if (!e.shiftKey && (active === last || !box.contains(active))) { e.preventDefault(); first.focus(); }
});

/* ── маршрутизация ───────────────────────────────────────────────────── */
const VIEWS: Record<string, string> = {
  nodes: "Пул", create: "Создать сервер", servers: "Серверы", profiles: "Мои профили", beget: "Хостинги", operations: "Журнал",
};
function currentView(): string {
  const p = (location.hash || "").replace(/^#\/?/, "").split("?")[0].split("/").filter(Boolean);
  return VIEWS[p[0]] ? p[0] : "nodes";
}
/* id ноды из маршрута `#/nodes/<id>` — открывает детальную карточку в
   отдельном виде. Пусто — обычные «Мои ноды». */
function routeNodeId(): string {
  const p = (location.hash || "").replace(/^#\/?/, "").split("/").filter(Boolean);
  // Таб ноды живёт хэш-параметром `?tab=` в том же маршруте — id берём до него.
  const raw = p[0] === "nodes" && p[1] ? p[1].split("?")[0] : "";
  return raw ? decodeURIComponent(raw) : "";
}
/* id ресурса из маршрута `#/resources/<id>` — deep-link в сегмент «Ресурсы». */
function routeResourceId(): number {
  const p = (location.hash || "").replace(/^#\/?/, "").split("/").filter(Boolean);
  const raw = p[0] === "resources" && p[1] ? p[1].split("?")[0] : "";
  return raw ? Number(raw) || 0 : 0;
}
let lastCabinetHash = location.hash;
let routeGeneration = 0;
async function route(): Promise<void> {
  const hash = location.hash, generation = ++routeGeneration;
  const allowed = await canLeaveBlueprint(hash);
  if (generation !== routeGeneration) return;
  if (!allowed || !leaveRunReview()) { history.replaceState(null, "", lastCabinetHash || "#/nodes"); return; }
  if (location.hash !== hash) history.replaceState(null, "", hash);
  lastCabinetHash = hash;
  const blueprint = isBlueprintRoute(hash);
  const runID = /^#\/profiles\/runs\/(\d+)$/.exec(hash)?.[1];
  if (!blueprint || runID) leaveBlueprintEditor();
  closeBlueprintRun();
  $("app").classList.toggle("blueprint-wide", blueprint);
  if (blueprint) {
    document.querySelectorAll<HTMLElement>(".view").forEach((el) => { el.hidden = el.id !== (runID ? "view-blueprint-run" : "view-blueprint-editor"); });
    document.querySelectorAll<HTMLElement>("[data-view]").forEach((el) => el.classList.toggle("active", el.dataset.view === "profiles"));
    $("view-title").textContent = runID ? "Запуск графа" : "Мои профили · редактор графа";
    wizard.leave(); clearServerSecret(); closeNodeDetail();
    if (runID) {
      const id = positiveID(runID);
      if (id) openBlueprintRun($("view-blueprint-run"), id);
      else $("view-blueprint-run").textContent = "Номер запуска недопустим. Откройте запуск по известному положительному ID.";
    } else openBlueprintEditor($("view-blueprint-editor"));
    return;
  }
  const nodeId = routeNodeId();
  const resId = nodeId ? 0 : routeResourceId();
  const v = resId ? "beget" : currentView();
  const viewId = nodeId ? "view-node" : "view-" + v;
  const nav = nodeId ? "nodes" : v;
  document.querySelectorAll<HTMLElement>(".view").forEach((el) => { el.hidden = el.id !== viewId; });
  document.querySelectorAll<HTMLElement>("[data-view]").forEach((a) =>
    a.classList.toggle("active", a.dataset.view === nav));
  // Кнопка «Дополнительно» подсвечивается, когда активен вторичный раздел.
  document.querySelectorAll<HTMLElement>(".more").forEach((m) =>
    m.querySelector(".more-btn")?.classList.toggle("active", !!m.querySelector("[data-view].active")));
  $("view-title").textContent = nodeId ? "Нода " + nodeId : VIEWS[v];
  // L5: уходя со страницы «Свои серверы», не держим секрет в скрытом DOM.
  if (v !== "servers") clearServerSecret();
  if (v === "create" && !nodeId) wizard.enter();
  else wizard.leave();
  if (v === "beget" && !nodeId) enterHosting(resId);
  if (v === "nodes" && !nodeId) enterAutopilot();
  else apStopPoll();
  if (v === "servers" && !nodeId) { loadServers(); loadResources(); }
  if (v === "profiles" && !nodeId) loadProfiles();
  if (nodeId) openNodeDetail(nodeId);
  else closeNodeDetail();
  renderAll();
}

/* ── ноды ────────────────────────────────────────────────────────────── */
function availColor(p: number): string { return p >= 99 ? "ok" : p >= 95 ? "warn" : "bad"; }

function nodeCardHTML(n: NodeInfo): string {
  const meta = STATUS_META[n.status] || STATUS_META.offline;
  const quar = n.quarantine != null;
  const master = (n.master_domains || []).length > 0;
  const dc2 = n.status === "offline" ? "unknown" : n.dc2_ok ? "ok" : "bad";
  const dc2Title = n.dc2_ok ? "DC2 доступен" : "DC2 недоступен — отказ здоровья не пересоздаёт VPS";
  // Клик по карточке открывает детальную страницу `#/nodes/<id>`; id — ещё и
  // ссылкой, чтобы карточка была достижима с клавиатуры.
  return `<div class="node-card${master ? " master" : ""}" data-id="${esc(n.node_id)}" role="link" tabindex="0"
      aria-label="Открыть ноду ${esc(n.node_id)}">
    <div class="node-main">
      <div class="node-top">
        <span class="node-id mono">${esc(n.node_id)}</span>
        <span class="badge ${meta.cls}"><i></i>${esc(meta.label)}</span>
        ${master ? `<span class="badge gold" title="${esc((n.master_domains || []).join(", "))}">★ мастер</span>` : ""}
        ${quar && n.status !== "quarantined" ? `<span class="badge warn"><i></i>карантин</span>` : ""}
      </div>
      <div class="node-sub">
        <span class="mono">${esc(n.ip || "—")}</span>
        <span class="checks">
          <span class="check-tag ${dc2}" title="${esc(dc2Title)}">DC2</span>
        </span>
      </div>
    </div>
    <div class="node-side">
      <div class="meter node-meter">
        <div class="meter-track"><div class="meter-fill ${availColor(n.availability)}" style="width:${Math.max(0, Math.min(100, n.availability))}%"></div></div>
        <span class="meter-value">${n.availability.toFixed(1)}%</span>
      </div>
    </div>
  </div>`;
}

function renderNodes(poll = false): void {
  const body = $("nodes-body");
  renderSection(body, poll, () => {
    $("nodes-hint").textContent = NODES.length ? `${NODES.length} ${plural(NODES.length, "нода", "ноды", "нод")}` : "";
    if (!NODES.length) {
      body.innerHTML = `<div class="empty-state">${EMPTY_SVG}
      <span class="t">Пока нет нод</span>
      <span class="s">Создайте сервер в разделе «Серверы» — нода появится после регистрации в пуле.</span></div>`;
      return;
    }
    body.innerHTML = NODES.map(nodeCardHTML).join("");
  });
}

function renderMetrics(poll = false): void {
  if (!ME) return;
  const used = ME.used_nodes, max = ME.max_nodes;
  const pool = NODES.filter((n) => n.status !== "offline").length;
  const problems = NODES.filter((n) => !n.dc2_ok || n.quarantine).length;
  const pct = max > 0 ? Math.min(100, Math.round((used / max) * 100)) : 0;
  const quotaCls = max > 0 && used >= max ? "warn" : "ok";
  renderSection($("node-metrics"), poll, () => {
  $("node-metrics").innerHTML = `
    <div class="card metric">
      <div class="metric-label">Квота нод</div>
      <div class="metric-value">${fmtNum(used)}<span class="sep">/</span>${fmtNum(max)}</div>
      <div class="meter"><div class="meter-track"><div class="meter-fill ${quotaCls}" style="width:${pct}%"></div></div>
        <span class="meter-value">${pct}%</span></div>
      <div class="metric-foot">занято мест</div>
    </div>
    <div class="card metric">
      <div class="metric-label">Нод в пуле</div>
      <div class="metric-value">${fmtNum(pool)}</div>
      <div class="metric-foot">зарегистрировано</div>
    </div>
    <div class="card metric">
      <div class="metric-label">Проблемы</div>
      <div class="metric-value ${problems ? "bad" : ""}">${fmtNum(problems)}</div>
      <div class="metric-foot">DC2 или карантин</div>
    </div>`;

    const qb = $("quota-banner");
    if (max > 0 && used >= max) {
      qb.innerHTML = `<div class="banner warn"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>
        <span>Квота исчерпана: новая регистрация заблокирована. Освободите место или запросите лимит у администратора.</span></div>`;
    } else {
      qb.replaceChildren();
    }
  });
}

/* ── ключ доступа (addendum-manual-servers.md §1) ────────────────────── */
/* Копирование с ЧЕСТНЫМ результатом: «Скопировано» ставим только после
   реальной записи в clipboard; при отказе — «Не удалось» и подсказка
   выделить вручную (без ложного успеха, как в users.ts — copyInviteLink). */
function copyText(btn: HTMLButtonElement, text: string): void {
  const orig = btn.textContent || "";
  const ok = () => { btn.textContent = "Скопировано"; setTimeout(() => { btn.textContent = orig; }, 1500); };
  const fail = () => {
    btn.textContent = "Не удалось";
    showToast("Не удалось скопировать — выделите ключ и скопируйте вручную.", true);
    setTimeout(() => { btn.textContent = orig; }, 1500);
  };
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).then(ok, fail);
    return;
  }
  // http без TLS: пробуем старый путь, но успех объявляем по его итогу.
  const ta = document.createElement("textarea");
  ta.value = text; ta.style.position = "fixed"; ta.style.opacity = "0";
  document.body.appendChild(ta); ta.select();
  let copied = false;
  try { copied = document.execCommand("copy"); } catch { copied = false; }
  document.body.removeChild(ta);
  copied ? ok() : fail();
}

function renderAuthkey(a: Authkey, poll = false): void {
  const body = $("authkey-body");
  renderSection(body, poll, () => {
  const badge = a.active ? `<span class="badge ok"><i></i>активен</span>` : `<span class="badge">отозван</span>`;
  const prefix = a.prefix ? `${esc(a.prefix)}…` : "—";
  const created = a.created_at ? fmtAgo(new Date(a.created_at * 1000).toISOString()) : "—";
  body.innerHTML = `<div class="kv" style="gap:8px">
      <span class="kv-k">Ключ доступа</span>
      <span class="kv-v mono">${prefix}</span>
      <span>${badge} <span class="dim">создан ${esc(created)}</span></span>
    </div>
    <div class="banner info" style="margin:12px 0 0"><span>Один ключ покрывает <b>все</b> ваши VPS. Раздельных ключей на сервер нет.</span></div>
    <div class="srv-actions" style="margin-top:12px">
      <button type="button" class="btn primary" id="ak-rotate">${a.active ? "Перевыпустить" : "Выпустить"}</button>
      <button type="button" class="btn danger" id="ak-revoke"${a.active ? "" : " disabled"}>Отозвать</button>
    </div>`;
  $("authkey-note").textContent =
    "Перевыпуск немедленно отзывает старый ключ: ноды, которые ещё не зарегистрированы с ним, потребуют новый. Отзыв запрещает новые регистрации ключом — уже работающие ноды продолжают работать.";
  $("ak-rotate").addEventListener("click", rotateAuthkey);
  $("ak-revoke").addEventListener("click", confirmRevokeAuthkey);
  });
}

/* Раздел «Ключ доступа» грузится независимо от остального обзора: сбой
   ключа не должен ронять ноды/операции, а его контролы — исчезать. При
   временном сбое показываем инлайн-ошибку с «Повторить» и делаем один
   self-heal через ~2с (окно рестарта регистратора). */
let AK_RETRY_DONE = false;
function loadAuthkey(poll = false): void {
  api.authkey()
    .then((a) => { AK_RETRY_DONE = false; renderAuthkey(a, poll); })
    .catch((e: Error & { status?: number; code?: string }) => {
      if (e.message === "unauthorized") return;
      const body = $("authkey-body");
      if ($("ak-rotate")) {
        // Контролы уже отрисованы: временный сбой их не затирает.
        if (!AK_RETRY_DONE) showToast("Не удалось обновить ключ доступа: " + e.message, true);
      } else {
        body.innerHTML = `<div class="banner bad" role="alert"><span>Не удалось загрузить ключ доступа: ${esc(e.message)}</span>
          <button class="btn sm" id="ak-retry" style="margin-left:auto">Повторить</button></div>`;
        $("ak-retry")?.addEventListener("click", () => { AK_RETRY_DONE = false; loadAuthkey(); });
      }
      if (!AK_RETRY_DONE && isRetryableLoad(e)) { AK_RETRY_DONE = true; setTimeout(loadAuthkey, 2000); }
    });
}

function rotateAuthkey(): void {
  const btn = $("ak-rotate") as HTMLButtonElement;
  const label = btn.textContent;
  btn.disabled = true; btn.textContent = "Выпуск…";
  api.rotateAuthkey()
    .then((r) => {
      openModal("Новый ключ доступа",
        `<div class="secret-reveal">
          <div class="warn-note">Ключ показывается один раз. Сохраните его сейчас — позже он недоступен.</div>
          <div class="key" id="ak-key" tabindex="0">${esc(r.authkey)}</div>
          <div class="dim" style="font-size:11.5px">Перевыпуск немедленно отозвал прежний ключ. Ноды, которые ещё не зарегистрированы с ключом, потребуют новый; уже работающие ноды продолжат работу.</div>
          <div class="dim" style="font-size:11.5px">Один ключ подходит для <b>всех</b> ваших VPS.</div>
        </div>`,
        [
          { label: "Копировать", onClick: (ev) => copyText(ev.currentTarget as HTMLButtonElement, r.authkey) },
          { label: "Закрыть", cls: "primary", onClick: () => { closeModal(); refresh().catch(() => {}); } },
        ], true);
      btn.disabled = false; btn.textContent = label;
    })
    .catch((e: Error) => { showToast(e.message, true); btn.disabled = false; btn.textContent = label; });
}

function confirmRevokeAuthkey(): void {
  openModal("Отозвать ключ доступа?",
    "Новые регистрации нод с этим ключом будут запрещены. Уже работающие ноды продолжат работу — отзыв их не затрагивает.",
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Отозвать", cls: "danger", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        btn.disabled = true; btn.textContent = "Отзыв…";
        api.revokeAuthkey()
          .then(() => { closeModal(); showToast("Ключ отозван."); refresh().catch(() => {}); })
          .catch((e: Error) => { showToast(e.message, true); btn.disabled = false; btn.textContent = "Отозвать"; });
      } },
    ]);
}

/* ── свои серверы (addendum-manual-servers.md §2/§3) ─────────────────── */
/* Уже существующий хост. Провайдер не участвует, платных вызовов нет.
   Контролы НИКОГДА не называют установку покупкой/созданием сервера. */
type ServerStateKey = "new" | "checked" | "installing" | "done" | "failed" | "deleted";
/* install — первый запуск; retry — после сбоя (исходный профиль закреплён);
   reinstall — повтор на уже установленном сервере со свободным выбором профиля. */
type InstallMode = "install" | "retry" | "reinstall";

function serverState(s: ManualServer): { key: ServerStateKey; label: string; cls: string } {
  const st = s.operation?.state;
  if (st === "installing" || st === "pending" || st === "sent" || st === "confirmed")
    return { key: "installing", label: "устанавливается", cls: "info" };
  if (st === "done") return { key: "done", label: "установлен", cls: "ok" };
  // L4: удалённая операция — нейтральная метка, а не «ошибка установки».
  if (st === "deleted") return { key: "deleted", label: "удалено", cls: "" };
  if (st === "failed" || st === "unknown")
    return { key: "failed", label: "ошибка установки", cls: "bad" };
  if (s.last_error) return { key: "failed", label: "ошибка", cls: "bad" };
  if (s.verified_at) return { key: "checked", label: "проверен", cls: "ok" };
  return { key: "new", label: "новый", cls: "" };
}

function serverCardHTML(s: ManualServer): string {
  const meta = serverState(s);
  const op = s.operation;
  const installing = meta.key === "installing";
  const authLabel = s.auth_type === "key" ? "приватный ключ" : "пароль";
  let installBtn = "";
  if (!installing && meta.key !== "deleted") {
    const mode: InstallMode = meta.key === "failed" ? "retry" : meta.key === "done" ? "reinstall" : "install";
    const label = mode === "retry" ? "Повторить установку" : mode === "reinstall" ? "Переустановить" : "Установить";
    installBtn = `<button class="btn sm${mode === "install" ? " primary" : ""}" data-sinstall="${s.id}" data-smode="${mode}">${label}</button>`;
  }
  const opHtml = op ? `<div class="kv" style="gap:6px">
      <span class="kv-k">Операция #${op.id} · ${esc(op.kind)}</span>
      <span class="kv-v"><span class="badge ${(OP_META[op.state] || { cls: "" }).cls}"><i></i>${esc((OP_META[op.state] || { label: op.state }).label)}</span></span>
    </div>${op.last_error ? `<pre class="op-err srv-out">${esc(op.last_error)}</pre>` : ""}` : "";
  return `<div class="node-card srv" data-sid="${s.id}">
    <div class="node-main">
      <div class="node-top">
        <span class="node-id">${esc(s.label)}</span>
        <span class="badge ${meta.cls}"><i></i>${esc(meta.label)}</span>
        <span class="badge">${esc(authLabel)}</span>
      </div>
      <div class="node-sub">
        <span class="mono">${esc(s.host)}:${s.port}</span>
        <span>пользователь <span class="mono">${esc(s.ssh_user)}</span></span>
        ${s.node_id ? `<span>нода <span class="mono">${esc(s.node_id)}</span></span>` : ""}
        <span class="dim">подключён ${esc(fmtAgo(new Date(s.created_at * 1000).toISOString()))}</span>
      </div>
      ${s.last_error && !op ? `<div class="node-problem" title="${esc(s.last_error)}">${esc(s.last_error)}</div>` : ""}
    </div>
    <div class="node-side">
      <button class="btn sm" data-scheck="${s.id}"${installing ? " disabled" : ""}>Проверить доступ</button>
      ${installBtn}
      <button class="btn sm danger" data-sdel="${s.id}"${installing ? " disabled" : ""}>Удалить</button>
    </div>
    ${opHtml ? `<div class="srv-more">${opHtml}</div>` : ""}
  </div>`;
}

/* L2: после замены списка прокрутку и фокус восстанавливает renderSection
   (по data-атрибуту кнопки); при переходе к пустому списку фокус уходит на
   стабильную «Обновить». */
function renderServers(poll = false): void {
  const body = $("servers-body");
  const hadFocus = body.contains(document.activeElement);
  const rendered = renderSection(body, poll, () => {
    if (!SERVERS.length) {
      body.innerHTML = `<div class="empty-state">${EMPTY_SVG}
      <span class="t">Серверы не подключены</span>
      <span class="s">Нажмите «Добавить сервер»: укажите IP и пароль или SSH-ключ. Сервер не создаётся и не оплачивается.</span></div>`;
      return;
    }
    body.innerHTML = `<div class="node-list">${SERVERS.map(serverCardHTML).join("")}</div>`;
  });
  if (rendered && hadFocus && !SERVERS.length) document.getElementById("servers-reload")?.focus();
}

let SRV_RETRY_DONE = false;
function loadServers(poll = false): void {
  api.servers()
    .then((r) => { SERVERS = r.servers || []; SRV_RETRY_DONE = false; renderServers(poll); })
    .catch((e: Error & { status?: number; code?: string }) => {
      SERVERS = [];
      // Список не загрузился — форма подключения (и «Обновить») остаются
      // доступны, ошибка инлайн, «Повторить» и один self-heal.
      $("servers-body").innerHTML = `<div class="empty-state"><span class="t">Не удалось загрузить серверы</span>
        <span class="s">${esc(e.message)}</span><button class="btn sm" id="srv-retry">Повторить</button></div>`;
      $("srv-retry")?.addEventListener("click", () => { SRV_RETRY_DONE = false; loadServers(); });
      if (!SRV_RETRY_DONE && isRetryableLoad(e)) { SRV_RETRY_DONE = true; setTimeout(loadServers, 2000); }
    });
}

/* Причина отказа установки — по кодам §2.2; прочее — дословно. */
function installErrText(e: Error & { code?: string }): string {
  switch (e.code) {
    case "quota_exceeded": return "Квота исчерпана: освободите место или запросите лимит у администратора.";
    case "ssh_unavailable": return "SSH-доступ недоступен: проверьте адрес, порт и секрет.";
    case "already_done": return "Установка на этот сервер уже завершена.";
    case "in_progress": return "Установка уже выполняется — дождитесь завершения.";
    case "not_found": return "Сервер не найден.";
    case "profile_unavailable": return "Профиль недоступен для установки: он не опубликован.";
    default: return e.message;
  }
}

/* Переменные установки выбранного профиля. Тот же редактор, что в визарде и
   редакторе профиля (lib/vars): объявленные переменные с дефолтами + свои
   сверху, подсказки имён, секреты — password с «глазом». Здесь меняются только
   ЗНАЧЕНИЯ для этой установки; объявленные имена/дефолты принадлежат профилю. */
let installDefaults: Promise<InstallDefaults> | undefined;
function loadInstallDefaults(): Promise<InstallDefaults> {
  // Whitelisted session metadata only; never privileged panel configuration.
  return installDefaults ??= api.installDefaults();
}

function renderInstallVars(decls: ProfileVar[], snapshot?: ProfileVar[], onChange?: (rows: ProfileVar[]) => void): void {
  mountVars("sv-install-vars", decls, snapshot, onChange);
}

/* Объявленные переменные профиля по id (published + свои). Нет профиля → []. */
function installProfileDecls(m: MyProfiles, id: number): ProfileVar[] {
  const p = [...(m.published || []), ...(m.profiles || [])].find((x) => id ? x.id === id : x.name === "sharedd-node");
  return p ? parseVarsSchema(p.vars_schema) : [];
}

/* Повтор — на тот же хост; при сбое исходный профиль закреплён, при переустановке
   профиль выбирается заново (как при первой установке). */
function openInstall(s: ManualServer, mode: InstallMode): void {
  const isRetry = mode === "retry";
  const actionLabel = isRetry ? "Повторить установку" : mode === "reinstall" ? "Переустановить" : "Установить";
  const title = isRetry ? `Повторить установку · ${s.label}` : mode === "reinstall" ? `Переустановка · ${s.label}` : `Установка · ${s.label}`;
  const banner = mode === "reinstall"
    ? `<div class="banner bad" style="margin:0 0 12px"><span>Повторная установка перезапишет текущую конфигурацию прокси на сервере <span class="mono">${esc(s.host)}:${s.port}</span> и перезапустит прокси. Выберите профиль.</span></div>`
    : `<div class="banner info" style="margin:0 0 12px"><span>Установка выполняется по SSH на ваш существующий сервер <span class="mono">${esc(s.host)}:${s.port}</span>. Новый сервер не создаётся и не оплачивается.</span></div>`;
  openModal(title,
    `${banner}
     <div class="field" id="sv-install-prof-wrap">
       <label for="sv-install-prof">Профиль установки</label>
        <select id="sv-install-prof" disabled><option value="">Загрузка профиля…</option></select>
       <span class="help" id="sv-profile-status" aria-live="polite">Загрузка профилей…</span>
     </div>
     <div class="field" style="gap:6px;margin-top:12px">
       <label>Переменные установки</label>
       <div id="sv-install-vars"><div class="vars-editor"></div></div>
        <span class="help">${esc(PROFILE_VARS_HELP)} Сброс удаляет переопределение; пустой секрет или ad_tag можно задать явно.</span>
     </div>
     <div class="banner bad" id="sv-install-err" role="alert" hidden style="margin:12px 0 0"></div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: actionLabel, cls: "primary", onClick: (ev) => doInstall(ev.currentTarget as HTMLButtonElement, s, mode) },
    ], true);
   let pendingRows: ProfileVar[] | undefined;
   if (isRetry) $("sv-install-vars").textContent = "Загрузка переменных исходного профиля…";
   else renderInstallVars([], undefined, (rows) => { pendingRows = rows; });
   const originalSelect = $("sv-install-prof") as HTMLSelectElement;
   const load = (): void => { textProfiles().then((m) => {
     const sel = $("sv-install-prof") as HTMLSelectElement | null;
     if (sel !== originalSelect || !document.body.contains(sel)) return;
     if (isRetry) {
       const id = s.operation?.profile_id;
       if (id === undefined || !Number.isInteger(id) || id < 0) throw new Error("Исходный профиль операции недоступен");
       const p = [...(m.published || []), ...(m.profiles || [])].find((p) => id ? p.id === id : p.name === "sharedd-node");
       if (!p || (!(m.published || []).some((x) => x.id === p.id) && !m.enabled)) throw new Error("Исходный профиль недоступен для повторной установки");
       sel.innerHTML = `<option value="${id}">${esc(p.name)}</option>`;
       $("sv-profile-status").textContent = "Повтор использует исходный профиль операции; сменить его нельзя.";
       renderInstallVars(parseVarsSchema(p.vars_schema));
       sel.dataset.ready = "true";
       delete sel.dataset.loadError;
       return;
     }
     // Переустановка: у бэкенда profile_id=0 значит «оставить текущий профиль»,
     // поэтому штатный профиль адресуем его реальным id (sharedd-node), а «оставить» —
     // отдельной опцией. Первая установка сохраняет sentinel 0 = «Стандартный профиль».
     const isReinstall = mode === "reinstall";
     const curId = s.operation?.profile_id ?? 0;
      const defaultId = [...m.published, ...m.profiles].find((p) => p.name === "sharedd-node")?.id;
     const opts: string[] = [];
      if (!isReinstall && defaultId !== undefined) {
        opts.push('<option value="0">Стандартный профиль</option>');
      } else if (isReinstall) {
       // ponytail: штатный профиль публикуется системой; если админ его снял с
       // публикации, реального id нет — остаётся только «оставить текущий».
       if (defaultId !== undefined) opts.push(`<option value="${defaultId}">Стандартный профиль</option>`);
        if ([...m.published, ...m.profiles].some((p) => p.id === curId)) opts.push(`<option value="0">Оставить текущий профиль (id=${curId})</option>`);
     }
     for (const p of m.published || [])
       if (!isReinstall || p.id !== defaultId) opts.push(`<option value="${p.id}">${esc(p.name)}</option>`);
     for (const p of m.profiles || [])
       if (!(m.published || []).some((x) => x.id === p.id)) opts.push(`<option value="${p.id}">${esc(p.name)} (мой)</option>`);
     sel.innerHTML = opts.join("");
     sel.disabled = false;
      sel.dataset.ready = String(opts.length > 0);
     delete sel.dataset.loadError;
      $("sv-profile-status").textContent = opts.length ? "Текстовые профили: опубликованные и ваши." : "Нет текстовых профилей — создайте их в «Мои профили».";
     // Переустановка предвыбирает текущий профиль (значение 0 = «оставить»);
     // если текущий и есть штатный — предвыбираем его реальный id.
     const preselect = isReinstall && defaultId !== curId ? 0 : (isReinstall ? defaultId : 0);
      sel.value = String(preselect);
      if (!sel.value && sel.options.length) sel.selectedIndex = 0;
     let selected = sel.value;
     sel.addEventListener("change", () => {
       const ed = document.querySelector<HTMLElement>("#sv-install-vars .vars-editor");
       if (ed && Object.keys(collectInstallVars(ed)).length && !window.confirm("Смена профиля сбросит введённые переменные установки. Продолжить?")) { sel.value = selected; return; }
       selected = sel.value;
       const vid = Number(sel.value) || 0;
       renderInstallVars(installProfileDecls(m, isReinstall && vid === 0 ? curId : vid));
     });
      renderInstallVars(installProfileDecls(m, Number(sel.value) || (isReinstall ? curId : 0)), pendingRows);
   }).catch(() => {
     if ($("sv-install-prof") !== originalSelect) return;
     $("sv-profile-status").innerHTML = `Не удалось загрузить ${isRetry ? "исходный профиль операции" : "профили"}. <button type="button" class="btn sm" id="sv-profile-retry">Повторить</button>`;
     if (isRetry) $("sv-install-vars").textContent = "Переменные исходного профиля недоступны. Повторная установка заблокирована до загрузки профиля.";
    $("sv-profile-retry").addEventListener("click", () => { $("sv-profile-status").textContent = "Загрузка профилей…"; load(); });
    originalSelect.dataset.loadError = "true";
  }); };
  load();
}

function doInstall(btn: HTMLButtonElement, s: ManualServer, mode: InstallMode): void {
  const isRetry = mode === "retry";
  const sel = $("sv-install-prof") as HTMLSelectElement | null;
  const profile_id = isRetry ? s.operation?.profile_id : sel ? (Number(sel.value) || 0) : 0;
  const errBox = $("sv-install-err");
  const ed = document.querySelector<HTMLElement>("#sv-install-vars .vars-editor");
  if (!sel || sel.dataset.ready !== "true") { errBox.textContent = sel?.dataset.loadError ? "Профили недоступны. Нажмите «Повторить» у списка профилей." : "Дождитесь загрузки профилей."; errBox.hidden = false; return; }
  // Клиентская подсказка по именам (server всё равно проверит авторитетно).
  const nameErr = ed ? validateVarsEditor(ed) : null;
  if (nameErr) {
    if (errBox && document.body.contains(errBox)) { errBox.textContent = nameErr; errBox.hidden = false; }
    return;
  }
  const vars = ed ? collectInstallVars(ed) : {};
  const label = btn.textContent;
  btn.disabled = true; btn.textContent = isRetry ? "Повтор…" : "Запуск…";
  api.installServer(s.id, { profile_id, vars })
    .then(() => {
      closeModal();
      showToast(isRetry ? "Повтор установки запущен. Новый сервер не создаётся." : mode === "reinstall" ? "Переустановка запущена." : "Установка запущена.");
      loadServers();
    })
    .catch((e: Error & { code?: string }) => {
      btn.disabled = false; btn.textContent = label;
      const box = $("sv-install-err");
      const msg = installErrText(e);
      if (box && document.body.contains(box)) { box.textContent = msg; box.hidden = false; }
      else showToast(msg, true);
    });
}

function checkServer(s: ManualServer): void {
  showToast("Проверка SSH-доступа…");
  api.checkServer(s.id)
    .then((r) => {
      showToast(r.verified ? "Доступ по SSH подтверждён." : "SSH недоступен: " + (r.last_error || "неизвестная ошибка"), !r.verified);
      loadServers();
    })
    .catch((e: Error) => showToast(e.message, true));
}

function confirmDeleteServer(s: ManualServer): void {
  openModal(`Удалить сервер «${s.label}»?`,
    "Запись будет удалена, место в квоте освободится. Нода (если создана) остаётся в пуле — её удаление отдельным действием в «Мои ноды». Сам сервер не затрагивается.",
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Удалить", cls: "danger", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        btn.disabled = true; btn.textContent = "Удаление…";
        api.deleteServer(s.id)
          .then(() => { closeModal(); showToast("Сервер удалён."); loadServers(); refresh().catch(() => {}); })
          .catch((e: Error) => { showToast(e.message, true); btn.disabled = false; btn.textContent = "Удалить"; });
      } },
    ]);
}

/* ── форма «Подключить свой сервер» ─────────────────────────────────── */
let svAuthType: "password" | "key" = "password";

function setAuthType(t: "password" | "key", focus = false): void {
  svAuthType = t;
  document.querySelectorAll<HTMLElement>("#sv-authtype button[data-authtype]").forEach((b) => {
    const on = b.dataset.authtype === t;
    b.classList.toggle("active", on);
    b.setAttribute("aria-checked", String(on));
    b.tabIndex = on ? 0 : -1; // L1: APG roving tabindex
    if (on && focus) b.focus();
  });
  $("sv-pass-wrap").hidden = t !== "password";
  $("sv-key-wrap").hidden = t !== "key";
  // L5: секрет неактивного способа не остаётся в DOM.
  if (t === "password") {
    ($("sv-key") as HTMLTextAreaElement).value = "";
    ($("sv-key-file") as HTMLInputElement).value = "";
  } else {
    ($("sv-pass") as HTMLInputElement).value = "";
  }
}

function clearServerSecret(): void {
  ($("sv-pass") as HTMLInputElement).value = "";
  ($("sv-key") as HTMLTextAreaElement).value = "";
  ($("sv-key-file") as HTMLInputElement).value = "";
}

function submitServerForm(e: Event): void {
  e.preventDefault();
  const errBox = $("sv-err");
  errBox.hidden = true;
  const showErr = (m: string): void => { errBox.textContent = m; errBox.hidden = false; };
  const label = ($("sv-label") as HTMLInputElement).value.trim();
  const host = ($("sv-host") as HTMLInputElement).value.trim();
  const portRaw = ($("sv-port") as HTMLInputElement).value.trim();
  const user = ($("sv-user") as HTMLInputElement).value.trim();
  const pass = ($("sv-pass") as HTMLInputElement).value;
  const key = ($("sv-key") as HTMLTextAreaElement).value;
  if (!label) { showErr("Укажите название."); return; }
  if (!host) { showErr("Укажите IP-адрес."); return; }
  if (svAuthType === "password" && !pass) { showErr("Введите пароль или переключитесь на вход по ключу."); return; }
  if (svAuthType === "key" && !key.trim()) { showErr("Вставьте приватный ключ или выберите файл."); return; }
  // B2: тот же host:port уже подключён — не шлём запрос и не тратим слот.
  const port = portRaw ? Number(portRaw) : 22;
  if (SERVERS.some((s) => s.host === host && s.port === port)) {
    showErr("Этот адрес уже подключён. Откройте его в списке или удалите прежнюю запись.");
    return;
  }
  const btn = $("sv-submit") as HTMLButtonElement;
  const label0 = btn.textContent;
  btn.disabled = true; btn.textContent = "Подключение…";
  const body: ManualServerPayload = { label, host };
  if (portRaw) body.port = Number(portRaw);
  if (user) body.ssh_user = user;
  if (svAuthType === "password") body.password = pass; else body.private_key = key;
  api.createServer(body)
    .then(() => {
      clearServerSecret();
      (["sv-label", "sv-host", "sv-port", "sv-user"] as const).forEach((id) => { ($(id) as HTMLInputElement).value = ""; });
      toggleServerForm(false); // возврат к списку
      showToast("Сервер подключён.");
      loadServers();
    })
    // Валидация бэкенда (400 validation_failed) — показываем причину дословно.
    .catch((e: Error) => showErr(e.message || "Не удалось подключить сервер."))
    .finally(() => { btn.disabled = false; btn.textContent = label0; });
}

/* «Добавить сервер» открывает существующую форму подключения (список — первым). */
function toggleServerForm(open: boolean): void {
  const form = $("server-form") as HTMLFormElement;
  form.hidden = !open;
  $("srv-add").setAttribute("aria-expanded", String(open));
  if (open) ($("sv-label") as HTMLInputElement).focus();
  else clearServerSecret();
}

function initServerForm(): void {
  ($("server-form") as HTMLFormElement).addEventListener("submit", submitServerForm);
  $("srv-add").addEventListener("click", () => toggleServerForm(!!($("server-form") as HTMLFormElement).hidden));
  $("sv-cancel").addEventListener("click", () => toggleServerForm(false));
  const group = $("sv-authtype");
  const radios = (): HTMLElement[] => Array.from(group.querySelectorAll<HTMLElement>("button[data-authtype]"));
  radios().forEach((b) =>
    b.addEventListener("click", () => setAuthType((b.dataset.authtype as "password" | "key"))));
  // L1: APG radiogroup — стрелки перемещают выбор и фокус (roving tabindex).
  group.addEventListener("keydown", (e) => {
    if (!["ArrowRight", "ArrowDown", "ArrowLeft", "ArrowUp"].includes(e.key)) return;
    e.preventDefault();
    const list = radios();
    const i = list.indexOf(document.activeElement as HTMLElement);
    const dir = e.key === "ArrowRight" || e.key === "ArrowDown" ? 1 : -1;
    const next = list[(i + dir + list.length) % list.length];
    setAuthType(next.dataset.authtype as "password" | "key", true);
  });
  setAuthType(svAuthType); // синхронизируем initial tabindex/aria/hidden
  ($("sv-key-file") as HTMLInputElement).addEventListener("change", (e) => {
    const f = (e.target as HTMLInputElement).files?.[0];
    if (!f) return;
    const rd = new FileReader();
    rd.onload = () => { ($("sv-key") as HTMLTextAreaElement).value = String(rd.result || ""); };
    rd.onerror = () => showToast("Не удалось прочитать файл ключа.", true);
    rd.readAsText(f);
  });
}

/* ── операции ────────────────────────────────────────────────────────── */
/* Действия строки. «Повторить установку» переиспользует существующий VPS и
   НИКОГДА не создаёт второй платный заказ. Кнопки нового заказа здесь нет. */
const STEP_META: Record<string, { label: string; cls: string }> = {
  running: { label: "выполняется", cls: "info" },
  ok:      { label: "готово", cls: "ok" },
  error:   { label: "ошибка", cls: "bad" },
};

/* Длительность шага в мс → «1.2 с» / «830 мс» / «—» (шаг ещё идёт). */
function fmtStepMs(ms: number): string {
  if (!ms) return "—";
  return ms >= 1000 ? (ms / 1000).toFixed(1) + " с" : ms + " мс";
}

/* Журнал шагов операции (Фаза 3): idx, имя, бейдж статуса, длительность;
   вывод шага (секреты уже замаскированы сервером) — в сворачиваемом блоке. */
function stepsHTML(o: Operation): string {
  const steps = o.steps || [];
  if (!steps.length) return `<div class="op-steps-empty dim">пошаговый журнал недоступен</div>`;
  return `<div class="op-steps">${steps.map((s: OperationStep) => {
    const meta = STEP_META[s.status] || { label: s.status, cls: "" };
    const key = `${o.id}:${s.idx}`;
    const out = s.output
      ? `<details class="op-step-log" data-oplog="${key}"${OPEN_LOGS.has(key) ? " open" : ""}>
          <summary>лог</summary><pre class="op-err srv-out">${esc(s.output)}</pre></details>`
      : "";
    return `<div class="op-step">
      <span class="op-step-idx mono">${s.idx + 1}</span>
      <span class="op-step-name">${esc(s.name)}</span>
      <span class="badge ${meta.cls}"><i></i>${esc(meta.label)}</span>
      <span class="op-step-dur dim mono">${esc(fmtStepMs(s.duration_ms))}</span>
      <div class="op-step-logwrap">${out}</div>
    </div>`;
  }).join("")}</div>`;
}

function actionCell(o: Operation): string {
  const steps = (o.steps || []).length
    ? `<button class="btn sm ghost" data-optoggle="${o.id}" aria-expanded="${OPEN_OPS.has(o.id)}">${OPEN_OPS.has(o.id) ? "Скрыть шаги" : `Шаги (${o.steps!.length})`}</button>`
    : "";
  if (o.state === "pending") return `${steps}<button class="btn sm" data-cancel="${o.id}">Отменить</button>`;
  if (o.state === "sent") return `${steps}<button class="btn sm" disabled title="Заказ уже отправлен провайдеру — отмена невозможна">Отменить</button>`;
  // Повтор допустим только для операции с уже созданным VPS (has_vps). Отсутствие
  // поля (старый backend) — прежнее поведение; has_vps:false — повтора нет.
  if ((o.state === "failed" || o.state === "waiting_vps") && o.has_vps !== false)
    return `${steps}<button class="btn sm" data-retry="${o.id}">Повторить установку</button>`;
  return steps || `<span class="dim">—</span>`;
}

function renderOperations(poll = false): void {
  const body = $("ops-body");
  const note = $("ops-note");
  const hasUnknown = OPS.some((o) => o.state === "unknown");
  const hasWaiting = OPS.some((o) => o.state === "waiting_vps");

  renderSection(body, poll, () => {
  if (!OPS.length) {
    body.innerHTML = `<tr><td colspan="6"><div class="empty-state">
      <span class="t">Операций пока нет</span>
      <span class="s">Здесь появится журнал заказов серверов.</span></div></td></tr>`;
  } else {
    body.innerHTML = OPS.map((o) => {
      const meta = OP_META[o.state] || { label: o.state, cls: "" };
      const hl = o.id === HIGHLIGHT_OP ? ' class="row-highlight"' : "";
      const open = OPEN_OPS.has(o.id);
      return `<tr data-op="${o.id}"${hl}>
        <td data-label="Создана" class="mono">${esc(fmtClock(new Date(o.created_at * 1000).toISOString()))}</td>
        <td data-label="Тип">${esc(o.kind)}</td>
        <td data-label="Нода" class="mono">${esc(o.node_id || "—")}</td>
        <td data-label="Состояние"><span class="badge ${meta.cls}"><i></i>${esc(meta.label)}</span></td>
        <td data-label="Ошибка" class="wrap"><span class="op-err">${esc(o.last_error || "—")}</span></td>
        <td data-label="Действие">${actionCell(o)}</td>
      </tr>
      <tr class="op-steps-row" data-opsteps="${o.id}"${open ? "" : " hidden"}>
        <td colspan="6">${stepsHTML(o)}</td>
      </tr>`;
    }).join("");
  }

  const notes: string[] = [];
  if (hasUnknown) {
    notes.push("Ответ провайдера потерян. Повторный платный заказ запрещён; регистратор сверяет ресурс у провайдера. Если сверка не удалась, запись остаётся для ручного разбора — создайте обращение к администратору с этим id.");
  }
  if (hasWaiting) {
    notes.push("«Ждём готовности VPS»: платный заказ подтверждён, но сервер не дошёл до RUNNING в отведённое окно. Установка ещё не запущена, место в квоте занято — новый заказ невозможен. «Повторить установку» переиспользует тот же сервер и не покупает второй.");
  }
  note.hidden = !notes.length;
  note.textContent = notes.join(" ");
  });
}

/* Пояснение к отказу повторной установки. */
function retryErrText(e: Error & { code?: string }): string {
  switch (e.code) {
    case "no_vps": return "Сервер ещё не создан — повтор установки невозможен";
    case "in_progress": return "Операция ещё выполняется — дождитесь завершения";
    case "already_done": return "Операция уже завершена";
    default: return e.message;
  }
}

/* ── Beget ───────────────────────────────────────────────────────────── */
function renderBeget(link: BegetLink, poll = false): void {
  const el = $("beget-status");
  renderSection(el, poll, () => {
  if (link.verified) {
    el.innerHTML = `<div class="kv" style="gap:8px">
      <span class="kv-v">Статус</span>
      <span><span class="badge ok"><i></i>подключён</span></span>
    </div>
    <div class="dim" style="margin-top:8px">SSH-ключ зарегистрирован. Секреты наружу не отдаются.</div>`;
  } else if (link.last_error) {
    el.innerHTML = `<div class="kv" style="gap:8px">
      <span class="kv-v">Статус</span>
      <span><span class="badge bad"><i></i>ошибка</span></span>
      <span style="margin-top:8px;color:var(--bad)">${esc(link.last_error)}</span>
    </div>
    <div class="banner info" style="margin:12px 0 0">Подключите личный аккаунт Beget, чтобы создавать серверы.</div>`;
  } else {
    el.innerHTML = `<div class="banner info" style="margin:0">Подключите личный аккаунт Beget — регистратор добавит SSH-ключ для установки.</div>`;
  }
  });
}

let BEGET_RETRY_DONE = false;
function loadBeget(poll = false): void {
  api.begetLink().then((l) => { BEGET_RETRY_DONE = false; renderBeget(l, poll); }).catch((e: Error & { status?: number; code?: string }) => {
    // Форма подключения Beget остаётся доступной; ошибка инлайн + «Повторить»,
    // один self-heal при временном сбое (рестарт регистратора).
    $("beget-status").innerHTML = `<div class="banner bad"><span>Не удалось загрузить статус: ${esc(e.message)}</span>
      <button class="btn sm" id="beget-retry" style="margin-left:auto">Повторить</button></div>`;
    $("beget-retry")?.addEventListener("click", () => { BEGET_RETRY_DONE = false; loadBeget(); });
    if (!BEGET_RETRY_DONE && isRetryableLoad(e)) { BEGET_RETRY_DONE = true; setTimeout(loadBeget, 2000); }
  });
}

/* ═══ Хостинги: подключения и облачные ресурсы ═════════════════════════
   docs/specs/hosting-blueprints/{design-cabinet-hosting.md, specs/cabinet-hosting-ui.md}.
   Сегменты «Подключения»/«Ресурсы» внутри #view-beget; асинхронные команды
   (create/delete) не показываются готовыми, пока ресурс не дошёл по опросу. */

const EYE_SVG =
  `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>`;

let HCONNS: HostingConnection[] = [];
let HCONNS_ERR: string | null = null;
let HCONNS_RETRY = false;
let OPEN_CONNS = new Set<number>();
let CRES: CloudResource[] = [];
let CRES_ERR: string | null = null;
let CRES_RETRY = false;
let CRES_HIGHLIGHT = 0;
let OPEN_RES = new Set<number>();
let HOSTING_TAB: "conns" | "resources" = "conns";
let CR_IDEM = "";
// Прогресс по ресурсу (принятая асинхронная команда) — рисуется внутри карточки.
let RES_PROGRESS: Record<number, string> = {};

/* Баланс: безопасный код → русская причина (design doc §3.2). */
const BAL_ERR: Record<string, string> = {
  provider_error: "Провайдер вернул ошибку",
  provider_timeout: "Таймаут провайдера",
  credentials_unavailable: "Учётные данные недоступны",
};
function balErrText(b: HostingBalance): string {
  if (!b.error) return "";
  return BAL_ERR[b.error.code] || "Баланс недоступен";
}
function balanceBadge(b: HostingBalance): string {
  const age = b.checked_at ? fmtAgo(new Date(b.checked_at * 1000).toISOString()) : "";
  const tail = age ? ` · ${esc(age)}` : "";
  if (b.freshness === "fresh") return `<span class="badge ok"><i></i>актуален${tail}</span>`;
  if (b.freshness === "stale") return `<span class="badge warn"><i></i>устарел${tail}</span>`;
  return `<span class="badge">недоступен</span>`;
}
function balanceAmount(b: HostingBalance): string {
  if (b.amount === null || b.amount === undefined) return "сумма не указана";
  return `${esc(b.amount)}${b.currency ? " " + esc(b.currency) : ""}`;
}
function capBadge(on: boolean, label: string): string {
  return on
    ? `<span class="badge ok">${esc(label)}</span>`
    : `<span class="badge" title="провайдер не поддерживает">${esc(label)}</span>`;
}
function announce(text: string): void {
  const el = document.getElementById("hosting-live");
  if (el) el.textContent = text;
}
function providerRu(p: string): string {
  return p === "nodehost" ? "Nodehost" : p === "beget" ? "Beget" : p;
}

/* ── сегменты ────────────────────────────────────────────────────────── */
function parseHostingTab(): "conns" | "resources" {
  const m = /[?&]tab=(conns|resources)/.exec(location.hash || "");
  return m && m[1] === "resources" ? "resources" : "conns";
}
function writeHostingTab(name: string): void {
  const base = (location.hash || "").split("?")[0] || "#/beget";
  history.replaceState(null, "", location.pathname + location.search + base + "?tab=" + name);
}
function activateHostingTab(name: string, focusPanel: boolean, writeHash = true): void {
  HOSTING_TAB = name === "resources" ? "resources" : "conns";
  document.querySelectorAll<HTMLButtonElement>("#hosting-tabs [role='tab']").forEach((b) => {
    const on = b.dataset.htab === HOSTING_TAB;
    b.setAttribute("aria-selected", String(on));
    b.tabIndex = on ? 0 : -1;
  });
  document.querySelectorAll<HTMLElement>("#view-beget [data-hpanel]").forEach((p) => {
    p.hidden = p.dataset.hpanel !== HOSTING_TAB;
  });
  if (writeHash) writeHostingTab(HOSTING_TAB);
  if (HOSTING_TAB === "resources") loadResources();
  if (focusPanel) document.getElementById("hpanel-" + HOSTING_TAB)?.focus({ preventScroll: true });
}
/* Вход в раздел «Хостинги»: сегмент из хэша (или resources при deep-link),
   загрузка подключений и без лишнего replaceState для `#/resources/{id}`. */
function enterHosting(highlightId = 0): void {
  loadBeget();
  if (highlightId) { CRES_HIGHLIGHT = highlightId; activateHostingTab("resources", false, false); }
  else activateHostingTab(parseHostingTab(), false, false);
  loadConnections();
}

/* ── подключения: список ─────────────────────────────────────────────── */
function connCardHTML(c: HostingConnection): string {
  const b = c.balance;
  const ver = c.verified_at
    ? `<span class="badge ok"><i></i>подключён</span>`
    : `<span class="badge">не проверен</span>`;
  const problem = b.error
    ? `<div class="node-problem" title="${esc(b.error.code)}">${esc(balErrText(b))}</div>` : "";
  const open = OPEN_CONNS.has(c.id);
  const fmt = (t: number | null): string => t ? esc(fmtAgo(new Date(t * 1000).toISOString())) : "—";
  return `<article class="node-card hc${open ? " open" : ""}" role="listitem" data-conn="${c.id}" aria-labelledby="hc-t-${c.id}">
    <div class="node-main">
      <div class="node-top">
        <span class="node-id" id="hc-t-${c.id}">${esc(c.label)}</span>
        <span class="badge mono">${esc(c.provider)}</span>
        ${ver}
      </div>
      <div class="node-sub">
        ${capBadge(c.capabilities.create, "создание")}${capBadge(c.capabilities.change_ip, "смена IP")}${capBadge(c.capabilities.delete, "удаление")}
        <span class="hc-balance"><span class="dim">баланс</span> ${balanceBadge(b)} <span class="mono">${balanceAmount(b)}</span></span>
      </div>
      ${problem}
    </div>
    <div class="node-side">
      <button type="button" class="btn sm" data-hcbalance="${c.id}">Обновить баланс</button>
      <button type="button" class="btn sm" data-hcverify="${c.id}">Проверить</button>
      <button type="button" class="btn sm ghost" data-hcedit="${c.id}">Изменить</button>
      <button type="button" class="btn sm danger" data-hcdel="${c.id}">Удалить</button>
      <button type="button" class="btn sm ghost" data-hctoggle="${c.id}" aria-expanded="${open}" aria-controls="hc-more-${c.id}">${open ? "Свернуть" : "Детали"}</button>
    </div>
    <div class="node-more" id="hc-more-${c.id}">
      <div class="kv"><span class="kv-k">ID</span><span class="kv-v mono">${c.id}</span></div>
      <div class="kv"><span class="kv-k">Ревизия</span><span class="kv-v mono">${c.revision}</span></div>
      <div class="kv"><span class="kv-k">Ревизия учётных данных</span><span class="kv-v mono">${c.credential_revision}</span></div>
      <div class="kv"><span class="kv-k">Проверено</span><span class="kv-v">${fmt(c.verified_at)}</span></div>
      <div class="kv"><span class="kv-k">Попытка баланса</span><span class="kv-v">${fmt(b.last_attempt_at)}</span></div>
      <div class="kv"><span class="kv-k">Область баланса</span><span class="kv-v mono">${esc(b.scope || "—")}</span></div>
    </div>
  </article>`;
}

function renderConnections(poll = false): void {
  const body = $("hosting-conns");
  renderSection(body, poll, () => {
    $("hosting-conns-hint").textContent = HCONNS.length
      ? `${HCONNS.length} ${plural(HCONNS.length, "подключение", "подключения", "подключений")}` : "";
    if (HCONNS_ERR !== null) {
      body.innerHTML = `<div role="listitem"><div class="banner bad" role="alert"><span>Не удалось загрузить подключения: ${esc(HCONNS_ERR)}</span>
        <button class="btn sm" data-hcreload="1" style="margin-left:auto">Повторить</button></div></div>`;
      return;
    }
    if (!HCONNS.length) {
      body.innerHTML = `<div class="empty-state" role="listitem">${EMPTY_SVG}
        <span class="t">Подключений пока нет</span>
        <span class="s">Добавьте подключение к Nodehost или Beget — регистратор сможет создавать и обслуживать серверы прямо отсюда.</span></div>`;
      return;
    }
    body.innerHTML = HCONNS.map(connCardHTML).join("");
  });
}

function loadConnections(poll = false): void {
  api.hostingConnections().then((r) => {
    HCONNS = r.connections || [];
    HCONNS_ERR = null; HCONNS_RETRY = false;
    renderConnections(poll);
  }).catch((e: Error & { status?: number; code?: string }) => {
    if (e.message === "unauthorized") return;
    HCONNS_ERR = e.message;
    renderConnections(poll);
    // 4xx не повторяем; 5xx/control_unavailable — один self-heal.
    if (!HCONNS_RETRY && isRetryableLoad(e)) { HCONNS_RETRY = true; setTimeout(() => loadConnections(), 2000); }
  });
}

/* ── подключение: форма add/edit ─────────────────────────────────────── */
function connCredsHTML(provider: Provider, isEdit: boolean): string {
  if (provider === "nodehost") {
    return `<div class="field" style="margin-top:12px">
      <label for="hc-api-key">API-ключ</label>
      <div class="input-group">
        <input id="hc-api-key" type="password" autocomplete="new-password">
        <button type="button" class="in-btn" data-reveal="hc-api-key" aria-label="Показать ключ" title="Показать ключ">${EYE_SVG}</button>
      </div>
      <span class="help">Write-only: не отображается после сохранения. Оставьте пустым при правке, чтобы не менять.</span></div>`;
  }
  return `<div class="srv-grid" style="margin-top:12px">
    <div class="field"><label for="hc-cloud-login">Логин облака</label><input id="hc-cloud-login" class="mono" autocomplete="off"></div>
    <div class="field"><label for="hc-cloud-pass">Пароль облака</label>
      <div class="input-group"><input id="hc-cloud-pass" type="password" autocomplete="new-password"><button type="button" class="in-btn" data-reveal="hc-cloud-pass" aria-label="Показать пароль">${EYE_SVG}</button></div>
    </div>
    <div class="field"><label for="hc-hosting-login">Логин хостинга (API)</label><input id="hc-hosting-login" class="mono" autocomplete="off"><span class="help">Необязательно; если указан — заполните и пароль.</span></div>
    <div class="field"><label for="hc-hosting-pass">Пароль хостинга (API)</label>
      <div class="input-group"><input id="hc-hosting-pass" type="password" autocomplete="new-password"><button type="button" class="in-btn" data-reveal="hc-hosting-pass" aria-label="Показать пароль">${EYE_SVG}</button></div>
    </div>
  </div>`;
}

function connEditor(conn: HostingConnection | null): void {
  const isEdit = conn !== null;
  const provider: Provider = conn?.provider || "nodehost";
  const sel = `<div class="field">
      <label for="hc-provider">Провайдер</label>
      <select id="hc-provider"${isEdit ? ' disabled title="провайдер неизменен"' : ""}>
        <option value="nodehost"${provider === "nodehost" ? " selected" : ""}>Nodehost</option>
        <option value="beget"${provider === "beget" ? " selected" : ""}>Beget</option>
      </select>
      <span class="help">${isEdit ? `Провайдер: ${esc(providerRu(provider))}` : "Провайдер подключения изменить нельзя."}</span>
    </div>`;
  const label = `<div class="field" style="margin-top:12px">
      <label for="hc-label">Название</label>
      <input id="hc-label" type="text" maxlength="100" autocomplete="off" value="${isEdit ? esc(conn!.label) : ""}" required>
    </div>`;
  openModal(isEdit ? `Подключение · ${conn!.label}` : "Добавить подключение",
    `<form id="hc-form" data-rev="${isEdit ? conn!.revision : 0}" novalidate>${sel}${label}<div id="hc-creds">${connCredsHTML(provider, isEdit)}</div>
      <div class="banner bad" id="hc-form-err" role="alert" hidden style="margin:12px 0 0"></div></form>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: isEdit ? "Сохранить" : "Добавить", cls: "primary", onClick: (ev) => submitConn(ev.currentTarget as HTMLButtonElement, conn) },
    ]);
  const provSel = $("hc-provider") as HTMLSelectElement | null;
  provSel?.addEventListener("change", () => {
    const wrap = document.getElementById("hc-creds");
    if (wrap) wrap.innerHTML = connCredsHTML(provSel.value as Provider, false);
  });
  ($("hc-label") as HTMLInputElement)?.focus();
}

function submitConn(btn: HTMLButtonElement, conn: HostingConnection | null): void {
  const form = $("hc-form") as HTMLFormElement | null;
  const errBox = $("hc-form-err");
  if (!form || !errBox) return;
  errBox.hidden = true;
  const showErr = (m: string, focusId?: string): void => {
    errBox.textContent = m; errBox.hidden = false;
    if (focusId) ($(focusId) as HTMLElement | null)?.focus();
  };
  const provider: Provider = conn ? conn.provider : (($("hc-provider") as HTMLSelectElement).value as Provider);
  const label = ($("hc-label") as HTMLInputElement).value.trim();
  if (!label) { showErr("Укажите название.", "hc-label"); return; }
  const creds: Record<string, string> = {};
  if (provider === "nodehost") {
    const key = ($("hc-api-key") as HTMLInputElement | null)?.value || "";
    if (!conn && !key) { showErr("Укажите API-ключ.", "hc-api-key"); return; }
    if (key) creds.api_key = key;
  } else {
    const cl = ($("hc-cloud-login") as HTMLInputElement | null)?.value.trim() || "";
    const cp = ($("hc-cloud-pass") as HTMLInputElement | null)?.value || "";
    const hl = ($("hc-hosting-login") as HTMLInputElement | null)?.value.trim() || "";
    const hp = ($("hc-hosting-pass") as HTMLInputElement | null)?.value || "";
    if ((!!cl) !== (!!cp)) { showErr("Заполните логин и пароль облака вместе.", cl ? "hc-cloud-pass" : "hc-cloud-login"); return; }
    if ((!!hl) !== (!!hp)) { showErr("Логин и пароль хостинга (API) заполняются вместе.", hl ? "hc-hosting-pass" : "hc-hosting-login"); return; }
    if (!conn && (!cl || !cp)) { showErr("Заполните логин и пароль облака.", "hc-cloud-login"); return; }
    if (cl) creds.cloud_login = cl;
    if (cp) creds.cloud_password = cp;
    if (hl) creds.hosting_api_login = hl;
    if (hp) creds.hosting_api_password = hp;
  }
  const body: HostingConnectionInput = { label };
  if (Object.keys(creds).length) body.credentials = creds;
  const lbl0 = btn.textContent;
  btn.disabled = true; btn.textContent = conn ? "Сохранение…" : "Добавление…";
  const call = conn
    ? api.updateHostingConnection(conn.id, { ...body, expected_revision: Number(form.dataset.rev) || conn.revision })
    : api.createHostingConnection({ ...body, provider });
  call.then(() => {
    closeModal();
    showToast(conn ? "Подключение сохранено" : "Подключение добавлено");
    loadConnections();
  }).catch((e: Error & { code?: string }) => {
    btn.disabled = false; btn.textContent = lbl0;
    const box = $("hc-form-err");
    if (!document.body.contains(box)) { showToast(e.message, true); return; }
    if (e.code === "revision_conflict") {
      box.innerHTML = `Настройки изменились; обновите данные. <button type="button" class="btn sm" id="hc-form-reload">Обновить</button>`;
      box.hidden = false;
      $("hc-form-reload")?.addEventListener("click", () => { if (conn) refreshConnRevision(conn.id); });
    } else if (e.code === "connection_busy") {
      box.textContent = "По подключению есть незавершённая команда — смена учётных данных отложена.";
      box.hidden = false;
    } else {
      box.textContent = e.message; box.hidden = false;
    }
  });
}

/* revision_conflict: обновляем только ревизию (введённое пользователем не
   затираем) — повторное «Сохранить» пройдёт по свежему CAS. */
function refreshConnRevision(id: number): void {
  api.hostingConnections().then((r) => {
    HCONNS = r.connections || [];
    const c = HCONNS.find((x) => x.id === id);
    const form = $("hc-form") as HTMLFormElement | null;
    if (c && form) form.dataset.rev = String(c.revision);
    const box = $("hc-form-err"); if (box) box.hidden = true;
    showToast("Данные обновлены — повторите сохранение.");
    renderConnections();
  }).catch((e: Error) => showToast(e.message, true));
}

/* ── проверка подключения (с опциональным 2FA) ───────────────────────── */
function verifyConn(conn: HostingConnection): void {
  openModal(`Проверка · ${conn.label}`,
    `<div class="field">
      <label for="hc-code">Код подтверждения (2FA)</label>
      <input id="hc-code" type="text" class="mono" inputmode="numeric" autocomplete="one-time-code" maxlength="8">
      <span class="help">Необязательно: введите, если провайдер запросил код. Код не сохраняется.</span>
    </div>
    <div class="banner bad" id="hc-verify-err" role="alert" hidden style="margin:12px 0 0"></div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Проверить", cls: "primary", onClick: (ev) => doVerify(ev.currentTarget as HTMLButtonElement, conn) },
    ]);
  // Первый запрос — без кода; 2FA раскроет поле (design doc §4).
  const primary = $("modal-foot").querySelector<HTMLButtonElement>("button.primary");
  doVerify(primary, conn);
}

function doVerify(btn: HTMLButtonElement | null, conn: HostingConnection): void {
  const codeEl = $("hc-code") as HTMLInputElement | null;
  const code = codeEl ? codeEl.value.trim() : "";
  const box = $("hc-verify-err");
  const lbl = btn ? btn.textContent : "Проверить";
  if (btn) { btn.disabled = true; btn.textContent = "Проверка…"; }
  api.verifyHostingConnection(conn.id, code || undefined)
    .then(() => {
      closeModal();
      showToast("Подключение проверено");
      announce(`Подключение ${conn.label}: проверено`);
      loadConnections();
    })
    .catch((e: Error & { code?: string; status?: number; reason?: string }) => {
      if (btn) { btn.disabled = false; btn.textContent = lbl; }
      if (!document.body.contains(box)) { showToast(e.message, true); return; }
      if (e.code === "provider_2fa_required" || e.reason === "code_required") {
        if (codeEl) { codeEl.style.borderColor = "var(--bad)"; codeEl.focus(); }
        box.textContent = "Провайдер требует код подтверждения (2FA).";
      } else if (e.status === 429) {
        box.textContent = e.message || "Слишком часто. Повторите позже.";
      } else if (e.code === "provider_timeout") {
        box.textContent = "Провайдер не ответил вовремя. Повторите позже.";
      } else {
        box.textContent = e.message;
      }
      box.hidden = false;
    });
}

/* ── обновление баланса ──────────────────────────────────────────────── */
function refreshBalance(btn: HTMLButtonElement, conn: HostingConnection): void {
  const lbl = btn.textContent;
  btn.disabled = true; btn.textContent = "Обновление…";
  api.refreshHostingBalance(conn.id).then((c) => {
    const i = HCONNS.findIndex((x) => x.id === c.id);
    if (i >= 0) HCONNS[i] = c; else HCONNS.push(c);
    renderConnections();
    showToast("Баланс обновлён");
    announce(`Баланс ${c.label}: ${balanceAmount(c.balance)}`);
  }).catch((e: Error & { status?: number }) => {
    if (e.status === 404) { showToast("Подключение не найдено", true); loadConnections(); }
    else showToast(e.message, true);
  }).finally(() => {
    if (document.body.contains(btn)) { btn.disabled = false; btn.textContent = lbl; }
  });
}

/* ── удаление подключения ────────────────────────────────────────────── */
function confirmDeleteConn(conn: HostingConnection): void {
  openModal(`Удалить подключение «${conn.label}»?`,
    "Подключение будет удалено. Если на нём есть ресурсы, операции или незавершённые эффекты, удаление отклоняется (409). Секреты удаляются вместе с подключением.",
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Удалить", cls: "danger", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        btn.disabled = true; btn.textContent = "Удаление…";
        api.deleteHostingConnection(conn.id).then(() => {
          closeModal(); showToast("Подключение удалено"); loadConnections();
        }).catch((e: Error & { code?: string; status?: number }) => {
          closeModal();
          if (e.code === "connection_in_use") showToast("Подключение используется ресурсом или операцией — удалить нельзя", true);
          else if (e.status === 404) showToast("Подключение не найдено", true);
          else showToast(e.message, true);
          loadConnections();
        });
      } },
    ]);
}

/* ═══ Ресурсы ══════════════════════════════════════════════════════════ */
const CR_META: Record<string, { label: string; cls: string }> = {
  planned:  { label: "запланирован", cls: "info" },
  creating: { label: "создаётся · ожидание IP…", cls: "info" },
  ready:    { label: "готов", cls: "ok" },
  deleting: { label: "удаляется…", cls: "warn" },
  deleted:  { label: "удалён", cls: "" },
  error:    { label: "Ошибка", cls: "bad" },
  installing: { label: "устанавливается", cls: "info" },
  admission_pending: { label: "ожидает допуска", cls: "info" },
  admitted: { label: "допущен", cls: "info" },
  superseded: { label: "заменён", cls: "warn" },
  unknown:  { label: "неизвестно", cls: "special" },
};
function crMeta(state: string): { label: string; cls: string } {
  return CR_META[state] || { label: state, cls: "info" }; // неизвестный state — дословно
}
/* Провайдер может оставить planned/creating с last_error: это провал, а не
   «ожидание IP». Причина важнее формального state. */
function crFailed(r: CloudResource): boolean {
  return r.state === "error" ||
    ((r.state === "planned" || r.state === "creating") && !!crReasonText(r.last_error));
}
function crReasonOf(r: CloudResource): string {
  if (!crFailed(r)) return "";
  return crReasonText(r.last_error) || (r.state === "error" ? "причина не указана" : "");
}
/* Продуктовая метка: «готов» существует только вместе с IP. */
function crMetaFor(r: CloudResource): { label: string; cls: string } {
  if (crFailed(r)) return { label: "Ошибка", cls: "bad" };
  if (r.state === "ready" && !resIP(r)) return { label: "создаётся · ожидание IP…", cls: "info" };
  return crMeta(r.state);
}
function resIP(r: CloudResource): string {
  return (r.ip || r.current_ip || "").trim();
}
function resReady(r: CloudResource): boolean {
  return r.state === "ready" && resIP(r) !== "";
}
const CR_REASON: Record<string, string> = {
  insufficient_funds: "Недостаточно средств",
  forbidden: "Доступ запрещён провайдером",
  validation_failed: "Некорректные параметры",
  provider_timeout: "Провайдер не ответил вовремя",
  no_ip: "Провайдер не выдал IP",
  worker_busy: "Сервис занят, повторите позже",
  credentials_unavailable: "Учётные данные недоступны",
  not_ready: "Сервер ещё не готов",
  ssh_unavailable: "SSH недоступен",
  not_implemented: "Провайдер не поддерживает действие",
  provider_reconcile_required: "Платный исход операции неясен — сверьте ресурсы с хостингом",
};
/* last_error приходит кодом (реже — текстом): маппим известный код, прочее
   показываем дословно, чтобы причина не терялась. */
function crReasonText(raw?: string): string {
  const v = (raw || "").trim();
  if (!v) return "";
  const code = v.split(/[\s:;]/)[0];
  return CR_REASON[code] || v;
}
const BEGET_BANNER =
  `<div class="banner info"><span>Управляемое облако Beget пока недоступно. Создание и обслуживание серверов у Beget выполняются через прежний поток — <a href="#/create">Создать сервер</a>.</span></div>`;

interface CrCheckState { status: "checking" | "ok" | "fail"; text: string; }
let CR_CHECK: Record<number, CrCheckState> = {};

function crCheckHTML(id: number): string {
  const c = CR_CHECK[id];
  if (!c) return "";
  if (c.status === "checking")
    return `<div class="cr-check info"><span class="cr-spin" aria-hidden="true"></span> Проверка SSH…</div>`;
  return `<div class="cr-check ${c.status === "ok" ? "ok" : "bad"}">${c.status === "ok" ? "SSH доступен" : "SSH недоступен: " + esc(c.text || "неизвестная ошибка")}</div>`;
}

function checkResourceSSH(r: CloudResource): void {
  CR_CHECK[r.id] = { status: "checking", text: "" };
  renderResources(); renderCloudServers();
  api.checkCloudResource(r.id)
    .then((res) => {
      CR_CHECK[r.id] = res.ssh_ok
        ? { status: "ok", text: "" }
        : { status: "fail", text: crReasonText(res.error) || "неизвестная ошибка" };
    })
    .catch((e: Error & { code?: string; status?: number }) => {
      CR_CHECK[r.id] = { status: "fail", text: CR_REASON[e.code || ""] || e.message };
    })
    .finally(() => { renderResources(); renderCloudServers(); });
}

/* ── сверка с хостингом (provider_reconcile_required) ──────────────────── */
/* busy — идёт вызов reconcile; hint — 409/неясный исход: подсказка вместо
   тупикового тоста. Кнопка показывается на любой ошибочной строке. */
const CR_RECON_HINT = "На аккаунте есть несвязанные серверы — проверьте вручную на Nodehost";
interface CrReconState { busy?: boolean; hint?: string; }
let CR_RECON: Record<number, CrReconState> = {};

function crReconBtn(r: CloudResource): string {
  if (!crFailed(r) && !CR_RECON[r.id]) return "";
  const busy = !!CR_RECON[r.id]?.busy;
  return `<button type="button" class="btn sm" data-crrecon="${r.id}"${busy ? " disabled" : ""}>${busy ? "Сверка…" : "Сверить с хостингом"}</button>`;
}
function crReconHTML(id: number): string {
  const hint = CR_RECON[id]?.hint;
  return hint ? `<div class="cr-check bad" role="status">${esc(hint)}</div>` : "";
}

function reconcileResource(r: CloudResource): void {
  CR_RECON[r.id] = { busy: true };
  renderResources(); renderCloudServers();
  api.reconcileCloudResource(r.id)
    .then((res) => {
      delete CR_RECON[r.id];
      showToast(res.resolved === "absent"
        ? "Хостинг подтвердил: сервера нет, запись удалена"
        : "Найден на хостинге: привязан (IP обновите опросом)");
      loadResources();
    })
    .catch((e: Error & { code?: string }) => {
      delete CR_RECON[r.id];
      if (e.code === "provider_reconcile_required")
        CR_RECON[r.id] = { hint: CR_RECON_HINT };
      else showToast(CR_REASON[e.code || ""] || e.message, true);
      renderResources(); renderCloudServers();
    });
}

function resButtons(r: CloudResource): string {
  const dis = r.state === "creating" || r.state === "deleting"
    ? ' disabled title="по ресурсу уже выполняется команда"' : "";
  if (crFailed(r)) return `<button type="button" class="btn sm" data-crreplace="${r.id}"${dis}>Заменить</button>
    <button type="button" class="btn sm danger" data-crdel="${r.id}"${dis}>Убрать запись</button>`;
  switch (r.state) {
    case "planned":
      return `<button type="button" class="btn sm danger" data-crdel="${r.id}">Убрать запись</button>`;
    case "creating":
      return `<button type="button" class="btn sm" disabled>Создаётся · ожидание IP…</button>`;
    case "deleting":
      return `<button type="button" class="btn sm" disabled>Удаляется…</button>`;
    case "deleted":
      return "";
    case "ready":
      if (!resReady(r)) return `<button type="button" class="btn sm" disabled>Ожидание IP…</button>`;
      return `<button type="button" class="btn sm" data-crcheck="${r.id}"${CR_CHECK[r.id]?.status === "checking" ? " disabled" : ""}>${CR_CHECK[r.id]?.status === "checking" ? "Проверка…" : "Проверить SSH"}</button>
        <button type="button" class="btn sm primary" data-crinstall="${r.id}"${dis}>Установить профиль</button>
        <button type="button" class="btn sm" data-crip="${r.id}"${dis}>Сменить IP</button>
        <button type="button" class="btn sm" data-crreplace="${r.id}"${dis}>Заменить</button>
        <button type="button" class="btn sm danger" data-crdel="${r.id}"${dis}>Удалить</button>`;
    default:
      return `<button type="button" class="btn sm danger" data-crdel="${r.id}"${dis}>Удалить</button>`;
  }
}

function resCardHTML(r: CloudResource): string {
  const m = crMetaFor(r);
  const ip = resIP(r);
  const reason = crReasonOf(r);
  const conn = HCONNS.find((c) => c.id === r.connection_id);
  const repBadge = r.replaces_resource_id ? `<span class="badge info">заменяет #${r.replaces_resource_id}</span>` : "";
  const byBadge = r.replaced_by_resource_id ? `<span class="badge">заменён на #${r.replaced_by_resource_id}</span>` : "";
  const nodeLink = r.node_id
    ? `<span>нода <a class="node-open mono" href="#/nodes/${encodeURIComponent(r.node_id)}">${esc(r.node_id)}</a></span>`
    : `<span class="dim">нода не создана</span>`;
  const hl = r.id === CRES_HIGHLIGHT ? " row-highlight" : "";
  const open = OPEN_RES.has(r.id);
  return `<article class="node-card cr${open ? " open" : ""}${hl}" role="listitem" data-res="${r.id}" aria-labelledby="cr-t-${r.id}">
    <div class="node-main">
      <div class="node-top">
        <span class="node-id" id="cr-t-${r.id}">${esc(r.label)}</span>
        <span class="badge mono">${esc(r.provider)}</span>
        <span class="badge ${m.cls}"><i></i>${esc(m.label)}</span>
        ${repBadge}${byBadge}
      </div>
      <div class="node-sub">
        ${ip ? `<span class="mono">${esc(ip)}</span>` : `<span class="dim">${crFailed(r) ? "IP не получен" : r.state === "deleted" ? "IP освобождён" : "ожидание IP…"}</span>`}
        ${nodeLink}
        <span class="dim">${conn ? esc(conn.label) : "подключение #" + r.connection_id}</span>
      </div>
      ${reason ? `<div class="node-problem" title="${esc(r.last_error || reason)}">${esc(reason)}</div>` : ""}
      ${RES_PROGRESS[r.id] ? `<div class="cr-progress">${RES_PROGRESS[r.id]}</div>` : ""}
      ${crCheckHTML(r.id)}${crReconHTML(r.id)}
    </div>
    <div class="node-side">${resButtons(r)}${crReconBtn(r)}
      <button type="button" class="btn sm ghost" data-crtoggle="${r.id}" aria-expanded="${open}" aria-controls="cr-more-${r.id}">${open ? "Свернуть" : "Детали"}</button></div>
    <div class="node-more" id="cr-more-${r.id}">
      <div class="kv"><span class="kv-k">ID</span><span class="kv-v mono">${r.id}</span></div>
      <div class="kv"><span class="kv-k">Ревизия</span><span class="kv-v mono">${r.revision}</span></div>
      <div class="kv"><span class="kv-k">Внешний ID</span><span class="kv-v mono wrap">${esc(r.external_id || "—")}</span></div>
      ${r.replaces_resource_id ? `<div class="kv"><span class="kv-k">Заменяет</span><span class="kv-v"><a class="mono" href="#/resources/${r.replaces_resource_id}">#${r.replaces_resource_id}</a></span></div>` : ""}
      ${r.replaced_by_resource_id ? `<div class="kv"><span class="kv-k">Заменён на</span><span class="kv-v"><a class="mono" href="#/resources/${r.replaced_by_resource_id}">#${r.replaced_by_resource_id}</a></span></div>` : ""}
    </div>
  </article>`;
}

/* Удалённые не показываем даже если бэкенд ещё отдаёт их в списке. */
function visibleCres(): CloudResource[] {
  return CRES.filter((r) => r.state !== "deleted");
}

function renderResources(poll = false): void {
  const body = $("cloud-res-list");
  renderSection(body, poll, () => {
    const items = visibleCres();
    $("cloud-res-hint").textContent = items.length
      ? `${items.length} ${plural(items.length, "ресурс", "ресурса", "ресурсов")}` : "";
    const note = $("cr-beget-note");
    const begetOnly = HCONNS.length > 0 && HCONNS.every((c) => c.provider === "beget");
    if (begetOnly) { note.innerHTML = BEGET_BANNER; note.hidden = false; }
    else { note.replaceChildren(); note.hidden = true; }
    if (CRES_ERR !== null) {
      body.innerHTML = `<div role="listitem"><div class="banner bad" role="alert"><span>Не удалось загрузить ресурсы: ${esc(CRES_ERR)}</span>
        <button class="btn sm" data-crreload="1" style="margin-left:auto">Повторить</button></div></div>`;
      return;
    }
    if (!items.length) {
      body.innerHTML = `<div class="empty-state" role="listitem">${EMPTY_SVG}
        <span class="t">Ресурсов пока нет</span>
        <span class="s">Создайте ресурс на Nodehost — после готовности установите профиль, и нода появится в пуле.</span></div>`;
      return;
    }
    body.innerHTML = items.map(resCardHTML).join("");
  });
}

/* ── «Ресурсы у провайдера» в разделе «Серверы» ──────────────────────── */
/* Те же карточки, что в «Хостинги → Ресурсы», но в одном списке со своими
   хостами; шаги установки берутся из операции по active_operation_id. */
function cloudServerButtons(r: CloudResource): string {
  if (crFailed(r)) return `<button type="button" class="btn sm" data-crreplace="${r.id}">Заменить</button>
    <button type="button" class="btn sm danger" data-crdel="${r.id}">Убрать запись</button>`;
  if (r.state === "creating") return `<button type="button" class="btn sm" disabled>Создаётся · ожидание IP…</button>`;
  if (r.state === "deleting") return `<button type="button" class="btn sm" disabled>Удаляется…</button>`;
  if (r.state === "deleted") return "";
  if (!resReady(r)) return `<button type="button" class="btn sm danger" data-crdel="${r.id}">Убрать запись</button>`;
  const checking = CR_CHECK[r.id]?.status === "checking";
  return `<button type="button" class="btn sm" data-crcheck="${r.id}"${checking ? " disabled" : ""}>${checking ? "Проверка…" : "Проверить SSH"}</button>
    <button type="button" class="btn sm primary" data-crinstall="${r.id}">Установить</button>
    <button type="button" class="btn sm" data-crip="${r.id}">Сменить IP</button>
    <button type="button" class="btn sm danger" data-crdel="${r.id}">Удалить</button>`;
}

function cloudServerCardHTML(r: CloudResource): string {
  const ip = resIP(r);
  const reason = crReasonOf(r);
  const op = r.active_operation_id != null ? OPS.find((o) => o.id === r.active_operation_id) : undefined;
  // Состояние установки видно по активной операции ресурса (как у своих серверов).
  let m = crMetaFor(r);
  if (op?.kind === "cloud_install") {
    if (op.state === "done") m = { label: "установлен", cls: "ok" };
    else if (op.state === "failed" || op.state === "unknown") m = { label: "ошибка установки", cls: "bad" };
    else if (op.state !== "deleted") m = { label: "устанавливается", cls: "info" };
  }
  const opMeta = op ? (OP_META[op.state] || { label: op.state, cls: "" }) : null;
  const opHtml = op && opMeta ? `<div class="srv-more">
      <div class="kv" style="gap:6px">
        <span class="kv-k">Операция #${op.id} · ${esc(op.kind)}</span>
        <span class="kv-v"><span class="badge ${opMeta.cls}"><i></i>${esc(opMeta.label)}</span></span>
      </div>
      ${op.last_error ? `<pre class="op-err srv-out">${esc(op.last_error)}</pre>` : ""}
      ${stepsHTML(op)}
    </div>` : "";
  return `<div class="node-card srv cr" role="listitem" data-cresid="${r.id}">
    <div class="node-main">
      <div class="node-top">
        <span class="node-id">${esc(r.label)}</span>
        <span class="badge mono">${esc(providerRu(r.provider))}</span>
        <span class="badge ${m.cls}"><i></i>${esc(m.label)}</span>
        ${r.node_id ? `<span class="badge">нода ${esc(r.node_id)}</span>` : ""}
      </div>
      <div class="node-sub">
        ${ip ? `<span class="mono">${esc(ip)}</span>` : `<span class="dim">${crFailed(r) ? "IP не получен" : "ожидание IP…"}</span>`}
        <span class="dim">${esc(r.external_id || "—")}</span>
        ${r.node_id ? `<a class="node-open" href="#/nodes/${encodeURIComponent(r.node_id)}">открыть ноду</a>` : ""}
      </div>
      ${reason ? `<div class="node-problem" title="${esc(r.last_error || reason)}">${esc(reason)}</div>` : ""}
      ${crCheckHTML(r.id)}${crReconHTML(r.id)}
    </div>
    <div class="node-side">${cloudServerButtons(r)}${crReconBtn(r)}</div>
    ${opHtml}
  </div>`;
}

function renderCloudServers(poll = false): void {
  const body = $("srv-cloud-list");
  if (!body) return;
  renderSection(body, poll, () => {
    const hint = $("srv-cloud-hint");
    const items = visibleCres();
    if (hint) hint.textContent = items.length
      ? `${items.length} ${plural(items.length, "ресурс", "ресурса", "ресурсов")}` : "";
    if (CRES_ERR !== null) {
      body.innerHTML = `<div role="listitem"><div class="banner bad" role="alert"><span>Не удалось загрузить ресурсы: ${esc(CRES_ERR)}</span>
        <button class="btn sm" data-crreload="1" style="margin-left:auto">Повторить</button></div></div>`;
      return;
    }
    if (!items.length) {
      body.innerHTML = `<div class="empty-state" role="listitem">${EMPTY_SVG}
        <span class="t">Ресурсов у провайдера нет</span>
        <span class="s">Создайте сервер у Nodehost в разделе «Хостинги → Ресурсы» — он появится здесь с прогрессом установки.</span></div>`;
      return;
    }
    body.innerHTML = items.map(cloudServerCardHTML).join("");
  });
}

function applyResourceHighlight(): void {
  if (!CRES_HIGHLIGHT) return;
  const el = document.querySelector<HTMLElement>(`[data-res="${CRES_HIGHLIGHT}"]`);
  const id = CRES_HIGHLIGHT;
  el?.scrollIntoView({ block: "center" });
  setTimeout(() => { if (CRES_HIGHLIGHT === id) { CRES_HIGHLIGHT = 0; renderResources(); } }, 2500);
}

function loadResources(poll = false): void {
  api.cloudResources().then((r) => {
    CRES = r.resources || [];
    CRES_ERR = null; CRES_RETRY = false;
    renderResources(poll);
    renderCloudServers(poll);
    applyResourceHighlight();
  }).catch((e: Error & { status?: number; code?: string }) => {
    if (e.message === "unauthorized") return;
    CRES_ERR = e.message;
    renderResources(poll);
    renderCloudServers(poll);
    if (!CRES_RETRY && isRetryableLoad(e)) { CRES_RETRY = true; setTimeout(() => loadResources(), 2000); }
  });
}

/* ── создание ресурса ────────────────────────────────────────────────── */
/* Каталог/оценка/профили формы. FROZEN DTO в api.ts: catalog?location=
   отдаёт network только вместе с локацией, поэтому смена локации перезапрашивает
   каталог. CR_CAT_SEQ гасит устаревшие ответы (быстрая смена локации). */
let CR_CATALOG: HostingCatalog | null = null;
let CR_CATALOG_ERR: string | null = null;
let CR_CAT_SEQ = 0;
let CR_EST_TIMER = 0;
let CR_EST_SEQ = 0;
let CR_PROFILES: MyProfiles | null = null;
let CR_PROF_ID = 0;
let CR_EST_LAST: EstimateResult | null = null;
const CR_STEPS = ["Создание", "Установка", "Готово"];

type CrBatchState = "pending" | "creating" | "ready" | "installing" | "done" | "error";
interface CrBatchItem { name: string; state: CrBatchState; resId?: number; reason?: string; }
interface CrBatchSpec {
  connID: number; count: number; name: string; hostname: string;
  conf: Omit<NodehostConfiguration, "name" | "hostname">;
  wantsInstall: boolean; profileID: number; vars: Record<string, string>;
}

function toggleCrForm(open: boolean): void {
  const card = $("cr-form-card") as HTMLElement;
  card.hidden = !open;
  $("cr-add").setAttribute("aria-expanded", String(open));
  if (open) {
    fillCrConnections();
    CR_IDEM = newIdemKey();
    CR_PROF_ID = 0;
    renderCrSteps(-1);
    ($("cr-err") as HTMLElement).hidden = true;
    ($("cr-estimate") as HTMLElement).hidden = true;
    ($("cr-batch") as HTMLElement).hidden = true;
    ($("cr-batch") as HTMLElement).replaceChildren();
    CR_EST_LAST = null;
    ($("cr-count") as HTMLInputElement).value = "1";
    loadCrCatalog("");
    loadCrProfiles();
    ($("cr-name") as HTMLInputElement).focus();
  } else {
    // Фокус не оставляем на скрытом submit — возвращаем на инициатора.
    window.clearTimeout(CR_EST_TIMER);
    renderCrSteps(-1);
    resetCrSubmitBtn();
    $("cr-add").focus();
  }
}

function resetCrSubmitBtn(): void {
  const btn = $("cr-submit") as HTMLButtonElement;
  btn.disabled = !CR_CATALOG; // каталог не загружен — создавать нельзя
  btn.textContent = "Создать ресурс";
}

function renderCrSteps(active: number): void {
  const el = $("cr-steps");
  if (active < 0) { el.hidden = true; el.replaceChildren(); return; }
  el.hidden = false;
  el.innerHTML = CR_STEPS.map((s, i) =>
    `<span class="wizard-step${i === active ? " active" : i < active ? " done" : ""}" role="listitem">
      <span class="n">${i < active ? "✓" : i + 1}</span>${esc(s)}</span>`).join("");
}

function fillCrConnections(): void {
  const sel = $("cr-conn") as HTMLSelectElement;
  const eligible = HCONNS.filter((c) => c.provider === "nodehost" && c.capabilities.create && !!c.verified_at);
  if (!eligible.length) {
    sel.innerHTML = `<option value="">Нет подходящих подключений</option>`;
    return;
  }
  sel.innerHTML = eligible.map((c) => `<option value="${c.id}">${esc(c.label)}</option>`).join("");
}

function crValue(id: string): string {
  return ($(id) as HTMLInputElement | null)?.value.trim() || "";
}

function setCrControlsEnabled(on: boolean): void {
  for (const id of ["cr-loc", "cr-os", "cr-cpu", "cr-cores", "cr-ram", "cr-disk", "cr-disktype"]) {
    ($(id) as HTMLInputElement).disabled = !on;
  }
}

function crClamp(id: string): void {
  const el = $(id) as HTMLInputElement;
  const v = Number(el.value);
  if (el.value === "" || !Number.isFinite(v)) return;
  const min = Number(el.min), max = Number(el.max);
  if (Number.isFinite(min) && v < min) el.value = String(min);
  else if (Number.isFinite(max) && v > max) el.value = String(max);
}

function loadCrCatalog(location: string): void {
  const connID = Number(($("cr-conn") as HTMLSelectElement).value) || 0;
  const state = $("cr-catalog-state");
  if (!connID) {
    CR_CATALOG = null; CR_CATALOG_ERR = "Нет подходящих подключений";
    state.innerHTML = `<div class="banner warn" style="margin:0"><span>Нет проверенных подключений Nodehost с поддержкой создания. Добавьте подключение выше.</span></div>`;
    setCrControlsEnabled(false);
    ($("cr-submit") as HTMLButtonElement).disabled = true;
    return;
  }
  const seq = ++CR_CAT_SEQ;
  if (!location) { CR_CATALOG = null; CR_CATALOG_ERR = null; setCrControlsEnabled(false); }
  else {
    // CPU приходит только для выбранной локации (бэкенд фильтрует каталог).
    const cpuSel = $("cr-cpu") as HTMLSelectElement;
    cpuSel.disabled = true;
    cpuSel.innerHTML = `<option value="">Загрузка…</option>`;
    ($("cr-cpu-help") as HTMLElement).textContent = "Загрузка CPU для локации…";
    ($("cr-net") as HTMLSelectElement).disabled = true;
  }
  state.hidden = false;
  state.innerHTML = `<div class="skeleton skel-line"></div>`;
  ($("cr-submit") as HTMLButtonElement).disabled = true;
  api.hostingCatalog(connID, location || undefined).then((cat) => {
    if (seq !== CR_CAT_SEQ) return;
    CR_CATALOG = cat;
    CR_CATALOG_ERR = null;
    applyCrCatalog(cat, location);
  }).catch((e: Error & { code?: string }) => {
    if (seq !== CR_CAT_SEQ) return;
    if (e.code === "not_implemented") { state.innerHTML = BEGET_BANNER; return; }
    CR_CATALOG_ERR = e.message;
    state.innerHTML = `<div class="banner bad" role="alert" style="margin:0"><span>Каталог недоступен: ${esc(e.message)}</span>
      <button type="button" class="btn sm" id="cr-cat-retry" style="margin-left:auto">Повторить</button></div>`;
    $("cr-cat-retry").addEventListener("click", () => loadCrCatalog(location));
  });
}

function fillSelect(id: string, items: { value: string; label: string }[], placeholder: string): void {
  const sel = $(id) as HTMLSelectElement;
  sel.innerHTML = items.length
    ? items.map((i) => `<option value="${esc(i.value)}">${esc(i.label)}</option>`).join("")
    : `<option value="">${esc(placeholder)}</option>`;
}

function applyCrCatalog(cat: HostingCatalog, location: string): void {
  const state = $("cr-catalog-state");
  state.hidden = true;
  if (cat.limits?.types && cat.limits.types.VPS === false) {
    state.hidden = false;
    state.innerHTML = `<div class="banner warn" style="margin:0"><span>Провайдер не отдаёт тип VPS для этого подключения — создание недоступно.</span></div>`;
    setCrControlsEnabled(false);
    ($("cr-submit") as HTMLButtonElement).disabled = true;
    return;
  }
  if (!location) {
    // Первый заход: каталог без location. Локации + лимиты + прочие списки;
    // сразу перезапрашиваем с первой локацией ради network.speeds.
    fillSelect("cr-loc", (cat.locations || []).map((l) => ({ value: l.code, label: `${l.code} · ${l.name}` })), "Локации недоступны");
    const locSel = $("cr-loc") as HTMLSelectElement;
    locSel.disabled = false;
    const lim = cat.limits;
    for (const [numId, helpId, l] of [
      ["cr-cores", "cr-cores-help", lim?.cores],
      ["cr-ram", "cr-ram-help", lim?.ram_gb],
      ["cr-disk", "cr-disk-help", lim?.disk_gb],
    ] as const) {
      const el = $(numId) as HTMLInputElement;
      if (l) { el.min = String(l.min); el.max = String(l.max); ($(helpId) as HTMLElement).textContent = `доступно ${l.min}–${l.max}`; }
    }
    fillSelect("cr-os", (cat.os || []).map((o) => ({ value: o.id, label: o.name })), "ОС недоступны");
    fillSelect("cr-disktype", (cat.disk_types || []).map((d) => ({ value: d.id, label: d.name })), "Типы дисков недоступны");
    setCrControlsEnabled(true);
    // CPU выбирается только после локации: каталог без location не фильтрован.
    const cpuSel = $("cr-cpu") as HTMLSelectElement;
    cpuSel.disabled = true;
    cpuSel.innerHTML = `<option value="">Сначала выберите локацию</option>`;
    ($("cr-cpu-help") as HTMLElement).textContent = "Выберите локацию.";
    ($("cr-net") as HTMLSelectElement).disabled = true;
    resetCrSubmitBtn();
    const first = locSel.value;
    if (first) loadCrCatalog(first);
    else { state.hidden = false; state.innerHTML = `<div class="banner warn" style="margin:0"><span>Каталог пуст: локации не получены. Повторите позже.</span></div>`; }
    return;
  }
  // Перезагрузка с локацией: CPU (фильтрованный бэкендом) + скорости сети.
  const cpuSel = $("cr-cpu") as HTMLSelectElement;
  const cpuHelp = $("cr-cpu-help") as HTMLElement;
  const models = cat.cpu_models || [];
  fillSelect("cr-cpu", models.map((m) => ({ value: m, label: m })), "CPU для локации недоступны");
  cpuSel.disabled = models.length === 0;
  cpuHelp.textContent = models.length
    ? "CPU, доступные для выбранной локации."
    : "Для выбранной локации CPU не найдены.";
  const netSel = $("cr-net") as HTMLSelectElement;
  const net = cat.network;
  if (net && net.available && net.speeds?.length) {
    fillSelect("cr-net", net.speeds.map((s) => ({ value: s, label: s })), "Скорости недоступны");
    if (net.speeds.length === 1) netSel.value = net.speeds[0];
    netSel.disabled = false;
  } else {
    netSel.innerHTML = `<option value="">Скорость не настраивается</option>`;
    netSel.disabled = true;
  }
  resetCrSubmitBtn();
  // Смена региона всегда освежает цену — даже если ответ каталога пришёл поздно.
  scheduleCrEstimate();
}

/* Debounce-оценка: любой ввод конфигурации → (600 мс) → estimate. Никогда не
   блокирует создание: 501 скрывает баннер, 502/504 — «цена недоступна». */
function scheduleCrEstimate(): void {
  window.clearTimeout(CR_EST_TIMER);
  CR_EST_TIMER = window.setTimeout(runCrEstimate, 600);
}

function crConfigForEstimate(): { cores: number; ram: number; disk: number; location: string } | null {
  // Оценка не ждёт каталога: при медленной/упавшей перезагрузке региона
  // цена всё равно пересчитывается по текущим полям формы.
  const cores = Number(crValue("cr-cores")) || 0;
  const ram = Number(crValue("cr-ram")) || 0;
  const disk = Number(crValue("cr-disk")) || 0;
  const location = crValue("cr-loc");
  if (!cores || !ram || !disk || !location) return null;
  return { cores, ram, disk, location };
}

function crCount(): number {
  const n = Math.floor(Number(crValue("cr-count")) || 1);
  return Math.min(20, Math.max(1, n));
}

/* Node ID = NAME-HASH; NAME — 1..10 latin/digits/._-, без пунктуации по краям. */
const CR_NAME_RE = /^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,8})?[A-Za-z0-9]$/;

/* Пачка добавляет -NN (2 цифры к номеру, 3 символа с дефисом). */
function crBatchNameMax(count: number): number {
  if (count <= 1) return 10;
  return count <= 9 ? 7 : 6;
}

function crNameError(name: string, count: number): string | null {
  if (!name) return "Укажите имя ресурса.";
  if (!CR_NAME_RE.test(name)) {
    return "Имя: 1–10 символов, латиница/цифры/._-, без . _ - по краям.";
  }
  const max = crBatchNameMax(count);
  if (name.length > max) {
    return `Для ${count} ресурсов имя должно быть не длиннее ${max} символов (суффикс -01…-NN).`;
  }
  return null;
}

function renderCrNameError(): void {
  const el = $("cr-name-err");
  const err = crNameError(crValue("cr-name"), crCount());
  el.textContent = err || "";
  el.hidden = !err;
  ($("cr-name") as HTMLInputElement).setAttribute("aria-invalid", String(!!err));
}

/* Оценка сервера — за 1 шт. При количестве >1 умножаем на клиенте и показываем
   «за 1 шт. × N». Ошибка цены никогда не блокирует создание. */
function renderCrEstimate(): void {
  const box = $("cr-estimate");
  const last = CR_EST_LAST;
  if (!last) { box.hidden = true; return; }
  const n = crCount();
  const region = crValue("cr-loc");
  const regionTail = region ? ` · регион ${region}` : "";
  if (n > 1) {
    const per = Number(last.amount);
    const total = Number.isFinite(per) ? (per * n).toFixed(2) : `${last.amount} × ${n}`;
    box.textContent = `≈ ${last.amount} ${last.currency} за 1 шт. · ×${n} = ${total} ${last.currency} за ${last.period} (${last.period_days} дн.)${regionTail}`;
  } else {
    box.textContent = `≈ ${last.amount} ${last.currency} за ${last.period} (${last.period_days} дн.)${regionTail}`;
  }
  box.className = "banner info";
  box.hidden = false;
}

function runCrEstimate(): void {
  const box = $("cr-estimate");
  const cfg = crConfigForEstimate();
  if (!cfg) { CR_EST_LAST = null; box.hidden = true; return; }
  const connID = Number(($("cr-conn") as HTMLSelectElement).value) || 0;
  const seq = ++CR_EST_SEQ;
  const networkSpeed = crValue("cr-net");
  const period = crValue("cr-period");
  api.estimateCloudResource({
    connection_id: connID,
    configuration: {
      ...cfg,
      ...(networkSpeed ? { networkSpeed } : {}),
      ...(period ? { period } : {}),
    },
  }).then((r) => {
    if (seq !== CR_EST_SEQ) return;
    CR_EST_LAST = r;
    renderCrEstimate();
  }).catch((e: Error & { status?: number; code?: string }) => {
    if (seq !== CR_EST_SEQ) return;
    CR_EST_LAST = null;
    if (e.code === "not_implemented" || e.status === 501) { box.hidden = true; return; }
    if (e.status === 502 || e.status === 504) {
      box.className = "banner warn";
      box.textContent = "Цена недоступна (провайдер не ответил). Создание ресурса это не блокирует.";
      box.hidden = false;
      return;
    }
    box.hidden = true;
  });
}

/* Профили установки: published + свои текстовые (format приходит в профиле). */
function loadCrProfiles(): void {
  const sel = $("cr-prof") as HTMLSelectElement;
  const help = $("cr-prof-help");
  textProfiles().then((m) => {
    CR_PROFILES = m;
    const pub = m.published || [];
    const own = (m.profiles || []).filter((p) => !pub.some((x) => x.id === p.id));
    if (!pub.length && !own.length) {
      sel.innerHTML = `<option value="">— без профиля —</option>`;
      help.innerHTML = `Текстовых профилей нет — создайте их в разделе <a href="#/profiles">Мои профили</a>. Установку можно запустить и позже, на карточке ресурса.`;
      return;
    }
    const opts = [`<option value="">— без профиля —</option>`];
    for (const p of pub) opts.push(`<option value="${p.id}">${esc(p.name)}</option>`);
    for (const p of own) opts.push(`<option value="${p.id}">${esc(p.name)} (мой)</option>`);
    sel.innerHTML = opts.join("");
    help.textContent = "Текстовые профили: опубликованные и ваши.";
  }).catch((e: Error) => {
    help.textContent = "Не удалось загрузить профили: " + e.message;
  });
}

function textProfileByID(id: number): Profile | undefined {
  return [...(CR_PROFILES?.published || []), ...(CR_PROFILES?.profiles || [])].find((p) => p.id === id);
}

function onCrProfileChange(): void {
  scheduleCrEstimate();
  const sel = $("cr-prof") as HTMLSelectElement;
  const instant = $("cr-instant") as HTMLInputElement;
  const wrap = $("cr-inst-vars-wrap");
  CR_PROF_ID = Number(sel.value) || 0;
  if (!CR_PROF_ID) {
    instant.checked = false;
    instant.disabled = true;
    wrap.hidden = true;
    return;
  }
  instant.disabled = false;
  instant.checked = true;
  const p = textProfileByID(CR_PROF_ID);
  mountVars("cr-inst-vars", parseVarsSchema(p?.vars_schema));
  wrap.hidden = false;
}

/* ── пачка ресурсов (количество > 1) ─────────────────────────────────── */
/* Строго последовательно: rate limits провайдера. Свой Idempotency-Key на
   каждый create; сбой элемента не останавливает пачку. */

function shortCrErr(e: Error & { code?: string; status?: number }): string {
  if (e.code === "quota_exceeded") return "квота исчерпана";
  if (e.code === "resource_busy") return "ресурс занят";
  if (e.code === "idempotency_conflict") return "конфликт повтора";
  if (e.code === "not_implemented") return "не поддерживается";
  if (e.status === 429) return "лимит запросов";
  return (e.message || "ошибка").slice(0, 120);
}

const CR_BATCH_LABEL: Record<CrBatchState, string> = {
  pending: "ожидание", creating: "создаётся · ожидание IP…", ready: "готов",
  installing: "установка", done: "готово", error: "ошибка",
};
const CR_BATCH_CLS: Record<CrBatchState, string> = {
  pending: "", creating: "info", ready: "info",
  installing: "info", done: "ok", error: "bad",
};

function renderCrBatch(items: CrBatchItem[]): void {
  const box = $("cr-batch") as HTMLElement;
  box.hidden = false;
  const created = items.filter((i) => i.resId != null).length;
  const errors = items.filter((i) => i.state === "error").length;
  box.innerHTML = `<div class="cr-batch-head"><span>Создано ${created}, ошибок ${errors}</span></div>
    <ol class="cr-batch-list">${items.map((i) =>
      // Причина важнее фазы: не оставляем «ожидание IP», если она уже есть.
      `<li><span class="badge ${i.reason && i.state !== "error" ? "bad" : CR_BATCH_CLS[i.state]}">${i.reason && i.state !== "error" ? "ошибка" : CR_BATCH_LABEL[i.state]}</span>
        <span class="nm">${esc(i.name)}</span>
        ${i.reason ? `<span class="why">${esc(i.reason)}</span>` : ""}</li>`).join("")}</ol>`;
}

function waitResourceReady(resId: number): Promise<{ ok: boolean; reason?: string }> {
  const deadline = Date.now() + 3 * 60 * 1000;
  return new Promise((resolve) => {
    const tick = (): void => {
      api.cloudResource(resId).then((r) => {
        const i = CRES.findIndex((x) => x.id === r.id);
        if (i >= 0) CRES[i] = r; else CRES.push(r);
        renderResources(true);
        if (resReady(r)) { resolve({ ok: true }); return; }
        if (r.state === "error" || r.state === "deleted") {
          resolve({ ok: false, reason: crReasonText(r.last_error) || (r.state === "deleted" ? "ресурс удалён" : "ресурс в ошибке") });
          return;
        }
        if (Date.now() > deadline) { resolve({ ok: false, reason: "таймаут готовности" }); return; }
        setTimeout(tick, 3000);
      }).catch((e: Error) => {
        if (Date.now() > deadline) resolve({ ok: false, reason: shortCrErr(e) });
        else setTimeout(tick, 3000);
      });
    };
    setTimeout(tick, 3000);
  });
}

function waitInstallDone(opId: number): Promise<{ ok: boolean; reason?: string }> {
  const deadline = Date.now() + 5 * 60 * 1000;
  return new Promise((resolve) => {
    const tick = (): void => {
      api.operations().then((r) => {
        const op = (r.operations || []).find((o) => o.id === opId);
        if (op?.state === "done") { resolve({ ok: true }); return; }
        if (op && (op.state === "failed" || op.state === "unknown")) {
          resolve({ ok: false, reason: op.last_error || "ошибка установки" }); return;
        }
        if (Date.now() > deadline) { resolve({ ok: false, reason: "таймаут установки" }); return; }
        setTimeout(tick, 3000);
      }).catch(() => {
        if (Date.now() > deadline) resolve({ ok: false, reason: "ошибка опроса установки" });
        else setTimeout(tick, 3000);
      });
    };
    setTimeout(tick, 3000);
  });
}

async function runCrBatch(spec: CrBatchSpec): Promise<void> {
  const btn = $("cr-submit") as HTMLButtonElement;
  const n = spec.count;
  const items: CrBatchItem[] = Array.from({ length: n }, (_, i) => ({
    name: `${spec.name}-${String(i + 1).padStart(2, "0")}`,
    state: "pending",
  }));
  renderCrBatch(items);
  btn.disabled = true; btn.textContent = "Создание…";
  renderCrSteps(0);
  for (let i = 0; i < items.length; i++) {
    const item = items[i];
    const suffix = `-${String(i + 1).padStart(2, "0")}`;
    item.state = "creating";
    renderCrBatch(items);
    const configuration: NodehostConfiguration = {
      ...spec.conf, name: item.name,
      ...(spec.hostname ? { hostname: `${spec.hostname}${suffix}` } : {}),
    };
    try {
      const ack = await api.createCloudResource(
        { connection_id: spec.connID, label: item.name, configuration, management: { mode: "manual" } },
        newIdemKey(),
      );
      if (ack.resource_id == null) {
        item.state = "error"; item.reason = "нет id ресурса";
      } else {
        item.resId = ack.resource_id;
        RES_PROGRESS[ack.resource_id] =
          `<div class="banner info" style="margin:0"><span>Ресурс ${esc(item.name)}: создаётся · ожидание IP…</span></div>`;
        renderResources();
        renderCloudServers();
        const rr = await waitResourceReady(ack.resource_id);
        delete RES_PROGRESS[ack.resource_id];
        if (rr.ok) item.state = "ready";
        else { item.state = "error"; item.reason = rr.reason || "не готов"; }
      }
    } catch (e) {
      item.state = "error";
      item.reason = shortCrErr(e as Error & { code?: string; status?: number });
    }
    renderCrBatch(items);
    loadResources();
  }
  if (spec.wantsInstall && spec.profileID) {
    for (const item of items) {
      if (item.state !== "ready" || item.resId == null) continue;
      item.state = "installing"; renderCrBatch(items);
      try {
        const op = await api.installCloudResource(item.resId, newIdemKey(), { profile_id: spec.profileID, vars: spec.vars });
        const ir = await waitInstallDone(op.id);
        if (ir.ok) item.state = "done";
        else { item.state = "error"; item.reason = `создан; установка: ${ir.reason || "ошибка"}`; }
      } catch (e) {
        const err = e as Error & { code?: string };
        item.state = "error";
        item.reason = `создан; установка: ${CLOUD_INSTALL_ERR[err.code || ""] || shortCrErr(err)}`;
      }
      renderCrBatch(items);
      loadResources();
    }
  }
  for (const item of items) if (item.state === "ready") item.state = "done";
  renderCrBatch(items);
  renderCrSteps(-1);
  const created = items.filter((i) => i.resId != null).length;
  const errors = items.filter((i) => i.state === "error").length;
  btn.textContent = errors ? `Создано ${created}, ошибок ${errors}` : `Создано ${created}`;
  btn.disabled = true;
  showToast(errors ? `Создано ${created}, ошибок ${errors} — причины в списке` : `Создано ${created}`, errors > 0);
  announce(`Пачка ресурсов: создано ${created}, ошибок ${errors}`);
  loadResources();
  loadConnections();
}

function submitCreateResource(e: Event): void {
  e.preventDefault();
  const errBox = $("cr-err");
  errBox.hidden = true;
  const showErr = (m: string, focusId?: string): void => {
    errBox.textContent = m; errBox.hidden = false;
    if (focusId) ($(focusId) as HTMLElement | null)?.focus();
  };
  const connID = Number(($("cr-conn") as HTMLSelectElement).value) || 0;
  if (!connID) { showErr("Выберите подключение Nodehost с поддержкой создания."); return; }
  if (!CR_CATALOG) { showErr("Дождитесь загрузки каталога."); return; }
  const name = crValue("cr-name");
  const count = crCount();
  const nameErr = crNameError(name, count);
  if (nameErr) { renderCrNameError(); showErr(nameErr, "cr-name"); return; }
  const cpu = crValue("cr-cpu");
  const cores = Number(crValue("cr-cores")) || 0;
  const ram = Number(crValue("cr-ram")) || 0;
  const disk = Number(crValue("cr-disk")) || 0;
  const diskType = crValue("cr-disktype");
  const os = crValue("cr-os");
  const location = crValue("cr-loc");
  const checks: [boolean, string, string][] = [
    [!name, "cr-name", "Укажите имя ресурса."],
    [!location, "cr-loc", "Укажите локацию."],
    [!os, "cr-os", "Укажите ОС."],
    [!cpu, "cr-cpu", "Укажите CPU."],
    [!cores, "cr-cores", "Укажите число ядер."],
    [!ram, "cr-ram", "Укажите объём ОЗУ."],
    [!disk, "cr-disk", "Укажите объём диска."],
    [!diskType, "cr-disktype", "Укажите тип диска."],
  ];
  for (const [bad, id, msg] of checks) {
    if (bad) { showErr(msg, id); return; }
  }
  // Мгновенная установка: профиль выбран, переменные валидны.
  const instant = $("cr-instant") as HTMLInputElement;
  const wantsInstall = CR_PROF_ID > 0 && instant.checked && !instant.disabled;
  if (wantsInstall && !CR_PROF_ID) { showErr("Выберите профиль установки."); return; }
  let vars: Record<string, string> = {};
  if (wantsInstall) {
    const ed = document.querySelector<HTMLElement>("#cr-inst-vars .vars-editor");
    const varsErr = ed ? validateVarsEditor(ed) : null;
    if (varsErr) { showErr(varsErr); return; }
    vars = ed ? collectInstallVars(ed) : {};
  }
  const networkSpeed = crValue("cr-net");
  const conf: Omit<NodehostConfiguration, "name" | "hostname"> = {
    cpu, cores, ram, disk, diskType, os, location,
    period: crValue("cr-period"),
    // Пустая скорость не отправляется вовсе — иначе бэкенд её валидирует.
    ...(networkSpeed ? { networkSpeed } : {}),
  };
  const hostname = crValue("cr-hostname");
  const btn = $("cr-submit") as HTMLButtonElement;
  if (count > 1) {
    // Последовательная пачка: по одному create на элемент с собственным ключом.
    void runCrBatch({ connID, count, name, hostname, conf, wantsInstall, profileID: CR_PROF_ID, vars });
    return;
  }
  const configuration: NodehostConfiguration = {
    ...conf, name,
    ...(hostname ? { hostname } : {}),
  };
  if (!CR_IDEM) CR_IDEM = newIdemKey();
  btn.disabled = true; btn.textContent = "Создание…";
  renderCrSteps(0);
  api.createCloudResource({ connection_id: connID, label: name, configuration, management: { mode: "manual" } }, CR_IDEM)
    .then((ack) => {
      announce(`Ресурс ${name}: создаётся · ожидание IP…`);
      CR_IDEM = "";
      if (ack.resource_id == null) { toggleCrForm(false); loadResources(); loadConnections(); return; }
      const resId: number = ack.resource_id; // сужение не живёт внутри колбэка
      RES_PROGRESS[resId] =
        `<div class="banner info" style="margin:0"><span>Ресурс ${esc(name)}: создаётся · ожидание IP… операция #${ack.operation_id}</span></div>`;
      if (wantsInstall) {
        // Форма остаётся открытой со шагами Создание → Установка → Готово.
        const pid = CR_PROF_ID;
        const rows = vars;
        renderCrSteps(1);
        pollResourceState(resId, ack.operation_id, name, "ready", "", () => {
          RES_PROGRESS[resId] =
            `<div class="banner info" style="margin:0"><span>Ресурс ${esc(name)} готов — установка профиля…</span></div>`;
          renderResources();
          installCreatedResource(resId, ack.operation_id, name, pid, rows);
        });
      } else {
        toggleCrForm(false);
        pollResourceState(resId, ack.operation_id, name, "ready");
      }
      loadResources();
      loadConnections();
    })
    .catch((e: Error & { code?: string; status?: number }) => {
      resetCrSubmitBtn();
      renderCrSteps(-1);
      if (e.code === "idempotency_conflict") {
        CR_IDEM = newIdemKey();
        showErr("Запрос с этим ключом уже был с другим телом. Обновите страницу и повторите.");
      } else if (e.code === "resource_busy") {
        showErr("По ресурсу уже выполняется команда.");
      } else if (e.code === "quota_exceeded") {
        showErr("Квота исчерпана: освободите место или увеличьте лимит.");
      } else if (e.code === "not_implemented") {
        errBox.innerHTML = BEGET_BANNER;
        errBox.hidden = false;
      } else {
        showErr(e.message);
      }
    });
}

/* Установка сразу после создания: ошибки установки показываются на карточке
   и НЕ ломают созданный ресурс (повтор — «Установить профиль» на карточке). */
function installCreatedResource(resId: number, _opId: number, label: string, profileID: number, vars: Record<string, string>): void {
  api.installCloudResource(resId, newIdemKey(), { profile_id: profileID, vars })
    .then((op) => {
      announce(`Ресурс ${label}: установка профиля запущена`);
      pollInstallOp(resId, op.id, label);
    })
    .catch((e: Error & { code?: string }) => {
      const msg = CLOUD_INSTALL_ERR[e.code || ""] || e.message;
      RES_PROGRESS[resId] =
        `<div class="banner bad" style="margin:0"><span>Ресурс ${esc(label)} создан, но установка не запустилась: ${esc(msg)}. Повтор — «Установить профиль» на карточке.</span></div>`;
      renderCrSteps(-1);
      renderResources();
      showToast(msg, true);
    });
}

function pollInstallOp(resId: number, opId: number, label: string): void {
  const deadline = Date.now() + 5 * 60 * 1000;
  const stop = (): void => { delete RES_PROGRESS[resId]; renderResources(); loadResources(); };
  const tick = (): void => {
    api.operations().then((r) => {
      const op = (r.operations || []).find((o) => o.id === opId);
      if (!op) {
        if (Date.now() > deadline) { renderCrSteps(-1); RES_PROGRESS[resId] = `<div class="banner info" style="margin:0"><span>Установка продолжается — статус в «Операциях».</span></div>`; renderResources(); }
        else setTimeout(tick, 3000);
        return;
      }
      if (op.state === "done") {
        renderCrSteps(3);
        showToast("Установка профиля завершена");
        announce(`Ресурс ${label}: установка завершена`);
        stop();
        return;
      }
      if (op.state === "failed" || op.state === "unknown") {
        renderCrSteps(-1);
        RES_PROGRESS[resId] =
          `<div class="banner bad" style="margin:0"><span>Ресурс ${esc(label)} создан, но установка не удалась: ${esc(op.last_error || "ошибка установки")}. Повтор — «Установить профиль».</span></div>`;
        renderResources();
        loadResources();
        return;
      }
      RES_PROGRESS[resId] = `<div class="banner info" style="margin:0"><span>Ресурс ${esc(label)}: установка профиля… (${esc(op.state)})</span></div>`;
      renderResources(true);
      if (Date.now() > deadline) { renderCrSteps(-1); RES_PROGRESS[resId] = `<div class="banner info" style="margin:0"><span>Установка продолжается — статус в «Операциях».</span></div>`; renderResources(); return; }
      setTimeout(tick, 3000);
    }).catch(() => { if (Date.now() <= deadline) setTimeout(tick, 3000); else { renderCrSteps(-1); stop(); } });
  };
  setTimeout(tick, 3000);
}

/* Опрос состояния ресурса после 202: «принято» ≠ «готово». Пока не ready/
   deleted/error — статус виден прогрессом, не ложным успехом (spec §9/§11). */
function pollResourceState(resId: number, opId: number, label: string, until: "ready" | "deleted" | "ip", fromIP = "", onReady?: () => void): void {
  const deadline = Date.now() + 3 * 60 * 1000;
  const tick = (): void => {
    api.cloudResource(resId).then((r) => {
      const i = CRES.findIndex((x) => x.id === r.id);
      if (i >= 0) CRES[i] = r; else CRES.push(r);
      const ip = resIP(r);
      const done = (until === "deleted" && r.state === "deleted")
        || (until === "ready" && resReady(r))
        || (until === "ip" && !!ip && ip !== fromIP);
      if (!done && (r.state === "creating" || (r.state === "ready" && !ip))) {
        RES_PROGRESS[r.id] =
          `<div class="banner info" style="margin:0"><span>Ресурс ${esc(label)}: создаётся · ожидание IP…</span></div>`;
      }
      renderResources(!done);
      renderCloudServers(!done);
      if (done) {
        delete RES_PROGRESS[r.id];
        // Хук мгновенной установки: успевает занять RES_PROGRESS до перерисовки.
        onReady?.();
        if (until === "ready") { showToast("Ресурс готов"); announce(`Ресурс ${label}: готов`); loadConnections(); }
        else if (until === "deleted") announce(`Ресурс ${label}: удалён`);
        else { showToast("IP изменён"); announce(`Ресурс ${label}: IP изменён`); }
        loadResources();
        return;
      }
      if (r.state === "error") {
        delete RES_PROGRESS[r.id];
        showToast(`Ресурс ${label}: ${crReasonText(r.last_error) || "ошибка"}`, true);
        renderResources(); loadResources();
        return;
      }
      if (Date.now() > deadline) {
        announce(`Ресурс ${label}: операция #${opId} — статус по-прежнему виден в разделе «Ресурсы»`);
        return;
      }
      setTimeout(tick, 3000);
    }).catch(() => { if (Date.now() <= deadline) setTimeout(tick, 3000); });
  };
  setTimeout(tick, 3000);
}

/* ── удаление ресурса ────────────────────────────────────────────────── */
function confirmDeleteRes(r: CloudResource): void {
  const removeOnly = r.state === "planned" || r.state === "error";
  openModal(removeOnly ? `Убрать запись «${r.label}»?` : `Удалить ресурс «${r.label}»?`,
    removeOnly
      ? "Сервера у провайдера нет — запись будет удалена без платного вызова и без влияния на ноды."
      : "Ресурс будет отмечен на удаление у провайдера; освобождение места ждёт подтверждения. Бан или ошибка установки удаление не запускают автоматически.",
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: removeOnly ? "Убрать" : "Удалить", cls: "danger", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        const lbl = btn.textContent;
        btn.disabled = true; btn.textContent = removeOnly ? "Удаление записи…" : "Удаление…";
        api.deleteCloudResource(r.id, newIdemKey()).then((ack) => {
          closeModal();
          showToast("Удаление запущено");
          announce(`Ресурс ${r.label}: удаляется…`);
          loadResources();
          pollResourceState(r.id, ack.operation_id, r.label, "deleted");
        }).catch((e: Error & { code?: string; status?: number }) => {
          btn.disabled = false; btn.textContent = lbl;
          if (e.code === "provider_reconcile_required") {
            // Исход удаления неясен: вместо тупикового тоста — кнопка сверки.
            closeModal();
            CR_RECON[r.id] = { hint: CR_RECON_HINT };
            renderResources(); renderCloudServers();
            return;
          }
          const msg = e.code === "confirmation_required" ? "Подтвердите действие."
            : e.code === "idempotency_key_required" ? "Повторите: не сгенерирован ключ запроса."
            : e.code === "resource_busy" ? "По ресурсу уже выполняется команда."
            : e.status === 404 ? "Ресурс не найден"
            : e.code === "not_implemented" ? "Действие пока не поддерживается на этом провайдере."
            : e.message;
          closeModal();
          showToast(msg, true);
          loadResources();
        });
      } },
    ]);
}

/* ── смена IP ────────────────────────────────────────────────────────── */
function changeIP(r: CloudResource): void {
  // ponytail: ip-options (501/404) не запрашиваем — маршрута нет, свободный
  // number-input и есть честное degraded-состояние. Add when endpoint lands.
  openModal(`Сменить IP · ${r.label}`,
    `<div class="field">
      <label for="cr-subnet">Подсеть (необязательно)</label>
      <input id="cr-subnet" type="number" inputmode="numeric" min="0">
      <span class="help">Оставьте пустым для адреса провайдера по умолчанию. Смена IP не создаёт новый сервер.</span>
    </div>
    <div class="banner bad" id="cr-ip-err" role="alert" hidden style="margin:12px 0 0"></div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Сменить IP", cls: "primary", onClick: (ev) => doChangeIP(ev.currentTarget as HTMLButtonElement, r) },
    ]);
}

function doChangeIP(btn: HTMLButtonElement, r: CloudResource): void {
  const sub = Number(($("cr-subnet") as HTMLInputElement | null)?.value) || 0;
  const lbl = btn.textContent;
  btn.disabled = true; btn.textContent = "Смена…";
  api.changeCloudIP(r.id, newIdemKey(), sub || undefined).then((res) => {
    closeModal();
    if ("operation_id" in res) {
      showToast("Смена IP запущена");
      pollResourceState(r.id, res.operation_id, r.label, "ip", resIP(r));
      return;
    }
    const i = CRES.findIndex((x) => x.id === res.id);
    if (i >= 0) CRES[i] = res; else CRES.push(res);
    renderResources();
    showToast("IP изменён");
    announce(`Ресурс ${r.label}: IP изменён`);
  }).catch((e: Error & { code?: string }) => {
    btn.disabled = false; btn.textContent = lbl;
    const msg = e.code === "resource_busy" ? "По ресурсу уже выполняется команда."
      : e.code === "not_implemented" ? "Действие пока не поддерживается на этом провайдере."
      : e.message;
    const b = $("cr-ip-err");
    if (b && document.body.contains(b)) { b.textContent = msg; b.hidden = false; }
    else showToast(msg, true);
  });
}

/* ── установка профиля на ресурс ─────────────────────────────────────── */
const CLOUD_INSTALL_ERR: Record<string, string> = {
  not_ready: "Сервер ещё не готов (нет RUNNING). Установка начнётся после готовности.",
  ssh_unavailable: "Нет доступа по SSH (пароль root недоступен). Пересоздайте ресурс.",
  in_progress: "Для этого ресурса уже выполняется команда.",
  not_implemented: "Действие пока не поддерживается на этом провайдере.",
  quota_exceeded: "Квота исчерпана: освободите место или увеличьте лимит.",
  user_disabled: "Учётная запись отключена.",
  owner_mismatch: "Личность ноды занята или не подтверждена этой операцией.",
  idempotency_key_required: "Повторите: не сгенерирован ключ запроса.",
  validation_failed: "Некорректные переменные установки.",
  profile_unavailable: "Профиль недоступен для установки: он не опубликован.",
};

/* Общий рендер редактора переменных в произвольный контейнер (используется
   и в установке своих серверов, и в установке на облачный ресурс). */
function mountVars(containerId: string, decls: ProfileVar[], snapshot?: ProfileVar[], onChange?: (rows: ProfileVar[]) => void): void {
  const wrap = $(containerId);
  if (!wrap) return;
  wrap.replaceChildren();
  const ed = document.createElement("div");
  ed.className = "vars-editor";
  wrap.appendChild(ed);
  renderVarsEditor(ed, decls, { install: true, defaults: loadInstallDefaults(), snapshot, onChange });
}

function openInstallRes(r: CloudResource): void {
  openModal(`Установка · ${r.label}`,
    `<div class="banner info" style="margin:0 0 12px"><span>Установка профиля на готовый сервер <span class="mono">${esc(resIP(r) || "—")}</span>. Новый сервер не создаётся и не оплачивается.</span></div>
     <div class="field">
       <label for="cr-install-prof">Профиль установки</label>
       <select id="cr-install-prof" disabled><option value="">Загрузка профиля…</option></select>
       <span class="help" id="cr-install-status" aria-live="polite">Загрузка профилей…</span>
     </div>
     <div class="field" style="gap:6px;margin-top:12px">
       <label>Переменные установки</label>
       <div id="cr-install-vars"><div class="vars-editor"></div></div>
       <span class="help">${esc(PROFILE_VARS_HELP)}</span>
     </div>
     <div class="banner bad" id="cr-install-err" role="alert" hidden style="margin:12px 0 0"></div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Установить", cls: "primary", onClick: (ev) => doInstallRes(ev.currentTarget as HTMLButtonElement, r) },
    ], true);
  mountVars("cr-install-vars", [], undefined, () => {});
  const sel = $("cr-install-prof") as HTMLSelectElement;
  textProfiles().then((m) => {
    if (!document.body.contains(sel)) return;
    const opts: string[] = [];
    for (const p of m.published || []) opts.push(`<option value="${p.id}">${esc(p.name)}</option>`);
    for (const p of m.profiles || [])
      if (!(m.published || []).some((x) => x.id === p.id)) opts.push(`<option value="${p.id}">${esc(p.name)} (мой)</option>`);
    sel.innerHTML = opts.join("");
    sel.disabled = false;
    sel.dataset.ready = String(opts.length > 0);
    $("cr-install-status").textContent = opts.length ? "Только подтверждённые текстовые профили; смена сбрасывает переменные установки." : "Нет подтверждённых текстовых профилей. Сделайте текстовую копию published по известному номеру в «Мои профили».";
    sel.addEventListener("change", () => {
      mountVars("cr-install-vars", installProfileDecls(m, Number(sel.value) || 0));
    });
    mountVars("cr-install-vars", installProfileDecls(m, Number(sel.value) || 0));
  }).catch(() => {
    const st = $("cr-install-status");
    if (st) st.textContent = "Не удалось загрузить профили.";
  });
}

function doInstallRes(btn: HTMLButtonElement, r: CloudResource): void {
  const sel = $("cr-install-prof") as HTMLSelectElement | null;
  const box = $("cr-install-err");
  if (!sel || sel.dataset.ready !== "true") {
    if (box) { box.textContent = "Дождитесь загрузки профилей."; box.hidden = false; }
    return;
  }
  const ed = document.querySelector<HTMLElement>("#cr-install-vars .vars-editor");
  const nameErr = ed ? validateVarsEditor(ed) : null;
  if (nameErr) { if (box) { box.textContent = nameErr; box.hidden = false; } return; }
  const vars = ed ? collectInstallVars(ed) : {};
  const pid = Number(sel.value) || 0;
  const lbl = btn.textContent;
  btn.disabled = true; btn.textContent = "Запуск…";
  api.installCloudResource(r.id, newIdemKey(), { profile_id: pid, vars }).then(() => {
    closeModal();
    showToast("Установка запущена");
    announce(`Ресурс ${r.label}: установка запущена`);
    loadResources();
    refresh().catch(() => {});
  }).catch((e: Error & { code?: string }) => {
    btn.disabled = false; btn.textContent = lbl;
    const msg = CLOUD_INSTALL_ERR[e.code || ""] || e.message;
    if (box && document.body.contains(box)) { box.textContent = msg; box.hidden = false; }
    else showToast(msg, true);
  });
}

/* ── замена ресурса ──────────────────────────────────────────────────── */
function configFieldsHTML(prefix: string, labelVal: string): string {
  const f = (id: string, label: string, ph = "", cls = ""): string =>
    `<div class="field"><label for="${prefix}-${id}">${esc(label)}</label>
      <input id="${prefix}-${id}" class="${cls}" autocomplete="off" placeholder="${esc(ph)}"></div>`;
  return `<div class="srv-grid">
      <div class="field"><label for="${prefix}-name">Имя ресурса</label>
        <input id="${prefix}-name" class="mono" autocomplete="off" maxlength="100" value="${esc(labelVal)}" required></div>
      ${f("hostname", "Hostname", "", "mono")}
      ${f("cpu", "CPU", "AMD Ryzen 9 7950X", "mono")}
      <div class="field"><label for="${prefix}-cores">Ядра</label><input id="${prefix}-cores" type="number" min="1" inputmode="numeric"></div>
      <div class="field"><label for="${prefix}-ram">ОЗУ, ГБ</label><input id="${prefix}-ram" type="number" min="1" inputmode="numeric"></div>
      <div class="field"><label for="${prefix}-disk">Диск, ГБ</label><input id="${prefix}-disk" type="number" min="1" inputmode="numeric"></div>
      ${f("disktype", "Тип диска", "SSD NVMe", "mono")}
      ${f("os", "ОС", "ubuntu-24.04", "mono")}
      ${f("loc", "Локация", "DE", "mono")}
      ${f("period", "Период", "1m", "mono")}
      ${f("net", "Скорость сети", "1gbps", "mono")}
    </div>`;
}

function openReplaceRes(r: CloudResource): void {
  openModal(`Заменить ресурс «${r.label}»?`,
    `<div class="banner warn" style="margin:0 0 12px"><span>Замена создаёт <b>новый сервер</b> у провайдера — это платный заказ, который вы подтверждаете явно. Старый ресурс не переиспользуется; удалить его можно отдельно.</span></div>
     ${configFieldsHTML("crr", r.label)}
     <div class="banner bad" id="cr-replace-err" role="alert" hidden style="margin:12px 0 0"></div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Заменить", cls: "primary", onClick: (ev) => doReplace(ev.currentTarget as HTMLButtonElement, r) },
    ], true);
}

function doReplace(btn: HTMLButtonElement, r: CloudResource): void {
  const box = $("cr-replace-err");
  const showErr = (m: string): void => { if (box) { box.textContent = m; box.hidden = false; } };
  const val = (id: string): string => ($(`crr-${id}`) as HTMLInputElement | null)?.value.trim() || "";
  const name = val("name"), cpu = val("cpu");
  const cores = Number(val("cores")) || 0, ram = Number(val("ram")) || 0, disk = Number(val("disk")) || 0;
  const diskType = val("disktype"), os = val("os"), location = val("loc");
  if (!name || !cpu || !cores || !ram || !disk || !diskType || !os || !location) {
    showErr("Заполните обязательные параметры конфигурации.");
    return;
  }
  const configuration: NodehostConfiguration = {
    name, hostname: val("hostname") || undefined, cpu, cores, ram, disk, diskType, os, location,
    period: val("period"), networkSpeed: val("net"),
  };
  const body: CloudReplaceBody = {
    connection_id: r.connection_id, label: name, configuration,
    confirm: true, expected_revision: r.revision,
  };
  const lbl = btn.textContent;
  btn.disabled = true; btn.textContent = "Замена…";
  api.replaceCloudResource(r.id, newIdemKey(), body).then((ack) => {
    closeModal();
    showToast("Замена запущена");
    announce(`Ресурс ${name}: замена запущена`);
    if (ack.resource_id != null) {
      RES_PROGRESS[ack.resource_id] =
        `<div class="banner info" style="margin:0"><span>Ресурс ${esc(name)}: создаётся · ожидание IP… операция #${ack.operation_id}</span></div>`;
      pollResourceState(ack.resource_id, ack.operation_id, name, "ready");
    }
    loadResources();
  }).catch((e: Error & { code?: string }) => {
    btn.disabled = false; btn.textContent = lbl;
    if (e.code === "revision_conflict") showErr("Состояние изменилось; обновите данные и повторите.");
    else if (e.code === "resource_busy") showErr("По ресурсу уже выполняется команда.");
    else if (e.code === "not_implemented") { if (box) { box.innerHTML = BEGET_BANNER; box.hidden = false; } }
    else showErr(e.message);
  });
}

/* ── события хостингов (делегирование на document) ───────────────────── */
document.addEventListener("click", (e) => {
  const t = e.target as HTMLElement;
  const htab = t.closest<HTMLElement>("#hosting-tabs [data-htab]");
  if (htab) { activateHostingTab(htab.dataset.htab || "conns", true); return; }
  if (t.closest("[data-hcreload]")) { loadConnections(); return; }
  if (t.closest("[data-crreload]")) { loadResources(); return; }
  const toggle = t.closest<HTMLElement>("[data-hctoggle]");
  if (toggle) {
    const id = Number(toggle.dataset.hctoggle);
    if (OPEN_CONNS.has(id)) OPEN_CONNS.delete(id); else OPEN_CONNS.add(id);
    renderConnections();
    return;
  }
  const rtoggle = t.closest<HTMLElement>("[data-crtoggle]");
  if (rtoggle) {
    const id = Number(rtoggle.dataset.crtoggle);
    if (OPEN_RES.has(id)) OPEN_RES.delete(id); else OPEN_RES.add(id);
    renderResources();
    return;
  }
  const bal = t.closest<HTMLButtonElement>("[data-hcbalance]");
  if (bal) { const c = HCONNS.find((x) => x.id === Number(bal.dataset.hcbalance)); if (c) refreshBalance(bal, c); return; }
  const ver = t.closest<HTMLElement>("[data-hcverify]");
  if (ver) { const c = HCONNS.find((x) => x.id === Number(ver.dataset.hcverify)); if (c) verifyConn(c); return; }
  const edit = t.closest<HTMLElement>("[data-hcedit]");
  if (edit) { const c = HCONNS.find((x) => x.id === Number(edit.dataset.hcedit)); if (c) connEditor(c); return; }
  const del = t.closest<HTMLElement>("[data-hcdel]");
  if (del) { const c = HCONNS.find((x) => x.id === Number(del.dataset.hcdel)); if (c) confirmDeleteConn(c); return; }
  // Точки входа со страницы ноды (node-detail рисует data-res-action).
  const ra = t.closest<HTMLElement>("[data-res-action]");
  if (ra) { openResourceAction(ra.dataset.resAction || ""); return; }
  const crchk = t.closest<HTMLElement>("[data-crcheck]");
  if (crchk) { const r = CRES.find((x) => x.id === Number(crchk.dataset.crcheck)); if (r) checkResourceSSH(r); return; }
  const crrecon = t.closest<HTMLElement>("[data-crrecon]");
  if (crrecon) { const r = CRES.find((x) => x.id === Number(crrecon.dataset.crrecon)); if (r) reconcileResource(r); return; }
  const install = t.closest<HTMLElement>("[data-crinstall]");
  if (install) { const r = CRES.find((x) => x.id === Number(install.dataset.crinstall)); if (r) openInstallRes(r); return; }
  const cip = t.closest<HTMLElement>("[data-crip]");
  if (cip) { const r = CRES.find((x) => x.id === Number(cip.dataset.crip)); if (r) changeIP(r); return; }
  const repl = t.closest<HTMLElement>("[data-crreplace]");
  if (repl) { const r = CRES.find((x) => x.id === Number(repl.dataset.crreplace)); if (r) openReplaceRes(r); return; }
  const crdel = t.closest<HTMLElement>("[data-crdel]");
  if (crdel) { const r = CRES.find((x) => x.id === Number(crdel.dataset.crdel)); if (r) confirmDeleteRes(r); return; }
});
document.addEventListener("keydown", (e) => {
  const tabs = Array.from(document.querySelectorAll<HTMLButtonElement>("#hosting-tabs [role='tab']"));
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
  activateHostingTab(tabs[next].dataset.htab || "conns", false);
});

/* Действия со страницы ноды: ресурс ищем по node_id (при необходимости —
   догружаем список), затем открываем тот же модал, что в разделе «Ресурсы». */
function openResourceAction(action: string): void {
  const nodeId = routeNodeId();
  const dispatch = (): void => {
    const r = CRES.find((x) => x.node_id === nodeId) || null;
    if (!r) { showToast("Ресурс этой ноды не найден — обновите данные.", true); loadResources(); return; }
    if (action === "install") openInstallRes(r);
    else if (action === "change-ip") changeIP(r);
    else if (action === "replace") openReplaceRes(r);
    else if (action === "delete") confirmDeleteRes(r);
  };
  if (!CRES.length) api.cloudResources().then((res) => { CRES = res.resources || []; dispatch(); })
    .catch((e: Error) => showToast(e.message, true));
  else dispatch();
}

/* Инициализация хостингов (формы, кнопки). Вызывается при старте. */
function initHosting(): void {
  ($("cr-form") as HTMLFormElement).addEventListener("submit", submitCreateResource);
  $("cr-add").addEventListener("click", () => toggleCrForm(!!($("cr-form-card") as HTMLElement).hidden));
  $("cr-cancel").addEventListener("click", () => toggleCrForm(false));
  $("hc-add").addEventListener("click", () => connEditor(null));
  $("cloud-reload").addEventListener("click", () => loadResources());
  $("srv-cloud-reload").addEventListener("click", () => loadResources());
  // Каталог зависит от подключения: смена — перезагрузка каталога и списков.
  ($("cr-conn") as HTMLSelectElement).addEventListener("change", () => { loadCrCatalog(""); scheduleCrEstimate(); });
  ($("cr-loc") as HTMLSelectElement).addEventListener("change", () => loadCrCatalog(crValue("cr-loc")));
  // Клиентский clamp + оценка: любой ввод конфигурации.
  for (const id of ["cr-cores", "cr-ram", "cr-disk"]) {
    ($(id) as HTMLInputElement).addEventListener("input", () => { crClamp(id); scheduleCrEstimate(); });
  }
  for (const id of ["cr-loc", "cr-os", "cr-cpu", "cr-disktype", "cr-period", "cr-net"]) {
    ($(id) as HTMLSelectElement).addEventListener("change", scheduleCrEstimate);
  }
  ($("cr-name") as HTMLInputElement).addEventListener("input", renderCrNameError);
  // Количество: только клиентский clamp + пересчёт «×N» из последней оценки.
  const countEl = $("cr-count") as HTMLInputElement;
  countEl.addEventListener("input", () => {
    const v = Number(countEl.value);
    if (countEl.value !== "" && (!Number.isFinite(v) || v < 1)) countEl.value = "1";
    else if (Number.isFinite(v) && v > 20) countEl.value = "20";
    renderCrEstimate();
    renderCrNameError();
  });
  ($("cr-prof") as HTMLSelectElement).addEventListener("change", onCrProfileChange);
}

/* ═══ Автопилот: N нод одним платным заказом ═══════════════════════════
   docs/specs/hosting-blueprints/fleet-autopilot.md. Один POST /api/me/fleets,
   затем GET статуса раз в 5 с (single-flight). В localStorage — только id
   последнего флота, чтобы после перезагрузки продолжить просмотр. */
const AP_STORAGE_KEY = "sharedd.autopilot.fleet";

const AP_STATE_META: Record<FleetState, { label: string; cls: string }> = {
  running:   { label: "выполняется", cls: "info" },
  succeeded: { label: "готово", cls: "ok" },
  partial:   { label: "частично", cls: "warn" },
  failed:    { label: "ошибка", cls: "bad" },
};

const AP_ITEM_META: Record<string, { label: string; cls: string }> = {
  pending:    { label: "ожидает", cls: "" },
  creating:   { label: "создаётся", cls: "info" },
  installing: { label: "установка", cls: "info" },
  admitted:   { label: "в пуле", cls: "ok" },
  error:      { label: "ошибка", cls: "bad" },
};

/* last_error — безопасный машинный код; неизвестное не выдумываем. */
const AP_REASON: Record<string, string> = {
  insufficient_funds: "Недостаточно средств",
  provider_timeout: "Провайдер не ответил вовремя",
  provider_error: "Ошибка провайдера",
  quota_exceeded: "Квота исчерпана",
  node_not_registered: "Нода не зарегистрировалась",
  system_graph_unavailable: "Системная политика замены недоступна",
  validation_failed: "Некорректные параметры",
  forbidden: "Доступ запрещён провайдером",
  not_ready: "Сервер ещё не готов",
  no_ip: "Провайдер не выдал IP",
  ssh_unavailable: "SSH недоступен",
  credentials_unavailable: "Учётные данные недоступны",
  not_implemented: "Провайдер не поддерживает действие",
  resource_busy: "Сервис занят, повторите позже",
  provider_reconcile_required: "Платный исход неясен — проверьте ресурсы у провайдера",
  profile_unavailable: "Профиль недоступен для установки",
  install_failed: "Ошибка установки профиля",
};

let AP_CATALOG: HostingCatalog | null = null;
let AP_CAT_SEQ = 0;
let AP_EST_TIMER = 0;
let AP_EST_SEQ = 0;
let AP_EST_LAST: EstimateResult | null = null;
let AP_FLEET: Fleet | null = null;
let AP_POLL_ID = 0;
let AP_POLL_TIMER = 0;
let AP_POLL_BUSY = false;
let AP_POLL_ACTIVE = false;
let AP_SUBMITTING = false;
let AP_LOADED = false;
const AP_SETTLED = new Set<number>();

function apValue(id: string): string {
  return ($(id) as HTMLInputElement | null)?.value.trim() || "";
}

function apCount(): number {
  const n = Math.floor(Number(apValue("ap-count")) || 1);
  return Math.min(20, Math.max(1, n));
}

function apEligibleConnections(): HostingConnection[] {
  return (HCONNS || []).filter((c) =>
    c.provider === "nodehost" && c.capabilities.create && !!c.verified_at);
}

function apSetControlsEnabled(on: boolean): void {
  for (const id of ["ap-loc", "ap-os", "ap-cores", "ap-ram", "ap-disk", "ap-disktype"]) {
    const el = $(id) as HTMLInputElement | null;
    if (el) el.disabled = !on;
  }
  if (!on) {
    const cpu = $("ap-cpu") as HTMLSelectElement | null;
    const net = $("ap-net") as HTMLSelectElement | null;
    if (cpu) cpu.disabled = true;
    if (net) net.disabled = true;
  }
}

function apUpdateSubmit(): void {
  const btn = $("ap-submit") as HTMLButtonElement | null;
  if (!btn) return;
  const running = AP_POLL_ACTIVE && AP_FLEET?.state === "running";
  btn.disabled = AP_SUBMITTING || running;
  btn.setAttribute("aria-busy", String(AP_SUBMITTING));
  const n = apCount();
  btn.textContent = AP_SUBMITTING ? "Отправка…" : running
    ? "Выполняется…"
    : `Развернуть ${n} ${plural(n, "ноду", "ноды", "нод")}`;
}

function apShowError(msg: string, focusId?: string): void {
  const box = $("ap-err");
  box.textContent = msg;
  box.hidden = false;
  if (focusId) ($(focusId) as HTMLElement | null)?.focus();
}

function apHideError(): void {
  const box = $("ap-err");
  box.hidden = true;
  box.replaceChildren();
}

function apStoreFleetId(id: number): void {
  try { localStorage.setItem(AP_STORAGE_KEY, String(id)); } catch { /* private mode */ }
}

function apReadFleetId(): number {
  try {
    const raw = localStorage.getItem(AP_STORAGE_KEY) || "";
    return positiveID(raw);
  } catch { return 0; }
}

function apClearFleetId(): void {
  try { localStorage.removeItem(AP_STORAGE_KEY); } catch { /* private mode */ }
}

/* ── подключение и каталог ───────────────────────────────────────────── */
function apLoadConnections(): void {
  const state = $("ap-catalog-state");
  const sel = $("ap-conn") as HTMLSelectElement;
  api.hostingConnections().then((r) => {
    HCONNS = r.connections || [];
    const eligible = apEligibleConnections();
    const prev = Number(sel.value) || 0;
    if (!eligible.length) {
      sel.innerHTML = `<option value="">Нет подходящих подключений</option>`;
      state.innerHTML = `<div class="banner warn" style="margin:0"><span>Нет проверенных подключений Nodehost с поддержкой создания. Добавьте и проверьте подключение в разделе «Хостинги».</span></div>`;
      AP_CATALOG = null;
      AP_EST_LAST = null;
      ($("ap-estimate") as HTMLElement).hidden = true;
      apSetControlsEnabled(false);
      apUpdateSubmit();
      return;
    }
    sel.innerHTML = eligible.map((c) => `<option value="${c.id}">${esc(c.label)}</option>`).join("");
    if (eligible.some((c) => c.id === prev)) sel.value = String(prev);
    apLoadCatalog("");
  }).catch((e: Error & { status?: number; code?: string }) => {
    if (e.message === "unauthorized") return;
    AP_CATALOG = null;
    apSetControlsEnabled(false);
    state.innerHTML = `<div class="banner bad" role="alert" style="margin:0"><span>Не удалось загрузить подключения: ${esc(e.message)}</span></div>`;
  });
}

function apLoadCatalog(location: string): void {
  const connID = Number(($("ap-conn") as HTMLSelectElement)?.value) || 0;
  const state = $("ap-catalog-state");
  const seq = ++AP_CAT_SEQ;
  if (!connID) {
    AP_CATALOG = null;
    AP_EST_LAST = null;
    ($("ap-estimate") as HTMLElement).hidden = true;
    apSetControlsEnabled(false);
    state.innerHTML = `<div class="banner warn" style="margin:0"><span>Выберите проверенное подключение Nodehost.</span></div>`;
    return;
  }
  if (!location) {
    AP_CATALOG = null;
    AP_EST_LAST = null;
    ($("ap-estimate") as HTMLElement).hidden = true;
    apSetControlsEnabled(false);
  } else {
    const cpu = $("ap-cpu") as HTMLSelectElement;
    cpu.disabled = true;
    cpu.innerHTML = `<option value="">Загрузка…</option>`;
    ($("ap-cpu-help") as HTMLElement).textContent = "Загрузка CPU для локации…";
    ($("ap-net") as HTMLSelectElement).disabled = true;
  }
  state.hidden = false;
  state.innerHTML = `<div class="skeleton skel-line"></div>`;
  api.hostingCatalog(connID, location || undefined).then((cat) => {
    if (seq !== AP_CAT_SEQ) return;
    AP_CATALOG = cat;
    apApplyCatalog(cat, location);
  }).catch((e: Error & { code?: string }) => {
    if (seq !== AP_CAT_SEQ) return;
    AP_CATALOG = null;
    AP_EST_LAST = null;
    ($("ap-estimate") as HTMLElement).hidden = true;
    apSetControlsEnabled(false);
    state.innerHTML = e.code === "not_implemented"
      ? `<div class="banner warn" style="margin:0"><span>Провайдер не поддерживает каталог для автопилота.</span></div>`
      : `<div class="banner bad" role="alert" style="margin:0"><span>Каталог недоступен: ${esc(e.message)}</span></div>`;
  });
}

function apApplyCatalog(cat: HostingCatalog, location: string): void {
  const state = $("ap-catalog-state");
  state.hidden = true;
  if (cat.limits?.types && cat.limits.types.VPS === false) {
    state.hidden = false;
    state.innerHTML = `<div class="banner warn" style="margin:0"><span>Провайдер не отдаёт тип VPS для этого подключения — создание недоступно.</span></div>`;
    apSetControlsEnabled(false);
    return;
  }
  if (!location) {
    fillSelect("ap-loc", (cat.locations || []).map((l) => ({ value: l.code, label: `${l.code} · ${l.name}` })), "Локации недоступны");
    const locSel = $("ap-loc") as HTMLSelectElement;
    locSel.disabled = false;
    const lim = cat.limits;
    for (const [numId, helpId, l] of [
      ["ap-cores", "ap-cores-help", lim?.cores],
      ["ap-ram", "ap-ram-help", lim?.ram_gb],
      ["ap-disk", "ap-disk-help", lim?.disk_gb],
    ] as const) {
      const el = $(numId) as HTMLInputElement;
      if (l) {
        el.min = String(l.min);
        el.max = String(l.max);
        ($(helpId) as HTMLElement).textContent = `доступно ${l.min}–${l.max}`;
      }
    }
    fillSelect("ap-os", (cat.os || []).map((o) => ({ value: o.id, label: o.name })), "ОС недоступны");
    fillSelect("ap-disktype", (cat.disk_types || []).map((d) => ({ value: d.id, label: d.name })), "Типы дисков недоступны");
    apSetControlsEnabled(true);
    const cpuSel = $("ap-cpu") as HTMLSelectElement;
    cpuSel.disabled = true;
    cpuSel.innerHTML = `<option value="">Сначала выберите локацию</option>`;
    ($("ap-cpu-help") as HTMLElement).textContent = "Выберите локацию.";
    ($("ap-net") as HTMLSelectElement).disabled = true;
    const first = locSel.value;
    if (first) apLoadCatalog(first);
    else {
      state.hidden = false;
      state.innerHTML = `<div class="banner warn" style="margin:0"><span>Каталог пуст: локации не получены. Повторите позже.</span></div>`;
    }
    return;
  }
  const cpuSel = $("ap-cpu") as HTMLSelectElement;
  const prevCPU = cpuSel.value;
  const models = cat.cpu_models || [];
  fillSelect("ap-cpu", models.map((m) => ({ value: m, label: m })), "CPU для локации недоступны");
  if (models.includes(prevCPU)) cpuSel.value = prevCPU;
  cpuSel.disabled = models.length === 0;
  ($("ap-cpu-help") as HTMLElement).textContent = models.length
    ? "CPU, доступные для выбранной локации."
    : "Для выбранной локации CPU не найдены.";
  const netSel = $("ap-net") as HTMLSelectElement;
  const net = cat.network;
  if (net && net.available && net.speeds?.length) {
    const prevNet = netSel.value;
    netSel.innerHTML = [`<option value="">Не задано</option>`]
      .concat(net.speeds.map((s) => `<option value="${esc(s)}">${esc(s)}</option>`)).join("");
    if (net.speeds.includes(prevNet)) netSel.value = prevNet;
    netSel.disabled = false;
  } else {
    netSel.innerHTML = `<option value="">Не задано</option>`;
    netSel.disabled = true;
  }
  apScheduleEstimate();
}

/* ── цена ────────────────────────────────────────────────────────────── */
function apScheduleEstimate(): void {
  window.clearTimeout(AP_EST_TIMER);
  AP_EST_TIMER = window.setTimeout(apRunEstimate, 600);
}

function apConfigForEstimate(): { cores: number; ram: number; disk: number; location: string } | null {
  const cores = Number(apValue("ap-cores")) || 0;
  const ram = Number(apValue("ap-ram")) || 0;
  const disk = Number(apValue("ap-disk")) || 0;
  const location = apValue("ap-loc");
  if (!cores || !ram || !disk || !location) return null;
  return { cores, ram, disk, location };
}

function apRenderEstimate(): void {
  const box = $("ap-estimate");
  const last = AP_EST_LAST;
  if (!last) { box.hidden = true; return; }
  const n = apCount();
  const region = apValue("ap-loc");
  const tail = region ? ` · регион ${region}` : "";
  if (n > 1) {
    const per = Number(last.amount);
    const total = Number.isFinite(per) ? (per * n).toFixed(2) : `${last.amount} × ${n}`;
    box.textContent = `≈ ${last.amount} ${last.currency} за 1 шт. · ×${n} = ${total} ${last.currency} за ${last.period} (${last.period_days} дн.)${tail}`;
  } else {
    box.textContent = `≈ ${last.amount} ${last.currency} за ${last.period} (${last.period_days} дн.)${tail}`;
  }
  box.className = "banner info";
  box.hidden = false;
}

function apRunEstimate(): void {
  const box = $("ap-estimate");
  const cfg = apConfigForEstimate();
  const connID = Number(($("ap-conn") as HTMLSelectElement)?.value) || 0;
  if (!cfg || !connID) { AP_EST_LAST = null; box.hidden = true; return; }
  const seq = ++AP_EST_SEQ;
  const networkSpeed = apValue("ap-net");
  const period = apValue("ap-period");
  api.estimateCloudResource({
    connection_id: connID,
    configuration: {
      ...cfg,
      ...(networkSpeed ? { networkSpeed } : {}),
      ...(period ? { period } : {}),
    },
  }).then((r) => {
    if (seq !== AP_EST_SEQ) return;
    AP_EST_LAST = r;
    apRenderEstimate();
  }).catch((e: Error & { status?: number; code?: string }) => {
    if (seq !== AP_EST_SEQ) return;
    AP_EST_LAST = null;
    if (e.code === "not_implemented" || e.status === 501) { box.hidden = true; return; }
    if (e.status === 502 || e.status === 504) {
      box.className = "banner warn";
      box.textContent = "Цена недоступна (провайдер не ответил). Развёртывание это не блокирует.";
      box.hidden = false;
      return;
    }
    box.hidden = true;
  });
}

/* ── профиль установки ───────────────────────────────────────────────── */
function apLoadProfiles(): void {
  const sel = $("ap-prof") as HTMLSelectElement;
  const help = $("ap-prof-help");
  textProfiles().then((m) => {
    const pub = m.published || [];
    const own = (m.profiles || []).filter((p) => !pub.some((x) => x.id === p.id));
    if (!pub.length && !own.length) {
      sel.innerHTML = `<option value="">Нет текстовых профилей</option>`;
      help.innerHTML = `Текстовых профилей нет — создайте их в разделе <a href="#/profiles">Мои профили</a>.`;
      return;
    }
    const opts = [`<option value="">Выберите профиль…</option>`];
    for (const p of pub) opts.push(`<option value="${p.id}">${esc(p.name)}</option>`);
    for (const p of own) opts.push(`<option value="${p.id}">${esc(p.name)} (мой)</option>`);
    sel.innerHTML = opts.join("");
    help.textContent = "Текстовые профили: опубликованные и ваши.";
  }).catch((e: Error) => {
    help.textContent = "Не удалось загрузить профили: " + e.message;
  });
}

/* ── отправка ────────────────────────────────────────────────────────── */
function apSubmitErrorText(e: Error & { code?: string; status?: number }): string {
  if (e.code === "idempotency_conflict") return "Запрос уже был отправлен с другим телом. Повторите развёртывание.";
  if (e.code === "quota_exceeded") return "Квота исчерпана: освободите место или увеличьте лимит.";
  if (e.code === "insufficient_funds") return "Недостаточно средств на аккаунте провайдера.";
  if (e.code === "provider_timeout") return "Провайдер не ответил вовремя. Повторите позже.";
  if (e.code === "not_implemented") return "Провайдер не поддерживает автопилот.";
  if (e.code === "validation_failed") return "Проверьте параметры конфигурации.";
  if (e.status === 429) return "Слишком часто: подождите перед следующим заказом.";
  return e.message || "Не удалось запустить автопилот.";
}

async function apSubmit(e: Event): Promise<void> {
  e.preventDefault();
  if (AP_SUBMITTING) return;
  apHideError();
  const connID = Number(($("ap-conn") as HTMLSelectElement)?.value) || 0;
  if (!connID) { apShowError("Выберите проверенное подключение Nodehost.", "ap-conn"); return; }
  if (!AP_CATALOG) { apShowError("Дождитесь загрузки каталога.", "ap-conn"); return; }
  const location = apValue("ap-loc");
  const os = apValue("ap-os");
  const cpu = apValue("ap-cpu");
  const diskType = apValue("ap-disktype");
  const cores = Number(apValue("ap-cores")) || 0;
  const ram = Number(apValue("ap-ram")) || 0;
  const disk = Number(apValue("ap-disk")) || 0;
  const checks: [boolean, string, string][] = [
    [!!location, "ap-loc", "Выберите локацию."],
    [!!os, "ap-os", "Выберите ОС."],
    [!!cpu, "ap-cpu", "Выберите CPU."],
    [!!diskType, "ap-disktype", "Выберите тип диска."],
    [cores > 0, "ap-cores", "Укажите число ядер."],
    [ram > 0, "ap-ram", "Укажите объём ОЗУ."],
    [disk > 0, "ap-disk", "Укажите объём диска."],
  ];
  for (const [ok, id, msg] of checks) {
    if (!ok) { apShowError(msg, id); return; }
  }
  const lim = AP_CATALOG.limits;
  if (lim?.cores && (cores < lim.cores.min || cores > lim.cores.max)) {
    apShowError(`Ядра: допустимо ${lim.cores.min}–${lim.cores.max}.`, "ap-cores"); return;
  }
  if (lim?.ram_gb && (ram < lim.ram_gb.min || ram > lim.ram_gb.max)) {
    apShowError(`ОЗУ: допустимо ${lim.ram_gb.min}–${lim.ram_gb.max} ГБ.`, "ap-ram"); return;
  }
  if (lim?.disk_gb && (disk < lim.disk_gb.min || disk > lim.disk_gb.max)) {
    apShowError(`Диск: допустимо ${lim.disk_gb.min}–${lim.disk_gb.max} ГБ.`, "ap-disk"); return;
  }
  const pid = Number(($("ap-prof") as HTMLSelectElement)?.value) || 0;
  if (!pid) { apShowError("Выберите профиль установки.", "ap-prof"); return; }
  const count = apCount();
  const period = apValue("ap-period");
  const networkSpeed = apValue("ap-net");
  const body: FleetCreateBody = {
    connection_id: connID,
    name: "fleet",
    count,
    configuration: {
      name: "fleet",
      cpu, cores, ram, disk, diskType, os, location,
      ...(period ? { period } : {}),
      ...(networkSpeed ? { networkSpeed } : {}),
    },
    installation_profile_id: pid,
    auto_replace: ($("ap-auto") as HTMLInputElement).checked,
  };
  const key = newIdemKey();
  AP_SUBMITTING = true;
  apUpdateSubmit();
  try {
    const ack = await api.createFleet(body, key);
    AP_SUBMITTING = false;
    apStoreFleetId(ack.fleet_id);
    AP_FLEET = null;
    AP_SETTLED.delete(ack.fleet_id);
    const box = $("ap-fleet");
    box.hidden = true;
    box.replaceChildren();
    apStartPoll(ack.fleet_id);
    showToast(`Автопилот запущен: ${count} ${plural(count, "нода", "ноды", "нод")}`);
    announce(`Автопилот запущен: ${count}`);
  } catch (err) {
    AP_SUBMITTING = false;
    apShowError(apSubmitErrorText(err as Error & { code?: string; status?: number }));
  } finally {
    apUpdateSubmit();
  }
}

/* ── статус флота ────────────────────────────────────────────────────── */
function apReasonText(raw: string): string {
  const v = (raw || "").trim();
  if (!v) return "причина не указана";
  const code = v.split(/[\s:;]/)[0];
  return AP_REASON[code] || crReasonText(v) || v;
}

function apItemHTML(it: FleetItem): string {
  const meta = AP_ITEM_META[it.state] || { label: it.state, cls: "info" };
  const reason = it.state === "error" ? apReasonText(it.last_error) : "";
  const ip = it.ip
    ? `<span class="mono">${esc(it.ip)}</span>`
    : `<span class="dim">${it.state === "error" ? "IP не получен" : "IP ожидается"}</span>`;
  const node = it.node_id
    ? `<a class="node-open mono" href="#/nodes/${encodeURIComponent(it.node_id)}">${esc(it.node_id)}</a>`
    : it.resource_id != null
      ? `<span class="dim">ресурс #${it.resource_id}</span>`
      : `<span class="dim">ресурс ещё не создан</span>`;
  return `<div class="node-card ap-item" role="listitem">
    <div class="node-main">
      <div class="node-top">
        <span class="node-id mono">${esc(it.label)}</span>
        <span class="badge ${meta.cls}"><i></i>${esc(meta.label)}</span>
      </div>
      <div class="node-sub">${ip}${node}</div>
      ${reason ? `<div class="ap-reason">${esc(reason)}</div>` : ""}
    </div>
  </div>`;
}

function apSummary(f: Fleet): string {
  const items = f.items || [];
  const ok = items.filter((i) => i.state === "admitted").length;
  if (f.state === "succeeded") return `${ok} ${plural(ok, "нода", "ноды", "нод")} в пуле`;
  if (f.state === "partial") return `${ok} из ${f.count}, ошибки по строкам`;
  if (f.state === "failed") return `0 из ${f.count}, ошибки по строкам`;
  return `Готово ${ok} из ${f.count} · выполняется…`;
}

function apRenderFleet(f: Fleet): void {
  const box = $("ap-fleet");
  box.hidden = false;
  const meta = AP_STATE_META[f.state] || { label: f.state, cls: "info" };
  const created = f.created_at
    ? `создан ${new Date(f.created_at * 1000).toLocaleString("ru-RU")}`
    : "";
  const bannerCls = f.state === "succeeded" ? "info" : f.state === "partial" ? "warn" : f.state === "failed" ? "bad" : "info";
  box.innerHTML = `<div class="ap-fleet-head">
      <span class="badge ${meta.cls}"><i></i>${esc(meta.label)}</span>
      <span class="dim">Флот #${f.id}${created ? " · " + esc(created) : ""}</span>
    </div>
    <div class="banner ${bannerCls} ap-fleet-sum"><span>${esc(apSummary(f))}</span></div>
    <div class="node-list" role="list">${(f.items || []).map(apItemHTML).join("")}</div>`;
}

function apRenderFleetError(msg: string): void {
  const box = $("ap-fleet");
  box.hidden = false;
  box.innerHTML = `<div class="banner bad" role="alert"><span>Не удалось получить статус автопилота: ${esc(msg)}</span></div>`;
}

function apStopPoll(): void {
  AP_POLL_ACTIVE = false;
  window.clearTimeout(AP_POLL_TIMER);
  AP_POLL_TIMER = 0;
}

function apStartPoll(id: number): void {
  AP_POLL_ID = id;
  AP_POLL_ACTIVE = true;
  window.clearTimeout(AP_POLL_TIMER);
  AP_POLL_TIMER = 0;
  if (AP_POLL_BUSY) { AP_POLL_TIMER = window.setTimeout(apTick, 200); return; }
  apTick();
}

function apTick(): void {
  if (!AP_POLL_ACTIVE || !AP_POLL_ID || AP_POLL_BUSY) return;
  const id = AP_POLL_ID;
  AP_POLL_BUSY = true;
  api.fleet(id).then((f) => {
    if (!AP_POLL_ACTIVE || AP_POLL_ID !== id) return;
    AP_FLEET = f;
    apRenderFleet(f);
    if (f.state === "running") {
      AP_POLL_TIMER = window.setTimeout(apTick, 5000);
    } else {
      AP_POLL_ACTIVE = false;
      AP_POLL_TIMER = 0;
      apAfterTerminal(f);
    }
  }).catch((e: Error & { status?: number }) => {
    if (!AP_POLL_ACTIVE || AP_POLL_ID !== id) return;
    if (e.status === 404) {
      AP_POLL_ACTIVE = false;
      AP_POLL_ID = 0;
      AP_FLEET = null;
      apClearFleetId();
      apRenderFleetError("флот не найден");
      return;
    }
    apRenderFleetError(e.message);
    AP_POLL_TIMER = window.setTimeout(apTick, 5000);
  }).finally(() => {
    AP_POLL_BUSY = false;
    apUpdateSubmit();
  });
}

function apAfterTerminal(f: Fleet): void {
  if (AP_SETTLED.has(f.id)) { apUpdateSubmit(); return; }
  AP_SETTLED.add(f.id);
  const items = f.items || [];
  const ok = items.filter((i) => i.state === "admitted").length;
  if (f.state === "succeeded") {
    showToast(`Автопилот: ${ok} ${plural(ok, "нода", "ноды", "нод")} в пуле`);
  } else if (f.state === "partial") {
    showToast(`Автопилот: ${ok} из ${f.count}, ошибки по строкам`, true);
  } else {
    showToast(`Автопилот: 0 из ${f.count}, ошибки по строкам`, true);
  }
  announce(apSummary(f));
  refresh().catch(() => {});
  loadResources();
  apUpdateSubmit();
}

function apResumeLast(): void {
  const id = apReadFleetId();
  if (!id) return;
  if (AP_POLL_ACTIVE && AP_POLL_ID === id) return;
  apStartPoll(id);
}

function enterAutopilot(): void {
  if (!AP_LOADED) {
    AP_LOADED = true;
    apLoadProfiles();
    apLoadConnections();
  } else if (!AP_CATALOG) {
    // Прошлая загрузка не удалась или подключений не было — пробуем снова.
    apLoadConnections();
  }
  apResumeLast();
}

/* ── события формы ───────────────────────────────────────────────────── */
function initAutopilot(): void {
  ($("ap-form") as HTMLFormElement).addEventListener("submit", (e) => { void apSubmit(e); });
  ($("ap-conn") as HTMLSelectElement).addEventListener("change", () => apLoadCatalog(""));
  ($("ap-loc") as HTMLSelectElement).addEventListener("change", () => apLoadCatalog(apValue("ap-loc")));
  for (const id of ["ap-cores", "ap-ram", "ap-disk"]) {
    ($(id) as HTMLInputElement).addEventListener("input", () => { crClamp(id); apScheduleEstimate(); });
  }
  for (const id of ["ap-os", "ap-cpu", "ap-disktype", "ap-period", "ap-net"]) {
    ($(id) as HTMLSelectElement).addEventListener("change", apScheduleEstimate);
  }
  const countEl = $("ap-count") as HTMLInputElement;
  countEl.addEventListener("input", () => {
    const v = Number(countEl.value);
    if (countEl.value !== "" && (!Number.isFinite(v) || v < 1)) countEl.value = "1";
    else if (Number.isFinite(v) && v > 20) countEl.value = "20";
    apUpdateSubmit();
    apRenderEstimate();
  });
  apUpdateSubmit();
}

/* ── мои профили (addendum-profiles-vps.md §3, user) ─────────────────── */
let MYPROF: MyProfiles | null = null;
let PROF_ERR: string | null = null;
let PROF_RETRY_DONE = false;

function profBadge(p: OwnProfile): string {
  return p.published
    ? `<span class="badge ok"><i></i>опубликован</span>`
    : `<span class="badge">черновик</span>`;
}

function renderProfiles(poll = false): void {
  const body = $("profiles-body");
  const dis = $("profiles-disabled");
  const createBtn = $("profiles-create") as HTMLButtonElement;
  renderSection(document.getElementById("view-profiles"), poll, () => {
  if (PROF_ERR !== null) {
    // Сбой загрузки СПИСКА не должен скрывать редактор: «Создать профиль»
    // остаётся доступной, ошибка инлайн с «Повторить».
    body.innerHTML = `<div class="empty-state"><span class="t">Не удалось загрузить профили</span>
      <span class="s">${esc(PROF_ERR)}</span>
      <button class="btn sm" id="profiles-retry">Повторить</button></div>`;
    $("profiles-retry")?.addEventListener("click", () => { PROF_RETRY_DONE = false; loadProfiles(); });
    return;
  }
  if (MYPROF === null) { body.innerHTML = `<div class="skeleton skel-block"></div>`; return; }
  const off = !MYPROF.enabled;
  dis.hidden = !off;
  dis.innerHTML = off
    ? `<div class="banner warn" style="margin:0 0 12px">Пользовательские профили отключены администратором: создание и правка локальных профилей недоступны.</div>`
    : "";
  createBtn.disabled = off;
  ($("profiles-create-graph") as HTMLButtonElement).disabled = off || MYPROF.profiles.length >= MYPROF.max;
  $("profiles-hint").textContent = `${MYPROF.profiles.length}/${MYPROF.max} локальных профилей · видны только вам`;
  renderPublishedProfiles();
  if (!MYPROF.profiles.length) {
    body.innerHTML = `<div class="empty-state">
      <span class="t">Профилей нет</span>
      <span class="s">Создайте текстовый профиль или граф. Сохранение графа не запускает сервер и не включает политику.</span></div>`;
    return;
  }
  body.innerHTML = MYPROF.profiles.map((p) => `<div class="node-card" data-pid="${p.id}">
    <div class="node-main">
      <div class="node-top">
        <span class="node-id mono">${esc(p.name)}</span>
        ${profBadge(p)}
        <span class="badge">${profileFormatLabel(p)}</span>
      </div>
      <div class="node-sub"><span>${esc(p.description || "—")}</span></div>
    </div>
    <div class="node-side srv-actions">
      <button class="btn sm" data-pedit="${p.id}">Изменить</button>
      <button class="btn sm danger" data-pdel="${p.id}">Удалить</button>
    </div>
  </div>`).join("");
  });
}

function profileFormatLabel(p: Profile): string {
  return p.format === "graph" ? "граф" : p.format === "text" ? "текст" : "формат не указан";
}

/* Опубликованные админом профили — только для копирования себе:
   правка/удаление чужого профиля пользователю недоступны (404 на сервере). */
function renderPublishedProfiles(): void {
  const box = $("profiles-published");
  const pub = MYPROF?.published || [];
  box.hidden = !pub.length;
  if (!pub.length) { box.innerHTML = ""; return; }
  box.innerHTML = `<div class="note" style="margin:0 0 6px">Опубликованные администратором</div>` + pub.map((p) => `<div class="node-card">
    <div class="node-main">
      <div class="node-top"><span class="node-id mono">${esc(p.name)}</span><span class="badge ok"><i></i>опубликован</span><span class="badge">${profileFormatLabel(p)}</span></div>
      <div class="node-sub"><span>${esc(p.description || "—")}</span></div>
    </div>
    <div class="node-side srv-actions">
      <button class="btn sm" data-pcopy="${p.id}">Сделать копию</button>
    </div>
  </div>`).join("");
}

/* «Сделать копию»: копия опубликованного профиля → свой черновик. */
function copyPublishedProfile(btn: HTMLButtonElement, id: number): void {
  btn.disabled = true;
  api.copyProfile(id)
    .then((p) => {
      loadProfiles();
      openProfile(p);
    })
    .catch((e: Error & { code?: string }) => {
      const msg = e.code === "profile_limit_reached"
        ? `Достигнут лимит профилей (${MYPROF?.max ?? "—"}). Удалите ненужный профиль перед созданием нового`
        : e.code === "duplicate_name" ? "Профиль с таким именем уже существует — выберите другое имя." : e.message;
      showToast(msg, true);
      btn.disabled = false;
    });
}

function openProfile(p: OwnProfile | Profile): void {
  if (p.format === "graph") { location.hash = `#/profiles/editor/${p.id}`; return; }
  if ("published" in p) profileEditor(p);
  else profileEditor(null, p);
}

function loadProfiles(poll = false): void {
  PROF_ERR = null;
  api.myProfiles().then((r) => { MYPROF = r; PROF_RETRY_DONE = false; renderProfiles(poll); })
    .catch((e: Error & { status?: number; code?: string }) => {
      PROF_ERR = e.code === "users_disabled" ? "Функция недоступна" : e.message;
      renderProfiles(poll);
      // self-heal при временном сбое (рестарт регистратора): повтор раз через ~2с.
      if (e.code !== "users_disabled" && !PROF_RETRY_DONE && isRetryableLoad(e)) {
        PROF_RETRY_DONE = true;
        setTimeout(loadProfiles, 2000);
      }
    });
}

/* Редактор локального профиля. published у пользователя всегда false;
   пригодность к установке появляется только после публикации админом. */
function profileEditor(p: OwnProfile | null, seed?: Pick<OwnProfile, "name" | "description" | "steps" | "vars_schema">): void {
  const isEdit = p !== null;
  const content = p || seed;
  openModal(isEdit ? `Профиль · ${p!.name}` : "Создать локальный профиль",
    `<div class="field profile-editor" style="gap:12px">
      <div><label for="mp-name">Имя</label>
        <input type="text" id="mp-name" class="mono" value="${esc(content?.name || "")}" maxlength="128"></div>
      <div><label for="mp-desc">Описание</label><input type="text" id="mp-desc" value="${esc(content?.description || "")}"${isEdit ? " readonly" : ""}></div>
      <div><label for="mp-steps">Шаги (shell-команды)</label>
        <textarea id="mp-steps" class="mono" rows="12" spellcheck="false" wrap="off" aria-describedby="mp-steps-help">${esc(content?.steps || "")}</textarea>
        <span id="mp-steps-help" class="help" style="color:var(--fg-3);font-size:11.5px">Упорядоченный список: строка «# Название» открывает шаг, следующие строки — его shell-команда. Шаги выполняются по SSH на вашем VPS; первый упавший шаг прерывает профиль. Потолок 64 КиБ.</span></div>
      <div class="field" style="gap:6px"><label>Переменные профиля</label>
        <div class="vars-editor" id="mp-vars"></div>
        <span class="help">${esc(PROFILE_VARS_HELP)}</span></div>
      <div class="note">Сохранение обновляет профиль на месте. Профиль остаётся черновиком, пока администратор его не опубликует.</div>
      <div class="banner bad" id="mp-err" role="alert" hidden style="margin:0"></div>
    </div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: isEdit ? "Сохранить" : "Создать", cls: "primary", onClick: (ev) => saveProfile(ev.currentTarget as HTMLButtonElement, p) },
    ], true);
  renderVarsEditor($("mp-vars"), parseVarsSchema(content?.vars_schema));
  $("mp-name").focus();
}

function saveProfile(btn: HTMLButtonElement, p: OwnProfile | null): void {
  const errBox = $("mp-err");
  errBox.hidden = true;
  const isEdit = p !== null;
  const name = ($("mp-name") as HTMLInputElement).value.trim();
  const description = ($("mp-desc") as HTMLInputElement).value.trim();
  const steps = ($("mp-steps") as HTMLTextAreaElement).value;
  const rows = collectVarsEditor($("mp-vars"));
  const vars_schema = serializeVarsSchema(rows);
  if (!name) { errBox.textContent = "Укажите имя профиля."; errBox.hidden = false; $("mp-name").focus(); return; }
  const nameChanged = isEdit && name !== p!.name;
  const contentChanged = !isEdit || steps !== (p!.steps || "") || vars_schema !== serializeVarsSchema(parseVarsSchema(p!.vars_schema));
  if (contentChanged && !steps.trim()) { errBox.textContent = "Шаги обязательны."; errBox.hidden = false; $("mp-steps").focus(); return; }
  const varErr = contentChanged ? validateVarsEditor($("mp-vars")) : null;
  if (varErr) { errBox.textContent = varErr; errBox.hidden = false; return; }
  if (isEdit && !contentChanged && !nameChanged) { closeModal(); showToast("Изменений нет."); return; }
  const label = btn.textContent;
  btn.disabled = true; btn.textContent = isEdit ? "Сохранение…" : "Создание…";
  let call: Promise<OwnProfile>;
  if (isEdit) {
    const payload: ProfilePayload = {};
    if (nameChanged) payload.name = name;
    if (contentChanged) { payload.steps = steps; payload.vars_schema = vars_schema; }
    call = api.updateProfile(p!.id, payload);
  } else {
    call = api.createProfile({ name, description, steps, vars_schema });
  }
  call.then(() => { closeModal(); showToast(!isEdit ? "Профиль создан." : contentChanged ? "Профиль сохранён." : "Профиль переименован."); loadProfiles(); })
    .catch((e: Error & { code?: string }) => {
      const msg = e.code === "profile_limit_reached"
        ? `Достигнут лимит профилей (${MYPROF?.max ?? "—"}). Удалите ненужный профиль перед созданием нового`
        : e.code === "user_profiles_disabled" ? "Пользовательские профили отключены администратором"
        : e.code === "duplicate_name" ? "Профиль с таким именем уже существует — выберите другое имя."
        : e.message; // прочая валидация — дословно
      if ($("modal").classList.contains("show")) { errBox.textContent = msg; errBox.hidden = false; }
      else showToast(msg, true);
      btn.disabled = false; btn.textContent = label;
    });
}

function removeProfile(p: OwnProfile): void {
  openModal(`Удалить профиль «${p.name}»?`,
    "Профиль будет удалён. Уже созданные серверы не затрагиваются.",
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Удалить", cls: "danger", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        btn.disabled = true; btn.textContent = "Удаление…";
        api.deleteProfile(p.id)
          .then(() => { closeModal(); showToast("Профиль удалён."); loadProfiles(); })
          .catch((e: Error & { code?: string }) => {
            if (e.code === "profile_in_use") {
              openModal("Профиль используется", "Профиль оставлен: он закреплён политикой или незавершённым запуском. Сначала проверьте управление нодой или откройте известный запуск по ID.", [
                { label: "Закрыть", onClick: closeModal },
                { label: "К управлению нодами", onClick: () => { closeModal(); location.hash = "#/nodes"; } },
                { label: "Открыть запуск по ID", onClick: () => { closeModal(); location.hash = "#/profiles"; $("profiles-run-id").focus(); } },
              ]);
            } else showToast(e.message, true);
            btn.disabled = false; btn.textContent = "Удалить";
          });
      } },
    ]);
}

/* ── визард создания сервера ─────────────────────────────────────────── */
const STEPS = ["Регион", "Тариф", "Профиль", "Переменные", "Подтверждение"];

interface WizardState {
  step: number;
  regions: Region[] | null;
  configs: Configuration[] | null;
  region: string;
  config: Configuration | null;
  myprof: MyProfiles | null;
  profile: Profile | null;
  vars: Record<string, unknown>;
  varRows: ProfileVar[] | null;
  profileError: string | null;
  submitError: string | null;
  idemKey: string;
  confirm: boolean;
  loading: boolean;
  result: Operation | null;
}
const wizard = {
  st: { step: 0, regions: null, configs: null, region: "", config: null, myprof: null, profile: null, vars: {}, varRows: null, profileError: null, submitError: null, idemKey: "", confirm: false, loading: false, result: null } as WizardState,
  entered: false,

  enter(): void {
    if (this.entered) { this.render(); return; }
    this.entered = true;
    this.loadRegions();
    this.loadProfiles();
    this.render();
  },

  loadRegions(): void {
    this.st.regions = null;
    this.st.configs = null;
    api.regions().then((r) => {
      this.st.regions = r.regions || [];
      this.st.configs = r.configurations || [];
      this.render();
    }).catch((e: Error) => { this.st.regions = []; this.st.configs = []; this.render(); showToast("Каталог тарифов: " + e.message, true); });
  },

  loadProfiles(): void {
    this.st.myprof = null;
    this.st.profileError = null;
    textProfiles()
      .then((r) => { this.st.myprof = r; this.render(); })
      .catch((e: Error) => { this.st.profileError = e.message; this.render(); });
  },

  goto(step: number): void {
    if (step < 0 || step >= STEPS.length) return;
    this.st.step = step;
    this.render();
  },

  submitted(): void {
    this.st = { step: 0, regions: this.st.regions, configs: this.st.configs, region: "", config: null, myprof: this.st.myprof, profile: null, vars: {}, varRows: null, profileError: this.st.profileError, submitError: null, idemKey: "", confirm: false, loading: false, result: null };
    this.render();
  },

  render(): void {
    const st = this.st;
    // Результат операции показываем вместо шагов: состояние + вывод + инвариант
    // «повтор установки не создаёт новый VPS». Кнопки второго платного заказа здесь нет.
    if (st.result) { this.renderResult(st.result); return; }
    const stepsHtml = STEPS.map((s, i) =>
      `<span class="wizard-step${i === st.step ? " active" : i < st.step ? " done" : ""}" role="listitem">
        <span class="n">${i < st.step ? "✓" : i + 1}</span>${esc(s)}</span>`).join("");
    $("wiz-steps").innerHTML = stepsHtml;

    // предусловие: подключённый Beget
    const linked = begetOk;
    const bodyEl = $("wiz-body");
    const footEl = $("wiz-foot");

    if (!linked) {
      bodyEl.innerHTML = `<div class="banner warn" style="margin:0">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>
        <span>Сначала подключите аккаунт Beget в разделе <a href="#/beget">Хостинги</a>. Без этого создать сервер нельзя.</span></div>`;
      footEl.innerHTML = "";
      return;
    }

    bodyEl.innerHTML = `${st.submitError ? `<div class="banner bad" role="alert">${esc(st.submitError)}</div>` : ""}${this.stepHTML()}`;
    footEl.innerHTML = this.footHTML();
    this.bindStepBody();
    this.bindFoot();
  },

  /* Результат операции: состояние + обрезанный вывод. После провала внятно
     сказано, что повтор установки НЕ создаёт новый VPS; кнопки, отправляющей
     второй платный заказ, здесь нет (только «К операциям»). */
  renderResult(op: Operation): void {
    $("wiz-steps").innerHTML = "";
    const meta = OP_META[op.state] || { label: op.state, cls: "" };
    const out = op.last_error ? esc(op.last_error) : "—";
    const hasVps = op.has_vps !== false;
    const failed = op.state === "failed" || op.state === "unknown";
    const retryable = (op.state === "failed" || op.state === "waiting_vps") && hasVps;
    $("wiz-body").innerHTML = `<div class="kv" style="gap:10px">
        <span class="kv-k">Операция</span><span class="kv-v mono">#${op.id}</span>
        <span class="kv-k">Нода</span><span class="kv-v mono">${esc(op.node_id || "—")}</span>
        <span class="kv-k">Состояние</span><span class="kv-v"><span class="badge ${meta.cls}"><i></i>${esc(meta.label)}</span></span>
      </div>
      <div class="field" style="margin-top:12px"><label>Вывод установки</label>
        <pre class="op-err" style="white-space:pre-wrap;word-break:break-word;max-height:240px;overflow:auto;margin:0">${out}</pre></div>
      ${op.state === "installing" ? `<div class="banner info" style="margin:12px 0 0">Установка выполняется. Статус также виден в разделе «Операции».</div>` : ""}
      ${op.state === "waiting_vps" ? `<div class="banner warn" style="margin:12px 0 0">
        <span>Сервер создан, но пока не готов (нет RUNNING). Установка начнётся после готовности; место в квоте занято, новый платный заказ невозможен. «Повторить установку» использует тот же сервер и не покупает новый.</span></div>` : ""}
      ${failed && hasVps ? `<div class="banner warn" style="margin:12px 0 0">
        <span>Повторный запуск установки НЕ создаёт новый VPS: сервер уже создан, повтор использует тот же ресурс. Новый платный заказ не отправляйте.</span></div>` : ""}
      ${op.state === "done" ? `<div class="banner info" style="margin:12px 0 0">Сервер создан и профиль применён. Нода появится в «Мои ноды» после регистрации в пуле.</div>` : ""}`;
    const more = op.state === "done"
      ? `<button type="button" class="btn" id="wiz-more">Создать ещё сервер</button>` : "";
    const retry = retryable ? `<button type="button" class="btn" id="wiz-retry">Повторить установку</button>` : "";
    $("wiz-foot").innerHTML = `<span class="spacer"></span>${more}${retry}<button type="button" class="btn primary" id="wiz-ops">К операциям</button>`;
    $("wiz-ops")?.addEventListener("click", () => { location.hash = "#/operations"; });
    $("wiz-more")?.addEventListener("click", () => { this.entered = false; this.submitted(); this.render(); });
    $("wiz-retry")?.addEventListener("click", (ev) => {
      const btn = ev.currentTarget as HTMLButtonElement;
      btn.disabled = true; btn.textContent = "Повтор…";
      api.retryInstall(op.id, { profile_id: this.st.profile?.id || 0, vars: this.st.vars })
        .then((res) => {
          HIGHLIGHT_OP = res.id;
          this.st.result = res;
          this.render();
          showToast("Повтор установки запущен. Новый сервер не создаётся.");
          refresh().catch(() => {});
        })
        .catch((e: Error & { code?: string }) => {
          btn.disabled = false; btn.textContent = "Повторить установку";
          showToast(retryErrText(e), true);
        });
    });
  },

  /* Уход из раздела «Создать сервер»: результат не должен липнуть к визарду
     при возврате; данные каталога сохраняем, чтобы не перезапрашивать зря. */
  leave(): void {
    if (!this.entered) return;
    this.entered = false;
    this.st.result = null;
  },

  stepHTML(): string {
    const st = this.st;
    switch (st.step) {
      case 0: return this.regionHTML();
      case 1: return this.tariffHTML();
      case 2: return this.profileHTML();
      case 3: return this.varsHTML();
      default: return this.confirmHTML();
    }
  },

  regionHTML(): string {
    const st = this.st;
    if (st.regions === null) return `<div class="skeleton skel-block"></div>`;
    if (!st.regions.length) return `<div class="empty-state"><span class="t">Каталог пуст</span><span class="s">Провайдер не вернул ни одного доступного региона.</span></div>`;
    const items = st.regions.map((r) =>
      `<button type="button" class="pick" data-region="${esc(r.id)}" aria-pressed="${r.id === st.region}" ${r.available ? "" : "disabled"}>
        <span class="dot"></span>
        <span class="pick-b"><span class="pick-t">${esc(r.name || r.name_en || r.id)}</span>
        <span class="pick-s mono">${esc(r.id)}${r.country ? " · " + esc(r.country) : ""}${r.available ? "" : " · недоступен"}</span></span>
      </button>`).join("");
    return `<div class="pick-list">${items}</div>`;
  },

  tariffHTML(): string {
    const st = this.st;
    if (st.configs === null) return `<div class="skeleton skel-block"></div>`;
    if (!st.configs.length) {
      // Провайдер не отдал каталог — degraded: конфигурацию можно не выбирать.
      return `<div class="banner info" style="margin:0">Каталог тарифов провайдер не вернул. Можно продолжить без выбора конфигурации — провайдер подберёт её по региону.</div>`;
    }
    const items = st.configs.map((c) =>
      `<button type="button" class="pick" data-config="${esc(c.id)}" aria-pressed="${st.config?.id === c.id}" ${c.available ? "" : "disabled"}>
        <span class="dot"></span>
        <span class="pick-b"><span class="pick-t">${esc(c.name || c.id)}</span>
        <span class="pick-s">${esc(cfgSpecs(c))} · <strong>${esc(cfgPrice(c))}</strong>${c.available ? "" : " · недоступен"}</span></span>
      </button>`).join("");
    return `<div class="pick-list">${items}</div>
      <div class="note" style="margin-top:10px">Цена за период указана числом; «цена не указана» — провайдер её не отдал. Сумма списывается с личного аккаунта Beget.</div>`;
  },

  allProfiles(): Array<Profile & { published?: boolean }> {
    const m = this.st.myprof;
    if (!m) return [];
    const seen = new Set<number>();
    const out: Array<Profile & { published?: boolean }> = [];
    for (const p of m.published || []) { if (!seen.has(p.id)) { seen.add(p.id); out.push(p); } }
    for (const p of m.profiles || []) { if (!seen.has(p.id)) { seen.add(p.id); out.push(p); } }
    return out;
  },

  profileHTML(): string {
    const st = this.st;
    if (st.profileError) return `<div class="banner bad" role="alert">Не удалось загрузить профили: ${esc(st.profileError)} <button type="button" class="btn sm" id="wiz-profiles-retry">Повторить</button></div>`;
    if (st.myprof === null) return `<div class="skeleton skel-block"></div>`;
    const m = st.myprof;
    const pub = m.published || [];
    const own = (m.profiles || []).filter((p) => !pub.some((x) => x.id === p.id));
    if (!pub.length && !own.length) {
      return `<div class="empty-state"><span class="t">Нет доступных профилей</span>
        <span class="s">Профиль установки не опубликован и у вас нет локальных. Создание сервера недоступно.</span></div>`;
    }
    const pick = (p: Profile & { published?: boolean }): string => {
      // Свой неопубликованный профиль владелец выбирать может (§7.4); он не
      // виден другим. Чужой неопубликованный сервер вообще не отдаёт.
      const own = p.published === false;
      const title = own ? "Ваш локальный профиль: доступен для установки только вам, пока не опубликован" : "";
      return `<button type="button" class="pick" data-profile="${p.id}" aria-pressed="${st.profile?.id === p.id}"${title ? ` title="${esc(title)}"` : ""}>
        <span class="dot"></span>
        <span class="pick-b"><span class="pick-t">${esc(p.name)}${own ? ` <span class="badge">мой профиль</span>` : ""}</span>
        <span class="pick-s">${esc(p.description || "")}</span></span>
      </button>`;
    };
    let html = "";
    if (pub.length) html += `<div class="note" style="margin:0 0 6px">Опубликованные</div><div class="pick-list">${pub.map(pick).join("")}</div>`;
    if (own.length) html += `<div class="note" style="margin:12px 0 6px">Мои профили</div><div class="pick-list">${own.map(pick).join("")}</div>`;
    if (m.enabled === false) html += `<div class="banner warn" style="margin:12px 0 0">Создание локальных профилей отключено администратором.</div>`;
    return html;
  },

  varsHTML(): string {
    return `<div class="field"><h2>Переменные установки</h2><div class="vars-editor" id="wiz-vars"></div>
      <span class="help">${esc(PROFILE_VARS_HELP)} Сброс удаляет переопределение; пустой секрет или ad_tag можно задать явно.</span></div>`;
  },

  /* Сводка переменных для шага подтверждения: секретные значения — •••. */
  varsSummaryHTML(): string {
    const decls = parseVarsSchema(this.st.profile?.vars_schema);
    // Объявленные secret-переменные + конвенционально секретные ключи (как в
    // provision.secretValues): extra-переменная authkey/password/node_secret
    // тоже маскируется, а не светится на шаге подтверждения.
    const secret = new Set<string>([
      ...decls.filter(isSecretVar).map((d) => d.name.trim()),
      "node_secret", "authkey", "password", "proxy_secret",
    ]);
    const keys = Object.keys(this.st.vars);
    if (!keys.length) return `<span class="dim">Без переопределений — применятся значения профиля и регистратора.</span>`;
    return `<div class="kv" style="gap:6px">${keys.map((k) =>
      `<span class="kv-k mono">${esc(k)}</span><span class="kv-v mono wrap">${esc(secret.has(k.trim()) ? "•••" : this.st.vars[k] === "" ? "Пустое значение (явно)" : String(this.st.vars[k]))}</span>`).join("")}</div>`;
  },

  confirmHTML(): string {
    const st = this.st;
    const regionLabel = st.regions?.find((r) => r.id === st.region)?.name || st.region;
    const configLabel = st.config ? `${st.config.name || st.config.id}` : "не выбрана";
    const configPrice = st.config ? cfgPrice(st.config) : "—";
    return `<div class="kv" style="gap:10px">
      <span class="kv-k">Регион</span><span class="kv-v">${esc(regionLabel)} <span class="mono dim">${esc(st.region)}</span></span>
      <span class="kv-k">Тариф</span><span class="kv-v">${esc(configLabel)} <span class="dim">· ${esc(configPrice)}</span></span>
      <span class="kv-k">Профиль</span><span class="kv-v">${esc(st.profile?.name || "—")}</span>
    </div>
    <div class="field" style="margin-top:12px"><label>Переменные установки</label>${this.varsSummaryHTML()}</div>
    <div class="banner warn" style="margin:14px 0 0">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>
      <span>Списание с личного аккаунта Beget. Заказ отправляется один раз — повтор того же запроса не создаёт второй сервер.</span>
    </div>
    <label class="check" style="margin-top:12px"><input type="checkbox" id="wiz-confirm"${st.confirm ? " checked" : ""}><span class="box"></span> Подтверждаю создание</label>`;
  },

  footHTML(): string {
    const st = this.st;
    const canNext = this.canAdvance();
    const back = st.step > 0 ? `<button type="button" class="btn" id="wiz-back">Назад</button>` : "";
    const next = st.step < STEPS.length - 1
      ? `<button type="button" class="btn primary" id="wiz-next"${canNext ? "" : " disabled"}>Далее</button>`
      : `<button type="button" class="btn primary" id="wiz-submit"${canNext && st.confirm && !st.loading ? "" : " disabled"}>${st.loading ? "Создание…" : "Создать сервер"}</button>`;
    return `<span class="spacer"></span>${back}${next}`;
  },

  canAdvance(): boolean {
    const st = this.st;
    switch (st.step) {
      case 0: return !!st.region;
      case 1: return true;
       case 2: return !!st.profile && !!st.myprof && !st.profileError;
      case 3: return this.varsValid();
      default: return true;
    }
  },

  varsValid(): boolean {
    const ed = $("wiz-vars");
    return !ed || !validateVarsEditor(ed, false);
  },

  collectVars(): void {
    const ed = $("wiz-vars");
    if (ed) this.st.vars = collectInstallVars(ed);
  },

  /* Fresh step nodes get one binding; footer bindings belong to render/refreshFoot. */
  bindStepBody(): void {
    const st = this.st;
    $("wiz-profiles-retry")?.addEventListener("click", () => this.loadProfiles());
    // шаг 0 — регион
    document.querySelectorAll<HTMLElement>("[data-region]").forEach((b) =>
      b.addEventListener("click", () => { st.region = b.dataset.region!; this.render(); }));
    // шаг 1 — тариф (configuration_id)
    document.querySelectorAll<HTMLElement>("[data-config]").forEach((b) =>
      b.addEventListener("click", () => {
        st.config = (st.configs || []).find((c) => c.id === b.dataset.config) || null;
        this.render();
      }));
    // шаг 2 — профиль (свои + опубликованные)
    document.querySelectorAll<HTMLElement>("[data-profile]").forEach((b) =>
      b.addEventListener("click", () => {
        if (st.profile?.id === Number(b.dataset.profile)) return;
        if (st.varRows?.some((r) => r.override) && !window.confirm("Смена профиля сбросит введённые переменные установки. Продолжить?")) return;
        st.profile = this.allProfiles().find((p) => p.id === Number(b.dataset.profile)) || null;
        st.vars = {};
        st.varRows = null;
        st.confirm = false;
        st.submitError = null;
        this.render();
      }));
    // шаг 3 — переменные (объявленные профилем + свои)
    if (st.step === 3) {
      renderVarsEditor($("wiz-vars"), parseVarsSchema(st.profile?.vars_schema), {
        install: true, defaults: loadInstallDefaults(), snapshot: st.varRows,
        onChange: (rows, vars) => { st.varRows = rows; st.vars = vars; this.refreshFoot(); },
      });
      $("wiz-foot").innerHTML = this.footHTML();
    }
    // шаг 4 — подтверждение
    const chk = $("wiz-confirm") as HTMLInputElement | null;
    if (chk) chk.addEventListener("change", () => { st.confirm = chk.checked; this.refreshFoot(); });
  },

  /* Кнопки подвала пересоздаются на каждый refreshFoot — их слушатели
     вешаем заново там же. */
  bindFoot(): void {
    const st = this.st;
    $("wiz-back")?.addEventListener("click", () => this.goto(st.step - 1));
    $("wiz-next")?.addEventListener("click", () => {
      if (st.step === 3) this.collectVars();
      if (!this.canAdvance()) return;
      if (st.step === STEPS.length - 2 && !st.idemKey) st.idemKey = newIdemKey();
      this.goto(st.step + 1);
    });
    $("wiz-submit")?.addEventListener("click", () => this.submit());
  },

  refreshFoot(): void { $("wiz-foot").innerHTML = this.footHTML(); this.bindFoot(); },

  submit(): void {
    const st = this.st;
    if (!st.idemKey) st.idemKey = newIdemKey();
    st.loading = true;
    st.submitError = null;
    this.refreshFoot();
    api.createVps({
      region: st.region,
      configuration_id: st.config?.id || "",
      profile_id: st.profile?.id || 0,
      vars: st.vars,
      confirm: true,
      idem_key: st.idemKey,
    }).then((op) => {
      st.loading = false;
      HIGHLIGHT_OP = op.id;
      st.result = op; // результат показываем вместо шагов; повтор не плодит заказ
      this.render();
      showToast("Операция создана. Статус — ниже и в разделе «Операции».");
      refresh().catch(() => {});
    }).catch((e: Error & { code?: string; status?: number }) => {
      st.loading = false;
      this.refreshFoot();
      const text =
        e.code === "create_in_progress" ? "Предыдущий заказ ещё не завершён. Новый сервер не создаётся — повторите тот же запрос позже" :
        e.code === "profile_unavailable" ? "Профиль недоступен для установки: он не опубликован" :
        e.code === "quota_exceeded" ? "Квота исчерпана: освободите место или запросите лимит у администратора" :
        e.status === 429 ? "Слишком часто: подождите перед следующим заказом" :
        "Не удалось создать сервер: " + e.message;
      st.submitError = text;
      this.render();
      showToast(text, true);
    });
  },
};

/* Характеристики и цена тарифа. price === null — провайдер не отдал цену:
   пишем «цена не указана», не выдумывая 0. RAM/disk приходят в МБ. */
function cfgSpecs(c: Configuration): string {
  const parts: string[] = [];
  if (c.cpu) parts.push(c.cpu + " vCPU");
  if (c.ram) parts.push(c.ram >= 1024 ? (c.ram / 1024) + " ГБ RAM" : c.ram + " МБ RAM");
  if (c.disk) parts.push(c.disk >= 1024 ? (c.disk / 1024) + " ГБ диск" : c.disk + " МБ диск");
  return parts.join(" · ") || "характеристики не указаны";
}
function cfgPrice(c: Configuration): string {
  if (c.price === null || c.price === undefined) return "цена не указана";
  return c.price.toLocaleString("ru-RU", { maximumFractionDigits: 2 }) + (c.period ? " / " + c.period : "");
}

/* признак подключённого Beget — обновляется в refresh() */
let begetOk = false;

/* ── обновление и отрисовка ──────────────────────────────────────────── */
function renderAll(poll = false): void {
  if (ME) { $("hdr-user").textContent = ME.username; renderMetrics(poll); }
  renderNodes(poll);
  renderOperations(poll);
}

let refreshing = false;
async function refresh(): Promise<void> {
  if (refreshing) return;
  refreshing = true;
  try {
    const [me, nodes, ops, link] = await Promise.all([
      api.me(), api.nodes(), api.operations(), api.begetLink(),
    ]);
    ME = me;
    NODES = nodes.nodes || [];
    OPS = ops.operations || [];
    begetOk = !!link.verified;
    renderBeget(link, true);
    loadAuthkey(true); // независимо: сбой ключа не роняет обзор и не прячет контролы
    $("last-upd").textContent = "обновлено " + new Date().toLocaleTimeString("ru-RU");
    $("live-text").textContent = "live";
    $("live-pill").className = "status-pill live";
    renderAll(true);
    // Опрос раз в 5 с не должен сбрасывать фокус/каретку/скролл в визарде:
    // renderSection пропускает отрисовку, пока фокус внутри раздела шага.
    renderSection(document.getElementById("view-create"), true, () => wizard.render());
  } catch (e) {
    const err = e as Error;
    if (err.message === "unauthorized") return;
    $("live-text").textContent = "нет связи";
    $("live-pill").className = "status-pill off";
  } finally {
    refreshing = false;
  }
}

/* ── удаление ноды ───────────────────────────────────────────────────── */
function confirmDeleteNode(id: string): void {
  openModal(
    `Удалить ноду ${id}?`,
    "Нода будет сразу удалена из пула: назначения снимаются, место в квоте освобождается. Агент с этой identity больше не сможет вернуться — вернуть ноду можно только новой установкой.",
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Удалить", cls: "danger", onClick: () => {
        closeModal();
        api.deleteNode(id)
          .then(() => { showToast("Нода удалена из пула, место освобождено."); refresh().catch(() => {}); })
          .catch((e: Error) => showToast(e.message, true));
      } },
    ],
  );
}

/* ── события ─────────────────────────────────────────────────────────── */
document.addEventListener("click", (e) => {
  const t = e.target as HTMLElement;
  const reveal = t.closest<HTMLElement>("button[data-reveal]");
  if (reveal) {
    const inp = $(reveal.dataset.reveal!) as HTMLInputElement;
    if (inp) inp.type = inp.type === "password" ? "text" : "password";
    return;
  }
  const pedit = t.closest<HTMLElement>("[data-pedit]");
  if (pedit) { const p = MYPROF?.profiles.find((x) => x.id === Number(pedit.dataset.pedit)); if (p) openProfile(p); return; }
  const pdel = t.closest<HTMLElement>("[data-pdel]");
  if (pdel) { const p = MYPROF?.profiles.find((x) => x.id === Number(pdel.dataset.pdel)); if (p) removeProfile(p); return; }
  const pcopy = t.closest<HTMLButtonElement>("button[data-pcopy]");
  if (pcopy) { const id = Number(pcopy.dataset.pcopy) || 0; if (id) copyPublishedProfile(pcopy, id); return; }
  const scheck = t.closest<HTMLElement>("[data-scheck]");
  if (scheck) { const s = SERVERS.find((x) => x.id === Number(scheck.dataset.scheck)); if (s) checkServer(s); return; }
  const sinst = t.closest<HTMLElement>("[data-sinstall]");
  if (sinst) { const s = SERVERS.find((x) => x.id === Number(sinst.dataset.sinstall)); if (s) openInstall(s, (sinst.dataset.smode as InstallMode) || "install"); return; }
  const sdel = t.closest<HTMLElement>("[data-sdel]");
  if (sdel) { const s = SERVERS.find((x) => x.id === Number(sdel.dataset.sdel)); if (s) confirmDeleteServer(s); return; }
  const del = t.closest<HTMLElement>("[data-del]");
  if (del) { confirmDeleteNode(del.dataset.del!); return; }
  const optog = t.closest<HTMLElement>("[data-optoggle]");
  if (optog) {
    const id = Number(optog.dataset.optoggle);
    OPEN_OPS.has(id) ? OPEN_OPS.delete(id) : OPEN_OPS.add(id);
    renderOperations();
    return;
  }
  const cancel = t.closest<HTMLElement>("[data-cancel]");
  if (cancel) {
    const id = Number(cancel.dataset.cancel);
    api.cancelOperation(id)
      .then(() => { showToast("Операция отменена."); refresh().catch(() => {}); })
      .catch((e: Error & { status?: number }) => {
        showToast(e.status === 409 ? "Операцию нельзя отменить на этом шаге" : e.message, true);
      });
    return;
  }
  const retry = t.closest<HTMLElement>("[data-retry]");
  if (retry) {
    const id = Number(retry.dataset.retry);
    openModal(
      `Повторить установку (операция #${id})?`,
      "Повтор применяет профиль к уже созданному серверу и НИКОГДА не покупает новый VPS — новый платный заказ не отправляется. Если сервер ещё не готов (нет RUNNING), повторите позже.",
      [
        { label: "Отмена", cls: "ghost", onClick: closeModal },
        { label: "Повторить установку", cls: "primary", onClick: (ev) => {
          const btn = ev.currentTarget as HTMLButtonElement;
          btn.disabled = true; btn.textContent = "Повтор…";
          api.retryInstall(id)
            .then((op) => { closeModal(); HIGHLIGHT_OP = op.id; showToast("Повтор установки запущен. Новый сервер не создаётся."); refresh().catch(() => {}); })
            .catch((e: Error & { code?: string }) => { showToast(retryErrText(e), true); btn.disabled = false; btn.textContent = "Повторить установку"; });
        } },
      ],
    );
    return;
  }
  const card = t.closest<HTMLElement>(".node-card[data-id]");
  if (card) {
    location.hash = "#/nodes/" + encodeURIComponent(card.dataset.id!);
    return;
  }
});
/* Клавиатура: Enter/Space на карточке-ссылке открывает ноду. */
document.addEventListener("keydown", (e) => {
  if (e.key !== "Enter" && e.key !== " ") return;
  const card = (e.target as HTMLElement).closest<HTMLElement>(".node-card[data-id]");
  if (!card) return;
  e.preventDefault();
  location.hash = "#/nodes/" + encodeURIComponent(card.dataset.id!);
});

/* Раскрытые логи шагов помним между перерисовками (innerHTML их сбрасывает).
   Событие toggle не всплывает — слушаем на фазе перехвата. */
$("ops-body").addEventListener("toggle", (e) => {
  const el = e.target as HTMLDetailsElement;
  if (!(el instanceof HTMLDetailsElement) || !el.dataset.oplog) return;
  el.open ? OPEN_LOGS.add(el.dataset.oplog) : OPEN_LOGS.delete(el.dataset.oplog);
}, true);

$("nodes-reload").addEventListener("click", () => refresh());
$("beget-reload").addEventListener("click", () => loadBeget());
$("servers-reload").addEventListener("click", () => loadServers());
$("profiles-create").addEventListener("click", () => profileEditor(null));
$("profiles-create-graph").addEventListener("click", () => { location.hash = "#/profiles/editor/new"; });
$("profiles-open-run").addEventListener("submit", (e) => { e.preventDefault(); const input = $("profiles-run-id") as HTMLInputElement, id = positiveID(input.value); if (id) location.hash = `#/profiles/runs/${id}`; else input.reportValidity(); });
initServerForm();
initHosting();
initAutopilot();
$("logout-btn").addEventListener("click", async () => {
  if (!await canLeaveBlueprint("logout")) return;
  if (!leaveRunReview()) return;
  leaveBlueprintEditor();
  api.logout().catch(() => {}).finally(() => location.replace("/signin"));
});

/* форма подключения Beget */
/* Троттлинг Beget (reason=rate_limited) не снимается сам: до перезагрузки
   страницы блокируем повтор, иначе частые запросы продлевают блокировку
   (сервер отдаёт 429 с пустым телом и точным текстом про ~30 минут). */
let begetRateLimited = false;
function showBegetRateLimit(msg: string): void {
  begetRateLimited = true;
  const box = $("beget-ratelimit");
  box.innerHTML = `<div class="banner warn" style="margin:0">${esc(msg)}</div>`;
  box.hidden = false;
  ($("beget-submit") as HTMLButtonElement).disabled = true;
}
const begetForm = $("beget-form") as HTMLFormElement;
begetForm.addEventListener("submit", (e) => {
  e.preventDefault();
  if (begetRateLimited) return; // повтор не отправляем
  const login = ($("bg-login") as HTMLInputElement).value.trim();
  const pass = ($("bg-pass") as HTMLInputElement).value;
  const codeEl = $("bg-code") as HTMLInputElement;
  const code = codeEl.value.trim();
  if (!login || !pass) return;
  codeEl.style.borderColor = ""; // сброс подсветки прошлой ошибки
  const btn = $("beget-submit") as HTMLButtonElement;
  btn.disabled = true;
  const label = btn.textContent;
  btn.textContent = "Проверка…";
  api.connectBeget(login, pass, code)
    .then(() => {
      ($("bg-pass") as HTMLInputElement).value = ""; // пароль не держим на экране
      codeEl.value = "";
      begetRateLimited = false;
      $("beget-ratelimit").hidden = true;
      showToast("Beget подключён.");
      loadBeget();
      refresh().catch(() => {});
    })
    .catch((err: Error & { code?: string; status?: number; reason?: string }) => {
      // Сервер отдаёт конкретное сообщение провайдера (beget_auth_failed) —
      // показываем его как есть, а не обобщённое «неверный логин или пароль».
      const text =
        err.code === "beget_auth_failed" ? err.message :
        err.status === 503 ? "Сервис временно недоступен" :
        "Не удалось подключить: " + err.message;
      if (err.reason === "rate_limited") {
        showBegetRateLimit(text); // без авто-повтора: кнопка остаётся выключенной
      } else {
        showToast(text, true);
      }
      if (err.reason === "code_required") {
        codeEl.style.borderColor = "var(--bad)";
        codeEl.focus();
      }
    })
    .finally(() => {
      btn.textContent = label;
      btn.disabled = begetRateLimited;
    });
});

window.addEventListener("hashchange", route);
document.addEventListener("visibilitychange", () => {
  if (document.hidden) { apStopPoll(); return; }
  refresh().catch(() => {});
  if (currentView() === "nodes" && !routeNodeId()) apResumeLast();
});

/* ── «Дополнительно»: общее меню для шапки и нижней панели ─────────────
   Делегирование на document: переживает перерисовки и не зависит от того,
   успели ли отработать другие init-функции. */
function initMoreMenus(): void {
  const allButtons = (): HTMLButtonElement[] =>
    Array.from(document.querySelectorAll<HTMLButtonElement>(".more-btn"));
  const closeAll = (): void => {
    allButtons().forEach((b) => {
      b.setAttribute("aria-expanded", "false");
      b.parentElement?.querySelector<HTMLElement>(".more-menu")?.setAttribute("hidden", "");
    });
  };
  document.addEventListener("click", (e) => {
    const t = e.target as HTMLElement;
    const btn = t.closest<HTMLButtonElement>(".more-btn");
    if (btn) {
      e.preventDefault();
      e.stopPropagation();
      const menu = btn.parentElement?.querySelector<HTMLElement>(".more-menu");
      if (!menu) return;
      const open = btn.getAttribute("aria-expanded") === "true";
      closeAll();
      if (open) return;
      btn.setAttribute("aria-expanded", "true");
      menu.hidden = false;
      // preventScroll: фокус не должен прокручивать sticky-шапку/страницу.
      menu.querySelector<HTMLElement>("a")?.focus({ preventScroll: true });
      return;
    }
    if (t.closest(".more")) {
      if (t.closest(".more-menu a")) closeAll();
      return;
    }
    closeAll();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    const open = allButtons().find((b) => b.getAttribute("aria-expanded") === "true");
    closeAll();
    open?.focus();
  });
}

/* ── старт ───────────────────────────────────────────────────────────── */
// Меню навигации биндим ПЕРВЫМ: оно не должно зависеть от остальных init.
initMoreMenus();
initTheme();
initBuildWatch();
route();
refresh().catch(() => {});
setInterval(() => {
  if (document.hidden) return;
  refresh().catch(() => {});
  // Прогресс установки своих серверов и ресурсов у провайдера — одним тактом 5 с.
  if (currentView() === "servers") { loadServers(true); loadResources(true); }
  // Хостинги: подключения и (на активном сегменте) ресурсы по общему такту.
  if (currentView() === "beget" || routeResourceId()) {
    loadConnections(true);
    if (HOSTING_TAB === "resources") loadResources(true);
  }
  // Детальная карточка ноды обновляется тем же тактом (тихо: renderSection).
  pollNodeDetail();
}, 5000);
