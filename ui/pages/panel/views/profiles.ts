/* Вид «Профили установки» (addendum-profiles-vps.md §3, admin).
   Bearer panel.token, эндпоинты registry/internal/server/profiles_admin_api.go.
   Правка шагов создаёт НОВУЮ версию (история append-only) и сбрасывает
   published — это явно написано в редакторе. Ошибка сервера показывается
   дословно, инлайн. Модалку/запрос берём у
   users.ts — вторая реализация того же не нужна. */

import { $, esc, renderSection } from "../../../lib/dom";
import { showToast } from "../state";
import { renderVarsEditor, collectVarsEditor, parseVarsSchema, serializeVarsSchema, validateVarsEditor, PROFILE_VARS_HELP } from "../../../lib/vars";
import { panelReq, openModal, closeModal, isRetryableLoad } from "./users";

interface AdminProfile {
  id: number; name: string; description: string; published: boolean;
  created_at: number; updated_at: number;
  steps?: string; vars_schema?: string; versions?: number;
}

let PROFILES: AdminProfile[] | null = null;
let LOAD_ERR: string | null = null;
// Одноразовый self-heal: после сбоя (5xx/control_unavailable/сеть) повторяем
// загрузку раз через ~2с, чтобы окно рестарта регистратора прошло само.
let AUTO_RETRIED = false;

const SKELETON =
  `<tr><td colspan="5"><div class="skeleton" style="height:20px;margin:8px 0"></div></td></tr>`;

function rowHTML(p: AdminProfile): string {
  const pub = p.published
    ? `<span class="badge ok"><i></i>опубликован</span>`
    : `<span class="badge">черновик</span>`;
  // Число версий сервер теперь отдаёт (profileAdminJSON.versions).
  return `<tr data-pid="${p.id}">
    <td data-label="Имя"><span class="mono">${esc(p.name)}</span></td>
    <td data-label="Описание" class="wrap">${esc(p.description || "—")}</td>
    <td data-label="Версий"><span class="mono">${p.versions ?? "—"}</span></td>
    <td data-label="Опубликован">${pub}</td>
    <td data-label="Действия"><span class="u-acts">
      <button class="btn sm" data-pedit="${p.id}">Изменить</button>
      <button class="btn sm ghost" data-ppub="${p.id}">${p.published ? "Снять с публикации" : "Опубликовать"}</button>
      <button class="btn sm danger" data-pdel="${p.id}" data-name="${esc(p.name)}">Удалить</button>
    </span></td>
  </tr>`;
}

function render(): void {
  const body = $("profiles-body");
  renderSection(body, false, () => {
  if (LOAD_ERR !== null) {
    body.innerHTML = `<tr><td colspan="5"><div class="banner bad" style="margin:0;color:var(--fg)">
      <span>Не удалось загрузить профили${LOAD_ERR ? ": " + esc(LOAD_ERR) : ""}</span>
      <button class="btn sm" id="profiles-retry" style="margin-left:auto">Повторить</button></div></td></tr>`;
    $("profiles-retry")?.addEventListener("click", () => { AUTO_RETRIED = false; load().catch(() => {}); });
    return;
  }
  if (PROFILES === null) return; // skeleton уже стоит
  if (!PROFILES.length) {
    body.innerHTML = `<tr><td colspan="5"><div class="empty-state">
      <span class="t">Профилей нет</span>
      <span class="s">Создайте профиль установки — его шаги будут применять на новых серверах.</span></div></td></tr>`;
    return;
  }
  body.innerHTML = PROFILES.map(rowHTML).join("");
  });
}

