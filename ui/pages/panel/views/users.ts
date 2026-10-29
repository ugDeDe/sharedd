/* Вид панели «Пользователи» (docs/specs/cabinet.md §3).
   Существующая панель, Bearer panel.token: список/создание/правка квоты,
   блокировка, rotate/revoke ключа, освобождение ноды. Эндпоинты —
   registry/internal/server/users_admin_api.go. */

import { $, esc, renderSection } from "../../../lib/dom";
import { showToast, getToken, saveSection } from "../state";

interface AdminUser {
  id: number; username: string; role: string;
  max_nodes: number; used_nodes: number; disabled: boolean;
}

interface AdminInvite {
  id: number; prefix: string;
  created_at: number; expires_at: number; // unix seconds (контракт §2)
  status: "active" | "exhausted" | "used" | "revoked" | "expired";
  used_by: number | null;
  used_by_username: string | null; // в таблицу не выводится: личность использовавшего не раскрываем
  max_uses: number;   // addendum §2: 1..1000
  used_count: number; // сколько регистраций уже прошло
}

interface LegacySecret {
  enabled: boolean;
  affected_nodes: number | null; // null = счётчик недоступен (контракт §3)
  as_of: string;                 // RFC3339
}

let USERS: AdminUser[] = [];

// Абсолютные даты с явным часовым поясом (invitations-legacy-ui.md §3).
const DTF = new Intl.DateTimeFormat("ru-RU",
  { day: "2-digit", month: "2-digit", year: "numeric", hour: "2-digit", minute: "2-digit", timeZoneName: "short" });
const fmtDate = (unixSec: number): string => DTF.format(new Date(unixSec * 1000));
const fmtISO = (iso: string): string => { const d = new Date(iso); return isNaN(+d) ? iso : DTF.format(d); };

