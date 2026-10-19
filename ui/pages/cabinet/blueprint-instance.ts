import { esc } from "../../lib/dom";
import { isSecretVar, parseVarsSchema } from "../../lib/vars";
import type { InstallDefaults } from "../../lib/vars";
import { api } from "./api";
import type { Blueprint, HostingConnection, ProfileVersion } from "./api";
import { positiveID, readVersion, runtimeGate } from "./blueprint-model";

export interface InstanceForm {
  collect: () => { inputs: Record<string, unknown>; vars: Record<string, string> };
  installedTextVersion: () => ProfileVersion | null; // для чек-листа политики снаружи формы
}
let formID = 0;
const field = (id: string, label: string, control: string): string =>
  `<div class="field"><label for="${id}">${esc(label)}</label>${control}</div>`;
const record = (v: unknown): Record<string, unknown> =>
  v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
const sensitiveVar = (v: ReturnType<typeof parseVarsSchema>[number]): boolean =>
  isSecretVar(v) || /secret|password|token|key|credential/i.test(v.name);

/** Explicit numeric opener: published lists contain neither format nor ordinal. */
export function mountVersionPicker(
  root: HTMLElement,
  format: "text" | "graph",
  onPick: (v: ProfileVersion | null) => void,
): void {
  const prefix = `bp-picker-${++formID}`;
  root.innerHTML = `<div class="help">Загрузка профилей…</div>`;
  const load = async (): Promise<void> => {
    try {
      const m = await api.myProfiles();
      if (!root.isConnected) return;
      const all = [...m.profiles, ...m.published.filter((p) => !m.profiles.some((x) => x.id === p.id))];
      root.innerHTML = `${field(prefix + "-profile", format === "graph" ? "Сохранённый граф-профиль" : "Текстовый профиль установки", `<select id="${prefix}-profile"><option value="">Выберите профиль</option>${all.map((p) => `<option value="${p.id}">${esc(p.name)} · #${p.id}</option>`).join("")}</select>`)}
        ${field(prefix + "-number", "Номер версии (не PK)", `<input id="${prefix}-number" type="number" min="1" step="1" aria-describedby="${prefix}-status">`)}
        <button class="btn" type="button" data-read-version>Загрузить версию</button>
        <p class="help" id="${prefix}-status" role="status">Опубликованный профиль: введите известный номер. API не возвращает latest или список версий.</p>`;
      const profile = root.querySelector<HTMLSelectElement>("select")!,
        number = root.querySelector<HTMLInputElement>("input")!,
        status = root.querySelector<HTMLElement>("[role=status]")!;
      let generation = 0;
      const invalidate = (): void => {
        generation++;
        onPick(null);
        status.textContent = "Загрузите выбранную версию перед подтверждением.";
      };
      profile.onchange = () => {
        invalidate();
        number.value = String(m.profiles.find((p) => p.id === Number(profile.value))?.versions || "");
      };
      number.oninput = invalidate;
      root.querySelector<HTMLButtonElement>("button")!.onclick = async (e) => {
        const id = positiveID(profile.value),
          ordinal = positiveID(number.value),
          seq = ++generation;
        if (!id || !ordinal) {
          status.textContent = "Выберите профиль и положительный номер версии.";
          number.focus();
          return;
        }
        const btn = e.currentTarget as HTMLButtonElement;
        btn.disabled = true;
        try {
          const v = await readVersion(id, ordinal);
          if (seq !== generation || !root.isConnected) return;
          if (v.format !== format || (format === "graph" && !v.blueprint))
            throw new Error(`Нужна версия формата ${format}; получен ${v.format}.`);
          status.textContent = `Версия ${v.version} · ${v.format} · PK ${v.profile_version_id}`;
          onPick(v);
        } catch (error) {
          if (seq === generation) {
            status.textContent = (error as Error).message;
            onPick(null);
          }
        } finally {
          btn.disabled = false;
        }
      };
    } catch (error) {
      root.innerHTML = `<div class="banner bad" role="alert">${esc((error as Error).message)} <button type="button" class="btn sm">Повторить GET</button></div>`;
      root.querySelector<HTMLButtonElement>("button")!.onclick = () => {
        void load();
      };
    }
  };
  void load();
}

