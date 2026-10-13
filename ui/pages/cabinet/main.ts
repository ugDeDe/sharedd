/* Кабинет пользователя (docs/specs/cabinet.md §2).

   Разделы — якоря одной страницы: #/nodes, #/create, #/beget, #/operations.
   Данные и мутации — через ./api (cookie-сессия + CSRF). Инвариант
   «отказ ≠ пересоздание» показан на нодах, в операциях и в визарде. */

import { $, esc, renderSection } from "../../lib/dom";
import { initBuildWatch } from "../../lib/update";
import { fmtClock, fmtAgo, fmtNum, plural } from "../../lib/format";
import { initTheme } from "../../lib/theme";
import { renderVarsEditor, collectVarsEditor, collectInstallVars, parseVarsSchema, serializeVarsSchema, validateVarsEditor, isSecretVar, PROFILE_VARS_HELP } from "../../lib/vars";
import type { ProfileVar, InstallDefaults } from "../../lib/vars";
import { api, newIdemKey } from "./api";
import type { Authkey, BegetLink, Configuration, ManualServer, ManualServerPayload, Me, MyProfiles, NodeInfo, Operation, OperationStep, OwnProfile, Profile, ProfilePayload, Region } from "./api";
import { openNodeDetail, closeNodeDetail, pollNodeDetail } from "./node-detail";

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
function openModal(title: string, body: string, actions: { label: string; cls?: string; onClick: () => void }[], wide = false): void {
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
  nodes: "Мои ноды", create: "Создать сервер", servers: "Серверы", profiles: "Мои профили", beget: "Хостинги", operations: "Операции",
};
function currentView(): string {
  const p = (location.hash || "").replace(/^#\/?/, "").split("/").filter(Boolean);
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
function route(): void {
  const v = currentView();
  const nodeId = routeNodeId();
  const viewId = nodeId ? "view-node" : "view-" + v;
  const nav = nodeId ? "nodes" : v;
  document.querySelectorAll<HTMLElement>(".view").forEach((el) => { el.hidden = el.id !== viewId; });
  document.querySelectorAll<HTMLElement>("[data-view]").forEach((a) =>
    a.classList.toggle("active", a.dataset.view === nav));
  $("view-title").textContent = nodeId ? "Нода " + nodeId : VIEWS[v];
  // L5: уходя со страницы «Свои серверы», не держим секрет в скрытом DOM.
  if (v !== "servers") clearServerSecret();
  if (v === "create" && !nodeId) wizard.enter();
  else wizard.leave();
  if (v === "beget" && !nodeId) loadBeget();
  if (v === "servers" && !nodeId) loadServers();
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
  if (!installing && meta.key !== "done" && meta.key !== "deleted") {
    const retry = meta.key === "failed";
    installBtn = `<button class="btn sm${retry ? "" : " primary"}" data-sinstall="${s.id}" data-sretry="${retry ? 1 : 0}">${retry ? "Повторить установку" : "Установить"}</button>`;
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
  const wrap = $("sv-install-vars");
  if (!wrap) return;
  // Replace the profile-specific editor without carrying stale overrides.
  wrap.replaceChildren();
  const ed = document.createElement("div");
  ed.className = "vars-editor";
  wrap.appendChild(ed);
  renderVarsEditor(ed, decls, { install: true, defaults: loadInstallDefaults(), snapshot, onChange });
}

/* Объявленные переменные профиля по id (published + свои). Нет профиля → []. */
function installProfileDecls(m: MyProfiles, id: number): ProfileVar[] {
  const p = [...(m.published || []), ...(m.profiles || [])].find((x) => id ? x.id === id : x.name === "sharedd-node");
  return p ? parseVarsSchema(p.vars_schema) : [];
}

/* Повтор — на тот же хост с исходным профилем; выбор доступен только впервые. */
function openInstall(s: ManualServer, isRetry: boolean): void {
  openModal(isRetry ? `Повторить установку · ${s.label}` : `Установка · ${s.label}`,
    `<div class="banner info" style="margin:0 0 12px"><span>Установка выполняется по SSH на ваш существующий сервер <span class="mono">${esc(s.host)}:${s.port}</span>. Новый сервер не создаётся и не оплачивается.</span></div>
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
      { label: isRetry ? "Повторить установку" : "Установить", cls: "primary", onClick: (ev) => doInstall(ev.currentTarget as HTMLButtonElement, s, isRetry) },
    ], true);
   let pendingRows: ProfileVar[] | undefined;
   if (isRetry) $("sv-install-vars").textContent = "Загрузка переменных исходного профиля…";
   else renderInstallVars([], undefined, (rows) => { pendingRows = rows; });
   const originalSelect = $("sv-install-prof") as HTMLSelectElement;
   const load = (): void => { api.myProfiles().then((m) => {
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
     const opts: string[] = ['<option value="0">Стандартный профиль</option>'];
    for (const p of m.published || []) opts.push(`<option value="${p.id}">${esc(p.name)}</option>`);
    for (const p of m.profiles || [])
      if (!(m.published || []).some((x) => x.id === p.id)) opts.push(`<option value="${p.id}">${esc(p.name)} (мой)</option>`);
     sel.innerHTML = opts.join("");
     sel.disabled = false;
     sel.dataset.ready = "true";
     delete sel.dataset.loadError;
    $("sv-profile-status").textContent = "Смена профиля сбрасывает переменные этой установки.";
    let selected = sel.value;
    sel.addEventListener("change", () => {
      const ed = document.querySelector<HTMLElement>("#sv-install-vars .vars-editor");
      if (ed && Object.keys(collectInstallVars(ed)).length && !window.confirm("Смена профиля сбросит введённые переменные установки. Продолжить?")) { sel.value = selected; return; }
      selected = sel.value;
      renderInstallVars(installProfileDecls(m, Number(sel.value) || 0));
    });
    renderInstallVars(installProfileDecls(m, 0), pendingRows);
   }).catch(() => {
     if ($("sv-install-prof") !== originalSelect) return;
     $("sv-profile-status").innerHTML = `Не удалось загрузить ${isRetry ? "исходный профиль операции" : "профили"}. <button type="button" class="btn sm" id="sv-profile-retry">Повторить</button>`;
     if (isRetry) $("sv-install-vars").textContent = "Переменные исходного профиля недоступны. Повторная установка заблокирована до загрузки профиля.";
    $("sv-profile-retry").addEventListener("click", () => { $("sv-profile-status").textContent = "Загрузка профилей…"; load(); });
    originalSelect.dataset.loadError = "true";
  }); };
  load();
}

function doInstall(btn: HTMLButtonElement, s: ManualServer, isRetry: boolean): void {
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
      showToast(isRetry ? "Повтор установки запущен. Новый сервер не создаётся." : "Установка запущена.");
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
  $("srv-add").addEventListener("click", () => toggleServerForm(($("server-form") as HTMLFormElement).hidden));
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
  $("profiles-hint").textContent = `${MYPROF.profiles.length}/${MYPROF.max} локальных профилей · видны только вам`;
  renderPublishedProfiles();
  if (!MYPROF.profiles.length) {
    body.innerHTML = `<div class="empty-state">
      <span class="t">Профилей нет</span>
      <span class="s">Создайте локальный профиль установки. Для установки на сервер его должен опубликовать администратор.</span></div>`;
    return;
  }
  body.innerHTML = MYPROF.profiles.map((p) => `<div class="node-card" data-pid="${p.id}">
    <div class="node-main">
      <div class="node-top">
        <span class="node-id mono">${esc(p.name)}</span>
        ${profBadge(p)}
      </div>
      <div class="node-sub"><span>${esc(p.description || "—")}</span>
        ${p.versions != null ? `<span class="dim"> · версий: ${p.versions}</span>` : ""}</div>
    </div>
    <div class="node-side srv-actions">
      <button class="btn sm" data-pedit="${p.id}">Изменить</button>
      <button class="btn sm danger" data-pdel="${p.id}">Удалить</button>
    </div>
  </div>`).join("");
  });
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
      <div class="node-top"><span class="node-id mono">${esc(p.name)}</span><span class="badge ok"><i></i>опубликован</span></div>
      <div class="node-sub"><span>${esc(p.description || "—")}</span></div>
    </div>
    <div class="node-side srv-actions">
      <!-- копия всегда активна: лимит/флаг отключения по-прежнему проверяет сервер -->
      <button class="btn sm" data-pcopy="${p.id}">Сделать копию</button>
    </div>
  </div>`).join("");
}

/* «Сделать копию»: копия опубликованного профиля → свой черновик; открываем
   её в обычном редакторе профиля (шаги/переменные доступны сразу). Ошибки
   (лимит, дубликат имени) показываем как вернёт сервер. */
function copyPublishedProfile(btn: HTMLButtonElement, id: number): void {
  const label = btn.textContent;
  btn.disabled = true; btn.textContent = "Копирование…";
  api.copyProfile(id)
    .then((p) => {
      showToast(`Создана копия «${p.name}».`);
      profileEditor(p);
      loadProfiles();
    })
    .catch((e: Error & { code?: string }) => {
      const msg = e.code === "profile_limit_reached"
        ? `Достигнут лимит профилей (${MYPROF?.max ?? "—"}). Удалите ненужный профиль перед копированием`
        : e.code === "user_profiles_disabled" ? "Пользовательские профили отключены администратором"
        : e.message; // duplicate_name и валидация — дословно
      showToast(msg, true);
      btn.disabled = false; btn.textContent = label;
    });
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
function profileEditor(p: OwnProfile | null): void {
  const isEdit = p !== null;
  openModal(isEdit ? `Профиль · ${p!.name}` : "Создать локальный профиль",
    `<div class="field profile-editor" style="gap:12px">
      <div><label for="mp-name">Имя</label>
        <input type="text" id="mp-name" class="mono" value="${isEdit ? esc(p!.name) : ""}" maxlength="128"></div>
      <div><label for="mp-desc">Описание</label><input type="text" id="mp-desc" value="${isEdit ? esc(p!.description) : ""}"${isEdit ? " readonly" : ""}></div>
      <div><label for="mp-steps">Шаги (shell-команды)</label>
        <textarea id="mp-steps" class="mono" rows="12" spellcheck="false" wrap="off" aria-describedby="mp-steps-help">${isEdit ? esc(p!.steps || "") : ""}</textarea>
        <span id="mp-steps-help" class="help" style="color:var(--fg-3);font-size:11.5px">Упорядоченный список: строка «# Название» открывает шаг, следующие строки — его shell-команда. Шаги выполняются по SSH на вашем VPS; первый упавший шаг прерывает профиль. Потолок 64 КиБ.</span></div>
      <div class="field" style="gap:6px"><label>Переменные профиля</label>
        <div class="vars-editor" id="mp-vars"></div>
        <span class="help">${esc(PROFILE_VARS_HELP)}</span></div>
      <div class="note">Переименование не создаёт версию. Изменение шагов или переменных создаёт НОВУЮ версию: история не перезаписывается. Профиль остаётся черновиком, пока администратор его не опубликует.</div>
      <div class="banner bad" id="mp-err" role="alert" hidden style="margin:0"></div>
    </div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: isEdit ? "Сохранить" : "Создать", cls: "primary", onClick: (ev) => saveProfile(ev.currentTarget as HTMLButtonElement, p) },
    ], true);
  renderVarsEditor($("mp-vars"), parseVarsSchema(p?.vars_schema));
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
  // Переименование — метаданные: версию не создаёт (backend RenameProfile).
  const nameChanged = isEdit && name !== p!.name;
  const versionChanged = !isEdit || steps !== (p!.steps || "") || vars_schema !== serializeVarsSchema(parseVarsSchema(p!.vars_schema));
  if (versionChanged && !steps.trim()) { errBox.textContent = "Шаги обязательны."; errBox.hidden = false; $("mp-steps").focus(); return; }
  const varErr = versionChanged ? validateVarsEditor($("mp-vars")) : null;
  if (varErr) { errBox.textContent = varErr; errBox.hidden = false; return; }
  if (isEdit && !versionChanged && !nameChanged) { closeModal(); showToast("Изменений нет."); return; }
  const label = btn.textContent;
  btn.disabled = true; btn.textContent = isEdit ? "Сохранение…" : "Создание…";
  let call: Promise<OwnProfile>;
  if (isEdit) {
    // Шлём только изменённое: отправка steps без изменений создала бы версию.
    const payload: ProfilePayload = {};
    if (nameChanged) payload.name = name;
    if (versionChanged) { payload.steps = steps; payload.vars_schema = vars_schema; }
    call = api.updateProfile(p!.id, payload);
  } else {
    call = api.createProfile({ name, description, steps, vars_schema });
  }
  call.then(() => { closeModal(); showToast(!isEdit ? "Профиль создан." : versionChanged ? "Профиль сохранён (новая версия)." : "Профиль переименован."); loadProfiles(); })
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
    "Профиль и все его версии будут удалены. Уже созданные серверы не затрагиваются.",
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Удалить", cls: "danger", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        btn.disabled = true; btn.textContent = "Удаление…";
        api.deleteProfile(p.id)
          .then(() => { closeModal(); showToast("Профиль удалён."); loadProfiles(); })
          .catch((e: Error) => { showToast(e.message, true); btn.disabled = false; btn.textContent = "Удалить"; });
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
    api.myProfiles()
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
    "Нода будет отмечена на удаление и освободится после подтверждения провайдера. Место освободит администратор. Ошибка здоровья или установки не создаёт новый сервер.",
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Удалить", cls: "danger", onClick: () => {
        closeModal();
        api.deleteNode(id)
          .then(() => { showToast("Нода отмечена на удаление. Место освободит администратор."); refresh().catch(() => {}); })
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
  if (pedit) { const p = MYPROF?.profiles.find((x) => x.id === Number(pedit.dataset.pedit)); if (p) profileEditor(p); return; }
  const pdel = t.closest<HTMLElement>("[data-pdel]");
  if (pdel) { const p = MYPROF?.profiles.find((x) => x.id === Number(pdel.dataset.pdel)); if (p) removeProfile(p); return; }
  const pcopy = t.closest<HTMLButtonElement>("button[data-pcopy]");
  if (pcopy) { const id = Number(pcopy.dataset.pcopy) || 0; if (id) copyPublishedProfile(pcopy, id); return; }
  const scheck = t.closest<HTMLElement>("[data-scheck]");
  if (scheck) { const s = SERVERS.find((x) => x.id === Number(scheck.dataset.scheck)); if (s) checkServer(s); return; }
  const sinst = t.closest<HTMLElement>("[data-sinstall]");
  if (sinst) { const s = SERVERS.find((x) => x.id === Number(sinst.dataset.sinstall)); if (s) openInstall(s, sinst.dataset.sretry === "1"); return; }
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
initServerForm();
$("logout-btn").addEventListener("click", () => {
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
document.addEventListener("visibilitychange", () => { if (!document.hidden) refresh().catch(() => {}); });

/* ── старт ───────────────────────────────────────────────────────────── */
initTheme();
initBuildWatch();
route();
refresh().catch(() => {});
setInterval(() => {
  if (document.hidden) return;
  refresh().catch(() => {});
  // Прогресс установки своих серверов виден и без ухода с раздела.
  if (currentView() === "servers") loadServers(true);
  // Детальная карточка ноды обновляется тем же тактом (тихо: renderSection).
  pollNodeDetail();
}, 5000);