export async function panelReq<T>(path: string, method = "GET", body?: unknown): Promise<T> {
  const resp = await fetch(path, {
    method,
    headers: Object.assign(
      { "Authorization": "Bearer " + getToken() },
      body === undefined ? {} : { "Content-Type": "application/json" },
    ),
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (resp.status === 204) return undefined as T;
  if (!resp.ok) {
    const data = await resp.json().catch(() => ({} as any));
    // 503 control_unavailable: сервер отдаёт русское объяснение, но если
    // message отсутствует — подставляем то же слово в слово.
    const fallback = data.error === "control_unavailable"
      ? "Хранилище пользователей недоступно — обратитесь к администратору для проверки журнала регистратора"
      : undefined;
    const err = new Error(data.message || fallback || data.error || ("HTTP " + resp.status)) as Error & { code?: string; status?: number };
    err.code = data.error;
    err.status = resp.status;
    throw err;
  }
  return resp.json() as Promise<T>;
}

/* Временный сбой загрузки раздела: 5xx, control_unavailable (рестарт
   регистратора) или сетевой сбой (status не проставлен). Такие ошибки
   self-heal-ятся повтором; 4xx (права/валидация) повторять бессмысленно. */
export function isRetryableLoad(e: Error & { status?: number; code?: string }): boolean {
  if (e.code === "control_unavailable") return true;
  const s = e.status ?? 0;
  return s >= 500 || s === 0;
}

/* ── модалка ─────────────────────────────────────────────────────────── */
let lastFocused: HTMLElement | null = null;
/* Фон уводим в inert, пока открыта модалка: aria-modal обещает удержание
   фокуса, а Tab не должен уходить за .modal-box. Тосты не глушим. */
function setPageInert(on: boolean): void {
  Array.from(document.body.children).forEach((el) => {
    if (el.id === "modal" || el.id === "toast") return;
    if (on) el.setAttribute("inert", ""); else el.removeAttribute("inert");
  });
}
export function fillModal(title: string, body: string, actions: { label: string; cls?: string; onClick: (ev: MouseEvent) => void }[], wide = false): void {
  /* Как openModal, но не перезаписывает точку возврата фокуса: замена
     содержимого открытой модалки (например, «Приглашение создано» после
     отправки формы) не должна уводить возврат фокуса от инициатора. */
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
    b.addEventListener("click", (ev) => a.onClick(ev));
    foot.appendChild(b);
  });
  $("modal").classList.add("show");
  setPageInert(true);
  (foot.querySelector("button") as HTMLButtonElement | null)?.focus();
}
export function openModal(title: string, body: string, actions: { label: string; cls?: string; onClick: (ev: MouseEvent) => void }[], wide = false): void {
  lastFocused = document.activeElement as HTMLElement;
  fillModal(title, body, actions, wide);
}
export function closeModal(): void {
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

/* ── отрисовка ───────────────────────────────────────────────────────── */
/* Инлайн-правка квоты (§3): клик «изм.» → input + Сохранить/Отмена (PATCH). */
let EDITING_QUOTA: number | null = null;

function quotaCell(u: AdminUser): string {
  if (EDITING_QUOTA === u.id) {
    return `<span class="q-edit">
      <input type="number" class="q-in" id="q-in-${u.id}" min="0" max="10000" value="${u.max_nodes}" aria-label="Максимум нод" title="Уменьшение не удаляет существующие ноды, но блокирует новые сверх лимита.">
      <button class="btn sm primary" data-qsave="${u.id}">Сохранить</button>
      <button class="btn sm ghost" data-qcancel="${u.id}">Отмена</button>
    </span>`;
  }
  return `<span class="q-val num">${u.max_nodes}</span>
    <button class="btn sm ghost" data-qedit="${u.id}" title="Изменить квоту">изм.</button>`;
}

function actionsCell(u: AdminUser): string {
  const revoke = u.used_nodes > 0
    ? `<button class="btn sm danger" data-revoke="${u.id}" data-name="${esc(u.username)}">Отозвать</button>`
    : "";
  return `<span class="u-acts">
    <button class="btn sm" data-rotate="${u.id}" data-name="${esc(u.username)}">Ключ</button>
    ${revoke}
    <button class="btn sm ghost" data-release="${u.id}" data-name="${esc(u.username)}">Освободить место</button>
    <button class="btn sm ghost" data-passwd="${u.id}" data-name="${esc(u.username)}">Сбросить пароль</button>
    <button class="btn sm danger" data-udel="${u.id}" data-name="${esc(u.username)}">Удалить</button>
  </span>`;
}

function statusCell(u: AdminUser): string {
  const badge = u.disabled
    ? `<span class="badge bad"><i></i>заблокирован</span>`
    : `<span class="badge ok"><i></i>активен</span>`;
  const action = u.disabled ? "Разблокировать" : "Заблокировать";
  return `<button class="btn sm ghost" data-toggle="${u.id}" title="${action}" aria-label="${action} пользователя ${esc(u.username)}" aria-pressed="${u.disabled}">${badge}</button>`;
}

export function render(poll = false): void {
  const body = $("users-body");
  renderSection(body, poll, () => {
  if (!USERS.length) {
    body.innerHTML = `<tr><td colspan="6"><div class="empty-state">
      <span class="t">Пользователей нет</span>
      <span class="s">Создайте первого.</span></div></td></tr>`;
    return;
  }
  body.innerHTML = USERS.map((u) => {
    const pct = u.max_nodes > 0 ? Math.min(100, Math.round((u.used_nodes / u.max_nodes) * 100)) : 0;
    return `<tr data-uid="${u.id}">
      <td data-label="Логин">${esc(u.username)}</td>
      <td data-label="Роль">${u.role === "admin" ? `<span class="badge special">админ</span>` : `<span class="badge">пользователь</span>`}</td>
      <td data-label="Квота">${quotaCell(u)}</td>
      <td data-label="Занято"><span class="u-quota"><span class="num">${u.used_nodes}</span>
        <span class="meter"><span class="meter-track"><span class="meter-fill ${pct >= 100 ? "warn" : "ok"}" style="width:${pct}%"></span></span></span></span></td>
      <td data-label="Статус">${statusCell(u)}</td>
      <td data-label="Действия">${actionsCell(u)}</td>
    </tr>`;
  }).join("");
  });
}

export async function load(): Promise<void> {
  loadInvites().catch(() => {});
  loadLegacy().catch(() => {});
  loadUserQuota().catch(() => {});
  try {
    const data = await panelReq<{ users: AdminUser[] }>("/panel/api/admin/users");
    USERS = data.users || [];
    render();
  } catch (e) {
    const err = e as Error & { code?: string };
    $("users-body").innerHTML = `<tr><td colspan="6"><div class="banner bad" style="margin:0">
      <span>Не удалось загрузить пользователей: ${esc(err.message)}</span>
      <button class="btn sm" id="users-retry" style="margin-left:auto">Повторить</button></div></td></tr>`;
    $("users-retry")?.addEventListener("click", () => load().catch(() => {}));
  }
}

/* ── дефолтная квота (plan-ui-batch.md §1.3) ─────────────────────────── */
/* Значение живёт в конфиге регистратора ([users] default_max_nodes), не в
   списке пользователей, — читаем/пишем через редактор конфига. */
async function loadUserQuota(): Promise<void> {
  try {
    const data = await panelReq<{ config?: { users?: { default_max_nodes?: number } } }>("/panel/api/config");
    const v = data.config?.users?.default_max_nodes;
    ($("udq-max") as HTMLInputElement).value = String(typeof v === "number" ? v : 0);
  } catch {
    // Сбой чтения конфига не должен мешать списку пользователей: поле
    // остаётся пустым, ошибку покажет только сохранение.
    ($("udq-max") as HTMLInputElement).value = "";
  }
}

function saveUserQuota(btn: HTMLButtonElement): void {
  const errBox = $("udq-err");
  errBox.hidden = true;
  const n = Number(($("udq-max") as HTMLInputElement).value);
  if (!Number.isInteger(n) || n < 0 || n > 1000000) {
    errBox.textContent = "Допустимо целое число 0..1000000.";
    errBox.hidden = false;
    return;
  }
  const label = btn.textContent;
  btn.disabled = true; btn.textContent = "Сохранение…";
  saveSection({ users: { default_max_nodes: n } }, "Дефолтная квота сохранена")
    .catch((e: Error) => { errBox.textContent = e.message; errBox.hidden = false; })
    .finally(() => { btn.disabled = false; btn.textContent = label; });
}

/* ── удаление пользователя целиком (plan-ui-batch.md §1.4) ───────────── */
function deleteUser(id: number, name: string): void {
  openModal(`Удалить пользователя «${name}»?`,
    `<p style="margin:0;font-size:12.5px;line-height:1.55">Пользователь будет удалён полностью. Его ноды <b>останутся в пуле</b> — они перепривязываются к системному <span class="mono">legacy</span>, место в квоте освобождается; физически ноды не удаляются. Ключи, сессии, профили, свои серверы и операции пользователя будут удалены. Действие необратимо.</p>
     <div class="banner bad" id="udel-err" role="alert" hidden style="margin:12px 0 0"></div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Удалить", cls: "danger", onClick: (ev) => {
        const b = ev.currentTarget as HTMLButtonElement;
        b.disabled = true; b.textContent = "Удаление…";
        panelReq<void>(`/panel/api/admin/users/${id}`, "DELETE")
          .then(() => { closeModal(); showToast("Пользователь удалён. Его ноды остались в пуле."); load().catch(() => {}); })
          .catch((e: Error & { code?: string }) => {
            // 400 legacy_protected / last_admin — текст сервера дословно.
            const box = document.getElementById("udel-err");
            if (box) { box.textContent = e.message; box.hidden = false; }
            else showToast(e.message, true);
            b.disabled = false; b.textContent = "Удалить";
          });
      } },
    ]);
}

/* ── создание ────────────────────────────────────────────────────────── */
function createUser(): void {
  openModal("Создать пользователя",
    `<div class="field" style="gap:12px">
      <div><label for="nu-user">Логин</label>
        <input type="text" id="nu-user" class="mono" pattern="[a-z0-9_.-]{3,32}" minlength="3" maxlength="32" autocomplete="off"></div>
      <div><label for="nu-pass">Пароль</label>
        <input type="password" id="nu-pass" minlength="10" autocomplete="new-password"></div>
      <div><label for="nu-role">Роль</label>
        <select id="nu-role"><option value="user">пользователь</option><option value="admin">админ</option></select></div>
      <div><label for="nu-max">Квота (мест)</label>
        <input type="number" id="nu-max" min="0" max="10000" value="0"></div>
      <div class="help" style="color:var(--fg-3);font-size:11.5px">Логин 3–32: a–z 0–9 _ . - · пароль ≥ 10 символов.</div>
    </div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Создать", cls: "primary", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        const username = ($("nu-user") as HTMLInputElement).value.trim();
        const password = ($("nu-pass") as HTMLInputElement).value;
        const role = ($("nu-role") as HTMLSelectElement).value;
        const max_nodes = Number(($("nu-max") as HTMLInputElement).value) || 0;
        if (!username || password.length < 10) { showToast("Логин 3–32 и пароль ≥ 10", true); return; }
        const label = btn.textContent;
        btn.disabled = true; btn.textContent = "Создание…";
        panelReq<AdminUser>("/panel/api/admin/users", "POST", { username, password, role, max_nodes })
          .then(() => { closeModal(); showToast("Пользователь создан."); load().catch(() => {}); })
          .catch((e: Error & { status?: number }) => showToast(e.status === 409 ? "Логин уже занят" : e.message, true))
          .finally(() => { btn.disabled = false; btn.textContent = label; });
      } },
    ]);
}

