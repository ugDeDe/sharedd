/* Таблица переменных профиля (addendum-profiles-vps.md §6).
   Формат: vars_schema = {"vars":[{name,default,description,secret}]}.
   {{ var }} подставляется в текст шагов на сервере (значение shell-квотится),
   своего шаблонизатора в браузере нет. Значения secret:true маскируются в
   выводе операции.

   Здесь — парсинг/сериализация и клиентские подсказки. Авторитет валидации —
   сервер: лимиты имён проверяются и на сохранении, и перед запуском; серверный
   текст показывается дословно вызывающим кодом. */

import { esc, icon } from "./dom";

export interface ProfileVar {
  name: string;
  default?: string;
  description?: string;
  secret?: boolean;
  synthetic?: boolean;
  override?: boolean;
  order?: number;
}

export interface InstallDefaults {
  proxy_port: number;
  proxy_sni: string;
  proxy_secret_configured: boolean;
}

const STANDARD = ["proxy_port", "proxy_sni", "proxy_secret", "ad_tag"];
const PROTECTED = ["node_id", "node_secret", "registry_url"];
export const PROFILE_VARS_HELP = "{{ var }} подставляется в шаги на сервере (значение shell-квотится). Для proxy_port, proxy_sni и proxy_secret: значение установки → непустое значение профиля → регистратор. ad_tag — отдельная переменная без автоматического значения. Системные переменные не переопределяются. Серверная проверка — авторитетна.";
const PORT_WARNING = "Порт установки должен совпадать с [shared_proxy].port регистратора. Изменение здесь не меняет настройки регистратора или допуск ноды к мастерству.";

export function isSecretVar(v: ProfileVar): boolean {
  return !!v.secret || ["proxy_secret", "authkey", "password", "node_secret"].includes(v.name.trim());
}

const NAME_RE = /^[a-zA-Z_][a-zA-Z0-9_]*$/;

/** Клиентская подсказка (не замена серверной валидации): null — имя приемлемо. */
export function varNameHint(name: string): string | null {
  const n = name.trim();
  if (!n) return "укажите имя переменной";
  if (!NAME_RE.test(n)) return "имя: латиница/цифры/_, первый символ — буква или _";
  if (PROTECTED.includes(n)) return "системная переменная — переопределение запрещено";
  return null;
}

export function parseVarsSchema(raw?: string): ProfileVar[] {
  if (!raw) return [];
  try {
    const o = JSON.parse(raw) as { vars?: unknown };
    if (!Array.isArray(o.vars)) return [];
    return o.vars.map((v) => {
      const r = (v ?? {}) as Record<string, unknown>;
      return {
        name: String(r.name ?? ""),
        default: r.default == null ? "" : String(r.default),
        description: r.description == null ? "" : String(r.description),
        secret: !!r.secret,
      };
    });
  } catch {
    return [];
  }
}

/** Сериализация в тело запроса. Пустые строки не пишем (у сервера omitempty);
    нет строк с именем → "" (сервер хранит пустую схему). */
export function serializeVarsSchema(rows: ProfileVar[]): string {
  const vars = rows
    .map((r) => ({ name: r.name.trim(), def: r.default ?? "", desc: (r.description ?? "").trim(), secret: !!r.secret }))
    .filter((r) => r.name !== "")
    .map((r) => {
      const o: Record<string, unknown> = { name: r.name };
      if (r.def) o.default = r.def;
      if (r.desc) o.description = r.desc;
      if (r.secret) o.secret = true;
      return o;
    });
  return vars.length ? JSON.stringify({ vars }) : "";
}

/* ── редактор (одна строка = одна переменная) ─────────────────────────── */

/* Иконка «глаз» (в общем наборе ui/lib/dom.ts её нет — как в settings.ts). */
const EYE = `<path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/>`;

interface EditorOptions {
  install?: boolean;
  defaults?: Promise<InstallDefaults>;
  snapshot?: ProfileVar[] | null;
  onChange?: (rows: ProfileVar[], vars: Record<string, string>) => void;
}
let editorId = 0;
const profileBaselines = new WeakMap<HTMLElement, { rows: ProfileVar[]; displayed: string }>();

function editorRows(rows: ProfileVar[]): ProfileVar[] {
  const existing = rows.map((r, order) => ({ ...r, order }));
  return [...STANDARD.map((name) => existing.find((r) => r.name === name) ?? { name, synthetic: true }),
    ...existing.filter((r) => !STANDARD.includes(r.name))];
}

