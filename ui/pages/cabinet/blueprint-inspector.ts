import { esc } from "../../lib/dom";
import type { Blueprint, BlueprintBinding, BlueprintBlock, BlueprintNode, BlueprintType } from "./api";
import { blockFor, reachable, TITLES, TYPES } from "./blueprint-model";
import type { Draft } from "./blueprint-model";

interface InspectorOptions {
  change: (next: Draft) => void;
  connect: (from: string, port: string, to: string) => void;
  remove: (id: string) => void;
  move: (x: number, y: number) => void;
  error: (message: string) => void;
}
const option = (value: string, label: string, current: string): string =>
  `<option value="${esc(value)}"${value === current ? " selected" : ""}>${esc(label)}</option>`;
const field = (id: string, label: string, control: string): string =>
  `<div class="field"><label for="${id}">${esc(label)}</label>${control}</div>`;
function input(id: string, value: string | number, extra = ""): string {
  return `<input id="${id}" ${extra.includes("type=") ? "" : 'type="text"'} value="${esc(String(value))}" aria-describedby="bp-message" ${extra}>`;
}

export function mountInspector(
  root: HTMLElement,
  draft: Draft,
  selected: string,
  blocks: BlueprintBlock[],
  opts: InspectorOptions,
): void {
  const g = draft.graph,
    n = g.nodes.find((x) => x.id === selected),
    b = n && blockFor(blocks, n.type);
  const nodes = (value: string): string =>
    option("", "Не выбран", value) + g.nodes.map((n) => option(n.id, n.id, value)).join("");
  const patchGraph = (patch: Partial<Blueprint>): void => opts.change({ ...draft, graph: { ...g, ...patch } });
  const patchNode = (next: BlueprintNode): void =>
    patchGraph({ nodes: g.nodes.map((x) => (x.id === next.id ? next : x)) });
  const bind = (name: string, binding?: BlueprintBinding): void => {
    if (!n) return;
    patchNode({
      ...n,
      in: {
        ...Object.fromEntries(Object.entries(n.in).filter(([key]) => key !== name)),
        ...(binding ? { [name]: binding } : {}),
      },
    });
  };
  root.innerHTML =
    `<h2 tabindex="-1" id="bp-inspector-title">${n ? esc(TITLES[n.type] || n.type) : "Настройки графа"}</h2>
    <button class="btn sm" type="button" data-settings>Настройки графа</button>` +
    (!n
      ? `
    ${field("bp-profile-name", "Имя профиля", input("bp-profile-name", draft.name, 'maxlength="100" required'))}
    ${field("bp-name", "Имя схемы (содержимое версии)", input("bp-name", g.name, 'maxlength="100" required'))}
    ${field("bp-description", "Описание профиля", `<textarea id="bp-description" rows="3" aria-describedby="bp-message">${esc(draft.description)}</textarea>`)}
    ${field("bp-provider", "Провайдер", `<select id="bp-provider">${["nodehost", "beget"].map((v) => option(v, v, g.provider)).join("")}</select>`)}
    ${field("bp-entry-initial", "Первичное создание · initial", `<select id="bp-entry-initial">${nodes(g.entries.initial)}</select>`)}
    ${field("bp-entry-observe", "Наблюдение · observe", `<select id="bp-entry-observe">${nodes(g.entries.observe)}</select>`)}
    <h3>Объявления входов · ${Object.keys(g.inputs).length}/32</h3>
    ${
      Object.entries(g.inputs)
        .map(
          ([name, decl]) =>
            `<div class="blueprint-declaration"><span class="mono">${esc(name)} · ${decl.type} · ${decl.required ? "обязательный" : "необязательный"}</span><button type="button" class="btn sm danger" data-remove-input="${esc(name)}" aria-label="Удалить вход ${esc(name)}">Удалить</button></div>`,
        )
        .join("") || "<p>Входы запуска не объявлены.</p>"
    }
    ${field("bp-input-name", "Имя нового входа", input("bp-input-name", "", 'pattern="[a-z][a-z0-9_]{0,63}" maxlength="64"'))}
    ${field("bp-input-type", "Тип входа", `<select id="bp-input-type">${TYPES.map((v) => option(v, v, "string")).join("")}</select>`)}
    <label class="check"><input type="checkbox" id="bp-input-required"><span class="box"></span>Обязательный вход</label>
    <button type="button" class="btn" data-add-input>Добавить вход</button>
    <p class="help">Переименование: удалите вход и добавьте новый. Затронутые привязки будут явно сняты. Серверные и секретные handles вводом JSON недоступны.</p>`
      : `
    <p class="mono">${esc(n.id)} · ${esc(n.type)}</p>
    <h3>Параметры</h3>
    ${
      (b?.params_schema || [])
        .map((p) =>
          field(
            `bp-param-${p.name}`,
            p.name,
            p.enum
              ? `<select id="bp-param-${p.name}" data-param="${p.name}">${option("", "Не задано", String(n.params[p.name] ?? ""))}${p.enum.map((v) => option(v, v, String(n.params[p.name] ?? ""))).join("")}</select>`
              : input(
                  `bp-param-${p.name}`,
                  n.params[p.name] ?? "",
                  `data-param="${p.name}" ${p.type === "integer" ? 'type="number" step="1" min="1"' : 'type="text"'} ${p.required ? "required" : ""}`,
                ),
          ),
        )
        .join("") || "<p>Нет параметров.</p>"
    }
    <h3>Входные данные</h3>
    ${(b?.ports.inputs || []).map((p) => bindingHTML(g, n, p.name, p.type, blocks)).join("") || "<p>Нет входных данных.</p>"}
    <h3>Повтор блока</h3>
    ${b?.retry_class === "safe-read" ? `<label class="check"><input id="bp-retry" type="checkbox"${n.retry ? " checked" : ""}><span class="box"></span>Задать safe-read retry</label>${n.retry ? field("bp-attempts", "Попыток · 1–5", input("bp-attempts", n.retry.max_attempts, 'type="number" min="1" max="5" step="1"')) + field("bp-backoff", "Задержка · 1–300 секунд", input("bp-backoff", n.retry.backoff_seconds, 'type="number" min="1" max="300" step="1"')) : ""}<p class="help">Это параметры схемы, не гарантия точного расписания движка.</p>` : `<p>Повтор блока запрещён.</p>${n.retry ? '<button type="button" class="btn danger" data-remove-retry>Удалить запрещённый retry</button>' : ""}`}
    <h3>Соединить с exec</h3>
    ${field("bp-connect-port", "Контрольный выход", `<select id="bp-connect-port">${(b?.ports.control || []).map((p) => option(p, p, "")).join("")}</select>`)}
    ${field("bp-connect-target", "Следующий блок", `<select id="bp-connect-target">${nodes("")}</select>`)}
    <button type="button" class="btn" data-connect${b?.ports.control?.length ? "" : " disabled"}>Соединить</button>
    <h3>Положение · только в интерфейсе</h3><div class="blueprint-toolbar">${[
      ["←", -10, 0],
      ["→", 10, 0],
      ["↑", 0, -10],
      ["↓", 0, 10],
    ]
      .map(
        ([label, x, y]) =>
          `<button type="button" class="btn" data-move-x="${x}" data-move-y="${y}" aria-label="Переместить ${esc(n.id)} ${label}">${label}</button>`,
      )
      .join("")}</div>
    <button type="button" class="btn danger" data-delete-node>Удалить блок</button>`);
  const el = <T extends HTMLElement>(id: string): T => root.querySelector<T>(`#${id}`)!;
  root.onchange = (e) => {
    const target = e.target as HTMLInputElement;
    if (target.validity && !target.validity.valid) {
      target.reportValidity();
      return;
    }
    if (!n) {
      if (target.id === "bp-name") patchGraph({ name: target.value });
      if (target.id === "bp-profile-name") opts.change({ ...draft, name: target.value });
      if (target.id === "bp-description") opts.change({ ...draft, description: target.value });
      if (target.id === "bp-provider") patchGraph({ provider: target.value as Blueprint["provider"] });
      if (target.id.startsWith("bp-entry-"))
        patchGraph({ entries: { ...g.entries, [target.id.slice(9)]: target.value } });
      return;
    }
    if (target.dataset.param) {
      const p = b?.params_schema?.find((p) => p.name === target.dataset.param);
      const value = p?.type === "integer" ? Number(target.value) : target.value;
      if (p?.type === "integer" && (!Number.isSafeInteger(value) || Number(value) <= 0)) {
        opts.error("Параметр должен быть положительным целым числом.");
        return;
      }
      patchNode({ ...n, params: { ...n.params, [target.dataset.param]: value } });
    }
    if (target.dataset.binding) {
      const name = target.dataset.binding;
      bind(
        name,
        target.value === "literal"
          ? { literal: primitiveDefault(b?.ports.inputs?.find((p) => p.name === name)?.type) }
          : target.value
            ? { ref: target.value }
            : undefined,
      );
    }
    if (target.dataset.literal) {
      const type = b?.ports.inputs?.find((p) => p.name === target.dataset.literal)?.type;
      if (type === "number" && (!target.value || !Number.isFinite(target.valueAsNumber))) {
        opts.error("Укажите конечное число.");
        return;
      }
      bind(target.dataset.literal, {
        literal: type === "bool" ? target.checked : type === "number" ? target.valueAsNumber : target.value,
      });
    }
    if (target.id === "bp-retry") {
      const { retry: _retry, ...rest } = n;
      patchNode(target.checked ? { ...n, retry: { max_attempts: 1, backoff_seconds: 1 } } : rest);
    }
    if (target.id === "bp-attempts" || target.id === "bp-backoff")
      patchNode({
        ...n,
        retry: {
          max_attempts: Number(el<HTMLInputElement>("bp-attempts").value),
          backoff_seconds: Number(el<HTMLInputElement>("bp-backoff").value),
        },
      });
  };
  root.onclick = (e) => {
    const t = e.target as HTMLElement;
    if (t.closest("[data-remove-retry]") && n) {
      const { retry: _retry, ...rest } = n;
      patchNode(rest);
      return;
    }
    if (t.closest("[data-connect]")) {
      const to = el<HTMLSelectElement>("bp-connect-target").value;
      if (n && to) opts.connect(n.id, el<HTMLSelectElement>("bp-connect-port").value, to);
      else opts.error("Выберите следующий блок.");
    }
    if (t.closest("[data-delete-node]") && n) opts.remove(n.id);
    const move = t.closest<HTMLElement>("[data-move-x]");
    if (move) opts.move(Number(move.dataset.moveX), Number(move.dataset.moveY));
    if (t.closest("[data-add-input]")) {
      const nameEl = el<HTMLInputElement>("bp-input-name"),
        name = nameEl.value;
      if (
        !/^[a-z][a-z0-9_]{0,63}$/.test(name) ||
        Object.prototype.hasOwnProperty.call(g.inputs, name) ||
        Object.keys(g.inputs).length >= 32
      ) {
        opts.error("Укажите уникальное имя входа: a–z, 0–9, _, до 64 символов; максимум 32 входа.");
        nameEl.focus();
        return;
      }
      patchGraph({
        inputs: {
          ...g.inputs,
          [name]: {
            type: el<HTMLSelectElement>("bp-input-type").value as BlueprintType,
            required: el<HTMLInputElement>("bp-input-required").checked,
          },
        },
      });
    }
    const remove = t.closest<HTMLElement>("[data-remove-input]");
    if (remove) {
      const name = remove.dataset.removeInput!;
      if (
        !window.confirm(
          `Удалить вход ${name}? Все ссылки input.${name} будут сняты; обязательные данные нужно задать заново.`,
        )
      )
        return;
      patchGraph({
        inputs: Object.fromEntries(Object.entries(g.inputs).filter(([k]) => k !== name)),
        nodes: g.nodes.map((node) => ({
          ...node,
          in: Object.fromEntries(Object.entries(node.in).filter(([, binding]) => binding.ref !== `input.${name}`)),
        })),
      });
    }
  };
}
function primitiveDefault(type?: BlueprintType): string | number | boolean {
  return type === "bool" ? false : type === "number" ? 0 : "";
}
function bindingHTML(
  g: Blueprint,
  n: BlueprintNode,
  name: string,
  type: BlueprintType,
  blocks: BlueprintBlock[],
): string {
  const value = n.in[name];
  const current = value?.ref ?? (value && "literal" in value ? "literal" : "");
  const refs = [
    ...Object.entries(g.inputs)
      .filter(([, v]) => v.type === type)
      .map(([key]) => `input.${key}`),
    ...g.nodes
      .filter((x) => x.id !== n.id)
      .flatMap((x) =>
        (blockFor(blocks, x.type)?.ports.outputs || []).filter((p) => p.type === type).map((p) => `${x.id}.${p.name}`),
      ),
    ...(type === "server" && reachable(g, g.entries.observe).has(n.id) && !reachable(g, g.entries.initial).has(n.id)
      ? ["run.current_server"]
      : []),
  ];
  const primitive = ["string", "number", "bool"].includes(type) && !/secret|password|token|credential|key/i.test(name);
  return `<div class="blueprint-binding">${field(`bp-binding-${name}`, `${name} · ${type}`, `<select id="bp-binding-${name}" data-binding="${name}" aria-describedby="bp-message">${option("", "Не задано", current)}${refs.map((ref) => option(ref, `${ref.startsWith("input.") ? "Вход запуска" : ref.startsWith("run.") ? "Текущее состояние" : "Выход блока"} · ${ref}`, current)).join("")}${primitive ? option("literal", "Константа (не секрет)", current) : ""}${current && current !== "literal" && !refs.includes(current) ? option(current, `Недоступная привязка · ${current}`, current) : ""}</select>`)}
    ${current === "literal" && primitive ? field(`bp-literal-${name}`, `Значение ${name}`, `<input id="bp-literal-${name}" data-literal="${name}" type="${type === "bool" ? "checkbox" : type === "number" ? "number" : "text"}" ${type === "bool" ? (value?.literal ? "checked" : "") : `value="${esc(String(value?.literal ?? ""))}"`} ${type === "number" ? 'step="any"' : ""} aria-describedby="bp-message">`) : ""}
    <span class="help">${value?.ref ? esc(value.ref) : "Ровно одна привязка ref или literal. Доступность ветки проверяет сервер."}</span></div>`;
}