/** Shared run/policy input snapshot. Opaque handles are never free-form JSON. */
export function mountInstance(
  root: HTMLElement,
  graph: Blueprint,
  policy = false,
  initial: Record<string, unknown> = {},
): InstanceForm {
  const prefix = `bp-instance-${++formID}`;
  let connections: HostingConnection[] | null = null,
    defaults: InstallDefaults | null = null,
    textVersion: ProfileVersion | null = null;
  let connectionError = "",
    defaultsError = "";
  const value = (name: string): string =>
    typeof initial[name] === "string" || typeof initial[name] === "number" ? String(initial[name]) : "";
  const htmlInput = (name: string, type: string, required: boolean, val = "", extra = ""): string =>
    `<input id="${prefix}-${name}" data-instance="${name}" type="${type}" ${required ? "required" : ""} value="${esc(val)}" aria-describedby="${prefix}-error" ${extra}>`;
  root.innerHTML = `<div class="blueprint-form-grid">${Object.entries(graph.inputs)
    .map(([name, decl]) => {
      const label = `${name} · ${decl.type}${decl.required ? " · обязательно" : ""}`;
      if (name === "connection_id" && decl.type === "number")
        return field(
          prefix + "-connection_id",
          "Подключение Nodehost · обязательно",
          `<select id="${prefix}-connection_id" data-instance="connection_id" required disabled><option value="">Загрузка…</option></select><p class="help" data-connection-info></p><button type="button" class="btn sm" data-reload-connections>Повторить GET подключений</button>`,
        );
      if (name === "configuration_ref" && decl.type === "config")
        return `<fieldset class="blueprint-config"><legend>Параметры VPS Nodehost · ввод вручную</legend><p class="help">Каталог недоступен (501). Цена API не предоставлена. RAM и диск — ГБ.</p><div class="blueprint-form-grid">${configFields(prefix, record(initial[name]))}</div></fieldset>`;
      if (name === "installation_profile_version_id" && decl.type === "number")
        return `<section class="blueprint-text-version" data-text-picker></section><div data-install-vars></div>`;
      if (["string", "number", "bool"].includes(decl.type) && !/secret|password|credential|token|key/i.test(name))
        return field(
          prefix + "-" + name,
          label,
          decl.type === "bool"
            ? `<select id="${prefix}-${name}" data-instance="${name}" ${decl.required ? "required" : ""}>${decl.required ? "" : '<option value="">Не задано</option>'}<option value="false"${initial[name] === false ? " selected" : ""}>Нет</option><option value="true"${initial[name] === true ? " selected" : ""}>Да</option></select>`
            : htmlInput(
                name,
                decl.type === "number" ? "number" : "text",
                decl.required,
                value(name),
                decl.type === "number" ? 'step="any"' : "",
              ),
        );
      return `<p class="banner warn">${esc(label)}: безопасного выбора ${decl.type} в текущем API нет. ${decl.required ? "Запуск заблокирован." : "Необязательное значение будет пропущено."}</p>`;
    })
    .join(
      "",
    )}</div><p class="help" data-defaults>Загрузка порта регистратора…</p><div class="banner bad" id="${prefix}-error" role="alert" hidden></div>`;
  const errorBox = root.querySelector<HTMLElement>(`#${prefix}-error`)!;
  const get = (name: string): HTMLInputElement | HTMLSelectElement | null =>
    root.querySelector(`[data-instance="${name}"]`);
  const connectionInfo = (): void => {
    const c = connections?.find((c) => c.id === Number(get("connection_id")?.value));
    const info = root.querySelector<HTMLElement>("[data-connection-info]");
    if (info)
      info.textContent = c
        ? `#${c.id} · ${c.verified_at ? "проверено" : "не проверено"} · create: ${c.capabilities.create ? "да" : "нет"} · баланс ${c.balance.amount ?? "—"} ${c.balance.currency ?? ""}, ${c.balance.freshness}, ${c.balance.scope}; снимок ${c.balance.checked_at ? new Date(c.balance.checked_at * 1000).toLocaleString("ru") : "—"}. Баланс не разрешает расходы.`
        : "Нет выбранного подключения. Добавьте его в разделе «Хостинги».";
  };
  const loadConnections = async (): Promise<void> => {
    try {
      const r = await api.hostingConnections();
      if (!root.isConnected) return;
      connections = r.connections.filter((c) => c.provider === graph.provider);
      connectionError = "";
      const sel = get("connection_id") as HTMLSelectElement | null;
      if (sel) {
        const previous = sel.value || value("connection_id");
        sel.innerHTML = `<option value="">Выберите подключение</option>${connections.map((c) => `<option value="${c.id}"${c.credentials_configured && c.capabilities.create ? "" : " disabled"}>${esc(c.label)} · #${c.id}</option>`).join("")}`;
        sel.disabled = false;
        sel.value = previous;
        sel.onchange = connectionInfo;
      }
      connectionInfo();
    } catch (error) {
      connectionError = (error as Error).message;
      const info = root.querySelector<HTMLElement>("[data-connection-info]");
      if (info) info.textContent = connectionError;
    }
  };
  if (get("connection_id")) void loadConnections();
  root.querySelector<HTMLButtonElement>("[data-reload-connections]")?.addEventListener("click", () => {
    void loadConnections();
  });
  void api
    .installDefaults()
    .then((d) => {
      defaults = d;
      const el = root.querySelector<HTMLElement>("[data-defaults]");
      if (el)
        el.textContent = `Порт регистратора: ${d.proxy_port}. Персональное переопределение порта и секретов недоступно.`;
    })
    .catch((e: Error) => {
      defaultsError = e.message;
      const el = root.querySelector<HTMLElement>("[data-defaults]");
      if (el) el.textContent = `Порт регистратора недоступен: ${e.message}`;
    });
  const picker = root.querySelector<HTMLElement>("[data-text-picker]");
  if (picker)
    mountVersionPicker(picker, "text", (v) => {
      textVersion = v;
      const vars = root.querySelector<HTMLElement>("[data-install-vars]")!;
      vars.innerHTML = v
        ? `<h3>Переменные установки</h3><p class="help">Секреты не вводятся. Пустое поле наследует серверное значение.${policy ? " Политика не принимает vars: только значения профиля/регистратора." : ""}</p>` +
          parseVarsSchema(v.vars_schema)
            .filter(
              (d) =>
                !sensitiveVar(d) && !["node_id", "node_secret", "registry_url", "port", "proxy_port"].includes(d.name),
            )
            .map((d, i) =>
              policy
                ? `<p>${esc(d.name)}: наследование из профиля/регистратора</p>`
                : field(
                    prefix + "-var-" + i,
                    d.name,
                    `<input id="${prefix}-var-${i}" data-install-var="${esc(d.name)}" autocomplete="off" aria-describedby="${prefix}-error" placeholder="Наследовать">`,
                  ),
            )
            .join("")
        : "";
    });
  const fail = (text: string, el?: HTMLElement | null): never => {
    errorBox.textContent = text;
    errorBox.hidden = false;
    el?.setAttribute("aria-invalid", "true");
    el?.focus();
    throw new Error(text);
  };
  return {
    collect: () => {
      errorBox.hidden = true;
      const gate = runtimeGate(graph);
      if (gate) return fail(gate);
      const invalid = root.querySelector<HTMLInputElement>("input:invalid,select:invalid");
      if (invalid)
        return fail("Заполните обязательные поля; числа должны быть положительными целыми там, где указано.", invalid);
      const inputs: Record<string, unknown> = {};
      for (const [name, decl] of Object.entries(graph.inputs)) {
        if (name === "configuration_ref" && decl.type === "config") {
          const cfg: Record<string, string | number> = {};
          root.querySelectorAll<HTMLInputElement | HTMLSelectElement>("[data-config]").forEach((el) => {
            if (el.value) cfg[el.dataset.config!] = el.type === "number" ? Number(el.value) : el.value.trim();
          });
          for (const key of ["name", "cpu", "diskType", "os", "location"]) {
            const v = cfg[key];
            if (typeof v !== "string" || !v || [...v].length > (key === "name" ? 100 : 64))
              return fail(`Конфигурация ${key}: заполните строку до ${key === "name" ? 100 : 64} символов.`);
          }
          for (const key of ["cores", "ram", "disk"]) {
            if (!Number.isSafeInteger(cfg[key]) || Number(cfg[key]) <= 0)
              return fail(`Конфигурация ${key}: требуется положительное целое число.`);
          }
          inputs[name] = cfg;
          continue;
        }
        if (name === "installation_profile_version_id" && decl.type === "number") {
          if (!textVersion)
            return fail("Загрузите числовую версию текстового профиля; profile ID не является PK версии.", picker);
          inputs[name] = textVersion.profile_version_id;
          continue;
        }
        const el = get(name);
        if (name === "connection_id" && decl.type === "number") {
          const conn = connections?.find((c) => c.id === Number(el?.value));
          if (connectionError || !conn || !conn.credentials_configured || !conn.capabilities.create)
            return fail("Выберите своё доступное подключение Nodehost. " + connectionError, el);
        }
        if (!el || el.value === "") {
          if (decl.required)
            return fail(`Не задан обязательный вход ${name}; безопасного источника может не быть.`, el);
          continue;
        }
        const v = decl.type === "number" ? Number(el.value) : decl.type === "bool" ? el.value === "true" : el.value;
        if (decl.type === "number" && !Number.isFinite(v)) return fail(`Вход ${name}: укажите конечное число.`, el);
        inputs[name] = v;
      }
      const vars: Record<string, string> = {};
      if (textVersion) {
        if (!defaults) return fail("Дождитесь порта регистратора. " + defaultsError);
        const decls = parseVarsSchema(textVersion.vars_schema);
        for (const d of decls) {
          if (["port", "proxy_port"].includes(d.name)) {
            if (policy && d.default && Number(d.default) !== defaults.proxy_port)
              return fail(
                "Порт текстового профиля расходится с портом регистратора; исправьте профиль до включения политики.",
              );
            if (!policy) vars[d.name] = String(defaults.proxy_port);
          }
          if (isSecretVar(d) && d.name === "proxy_secret" && !d.default && !defaults.proxy_secret_configured)
            return fail(
              "Для proxy_secret нет серверного источника. Настройте его у регистратора, секрет здесь не вводится.",
            );
          if (sensitiveVar(d) && !d.default && !["proxy_secret", "authkey", "node_secret"].includes(d.name))
            return fail(
              `Для секретной переменной ${d.name} не подтверждён серверный источник. Запуск без ввода секретов заблокирован; настройте источник в текстовом профиле.`,
            );
        }
        root.querySelectorAll<HTMLInputElement>("[data-install-var]").forEach((el) => {
          if (el.value !== "") vars[el.dataset.installVar!] = el.value;
        });
      }
      return { inputs, vars };
    },
    installedTextVersion: () => textVersion,
  };
}
function configFields(prefix: string, initial: Record<string, unknown>): string {
  const fields: [string, string, string, number][] = [
    ["name", "Имя VPS", "text", 100],
    ["cpu", "CPU (код)", "text", 64],
    ["cores", "Ядра", "number", 0],
    ["ram", "RAM · ГБ", "number", 0],
    ["disk", "Диск · ГБ", "number", 0],
    ["diskType", "Тип диска (код)", "text", 64],
    ["os", "ОС (код)", "text", 64],
    ["location", "Локация (код)", "text", 64],
    ["networkSpeed", "Скорость сети · необязательно", "text", 64],
  ];
  return (
    fields
      .map(([name, label, type, max]) =>
        field(
          `${prefix}-cfg-${name}`,
          label,
          `<input id="${prefix}-cfg-${name}" data-config="${name}" type="${type}" value="${esc(String(initial[name] ?? ""))}" ${name === "networkSpeed" ? "" : "required"} ${max ? `maxlength="${max}"` : 'min="1" step="1"'} aria-describedby="${prefix}-error">`,
        ),
      )
      .join("") +
    field(
      prefix + "-period",
      "Период · необязательно",
      `<select id="${prefix}-period" data-config="period">${["", "1d", "3d", "1w", "1m", "3m", "6m", "1y"].map((v) => `<option value="${v}"${initial.period === v ? " selected" : ""}>${v || "Не задан"}</option>`).join("")}</select>`,
    )
  );
}