/* ── правка квоты (инлайн) ───────────────────────────────────────────── */
function saveQuota(id: number, btn: HTMLButtonElement): void {
  const inp = $("q-in-" + id) as HTMLInputElement | null;
  if (!inp) return;
  const max_nodes = Number(inp.value);
  if (!Number.isFinite(max_nodes) || max_nodes < 0) { showToast("Квота: целое число ≥ 0", true); return; }
  const label = btn.textContent;
  btn.disabled = true; btn.textContent = "Сохранение…";
  panelReq<AdminUser>(`/panel/api/admin/users/${id}`, "PATCH", { max_nodes })
    .then(() => { EDITING_QUOTA = null; showToast("Квота обновлена."); load().catch(() => {}); })
    .catch((e: Error) => { showToast(e.message, true); btn.disabled = false; btn.textContent = label; });
}

/* ── блокировка ──────────────────────────────────────────────────────── */
function toggleDisabled(id: number, btn: HTMLButtonElement): void {
  const u = USERS.find((x) => x.id === id); if (!u) return;
  const label = btn.textContent;
  btn.disabled = true; btn.textContent = "…";
  panelReq<AdminUser>(`/panel/api/admin/users/${id}`, "PATCH", { disabled: !u.disabled })
    .then(() => { showToast(u.disabled ? "Пользователь разблокирован." : "Пользователь заблокирован."); load().catch(() => {}); })
    .catch((e: Error & { code?: string }) => { showToast(e.code === "last_admin" ? "Нельзя заблокировать последнего администратора" : e.message, true); btn.disabled = false; btn.textContent = label; });
}

