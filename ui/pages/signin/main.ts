/* Страница входа и регистрации (docs/specs/cabinet.md §1).

   Аутентификация — cookie sharedd_session (ставит сервер). На успешном
   входе уходим в /cabinet. CSRF для login/register не нужен: эти маршруты
   не проходят requireCSRF (registry/internal/server/cabinet_api.go). */

import { $ } from "../../lib/dom";
import { initTheme } from "../../lib/theme";

interface ApiErr { status: number; code: string; message: string; }

async function post(path: string, body: unknown): Promise<Response> {
  return fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

async function toErr(resp: Response): Promise<ApiErr> {
  const data = await resp.json().catch(() => ({} as any));
  return { status: resp.status, code: data.error || "", message: data.message || "" };
}

function alertBox(text: string, info = false): void {
  const el = $("auth-alert");
  el.textContent = text;
  el.classList.toggle("info", info);
  el.classList.toggle("bad", !info);
  // Успех — polite status, ошибка — alert (spec §6)
  el.setAttribute("role", info ? "status" : "alert");
  el.hidden = false;
}

function clearAlert(): void { $("auth-alert").hidden = true; }

/* ── вкладки ─────────────────────────────────────────────────────────── */
function showTab(which: "login" | "register"): void {
  const isLogin = which === "login";
  $("panel-login").hidden = !isLogin;
  $("panel-register").hidden = isLogin;
  $("tab-login").classList.toggle("active", isLogin);
  $("tab-register").classList.toggle("active", !isLogin);
  $("tab-login").setAttribute("aria-selected", String(isLogin));
  $("tab-register").setAttribute("aria-selected", String(!isLogin));
  // roving tabindex: в порядок табуляции входит только активная вкладка
  $("tab-login").tabIndex = isLogin ? 0 : -1;
  $("tab-register").tabIndex = isLogin ? -1 : 0;
  if (!isLogin) renderRegister();
}

/* ── регистрация по приглашению (invitations-legacy-ui.md §4) ────────── */
/* INVITE живёт только в памяти вкладки: не поле формы, не localStorage,
   не хранится в адресной строке после чтения. */
let INVITE: string | null = null;
// Терминальный отказ, подтверждённый сервером: форма закрывается до новой ссылки.
let INVITE_DEAD: string | null = null;

const INVITE_BANNERS: Record<string, string> = {
  invite_expired: "Срок приглашения истёк. Попросите администратора прислать новое",
  invite_used: "Приглашение уже использовано. Если вы создали аккаунт, войдите. Иначе попросите новое приглашение",
  invite_revoked: "Приглашение отозвано администратором. Попросите новое приглашение",
  invite_invalid: "Приглашение недействительно. Проверьте ссылку или попросите новое",
  invite_required: "Приглашение недействительно. Проверьте ссылку или попросите новое",
};

function renderRegister(): void {
  const dead = $("reg-invite-alert");
  const gate = $("reg-gate");
  const note = $("reg-invite-note");
  const form = $("reg-form") as HTMLFormElement;
  if (INVITE_DEAD) {
    $("reg-invite-alert-text").textContent = INVITE_BANNERS[INVITE_DEAD] ?? INVITE_BANNERS.invite_invalid;
    dead.hidden = false; gate.hidden = true; note.hidden = true; form.hidden = true;
    return;
  }
  dead.hidden = true;
  const hasInvite = INVITE !== null;
  gate.hidden = hasInvite;
  note.hidden = !hasInvite;
  form.hidden = !hasInvite;
}

$("reg-gate-login").addEventListener("click", () => showTab("login"));
$("reg-dead-login").addEventListener("click", () => showTab("login"));

$("tab-login").addEventListener("click", () => { clearAlert(); showTab("login"); });
$("tab-register").addEventListener("click", () => { clearAlert(); showTab("register"); });

// APG: стрелки переключают вкладки внутри tablist
document.querySelector(".tabrow")?.addEventListener("keydown", (e) => {
  const k = (e as KeyboardEvent).key;
  if (k !== "ArrowLeft" && k !== "ArrowRight") return;
  e.preventDefault();
  const next = $("tab-login").classList.contains("active") ? "register" : "login";
  clearAlert();
  showTab(next);
  $("tab-" + next).focus();
});

/* ── показать/скрыть пароль ──────────────────────────────────────────── */
document.addEventListener("click", (e) => {
  const b = (e.target as HTMLElement).closest<HTMLElement>("button[data-reveal]");
  if (!b) return;
  const inp = $(b.dataset.reveal!) as HTMLInputElement;
  if (inp) inp.type = inp.type === "password" ? "text" : "password";
});

/* ── фокус на первое невалидное поле формы ───────────────────────────── */
function focusFirstInvalid(form: HTMLFormElement): void {
  const bad = form.querySelector<HTMLInputElement>("input:invalid");
  if (bad) bad.focus();
}

/* ── вход ────────────────────────────────────────────────────────────── */
const loginForm = $("panel-login") as HTMLFormElement;
loginForm.addEventListener("submit", (e) => {
  e.preventDefault();
  clearAlert();
  const username = ($("li-user") as HTMLInputElement).value.trim();
  const password = ($("li-pass") as HTMLInputElement).value;
  if (!username || !password) { focusFirstInvalid(loginForm); return; }

  const btn = $("li-submit") as HTMLButtonElement;
  btn.disabled = true;
  const label = btn.textContent;
  btn.textContent = "Вход…";

  post("/api/auth/login", { username, password })
    .then(async (resp) => {
      if (resp.ok) { location.replace("/cabinet"); return; }
      const err = await toErr(resp);
      const text =
        err.status === 401 ? "Неверный логин или пароль" :
        err.status === 403 ? "Пользователь заблокирован" :
        err.code === "users_disabled" || err.status === 503 ? "Сервис временно недоступен" :
        "Не удалось войти. Повторите позже.";
      alertBox(text);
    })
    .catch(() => alertBox("Нет связи с регистратором. Повторите позже."))
    .finally(() => { btn.disabled = false; btn.textContent = label; });
});

/* ── регистрация ─────────────────────────────────────────────────────── */
const regForm = $("reg-form") as HTMLFormElement;
regForm.addEventListener("submit", (e) => {
  e.preventDefault();
  if (regForm.hidden) return; // заглушка/терминальное состояние: формы нет
  clearAlert();
  const username = ($("rg-user") as HTMLInputElement).value.trim();
  const password = ($("rg-pass") as HTMLInputElement).value;
  const password2 = ($("rg-pass2") as HTMLInputElement).value;
  if (!username || !password) { focusFirstInvalid(regForm); return; }
  if (password !== password2) { alertBox("Пароли не совпадают"); ($("rg-pass2") as HTMLInputElement).focus(); return; }

  const btn = $("rg-submit") as HTMLButtonElement;
  btn.disabled = true;
  const label = btn.textContent;
  btn.textContent = "Создание…";

  const body: Record<string, string> = { username, password };
  if (INVITE !== null) body.invite = INVITE; // не поле, не хранилище — только запрос

  post("/api/auth/register", body)
    .then(async (resp) => {
      if (resp.ok) {
        alertBox("Аккаунт создан. Администратор выдаст квоту — до этого создать ноды нельзя.", true);
        ($("li-user") as HTMLInputElement).value = username;
        ($("rg-pass") as HTMLInputElement).value = "";
        ($("rg-pass2") as HTMLInputElement).value = "";
        INVITE = null;
        INVITE_DEAD = null; // invite-контекст очищен
        if (location.pathname !== "/signin") history.replaceState(null, "", "/signin");
        renderRegister();
        showTab("login");
        return;
      }
      const err = await toErr(resp);
      // Сначала согласованные причины приглашения (403), потом обычная валидация:
      // 409 по логину не расходует приглашение — можно исправить логин и повторить.
      if (err.code && INVITE_BANNERS[err.code]) { INVITE_DEAD = err.code; renderRegister(); return; }
      const text =
        err.status === 409 ? "Логин уже занят" :
        err.status === 429 ? "Слишком много попыток. Повторите позже" :
        err.status === 400 ? "Логин: 3–32, a–z 0–9 _ . - · пароль не короче 10" :
        err.code === "users_disabled" || err.status === 503 ? "Сервис временно недоступен" :
        "Не удалось создать аккаунт. Повторите позже.";
      alertBox(text);
    })
    // Сеть/потеря ответа — не истечение и не отзыв: приглашение остаётся,
    // автоматического повтора нет. Аккаунт мог уже создаться — предложить вход.
    .catch(() => alertBox("Нет связи с регистратором. Повторите позже. Если аккаунт уже создан — войдите."))
    .finally(() => { btn.disabled = false; btn.textContent = label; });
});

/* ── старт: уже вошли — сразу в кабинет ──────────────────────────────── */
initTheme();

// Токен приглашения читаем из URL один раз и сразу чистим адресную строку:
// он не должен остаться в истории, referrer или подсказках браузера.
{
  const u = new URL(location.href);
  const inv = u.searchParams.get("invite");
  if (inv) {
    INVITE = inv;
    u.search = "";
    history.replaceState(null, "", u.pathname + u.hash);
  }
}
renderRegister();
if (INVITE !== null) showTab("register");

fetch("/api/auth/me", { cache: "no-store" })
  .then((r) => { if (r.ok) location.replace("/cabinet"); }) // активная сессия приглашение не расходует
  .catch(() => {});