/** Shared profile-default / installation-override editor. No inherited values are serialized. */
export function renderVarsEditor(root: HTMLElement, rows: ProfileVar[], opts: EditorOptions = {}): void {
  profileBaselines.delete(root);
  const id = `vars-${++editorId}`;
  const seeded = editorRows(rows);
  let state = opts.snapshot ? [...seeded.map((v) => {
    const saved = opts.snapshot!.find((r) => r.name === v.name);
    return saved ? { ...v, default: saved.default, override: saved.override } : v;
  }), ...opts.snapshot.filter((r) => !seeded.some((v) => v.name === r.name))] : seeded;
  let defaults: InstallDefaults | undefined;
  let defaultsStatus = "Загрузка значений регистратора…";
  const inheritance = (name: string): { value: string; source: string; placeholder: string } => {
    const d = rows.find((r) => r.name === name && r.default !== undefined && r.default !== "");
    if (d) return { value: d.default!, source: "Из профиля", placeholder: "Из профиля" };
    if (!STANDARD.slice(0, 3).includes(name)) return { value: "", source: "Без значения по умолчанию", placeholder: "Не задано" };
    const value = defaults ? name === "proxy_port" ? String(defaults.proxy_port) : name === "proxy_sni" ? defaults.proxy_sni : "" : "";
    return { value, source: "Из регистратора", placeholder: !defaults ? "Значение регистратора недоступно в интерфейсе; применяется сервером" : name === "proxy_secret" ? defaults.proxy_secret_configured ? "Задаётся регистратором" : "Секрет регистратора не задан" : "Не задано регистратором" };
  };
  const fixed = (v: ProfileVar): boolean => STANDARD.includes(v.name) || (!!opts.install && rows.some((r) => r.name === v.name));
  const read = (): ProfileVar[] => Array.from(root.querySelectorAll<HTMLElement>("[data-vrow]")).map((row, i) => ({
    ...state[i],
    name: row.querySelector<HTMLInputElement>("[data-vname]")!.value,
    default: row.querySelector<HTMLInputElement>("[data-vdefault]")!.value,
    description: row.querySelector<HTMLInputElement>("[data-vdesc]")?.value ?? state[i].description,
    secret: row.querySelector<HTMLInputElement>("[data-vsecret]")?.checked ?? state[i].secret,
    override: row.dataset.override === "true",
  }));
  const notify = (): void => { state = read(); opts.onChange?.(state, collectInstallVars(root)); };
  const paint = (): void => {
    root.innerHTML = `<div class="vars-rows">${state.map((v, i) => {
      const key = `${id}-${i}`;
      const protectedKey = PROTECTED.includes(v.name.trim());
      const secret = isSecretVar(v);
      const inherited = inheritance(v.name);
      const value = protectedKey ? "" : opts.install && fixed(v) && !v.override ? inherited.value : v.default ?? "";
      const hint = v.name ? varNameHint(v.name) : null;
      const label = opts.install ? "Значение для этой установки" : "Значение по умолчанию профиля";
      return `<div class="vars-row${opts.install ? " vars-row-install" : ""}" data-vrow data-synthetic="${!!v.synthetic}" data-order="${v.order ?? rows.length + i}" data-override="${!!v.override}" data-fixed="${fixed(v)}">
        <div class="field"><label for="${key}-name">Имя переменной</label><input id="${key}-name" class="mono" data-vname autocomplete="off" spellcheck="false" value="${esc(v.name)}"${fixed(v) || protectedKey ? " readonly" : ""} aria-describedby="${key}-hint"></div>
        <div class="field"><label for="${key}-value">${label}</label><div class="input-group">
          <input id="${key}-value" type="${secret ? "password" : "text"}" data-vdefault autocomplete="off" spellcheck="false" value="${esc(value)}" placeholder="${esc(opts.install ? inherited.placeholder : "Пусто — наследовать")}" aria-describedby="${key}-hint ${key}-help"${protectedKey ? " disabled" : ""}>
          <button type="button" class="in-btn" data-vreveal aria-label="Показать значение ${esc(v.name)}" aria-pressed="false"${secret && !protectedKey ? "" : " hidden"}>${icon(EYE)}</button></div></div>
        ${opts.install ? `<div class="field"><span data-vsource>${v.override ? "Для этой установки" : inherited.source}</span><button type="button" class="btn sm ghost" data-vreset${v.override ? "" : " disabled"} aria-label="Сбросить переопределение ${esc(v.name)}">Сбросить переопределение</button></div>` : `<div class="field"><label for="${key}-desc">Описание</label><input id="${key}-desc" data-vdesc value="${protectedKey ? "" : esc(v.description ?? "")}" autocomplete="off"${protectedKey ? " disabled" : ""}></div><label class="check vars-secret"><input type="checkbox" data-vsecret${v.secret ? " checked" : ""}${protectedKey ? " disabled" : ""}><span class="box"></span>секрет</label>`}
        ${fixed(v) ? "" : `<button type="button" class="btn sm ghost" data-vremove aria-label="Удалить переменную ${esc(v.name)}">×</button>`}
        <span id="${key}-hint" class="vars-hint${hint ? " bad" : ""}" data-vhint aria-live="polite">${hint ? esc(hint) : ""}</span>
        <span id="${key}-help" class="vars-hint">${v.name === "proxy_port" ? PORT_WARNING : opts.install ? esc(v.description ?? "") : ""}</span>
      </div>`;
    }).join("")}</div><button type="button" class="btn sm" data-vars-add>Добавить переменную</button>
      ${opts.install ? `<div class="help" data-defaults-status aria-live="polite">${esc(defaultsStatus)}</div>` : ""}
      <div class="note"><b>Системные переменные · только чтение</b><br><span class="mono">node_id</span> — Выдаётся сервером<br><span class="mono">node_secret</span> — Выдаётся сервером; значение недоступно<br><span class="mono">registry_url</span> — Задаётся регистратором</div>`;
    validateVarsEditor(root, false);
  };
  root.onclick = (e) => {
    const t = e.target as HTMLElement;
    if (t.closest("[data-vars-add]")) {
      state = [...read(), { name: "", override: true }]; paint();
      root.querySelector<HTMLInputElement>("[data-vrow]:last-child [data-vname]")?.focus(); notify(); return;
    }
    const row = t.closest<HTMLElement>("[data-vrow]");
    if (!row) return;
    const inp = row.querySelector<HTMLInputElement>("[data-vdefault]")!;
    const rev = t.closest<HTMLElement>("[data-vreveal]");
    if (rev) {
      inp.type = inp.type === "password" ? "text" : "password";
      rev.setAttribute("aria-pressed", String(inp.type === "text"));
      rev.setAttribute("aria-label", `${inp.type === "text" ? "Скрыть" : "Показать"} значение ${row.querySelector<HTMLInputElement>("[data-vname]")!.value}`); return;
    }
    if (t.closest("[data-vreset]")) {
      row.dataset.override = "false";
      inp.value = row.dataset.fixed === "true" ? inheritance(row.querySelector<HTMLInputElement>("[data-vname]")!.value).value : "";
      inp.type = isSecretVar(read()[Array.from(root.querySelectorAll("[data-vrow]")).indexOf(row)]) ? "password" : "text";
      row.querySelector<HTMLElement>("[data-vreveal]")?.setAttribute("aria-pressed", "false");
      row.querySelector<HTMLElement>("[data-vreveal]")?.setAttribute("aria-label", `Показать значение ${row.querySelector<HTMLInputElement>("[data-vname]")!.value}`);
      row.querySelector<HTMLElement>("[data-vsource]")!.textContent = inheritance(row.querySelector<HTMLInputElement>("[data-vname]")!.value).source;
      (row.querySelector("[data-vreset]") as HTMLButtonElement).disabled = true;
      inp.focus(); validateVarsEditor(root, false); notify(); return;
    }
    if (t.closest("[data-vremove]")) {
      const idx = Array.from(root.querySelectorAll("[data-vrow]")).indexOf(row);
      state = read().filter((_, i) => i !== idx); paint();
      (root.querySelectorAll<HTMLElement>("[data-vname]")[Math.max(0, idx - 1)] ?? root.querySelector<HTMLElement>("[data-vars-add]"))?.focus(); notify();
    }
  };
  root.oninput = (e) => {
    const inp = e.target as HTMLInputElement;
    const row = inp.closest<HTMLElement>("[data-vrow]");
    if (!row) return;
    if (opts.install) {
      row.dataset.override = "true";
      row.querySelector<HTMLElement>("[data-vsource]")!.textContent = "Для этой установки";
      (row.querySelector("[data-vreset]") as HTMLButtonElement).disabled = false;
    }
    if (inp.matches("[data-vname], [data-vsecret]")) {
      const value = row.querySelector<HTMLInputElement>("[data-vdefault]")!;
      const secret = isSecretVar({ name: row.querySelector<HTMLInputElement>("[data-vname]")!.value, secret: row.querySelector<HTMLInputElement>("[data-vsecret]")?.checked });
      const protectedKey = PROTECTED.includes(row.querySelector<HTMLInputElement>("[data-vname]")!.value.trim());
      value.disabled = protectedKey;
      if (protectedKey) value.value = "";
      value.type = secret ? "password" : "text";
      const reveal = row.querySelector<HTMLElement>("[data-vreveal]")!;
      reveal.hidden = !secret || protectedKey; reveal.setAttribute("aria-pressed", "false");
      reveal.setAttribute("aria-label", `Показать значение ${row.querySelector<HTMLInputElement>("[data-vname]")!.value}`);
    }
    validateVarsEditor(root, false); notify();
  };
  root.dataset.install = String(!!opts.install);
  paint();
  if (!opts.install) profileBaselines.set(root, { rows, displayed: serializeVarsSchema(collectVarsEditor(root)) });
  if (opts.install) opts.defaults?.then((d) => {
    defaults = d;
    root.querySelectorAll<HTMLElement>("[data-vrow]").forEach((row) => {
      const n = row.querySelector<HTMLInputElement>("[data-vname]")!.value.trim();
      const inp = row.querySelector<HTMLInputElement>("[data-vdefault]")!;
      const inherited = inheritance(n);
      inp.placeholder = inherited.placeholder;
      if (row.dataset.fixed === "true" && row.dataset.override !== "true" && !PROTECTED.includes(n)) inp.value = inherited.value;
    });
    defaultsStatus = "Значения регистратора получены; при запуске используются актуальные настройки.";
    root.querySelector<HTMLElement>("[data-defaults-status]")!.textContent = defaultsStatus;
  }).catch((e: Error) => {
    defaultsStatus = `Значения регистратора недоступны: ${e.message}. Наследование применяется сервером.`;
    root.querySelector<HTMLElement>("[data-defaults-status]")!.textContent = defaultsStatus;
  });
}

