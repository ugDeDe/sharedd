import { esc } from "../../lib/dom";
import { api } from "./api";
import type { APIError, BlueprintBlock, BlueprintDiagnostic, GraphProfilePayload, ProfileVersion } from "./api";
import {
  blockFor,
  bytes,
  clone,
  connectError,
  edgeCount,
  emptyGraph,
  layout,
  localDiagnostics,
  positiveID,
  readVersion,
  runtimeGate,
  stableJSON,
  template,
  TITLES,
  EFFECTS,
} from "./blueprint-model";
import type { Draft } from "./blueprint-model";
import { BlueprintCanvas } from "./blueprint-canvas";
import { mountInspector } from "./blueprint-inspector";
import { openRunReview } from "./blueprint-runs";

const PANEL_TAB_SELECTOR = '.blueprint-tabs button[role="tab"][data-bp-tab]';
let active: Editor | null = null;
let editorHash = "";
let leaveDialog: Promise<boolean> | null = null;
export function isBlueprintRoute(hash = location.hash): boolean {
  return /^#\/profiles\/(?:editor\/(?:new|[1-9]\d*)(?:\?.*)?|runs\/[1-9]\d*)$/.test(hash);
}
export function leaveBlueprintEditor(): void {
  active?.dispose();
  active = null;
  editorHash = "";
}
export function openBlueprintEditor(root: HTMLElement): void {
  if (active && editorHash === location.hash) return;
  leaveBlueprintEditor();
  editorHash = location.hash;
  active = new Editor(root);
  void active.load();
}
export async function canLeaveBlueprint(nextHash: string): Promise<boolean> {
  if (active?.working) return false;
  if (!active || nextHash === editorHash || !active.dirty) return true;
  if (leaveDialog) return leaveDialog;
  leaveDialog = new Promise<boolean>((resolve) => {
    const dialog = document.createElement("dialog");
    dialog.className = "blueprint-dialog";
    dialog.setAttribute("aria-label", "Несохранённые изменения графа");
    dialog.innerHTML = `<h2>Есть несохранённые изменения</h2><p>Сохранить новую версию перед уходом? Сохранение не запускает граф.</p><div class="blueprint-toolbar"><button class="btn" data-stay autofocus>Остаться</button><button class="btn" data-discard>Не сохранять</button><button class="btn primary" data-save>Сохранить версию</button></div>`;
    const finish = (ok: boolean): void => {
      dialog.close();
      dialog.remove();
      leaveDialog = null;
      resolve(ok);
    };
    dialog.oncancel = (e) => {
      e.preventDefault();
      finish(false);
    };
    dialog.querySelector("[data-stay]")!.addEventListener("click", () => finish(false));
    dialog.querySelector("[data-discard]")!.addEventListener("click", () => finish(true));
    dialog.querySelector<HTMLButtonElement>("[data-save]")!.onclick = async (e) => {
      (e.currentTarget as HTMLButtonElement).disabled = true;
      const ok = await active?.save();
      finish(!!ok);
    };
    document.body.appendChild(dialog);
    dialog.showModal();
  });
  return leaveDialog;
}
window.addEventListener("beforeunload", (e) => {
  if (active?.dirty) {
    e.preventDefault();
    e.returnValue = "";
  }
});