/* ── rotate / revoke ключа ───────────────────────────────────────────── */
function rotateKey(id: number, name: string, btn: HTMLButtonElement): void {
  const label = btn.textContent;
  btn.disabled = true; btn.textContent = "Открытие…";
  panelReq<{ authkey: string }>(`/panel/api/admin/users/${id}/authkey/rotate`, "POST")
    .then((r) => {
      openModal(`Новый ключ · ${name}`,
        `<div class="secret-reveal">
          <div class="warn-note">Ключ показывается один раз. Сохраните его сейчас — позже он недоступен.</div>
          <div class="key">${esc(r.authkey)}</div>
          <div class="dim" style="font-size:11.5px">Новый ключ отзовёт все прежние. Существующие ноды продолжат работу; новые регистрации старым ключом будут отклонены.</div>
        </div>`,
        [
          { label: "Копировать", onClick: (ev: MouseEvent) => {
            const btn = ev.currentTarget as HTMLButtonElement;
            const done = () => { btn.textContent = "скопировано"; setTimeout(() => { btn.textContent = "Копировать"; }, 1500); };
            if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(r.authkey).then(done, () => { btn.textContent = "не вышло"; });
            else done();
          } },
          { label: "Закрыть", cls: "primary", onClick: () => { closeModal(); load().catch(() => {}); } },
        ]);
    })
    .catch((e: Error) => { showToast(e.message, true); btn.disabled = false; btn.textContent = label; });
}

function revokeKey(id: number, name: string): void {
  openModal(`Отозвать ключ пользователя ${name}?`,
    "Существующие ноды продолжат работу; новые регистрации с этим ключом будут запрещены.",
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Отозвать", cls: "danger", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        btn.disabled = true; btn.textContent = "Отзыв…";
        panelReq<void>(`/panel/api/admin/users/${id}/authkey`, "DELETE")
          .then(() => { closeModal(); showToast("Ключ отозван."); load().catch(() => {}); })
          .catch((e: Error) => { showToast(e.message, true); btn.disabled = false; btn.textContent = "Отозвать"; });
      } },
    ]);
}

/* ── освободить ноду ─────────────────────────────────────────────────── */
function releaseNode(id: number, name: string): void {
  openModal(`Освободить ноду · ${name}`,
    `<div class="field">
      <label for="rn-node">node_id</label>
      <input type="text" id="rn-node" class="mono" placeholder="NAME-hash" autocomplete="off">
      <span class="help" style="color:var(--fg-3);font-size:11.5px">Нужно, когда нода удалена физически: место освобождается, владелец переходит в legacy.</span>
    </div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Освободить", cls: "primary", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        const node_id = ($("rn-node") as HTMLInputElement).value.trim();
        if (!node_id) return;
        const label = btn.textContent;
        btn.disabled = true; btn.textContent = "Освобождение…";
        panelReq<{ released: boolean }>(`/panel/api/admin/users/${id}/release-node`, "POST", { node_id })
          .then(() => { closeModal(); showToast("Место освобождено."); load().catch(() => {}); })
          .catch((e: Error) => { showToast(e.message, true); btn.disabled = false; btn.textContent = label; });
      } },
    ]);
}

/* ── сброс пароля ────────────────────────────────────────────────────── */
function resetPassword(id: number, name: string): void {
  openModal(`Сбросить пароль · ${name}`,
    `<div class="field">
      <label for="rp-pass">Новый пароль</label>
      <input type="password" id="rp-pass" minlength="10" autocomplete="new-password">
      <span class="help" style="color:var(--fg-3);font-size:11.5px">Пароль ≥ 10 символов.</span>
    </div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Сбросить", cls: "primary", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        const password = ($("rp-pass") as HTMLInputElement).value;
        if (password.length < 10) { showToast("Пароль ≥ 10 символов", true); return; }
        const label = btn.textContent;
        btn.disabled = true; btn.textContent = "Сброс…";
        panelReq<AdminUser>(`/panel/api/admin/users/${id}`, "PATCH", { password })
          .then(() => { closeModal(); showToast("Пароль обновлён."); })
          .catch((e: Error) => { showToast(e.message, true); btn.disabled = false; btn.textContent = label; });
      } },
    ]);
}