/** Keep original declaration order and omit synthetic blank standard rows. */
export function collectVarsEditor(root: HTMLElement): ProfileVar[] {
  const rows = Array.from(root.querySelectorAll<HTMLElement>("[data-vrow]")).map((row) => ({
    name: row.querySelector<HTMLInputElement>("[data-vname]")!.value,
    default: row.querySelector<HTMLInputElement>("[data-vdefault]")!.value,
    description: row.querySelector<HTMLInputElement>("[data-vdesc]")?.value ?? "",
    secret: row.querySelector<HTMLInputElement>("[data-vsecret]")?.checked ?? false,
    synthetic: row.dataset.synthetic === "true",
    order: Number(row.dataset.order),
  })).filter((r) => !r.synthetic || !!r.default || !!r.description || r.secret).sort((a, b) => a.order - b.order);
  const baseline = profileBaselines.get(root);
  return baseline && baseline.displayed === serializeVarsSchema(rows) ? baseline.rows : rows;
}

export function collectInstallVars(root: HTMLElement): Record<string, string> {
  // ponytail: presence, not truthiness, distinguishes explicit empty from inheritance.
  return Object.fromEntries(Array.from(root.querySelectorAll<HTMLElement>('[data-vrow][data-override="true"]')).map((row) => [
    row.querySelector<HTMLInputElement>("[data-vname]")!.value.trim(),
    row.querySelector<HTMLInputElement>("[data-vdefault]")!.value,
  ]).filter(([name]) => !!name));
}