export async function load(): Promise<void> {
  if (PROFILES === null) $("profiles-body").innerHTML = SKELETON;
  try {
    const data = await panelReq<{ profiles: AdminProfile[] }>("/panel/api/admin/profiles");
    PROFILES = data.profiles || [];
    LOAD_ERR = null;
    AUTO_RETRIED = false;
    $("profiles-create").hidden = false;
    render();
  } catch (e) {
    const err = e as Error & { status?: number; code?: string };
    if (PROFILES === null) {
      // (a) Сбой загрузки СПИСКА не должен скрывать редактор: «Создать
      // профиль» остаётся доступной, ошибка — инлайн с «Повторить».
      LOAD_ERR = err.message || "";
      $("profiles-create").hidden = false;
      render();
      // (b) Одноразовый self-heal при временном сбое (рестарт регистратора).
      if (!AUTO_RETRIED && isRetryableLoad(err)) {
        AUTO_RETRIED = true;
        setTimeout(() => { load().catch(() => {}); }, 2000);
      }
    } else {
      showToast("Не удалось обновить профили: " + err.message, true);
    }
  }
}

/* ── редактор (create/edit) ──────────────────────────────────────────── */
function editor(p: AdminProfile | null): void {
  const isEdit = p !== null;
  // Имя редактируемо и при правке: PUT принимает name. Переименование —
  // метаданные (новой версии не создаёт, в отличие от шагов).
  const nameField = isEdit
    ? `<div><label for="p-name">Имя</label><input type="text" id="p-name" class="mono" value="${esc(p!.name)}">
        <span class="help" style="color:var(--fg-3);font-size:11.5px">Переименование новых версий не создаёт.</span></div>`
    : `<div><label for="p-name">Имя</label><input type="text" id="p-name" class="mono" autocomplete="off"></div>`;
  const pubField = isEdit ? "" :
    `<label class="check" style="margin:4px 0"><input type="checkbox" id="p-pub"><span class="box"></span> Опубликовать сразу</label>`;
  openModal(isEdit ? `Профиль · ${p!.name}` : "Создать профиль",
    `<div class="field profile-editor" style="gap:12px">
      ${nameField}
      <div><label for="p-desc">Описание</label><input type="text" id="p-desc" value="${isEdit ? esc(p!.description) : ""}"></div>
      <div><label for="p-steps">Шаги (shell-команды)</label>
        <textarea id="p-steps" class="mono" rows="12" spellcheck="false" wrap="off" aria-describedby="p-steps-help">${isEdit ? esc(p!.steps || "") : ""}</textarea>
        <span id="p-steps-help" class="help" style="color:var(--fg-3);font-size:11.5px">Упорядоченный список: строка «# Название» открывает шаг, следующие строки — его shell-команда (могут быть многострочными). Шаги выполняются по SSH на VPS пользователя; первый упавший шаг прерывает профиль. Потолок 64 КиБ.</span></div>
      <div class="field" style="gap:6px"><label>Переменные профиля</label>
        <div class="vars-editor" id="p-vars"></div>
        <span class="help">${esc(PROFILE_VARS_HELP)}</span></div>
      ${pubField}
      ${isEdit ? `<div class="note">Изменение шагов или переменных создаёт НОВУЮ версию: история версий не перезаписывается, а публикация сбрасывается — опубликуйте профиль заново.</div>` : ""}
      <div class="banner bad" id="p-err" role="alert" hidden style="margin:0"></div>
    </div>`,
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: isEdit ? "Сохранить" : "Создать", cls: "primary", onClick: (ev) => save(ev.currentTarget as HTMLButtonElement, p) },
    ], true);
  renderVarsEditor($("p-vars"), parseVarsSchema(p?.vars_schema));
  $("p-name").focus();
}