/* ── приглашения (invitations-legacy-ui.md §3) ───────────────────────── */
/* INVITES === null — список ещё не загружался (skeleton в разметке);
   INVITES_LOAD_ERR — показываем ошибку, а не ложное пустое состояние. */
let INVITES: AdminInvite[] | null = null;
let INVITES_LOAD_ERR: string | null = null;

const INVITE_BADGE: Record<string, string> = {
  active:    `<span class="badge ok"><i></i>Активно</span>`,
  exhausted: `<span class="badge warn"><i></i>Исчерпано</span>`,
  used:      `<span class="badge info"><i></i>Использовано</span>`,
  expired:   `<span class="badge">Истекло</span>`,
  revoked:   `<span class="badge">Отозвано</span>`,
};

const INVITE_SKELETON =
  `<tr><td colspan="6"><div class="skeleton" style="height:20px;margin:8px 0"></div></td></tr>`;

function renderInvites(poll = false): void {
  const body = $("invites-body");
  renderSection(body, poll, () => {
  if (INVITES_LOAD_ERR !== null) {
    body.innerHTML = `<tr><td colspan="6"><div class="banner bad" style="margin:0;color:var(--fg)">
      <span>Не удалось загрузить приглашения${INVITES_LOAD_ERR ? ": " + esc(INVITES_LOAD_ERR) : ""}</span>
      <button class="btn sm" id="invites-retry" style="margin-left:auto">Повторить</button></div></td></tr>`;
    $("invites-retry")?.addEventListener("click", () => loadInvites().catch(() => {}));
    return;
  }
  if (INVITES === null) return; // первая загрузка: skeleton уже стоит
  if (!INVITES.length) {
    body.innerHTML = `<tr><td colspan="6"><div class="empty-state">
      <span class="t">Приглашений пока нет</span>
      <span class="s">Создайте приглашение, чтобы разрешить регистрацию аккаунтов.</span></div></td></tr>`;
    return;
  }
  body.innerHTML = INVITES.map((iv) => `<tr>
    <td data-label="Приглашение"><span class="mono">${esc(iv.prefix)}</span></td>
    <td data-label="Создано">${fmtDate(iv.created_at)}</td>
    <td data-label="Действует до">${fmtDate(iv.expires_at)}</td>
    <td data-label="Регистраций">${iv.used_count}/${iv.max_uses} использовано</td>
    <td data-label="Статус">${INVITE_BADGE[iv.status] || `<span class="badge">${esc(iv.status)}</span>`}</td>
    <td data-label="Действия">${iv.status === "active"
      ? `<button class="btn sm danger" data-irevoke="${iv.id}" aria-label="Отозвать приглашение ${iv.id}">Отозвать</button>`
      : `<span aria-hidden="true">—</span>`}</td>
  </tr>`).join("");
  });
}

async function loadInvites(): Promise<void> {
  if (INVITES === null) $("invites-body").innerHTML = INVITE_SKELETON;
  try {
    const data = await panelReq<{ invites: AdminInvite[] }>("/panel/api/admin/invites");
    // «Новые сверху» (spec §3): сортируем по id desc, не полагаясь на порядок сервера
    INVITES = [...(data.invites || [])].sort((a, b) => b.id - a.id);
    INVITES_LOAD_ERR = null;
    $("invites-stale").hidden = true;
    $("invites-create").hidden = false;
    renderInvites();
  } catch (e) {
    const err = e as Error & { status?: number };
    if (INVITES === null) {
      // первой загрузки нет — пустое состояние не подставляем, мутации недоступны
      INVITES_LOAD_ERR = err.message || "";
      $("invites-create").hidden = true;
      renderInvites();
    } else {
      // ошибка обновления: старый список сохраняется, пометка устаревания
      $("invites-stale").hidden = false;
    }
  }
}