export function validateVarsEditor(root: HTMLElement, focus = true): string | null {
  let first: HTMLInputElement | undefined;
  let message: string | null = null;
  const seen = new Set<string>();
  root.querySelectorAll<HTMLElement>("[data-vrow]").forEach((row) => {
    const name = row.querySelector<HTMLInputElement>("[data-vname]")!;
    const value = row.querySelector<HTMLInputElement>("[data-vdefault]")!;
    const n = name.value.trim();
    let hint = n ? varNameHint(n) : value.value ? "укажите имя переменной" : null;
    if (!hint && n && seen.has(n)) hint = "имя переменной уже используется";
    seen.add(n);
    const checkValue = root.dataset.install === "true" ? row.dataset.override === "true" : value.value !== "";
    const valueHint = checkValue && n === "proxy_port" && (!/^\d+$/.test(value.value) || Number(value.value) < 1 || Number(value.value) > 65535)
      ? "Порт: целое число от 1 до 65535, без пробелов." : checkValue && n === "proxy_sni" && !value.value ? "Укажите SNI или сбросьте переопределение." : null;
    name.setAttribute("aria-invalid", String(!!hint)); value.setAttribute("aria-invalid", String(!!valueHint));
    const box = row.querySelector<HTMLElement>("[data-vhint]")!;
    box.textContent = hint ?? valueHint ?? ""; box.classList.toggle("bad", !!(hint || valueHint));
    if (!message && (hint || valueHint)) { first = hint ? name : value; message = hint ?? valueHint; }
  });
  if (focus) first?.focus();
  return message;
}

/** Первая клиентская ошибка имени (пустые строки не считаются ошибкой) или null. */
export function firstVarNameError(rows: ProfileVar[]): string | null {
  for (const r of rows) {
    if (r.name.trim() === "") continue;
    const h = varNameHint(r.name);
    if (h) return `Переменная «${r.name.trim()}»: ${h}`;
  }
  return null;
}