function save(btn: HTMLButtonElement, p: AdminProfile | null): void {
  const isEdit = p !== null;
  const errBox = $("p-err");
  errBox.hidden = true;
  const name = ($("p-name") as HTMLInputElement).value.trim();
  const description = ($("p-desc") as HTMLInputElement).value.trim();
  const steps = ($("p-steps") as HTMLTextAreaElement).value;
  const rows = collectVarsEditor($("p-vars"));
  const vars_schema = serializeVarsSchema(rows);
  const versionChanged = !isEdit || steps !== (p!.steps || "") || vars_schema !== serializeVarsSchema(parseVarsSchema(p!.vars_schema));
  if (!name) { errBox.textContent = "Укажите имя профиля."; errBox.hidden = false; $("p-name").focus(); return; }
  if (versionChanged && !steps.trim()) { errBox.textContent = "Шаги обязательны."; errBox.hidden = false; $("p-steps").focus(); return; }
   const varErr = versionChanged ? validateVarsEditor($("p-vars")) : null;
  if (varErr) { errBox.textContent = varErr; errBox.hidden = false; return; }

  const body: Record<string, unknown> = { name, description };
  // Metadata-only edits must not append a version or unpublish the profile.
  if (versionChanged) {
    body.steps = steps;
    body.vars_schema = vars_schema;
  }
  // Published changes separately on edit; an empty description clears metadata.
  if (!isEdit && ($("p-pub") as HTMLInputElement | null)?.checked) body.published = true;

  const label = btn.textContent;
  btn.disabled = true; btn.textContent = isEdit ? "Сохранение…" : "Создание…";
  const path = isEdit ? `/panel/api/admin/profiles/${p!.id}` : "/panel/api/admin/profiles";
  panelReq<AdminProfile>(path, isEdit ? "PUT" : "POST", body)
    .then(() => { closeModal(); showToast(isEdit ? (body.steps === undefined ? "Профиль сохранён." : "Профиль сохранён (новая версия).") : "Профиль создан."); load().catch(() => {}); })
    .catch((e: Error & { code?: string }) => {
      // validation — дословно и инлайн.
      const msg = e.code === "duplicate" || e.code === "duplicate_name" || /duplicate/i.test(e.message)
        ? "Профиль с таким именем уже существует" : e.message;
      if ($("modal").classList.contains("show")) { errBox.textContent = msg; errBox.hidden = false; }
      else showToast(msg, true);
      btn.disabled = false; btn.textContent = label;
    });
}

function togglePublished(p: AdminProfile): void {
  panelReq<AdminProfile>(`/panel/api/admin/profiles/${p.id}`, "PUT", { published: !p.published })
    .then(() => { showToast(p.published ? "Снят с публикации." : "Опубликован."); load().catch(() => {}); })
    .catch((e: Error) => showToast(e.message, true));
}

function removeProfile(p: AdminProfile): void {
  openModal(`Удалить профиль «${p.name}»?`,
    "Профиль и все его версии будут удалены. Уже созданные серверы не затрагиваются.",
    [
      { label: "Отмена", cls: "ghost", onClick: closeModal },
      { label: "Удалить", cls: "danger", onClick: (ev) => {
        const btn = ev.currentTarget as HTMLButtonElement;
        btn.disabled = true; btn.textContent = "Удаление…";
        panelReq<void>(`/panel/api/admin/profiles/${p.id}`, "DELETE")
          .then(() => { closeModal(); showToast("Профиль удалён."); load().catch(() => {}); })
          .catch((e: Error) => { showToast(e.message, true); btn.disabled = false; btn.textContent = "Удалить"; });
      } },
    ]);
}

export function init(): void {
  $("profiles-create").addEventListener("click", () => editor(null));
  $("profiles-body").addEventListener("click", (e) => {
    const t = e.target as HTMLElement;
    const ed = t.closest<HTMLElement>("[data-pedit]");
    if (ed) { const p = PROFILES?.find((x) => x.id === Number(ed.dataset.pedit)); if (p) editor(p); return; }
    const pub = t.closest<HTMLElement>("[data-ppub]");
    if (pub) { const p = PROFILES?.find((x) => x.id === Number(pub.dataset.ppub)); if (p) togglePublished(p); return; }
    const del = t.closest<HTMLElement>("[data-pdel]");
    if (del) { const p = PROFILES?.find((x) => x.id === Number(del.dataset.pdel)); if (p) removeProfile(p); return; }
  });
}