/* Создание: срок из согласованного контракта (default 72ч), число регистраций 1..1000. */
function createInvite(): void {
  openModal("Создать приглашение",
    `<div class="field" style="gap:12px">
      <p style="margin:0;font-size:12.5px;line-height:1.5">Одно приглашение позволяет зарегистрировать несколько аккаунтов — их число задаётся ниже. После исчерпания или истечения срока ссылка не действует.</p>
      <div><label for="inv-ttl">Срок действия</label>
        <select id="inv-ttl">
          <option value="24">24 часа</option>
          <option value="72" selected>72 часа</option>
          <option value="168">168 часов (7 дней)</option>
          <option value="720">720 часов (30 дней)</option>
        </select></div>
      <div><label for="inv-max">Число регистраций</label>
        <input type="number" id="inv-max" min="1" max="1000" value="1">
        <span class="help" style="color:var(--fg-3);font-size:11.5px">Сколько аккаунтов можно создать по этой ссылке (1–1000). Счётчик не связан с квотой нод.</span></div>
      <div class="banner bad" id="inv-create-err" role="alert" hidden style="margin:0"></div>
    </div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Создать приглашение", cls: "primary", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        const ttl = Number(($("inv-ttl") as HTMLSelectElement).value) || 72;
        const max_uses = Number(($("inv-max") as HTMLInputElement).value) || 1;
        const errBox = $("inv-create-err");
        errBox.hidden = true;
        if (!Number.isFinite(max_uses) || max_uses < 1 || max_uses > 1000) {
          errBox.textContent = "Число регистраций: целое 1..1000.";
          errBox.hidden = false;
          return;
        }
        btn.disabled = true; btn.textContent = "Создание…";
        panelReq<{ id: number; invite_url: string; expires_at: number; prefix: string; max_uses: number }>(
            "/panel/api/admin/invites", "POST", { ttl_hours: ttl, max_uses })
          .then((r) => showCreatedInvite(r))
          .catch((e: Error & { status?: number }) => {
            const msg = e.status === undefined
              // Ответ потерян: исход создания неизвестен. POST не повторяем
              // автоматически — обновляем список и объясняем.
              ? "Результат создания неизвестен. Проверьте список приглашений"
              : e.message;
            if (e.status === undefined) loadInvites().catch(() => {});
            // После Escape модалка закрыта — баннер отцеплен от DOM; тогда тост.
            if ($("modal").classList.contains("show")) {
              errBox.textContent = msg;
              errBox.hidden = false;
            } else {
              showToast(msg, true);
            }
            btn.disabled = false; btn.textContent = "Создать приглашение";
          });
      } },
    ]);
}

/* Одноразовое раскрытие ссылки: после закрытия модалки токена нет ни в DOM,
   ни в замыканиях (fillModal/closeModal очищают body и foot). */
function showCreatedInvite(r: { invite_url: string; expires_at: number; prefix: string; max_uses: number }): void {
  fillModal("Приглашение создано",
    `<div class="field" style="gap:12px">
      <p style="margin:0;font-size:12.5px">Приглашение <span class="mono">${esc(r.prefix)}</span> · действует до ${esc(fmtDate(r.expires_at))} · регистраций: ${r.max_uses}</p>
      <p style="margin:0;font-size:12.5px;line-height:1.55">Ссылка показывается только сейчас. Скопируйте её и передайте получателю. После закрытия получить её повторно нельзя.</p>
      <input type="text" id="inv-url" class="mono" readonly value="${esc(r.invite_url)}"
        aria-label="Ссылка приглашения"
        style="height:auto;min-height:34px;padding:7px 11px;word-break:break-all">
      <div class="banner bad" id="inv-copy-err" role="alert" hidden style="margin:0">Не удалось скопировать. Выделите ссылку и скопируйте вручную</div>
    </div>`,
    [
      { label: "Копировать ссылку", onClick: (ev) => copyInviteLink(ev.currentTarget as HTMLButtonElement, r.invite_url) },
      { label: "Закрыть", cls: "primary", onClick: () => { closeModal(); loadInvites().catch(() => {}); } },
    ]);
  // ручной фолбэк: клик по полю выделяет ссылку целиком
  $("inv-url")?.addEventListener("click", (ev) => (ev.currentTarget as HTMLInputElement).select());
}

function copyInviteLink(btn: HTMLButtonElement, url: string): void {
  const errBox = $("inv-copy-err");
  const reset = () => { btn.textContent = "Копировать ссылку"; };
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(url).then(() => {
      // «Скопировано» и объявление — только после реальной записи в clipboard
      btn.textContent = "Скопировано";
      showToast("Ссылка скопирована.");
      if (errBox) errBox.hidden = true;
      setTimeout(reset, 1500);
    }, () => {
      if (errBox) errBox.hidden = false; // без ложного успеха (в отличие от rotateKey-фолбэка)
      reset();
    });
  } else {
    if (errBox) errBox.hidden = false;
    ($("inv-url") as HTMLInputElement | null)?.select();
  }
}

function revokeInvite(id: number): void {
  openModal(`Отозвать приглашение ${id}?`,
    "По этой ссылке больше нельзя будет зарегистрироваться. Уже созданный аккаунт не изменится.",
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Отозвать", cls: "danger", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        btn.disabled = true; btn.textContent = "Отзыв…";
        panelReq<void>(`/panel/api/admin/invites/${id}`, "DELETE")
          .then(() => loadInvites().then(() => {
            closeModal();
            // Статус обновляем после серверного подтверждения; строку не удаляем.
            const now = INVITES?.find((x) => x.id === id)?.status;
            if (now === "exhausted") showToast("Приглашение уже исчерпано — созданные аккаунты не изменены.", true);
            else if (now === "expired") showToast("Приглашение уже истекло.", true);
            else showToast("Приглашение отозвано.");
          }))
          .catch((e: Error) => {
            showToast(e.message, true);
            btn.disabled = false; btn.textContent = "Отозвать";
          });
      } },
    ]);
}

/* ── «Разрешить общий секрет» (invitations-legacy-ui.md §5) ──────────── */
let LEGACY: LegacySecret | null = null;
let LEGACY_BUSY = false;

function legacyWarnText(s: LegacySecret): string {
  if (s.affected_nodes === null)
    return "Число затронутых нод неизвестно. Проверьте миграцию агентов перед отключением";
  if (s.affected_nodes > 0)
    return `Ноды, зависящие от общего токена: ${s.affected_nodes}. После отключения их запросы к API будут отклоняться, пока они не перейдут на персональные учётные данные.` +
      (s.as_of ? ` Данные на ${fmtISO(s.as_of)}.` : "");
  return "";
}

function applyLegacy(s: LegacySecret): void {
  LEGACY = s;
  renderSection($("legacy-card"), false, () => {
  $("legacy-load").hidden = true;
  $("legacy-err").hidden = true;
  $("legacy-row").hidden = false;
  const check = $("legacy-check") as HTMLInputElement;
  check.disabled = false;
  check.checked = s.enabled; // состояние только с сервера
  $("legacy-state-badge").innerHTML = s.enabled
    ? `<span class="badge ok"><i></i>Включено</span>`
    : `<span class="badge">Выключено</span>`;
  const warn = $("legacy-warn");
  const text = legacyWarnText(s);
  warn.textContent = text;
  warn.hidden = text === "";
  });
}

async function loadLegacy(force = false): Promise<boolean> {
  if (!force && LEGACY_BUSY) return true; // во время сохранения состояние рисует saveLegacy
  $("legacy-load").hidden = false;
  $("legacy-row").hidden = true;
  $("legacy-err").hidden = true;
  $("legacy-warn").hidden = true;
  try {
    applyLegacy(await panelReq<LegacySecret>("/panel/api/admin/legacy-secret"));
    return true;
  } catch (e) {
    // Ошибка чтения: OFF не подставляем, контрол недоступен до успешного чтения.
    const err = e as Error;
    $("legacy-load").hidden = true;
    $("legacy-row").hidden = true;
    const box = $("legacy-err");
    box.innerHTML = `<span>Не удалось загрузить режим совместимости${err.message ? ": " + esc(err.message) : ""}</span>
      <button class="btn sm" id="legacy-retry" style="margin-left:auto">Повторить</button>`;
    box.hidden = false;
    $("legacy-retry")?.addEventListener("click", () => loadLegacy(true).catch(() => {}));
    return false;
  }
}

async function legacyFail(msg: string): Promise<void> {
  // Сначала перечитываем правду с сервера (повтор отправки не делаем),
  // потом показываем итог неудачи — иначе loadLegacy затрёт сообщение.
  if (!(await loadLegacy(true))) return; // свой экран ошибки с «Повторить» уже показан
  const box = $("legacy-err");
  box.textContent = msg;
  box.hidden = false;
}

async function saveLegacy(enabled: boolean): Promise<void> {
  if (LEGACY_BUSY || !LEGACY) return;
  LEGACY_BUSY = true;
  const check = $("legacy-check") as HTMLInputElement;
  check.disabled = true;
  $("legacy-err").hidden = true;
  // Явное включение и подтверждённое выключение одинаково показывают «Сохранение…»
  $("legacy-state-badge").innerHTML = `<span class="badge">Сохранение…</span>`;
  try {
    const r = await panelReq<{ enabled: boolean; persisted?: boolean }>(
      "/panel/api/admin/legacy-secret", "POST", { enabled });
    if (r.persisted !== true) {
      // неподтверждённая персистентность (в т.ч. отсутствующее поле) — ошибка, не зелёный успех
      await legacyFail("Изменение не сохранено постоянно. Проверьте состояние регистратора");
      return;
    }
    applyLegacy({ ...LEGACY, enabled: r.enabled }); // успех только после durable-подтверждения
    showToast(enabled ? "Общий секрет включён." : "Общий секрет отключён.");
  } catch (e) {
    const err = e as Error & { status?: number };
    if (err.status === undefined) await legacyFail("Результат сохранения неизвестен. Состояние перечитано.");
    else await legacyFail("Не удалось сохранить режим. Изменение не подтверждено");
  } finally {
    LEGACY_BUSY = false;
    check.disabled = false;
  }
}

function confirmLegacyOff(): void {
  const s = LEGACY;
  const warn = s ? legacyWarnText(s) : "";
  openModal("Отключить общий секрет?",
    `<p style="margin:0;font-size:12.5px;line-height:1.55">После отключения общий Bearer-токен перестаёт приниматься на всех машинных маршрутах — регистрация, heartbeat, отчёты, retire, получение конфигурации. Существующие соединения при этом не разрываются: переключатель меняет только проверку доступа.</p>` +
    (warn ? `<div class="banner warn" role="alert" style="margin:10px 0 0">${esc(warn)}</div>` : "") +
    `<p style="margin:10px 0 0;font-size:12.5px">До отключения обновите агенты и проверьте персональные учётные данные.</p>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Отключить общий секрет", cls: "danger", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        btn.disabled = true; btn.textContent = "Сохранение…"; // повторная отправка заблокирована
        saveLegacy(false).finally(() => closeModal());
      } },
    ]);
}