class Editor {
  private draft: Draft = { graph: emptyGraph(), name: "Новая схема", description: "", positions: {} };
  private baseline = "";
  private blocks: BlueprintBlock[] = [];
  private selected = "";
  private undo: Draft[] = [];
  private redo: Draft[] = [];
  private diagnostics: BlueprintDiagnostic[] = [];
  private valid = false;
  private busy = false;
  private disposed = false;
  private profileID = 0;
  private loaded: ProfileVersion | null = null;
  private conflict = false;
  private uncertain = false;
  private enabled = false;
  private limitReached = false;
  private panel = matchMedia("(max-width:720px)").matches ? "blocks" : "canvas";
  private canvas: BlueprintCanvas | null = null;
  private pendingInput = false;
  private pendingWrite: GraphProfilePayload | null = null;
  constructor(private root: HTMLElement) {}
  get dirty(): boolean {
    return this.pendingInput || (!!this.baseline && this.content() !== this.baseline);
  }
  get working(): boolean {
    return this.busy;
  }
  dispose(): void {
    this.disposed = true;
    this.canvas?.dispose();
    this.root.onclick = null;
    this.root.onkeydown = null;
    this.root.oninput = null;
    this.root.replaceChildren();
  }
  private el<T extends HTMLElement = HTMLElement>(selector: string): T {
    return this.root.querySelector<T>(selector)!;
  }
  private content(): string {
    return JSON.stringify({ graph: this.draft.graph, name: this.draft.name, description: this.draft.description });
  }
  private say(text: string, error = false): void {
    const el = this.root.querySelector<HTMLElement>("#bp-message");
    if (el) {
      el.textContent = text;
      el.className = `banner ${error ? "bad" : "info"}`;
    }
  }
  async load(): Promise<void> {
    this.root.innerHTML = `<p aria-busy="true">Загрузка блоков и версии…</p><div class="skeleton skel-block"></div>`;
    try {
      const path = location.hash.slice(2).split("?")[0].split("/"),
        query = new URLSearchParams(location.hash.split("?")[1] || "");
      const [registry, m] = await Promise.all([api.blueprintBlocks(), api.myProfiles()]);
      if (this.disposed) return;
      this.blocks = registry.blocks;
      this.enabled = m.enabled;
      this.limitReached = m.profiles.length >= m.max;
      const id = positiveID(path[2] || "");
      if (path[2] !== "new" && !id) throw new Error("Номер профиля недопустим.");
      let name = "",
        description = "",
        graph = template(query.get("starter") || "empty");
      if (id) {
        const version = positiveID(query.get("version") || "");
        const v = await readVersion(id, version);
        if (this.disposed) return;
        if (v.format !== "graph" || !v.blueprint)
          throw new Error("Эта версия — текстовый профиль. Откройте его из списка профилей.");
        const p = [...m.profiles, ...m.published].find((p) => p.id === id);
        graph = clone(v.blueprint);
        name = p?.name || graph.name;
        description = p?.description || "";
        if (query.get("copy") === "1" || !m.profiles.some((p) => p.id === id)) name = `${name} (копия)`.slice(0, 100);
        else {
          this.profileID = id;
          this.loaded = v;
        }
      }
      this.draft = { graph, name: name || graph.name, description, positions: layout(graph) };
      this.baseline = this.loaded
        ? this.content()
        : JSON.stringify({ graph: emptyGraph(), name: "Новая схема", description: "" });
      this.shell();
      this.paint();
      this.el("h2").focus({ preventScroll: true });
    } catch (error) {
      if (this.disposed) return;
      this.root.innerHTML = `<div class="banner bad" role="alert">Версия или блоки недоступны: ${esc((error as Error).message)}</div><a class="btn" href="#/profiles">К профилям</a> <button class="btn" type="button" data-load>Повторить GET</button>`;
      this.el("[data-load]").onclick = () => {
        void this.load();
      };
    }
  }
  private shell(): void {
    this.root.innerHTML = `<div class="blueprint-toolbar"><a class="btn ghost" href="#/profiles">← Мои профили</a><h2 tabindex="-1">Редактор графа</h2><span id="bp-state" role="status"></span><button class="btn" data-validate>Проверить</button><button class="btn primary" data-save>Сохранить версию</button><button class="btn" data-run aria-describedby="bp-gate">Запустить…</button></div>
      <p id="bp-gate" class="banner info"></p><div id="bp-message" class="banner info" role="status" aria-live="polite">Сохранение создаёт неизменяемую версию. Оно не запускает граф и не включает политику.</div>
      <div id="bp-conflict" hidden class="banner warn"><span>Профиль изменён в другом окне. Черновик сохранён в памяти.</span><button class="btn" data-latest>Загрузить актуальную</button><button class="btn" data-copy>Сохранить отдельной копией</button></div>
      <div id="bp-uncertain" hidden class="banner warn"><span>Результат сохранения неизвестен. Повторная запись запрещена до сверки.</span><button class="btn" data-reconcile>Сверить GET</button></div>
      <div class="blueprint-toolbar"><label for="bp-starter">Шаблоны</label><select id="bp-starter"><option value="">Выберите…</option><option value="nodehost">Nodehost · замена VPS</option><option value="beget">Beget · IPv4 или замена VPS · только проектирование</option><option value="empty">Пустая схема</option></select><button class="btn" data-template>Применить как черновик</button><button class="btn" data-undo>Отменить</button><button class="btn" data-redo>Повторить</button><button class="btn" data-fit>Показать всё</button><button class="btn" data-zoom="0.8" aria-label="Уменьшить схему">−</button><span id="bp-zoom">100%</span><button class="btn" data-zoom="1.25" aria-label="Увеличить схему">+</button><span id="bp-counts"></span></div>
      <nav class="nd-tabs blueprint-tabs" role="tablist" aria-label="Панели редактора">${[
        ["canvas", "Схема"],
        ["blocks", "Блоки"],
        ["inspector", "Свойства"],
        ["diagnostics", "Проверка"],
      ]
        .map(
          ([key, label]) =>
            `<button type="button" role="tab" id="bp-tab-${key}" data-bp-tab="${key}" aria-controls="bp-panel-${key}" aria-selected="${this.panel === key}" tabindex="${this.panel === key ? 0 : -1}">${label}</button>`,
        )
        .join("")}</nav>
      <div class="blueprint-workspace"><aside class="blueprint-panel blueprint-palette" id="bp-panel-blocks" role="tabpanel" aria-labelledby="bp-tab-blocks"><h3>Добавить блок</h3><label for="bp-search">Поиск по названию / типу</label><input id="bp-search" type="search"><div id="bp-palette"></div><h3>Блоки схемы</h3><div id="bp-node-list"></div><h3>Контрольные связи</h3><div id="bp-connections"></div></aside>
      <section class="blueprint-canvas" id="bp-panel-canvas" role="tabpanel" aria-labelledby="bp-tab-canvas" tabindex="0" aria-label="Схема. Стрелки перемещают вид; на выбранном блоке — блок. Shift — шаг 50."></section>
      <aside class="blueprint-panel blueprint-inspector" id="bp-panel-inspector" role="tabpanel" aria-labelledby="bp-tab-inspector"></aside></div>
      <section class="blueprint-panel blueprint-diagnostics" id="bp-panel-diagnostics" role="tabpanel" aria-labelledby="bp-tab-diagnostics"><h3>Проверка схемы</h3><div id="bp-diagnostics"></div></section>`;
    this.canvas = new BlueprintCanvas(this.el(".blueprint-canvas"), {
      select: (id, focus) => this.select(id, focus),
      connect: (from, port, to) => this.connect(from, port, to),
      move: (positions) => this.change({ ...this.draft, positions }),
      announce: (text) => this.say(text),
    });
    this.root.onclick = (e) => {
      void this.click(e).catch((error: Error) => this.say(error.message, true));
    };
    this.el<HTMLInputElement>("#bp-search").oninput = () => this.palette();
    this.root.onkeydown = (e) => this.key(e);
    this.root.oninput = (e) => {
      if ((e.target as HTMLElement).closest(".blueprint-inspector")) {
        this.pendingInput = true;
        this.valid = false;
        this.diagnostics = [];
        this.findings();
        this.status();
      }
    };
  }
  private paint(): void {
    const activeElement = this.root.contains(document.activeElement) ? (document.activeElement as HTMLElement) : null;
    const focused = activeElement?.id
      ? `#${CSS.escape(activeElement.id)}`
      : activeElement?.dataset.dragNode
        ? `[data-drag-node="${CSS.escape(activeElement.dataset.dragNode)}"]`
        : activeElement?.hasAttribute("data-move-x")
          ? `[data-move-x="${activeElement.dataset.moveX}"][data-move-y="${activeElement.dataset.moveY}"]`
          : "";
    this.canvas?.render(this.draft.graph, this.blocks, this.draft.positions, this.selected, this.diagnostics);
    this.inspector();
    this.palette();
    this.lists();
    this.status();
    this.findings();
    this.panels();
    if (focused) this.root.querySelector<HTMLElement>(focused)?.focus({ preventScroll: true });
  }
  private status(): void {
    const gate = runtimeGate(this.draft.graph);
    this.el("#bp-gate").textContent =
      gate ||
      "Nodehost. Проверка схемы не гарантирует выполнение. Ошибка здоровья или установки не создаёт новый сервер. observe.arm не включает новую политику автоматически.";
    this.el("#bp-gate").className = `banner ${gate ? "warn" : "info"}`;
    this.el("#bp-state").textContent =
      `${this.draft.name} · ${this.draft.graph.provider} · ${this.loaded ? `версия ${this.loaded.version} · PK ${this.loaded.profile_version_id}` : "локальный черновик"} · ${this.dirty ? "Есть изменения" : this.loaded ? "Сохранено" : "Не сохранён"}`;
    this.el<HTMLButtonElement>("[data-save]").disabled =
      this.busy || this.uncertain || !this.enabled || (!this.profileID && this.limitReached);
    this.el<HTMLButtonElement>("[data-validate]").disabled = this.busy;
    this.el<HTMLButtonElement>("[data-run]").disabled =
      this.busy || this.dirty || !this.loaded || !!gate || this.uncertain;
    this.el<HTMLButtonElement>("[data-undo]").disabled = this.busy || !this.undo.length;
    this.el<HTMLButtonElement>("[data-redo]").disabled = this.busy || !this.redo.length;
    this.el("#bp-counts").textContent =
      `${this.draft.graph.nodes.length}/128 блоков · ${edgeCount(this.draft.graph)}/256 связей · ${bytes(this.draft.graph)} байт`;
    this.el("#bp-conflict").hidden = !this.conflict;
    this.el("#bp-uncertain").hidden = !this.uncertain;
    this.el(".blueprint-workspace").toggleAttribute("inert", this.busy);
    if (!this.enabled || (!this.profileID && this.limitReached))
      this.say(
        !this.enabled
          ? "Пользовательские профили отключены: доступно чтение."
          : "Достигнут лимит профилей. Сохранение новой копии недоступно.",
        true,
      );
  }
  private change(next: Draft): void {
    if (this.busy) return;
    if (edgeCount(next.graph) > 256 || bytes(next.graph) > 256 * 1024) {
      this.say("Лимит схемы: 256 связей и 256 КиБ UTF-8. Сократите содержимое.", true);
      return;
    }
    this.undo = [...this.undo.slice(-79), clone(this.draft)];
    this.redo = [];
    this.draft = next;
    this.pendingInput = false;
    this.valid = false;
    this.diagnostics = [];
    this.paint();
  }
  private inspector(): void {
    mountInspector(this.el(".blueprint-inspector"), this.draft, this.selected, this.blocks, {
      change: (d) => this.change(d),
      connect: (from, port, to) => this.connect(from, port, to),
      remove: (id) => this.remove(id),
      move: (x, y) => this.canvas?.nudge(x, y),
      error: (text) => this.say(text, true),
    });
  }
  private select(id: string, focus = false): void {
    this.selected = id;
    this.canvas?.select(id, focus);
    this.inspector();
    this.lists();
  }
  private palette(): void {
    const q = this.el<HTMLInputElement>("#bp-search").value.toLowerCase();
    this.el("#bp-palette").innerHTML =
      this.blocks
        .filter(
          (b) =>
            b.capabilities.includes(this.draft.graph.provider) &&
            `${TITLES[b.type]} ${b.type}`.toLowerCase().includes(q),
        )
        .map(
          (b) =>
            `<div class="blueprint-palette-item"><b>${esc(TITLES[b.type] || b.type)}</b><span class="mono">${esc(b.type)}</span><span>${EFFECTS[b.effects]}</span><button type="button" class="btn sm" data-add="${esc(b.type)}"${this.draft.graph.nodes.length >= 128 ? " disabled" : ""}>Добавить</button></div>`,
        )
        .join("") || "<p>Подходящих блоков нет. Очистите поиск.</p>";
  }
  private lists(): void {
    this.el("#bp-node-list").innerHTML =
      this.draft.graph.nodes
        .map(
          (n) =>
            `<button type="button" class="btn blueprint-list-node" data-select="${esc(n.id)}" aria-pressed="${n.id === this.selected}">${esc(n.id)} · ${esc(TITLES[n.type] || n.type)}</button>`,
        )
        .join("") || "<p>Добавьте блок или выберите шаблон.</p>";
    this.el("#bp-connections").innerHTML =
      this.draft.graph.edges
        .map(
          (e, i) =>
            `<div class="blueprint-connection"><button class="btn ghost sm" type="button" data-select="${esc(e.from)}">${esc(e.from)} · ${esc(e.port)} → ${esc(e.to)} · exec</button><button class="btn sm danger" type="button" data-remove-edge="${i}" aria-label="Удалить связь ${esc(e.from)} ${esc(e.port)} ${esc(e.to)}">Удалить</button></div>`,
        )
        .join("") || "<p>Связей пока нет. Выберите блок → выход и следующий блок в свойствах.</p>";
  }
  private findings(): void {
    this.el("#bp-diagnostics").innerHTML = this.diagnostics.length
      ? `<ol>${this.diagnostics.map((d, i) => `<li><button class="btn ghost" type="button" data-diagnostic="${i}">${esc(d.node_id || "Граф")} ${esc(d.port || "")} · ${esc(d.message)} <span class="mono">${esc(d.code)}</span></button></li>`).join("")}</ol>`
      : `<p>${this.valid ? "Схема корректна. Это не разрешение на выполнение." : "Проверка ещё не выполнена."}</p>`;
  }
  private panels(): void {
    this.root.dataset.panel = this.panel;
    this.root.querySelectorAll<HTMLButtonElement>(PANEL_TAB_SELECTOR).forEach((el) => {
      const on = el.dataset.bpTab === this.panel;
      el.setAttribute("aria-selected", String(on));
      el.tabIndex = on ? 0 : -1;
    });
    if (this.panel === "canvas") this.canvas?.refresh();
  }
  private connect(from: string, port: string, to: string): void {
    const g = this.draft.graph;
    const n = g.nodes.find((n) => n.id === from);
    if (!n || !blockFor(this.blocks, n.type)?.ports.control?.includes(port) || !g.nodes.some((n) => n.id === to))
      return;
    const error = connectError(g, { from, port, to });
    if (error) {
      this.say(error, true);
      return;
    }
    const old = g.edges.find((e) => e.from === from && e.port === port);
    if (old && !window.confirm(`Заменить связь ${from} · ${port} → ${old.to} на ${to}?`)) return;
    this.change({
      ...this.draft,
      graph: { ...g, edges: [...g.edges.filter((e) => !(e.from === from && e.port === port)), { from, port, to }] },
    });
    this.say("Контрольная связь добавлена.");
  }
  private remove(id: string): void {
    const g = this.draft.graph,
      edges = g.edges.filter((e) => e.from === id || e.to === id);
    const refs = g.nodes.flatMap((n) =>
      Object.entries(n.in)
        .filter(([, b]) => b.ref?.startsWith(id + "."))
        .map(([port]) => `${n.id}.${port}`),
    );
    const roles = Object.entries(g.entries)
      .filter(([, value]) => value === id)
      .map(([key]) => key);
    if (
      !window.confirm(
        `Удалить блок ${id}? Связи: ${edges.map((e) => `${e.from}·${e.port}→${e.to}`).join(", ") || "нет"}; привязки: ${refs.join(", ") || "нет"}; входные роли: ${roles.join(", ") || "нет"}. Они будут сняты, без переподключения.`,
      )
    )
      return;
    this.selected = "";
    this.change({
      ...this.draft,
      positions: Object.fromEntries(Object.entries(this.draft.positions).filter(([key]) => key !== id)),
      graph: {
        ...g,
        entries: {
          initial: g.entries.initial === id ? "" : g.entries.initial,
          observe: g.entries.observe === id ? "" : g.entries.observe,
        },
        edges: g.edges.filter((e) => e.from !== id && e.to !== id),
        nodes: g.nodes
          .filter((n) => n.id !== id)
          .map((n) => ({
            ...n,
            in: Object.fromEntries(Object.entries(n.in).filter(([, b]) => !b.ref?.startsWith(id + "."))),
          })),
      },
    });
    this.say("Блок удалён. Требуемые привязки и входы задайте заново.");
    this.el<HTMLElement>("[data-select], [data-add]")?.focus();
  }
  private async validate(): Promise<boolean> {
    this.diagnostics = localDiagnostics(this.draft.graph);
    if (bytes({ format: "graph", blueprint: this.draft.graph }) > 256 * 1024)
      this.diagnostics = [
        ...this.diagnostics,
        { code: "envelope_limit", message: "Полное тело проверки превышает 256 КиБ UTF-8." },
      ];
    if (!this.diagnostics.length) {
      const result = await api.validateBlueprint(this.draft.graph);
      this.diagnostics = result.diagnostics;
      this.valid = result.valid;
    } else this.valid = false;
    this.findings();
    this.say(
      this.valid
        ? "Схема корректна; выполнение разрешается отдельно."
        : `Схема требует исправлений: ${this.diagnostics.length}.`,
      !this.valid,
    );
    if (!this.valid) this.el<HTMLElement>("[data-diagnostic]")?.focus();
    return this.valid;
  }
  async save(copy = false): Promise<boolean> {
    const focused = document.activeElement;
    if (focused instanceof HTMLElement && this.root.contains(focused)) focused.blur();
    if (this.busy || this.uncertain || !this.enabled || (!this.profileID && this.limitReached)) return false;
    const invalid = this.root.querySelector<HTMLInputElement>(".blueprint-inspector input:invalid");
    if (invalid) {
      invalid.reportValidity();
      invalid.focus();
      return false;
    }
    this.busy = true;
    this.status();
    let writing = false,
      acknowledged = false;
    try {
      if (!this.draft.name.trim() || [...this.draft.name].length > 100) throw new Error("Имя профиля: 1–100 символов.");
      if (!(await this.validate())) return false;
      const payload: GraphProfilePayload = {
        name: this.draft.name.trim(),
        description: this.draft.description,
        format: "graph",
        blueprint: this.draft.graph,
      };
      const body = !copy && this.profileID ? { ...payload, expected_version: this.loaded!.version } : payload;
      if (bytes(body) > 256 * 1024) throw new Error("Полное тело запроса превышает 256 КиБ UTF-8.");
      writing = true;
      this.pendingWrite = clone(payload);
      const p =
        !copy && this.profileID
          ? await api.saveGraphProfile(this.profileID, { ...payload, expected_version: this.loaded!.version })
          : await api.createGraphProfile(payload);
      acknowledged = true;
      this.profileID = p.id;
      const v = await readVersion(p.id, p.versions!);
      if (stableJSON(v.blueprint) !== stableJSON(payload.blueprint))
        throw new Error("Прочитанная версия отличается от отправленной. Нужна сверка; черновик не потерян.");
      this.accept(v);
      this.say("Новая версия сохранена. Запуск и политика не включены.");
      return true;
    } catch (error) {
      const e = error as APIError;
      if (e.diagnostics) {
        this.diagnostics = e.diagnostics;
        this.valid = false;
        this.findings();
        this.el<HTMLElement>("[data-diagnostic]")?.focus();
      }
      this.conflict = e.code === "revision_conflict";
      this.uncertain = acknowledged || (writing && (!e.status || e.status >= 500));
      if (!this.uncertain) this.pendingWrite = null;
      this.say(this.conflict ? "Профиль изменён в другом окне; черновик и история сохранены." : e.message, true);
      return false;
    } finally {
      this.busy = false;
      if (!this.disposed) this.status();
    }
  }
  private accept(v: ProfileVersion, saved?: GraphProfilePayload): void {
    this.loaded = v;
    this.pendingInput = false;
    this.baseline = saved
      ? JSON.stringify({ graph: saved.blueprint, name: saved.name, description: saved.description })
      : this.content();
    this.conflict = false;
    this.uncertain = false;
    this.pendingWrite = null;
    const hash = `#/profiles/editor/${v.profile_id}?version=${v.version}`;
    history.replaceState(null, "", hash);
    editorHash = hash;
  }
  private async reconcile(): Promise<void> {
    const sent = this.pendingWrite;
    const m = await api.myProfiles(),
      p = m.profiles.find((p) =>
        this.profileID ? p.id === this.profileID : p.name === (sent?.name || this.draft.name.trim()),
      );
    if (!p?.versions) {
      this.say("Сохранение не подтверждено. Черновик оставлен; автоматического повтора нет.", true);
      return;
    }
    const v = await readVersion(p.id, p.versions);
    if (
      stableJSON(v.blueprint) === stableJSON(sent?.blueprint || this.draft.graph) &&
      p.name === (sent?.name || this.draft.name.trim()) &&
      p.description === (sent?.description ?? this.draft.description)
    ) {
      this.profileID = p.id;
      this.accept(v, sent || undefined);
      this.status();
      this.say("Сохранение подтверждено чтением версии.");
    } else
      this.say(
        "Актуальное содержимое отличается. Не повторяйте запись вслепую; откройте актуальную версию отдельно и сравните.",
        true,
      );
  }
  private async click(e: MouseEvent): Promise<void> {
    const t = e.target as HTMLElement;
    const panel = t.closest<HTMLButtonElement>(PANEL_TAB_SELECTOR);
    if (panel) {
      this.panel = panel.dataset.bpTab!;
      this.panels();
      return;
    }
    if (t.closest("[data-settings]")) {
      this.select("");
      return;
    }
    if (t.closest("[data-save]")) {
      await this.save();
      return;
    }
    if (t.closest("[data-validate]")) {
      if (this.busy) return;
      this.busy = true;
      this.status();
      try {
        await this.validate();
      } finally {
        this.busy = false;
        this.status();
      }
      return;
    }
    if (t.closest("[data-run]")) {
      if (this.loaded && !this.dirty) openRunReview(this.loaded, this.blocks);
      return;
    }
    if (t.closest("[data-reconcile]")) {
      await this.reconcile();
      return;
    }
    if (this.busy) return;
    if (t.closest("[data-latest]")) {
      if (window.confirm("Загрузить актуальную версию? Локальный черновик будет удалён.")) {
        const m = await api.myProfiles(),
          p = m.profiles.find((p) => p.id === this.profileID);
        if (p?.versions) {
          const v = await readVersion(p.id, p.versions);
          if (v.blueprint) {
            this.draft = {
              graph: clone(v.blueprint),
              name: p.name,
              description: p.description,
              positions: layout(v.blueprint),
            };
            this.undo = [];
            this.redo = [];
            this.accept(v);
            this.paint();
          }
        }
      }
      return;
    }
    if (t.closest("[data-copy]")) {
      this.select("");
      this.say("Укажите новое имя профиля, затем сохраните отдельную копию.");
      if (window.confirm("Сохранить отдельный профиль под текущим именем? При совпадении имён сервер вернёт ошибку."))
        await this.save(true);
      return;
    }
    if (this.busy) return;
    const select = t.closest<HTMLElement>("[data-select]");
    if (select) {
      this.select(select.dataset.select!, false);
      this.panel = "inspector";
      this.panels();
      return;
    }
    const diag = t.closest<HTMLElement>("[data-diagnostic]");
    if (diag) {
      const d = this.diagnostics[Number(diag.dataset.diagnostic)];
      this.panel = "inspector";
      this.panels();
      this.select(d.node_id || "", true);
      const field =
        d.port &&
        this.root.querySelector<HTMLElement>(`#bp-binding-${CSS.escape(d.port)}, #bp-param-${CSS.escape(d.port)}`);
      (field || this.el("#bp-inspector-title")).focus({ preventScroll: true });
      return;
    }
    const edge = t.closest<HTMLElement>("[data-remove-edge]");
    if (edge) {
      this.change({
        ...this.draft,
        graph: {
          ...this.draft.graph,
          edges: this.draft.graph.edges.filter((_, i) => i !== Number(edge.dataset.removeEdge)),
        },
      });
      this.say("Связь удалена.");
      return;
    }
    if (t.closest("[data-template]")) {
      const starter = this.el<HTMLSelectElement>("#bp-starter").value;
      if (
        starter &&
        (!this.dirty ||
          window.confirm("Заменить несохранённую схему шаблоном? Это только локальный черновик, без запуска."))
      ) {
        const graph = template(starter);
        this.selected = "";
        this.change({ ...this.draft, graph, positions: layout(graph) });
      }
      return;
    }
    const add = t.closest<HTMLElement>("[data-add]");
    if (add) {
      const b = blockFor(this.blocks, add.dataset.add!);
      if (!b || this.draft.graph.nodes.length >= 128) return;
      let i = 1;
      while (this.draft.graph.nodes.some((n) => n.id === `block_${i}`)) i++;
      const id = `block_${i}`;
      const params = Object.fromEntries(
        (b.params_schema || []).map((p) => [
          p.name,
          p.enum?.[0] ??
            (p.type === "integer" ? (["install.text", "ip.switch_advertised"].includes(b.type) ? 1200 : 600) : ""),
        ]),
      );
      this.selected = id;
      this.change({
        ...this.draft,
        graph: { ...this.draft.graph, nodes: [...this.draft.graph.nodes, { id, type: b.type, params, in: {} }] },
        positions: { ...this.draft.positions, [id]: { x: 80 + i * 24, y: 80 + i * 24 } },
      });
      this.canvas?.select(id, true);
      this.say(`Добавлен блок ${id}.`);
      return;
    }
    if (t.closest("[data-undo], [data-redo]")) {
      const undo = !!t.closest("[data-undo]"),
        stack = undo ? this.undo : this.redo,
        next = stack[stack.length - 1];
      if (next) {
        if (undo) {
          this.redo = [...this.redo, clone(this.draft)];
          this.undo = this.undo.slice(0, -1);
        } else {
          this.undo = [...this.undo, clone(this.draft)];
          this.redo = this.redo.slice(0, -1);
        }
        this.draft = next;
        this.valid = false;
        this.diagnostics = [];
        this.paint();
        this.say(undo ? "Изменение отменено." : "Изменение повторено.");
      }
      return;
    }
    const zoom = t.closest<HTMLElement>("[data-zoom]");
    if (zoom) this.el("#bp-zoom").textContent = `${this.canvas?.scale(Number(zoom.dataset.zoom))}%`;
    if (t.closest("[data-fit]")) this.el("#bp-zoom").textContent = `${this.canvas?.fit()}%`;
  }
  private key(e: KeyboardEvent): void {
    const t = e.target as HTMLElement;
    if (t.matches("input,select,textarea,[contenteditable=true]")) return;
    const tabs = Array.from(this.root.querySelectorAll<HTMLElement>(PANEL_TAB_SELECTOR));
    const i = tabs.indexOf(t);
    if (i >= 0 && ["ArrowRight", "ArrowLeft", "Home", "End"].includes(e.key)) {
      e.preventDefault();
      const index =
        e.key === "Home"
          ? 0
          : e.key === "End"
            ? tabs.length - 1
            : (i + (e.key === "ArrowRight" ? 1 : -1) + tabs.length) % tabs.length;
      this.panel = tabs[index].dataset.bpTab!;
      this.panels();
      tabs[index].focus();
      return;
    }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "z") {
      e.preventDefault();
      this.el<HTMLButtonElement>(e.shiftKey ? "[data-redo]" : "[data-undo]").click();
    }
    if ((e.key === "Delete" || e.key === "Backspace") && this.selected && !this.busy) {
      e.preventDefault();
      this.remove(this.selected);
    }
  }
}