function syncLegacyCheck(): void {
  if (LEGACY) ($("legacy-check") as HTMLInputElement).checked = LEGACY.enabled;
}

/* ── инициализация ───────────────────────────────────────────────────── */
export function init(): void {
  $("users-create").addEventListener("click", createUser);
  $("users-body").addEventListener("click", (e) => {
    const t = e.target as HTMLElement;
    const q = t.closest<HTMLElement>("[data-qedit]");
    if (q) { EDITING_QUOTA = Number(q.dataset.qedit); render(); ($("q-in-" + EDITING_QUOTA) as HTMLInputElement | null)?.focus(); return; }
    const qc = t.closest<HTMLElement>("[data-qcancel]"); if (qc) { EDITING_QUOTA = null; render(); return; }
    const qs = t.closest<HTMLElement>("[data-qsave]"); if (qs) { saveQuota(Number(qs.dataset.qsave), qs as HTMLButtonElement); return; }
    const rot = t.closest<HTMLElement>("[data-rotate]"); if (rot) { rotateKey(Number(rot.dataset.rotate), rot.dataset.name!, rot as HTMLButtonElement); return; }
    const rev = t.closest<HTMLElement>("[data-revoke]"); if (rev) { revokeKey(Number(rev.dataset.revoke), rev.dataset.name!); return; }
    const rel = t.closest<HTMLElement>("[data-release]"); if (rel) { releaseNode(Number(rel.dataset.release), rel.dataset.name!); return; }
    const pwd = t.closest<HTMLElement>("[data-passwd]"); if (pwd) { resetPassword(Number(pwd.dataset.passwd), pwd.dataset.name!); return; }
    const tg = t.closest<HTMLElement>("[data-toggle]"); if (tg) { toggleDisabled(Number(tg.dataset.toggle), tg as HTMLButtonElement); return; }
    const udel = t.closest<HTMLElement>("[data-udel]"); if (udel) { deleteUser(Number(udel.dataset.udel), udel.dataset.name!); return; }
  });
  $("udq-save").addEventListener("click", (e) => saveUserQuota(e.currentTarget as HTMLButtonElement));
  $("invites-create").addEventListener("click", createInvite);
  $("invites-body").addEventListener("click", (e) => {
    const rv = (e.target as HTMLElement).closest<HTMLElement>("[data-irevoke]");
    if (rv) revokeInvite(Number(rv.dataset.irevoke));
  });
  $("legacy-check").addEventListener("change", () => {
    if (LEGACY_BUSY || !LEGACY) { syncLegacyCheck(); return; }
    const check = $("legacy-check") as HTMLInputElement;
    if (check.checked === LEGACY.enabled) { syncLegacyCheck(); return; }
    if (!check.checked) {
      // OFF — только через подтверждение; до него действует прежний режим ON
      check.checked = true;
      confirmLegacyOff();
    } else {
      saveLegacy(true).catch(() => {});
    }
  });
}
