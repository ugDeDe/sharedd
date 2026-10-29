import { esc } from "../../lib/dom";
import type { Blueprint, BlueprintBlock, BlueprintDiagnostic } from "./api";
import { blockFor, EFFECTS, TITLES } from "./blueprint-model";
import type { Point } from "./blueprint-model";

interface CanvasOptions {
  select: (id: string, focus?: boolean) => void;
  connect: (from: string, port: string, to: string) => void;
  move: (positions: Record<string, Point>) => void;
  announce: (text: string) => void;
}
export class BlueprintCanvas {
  private graph: Blueprint | null = null;
  private positions: Record<string, Point> = {};
  private selected = "";
  private zoom = 1;
  private pan: Point = { x: 0, y: 0 };
  private connection: { from: string; port: string } | null = null;
  private drag: { pointer: number; id: string; x: number; y: number; origin: Point; moved: boolean } | null = null;
  private world: HTMLElement;
  private svg: SVGSVGElement;
  private observer: ResizeObserver;
  private space = false;
  private invalidNodes = new Set<string>();
  constructor(
    private root: HTMLElement,
    private opts: CanvasOptions,
  ) {
    root.innerHTML = `<div class="blueprint-world"><svg class="blueprint-wires" aria-hidden="true"></svg><div class="blueprint-nodes"></div></div>`;
    this.world = root.querySelector<HTMLElement>(".blueprint-world")!;
    this.svg = root.querySelector<SVGSVGElement>("svg")!;
    this.observer = new ResizeObserver(() => this.wires());
    this.observer.observe(root);
    root.onpointerdown = (e) => this.down(e);
    root.onpointermove = (e) => this.motion(e);
    root.onpointerup = (e) => this.up(e);
    root.onpointercancel = () => {
      if (this.drag?.id) this.positions = { ...this.positions, [this.drag.id]: this.drag.origin };
      else if (this.drag) this.pan = this.drag.origin;
      this.drag = null;
      this.renderPositions();
    };
    root.onclick = (e) => {
      const t = e.target as HTMLElement;
      const output = t.closest<HTMLElement>("[data-output]");
      const node = t.closest<HTMLElement>("[data-canvas-node]");
      if (!node) return;
      const id = node.dataset.canvasNode!;
      if (output) this.startConnection(id, output.dataset.output!);
      else if (t.closest("[data-exec]") && this.connection) this.finishConnection(id);
      else this.opts.select(id);
    };
    root.onkeydown = (e) => {
      if (e.code === "Space" && e.target === root) {
        e.preventDefault();
        this.space = true;
        return;
      }
      if (e.key === "Escape") {
        this.cancelConnection();
        return;
      }
      const delta: Record<string, Point> = {
        ArrowLeft: { x: -1, y: 0 },
        ArrowRight: { x: 1, y: 0 },
        ArrowUp: { x: 0, y: -1 },
        ArrowDown: { x: 0, y: 1 },
      };
      if (!delta[e.key] || (e.target as HTMLElement).matches("input,select,textarea")) return;
      const d = delta[e.key];
      e.preventDefault();
      if (e.target === root) {
        this.pan = { x: this.pan.x - d.x * 50, y: this.pan.y - d.y * 50 };
        this.transform();
      } else if (this.selected) this.nudge(d.x * (e.shiftKey ? 50 : 10), d.y * (e.shiftKey ? 50 : 10));
    };
    root.onkeyup = (e) => {
      if (e.code === "Space") this.space = false;
    };
    root.onblur = () => {
      this.space = false;
    };
    root.addEventListener(
      "wheel",
      (e) => {
        if (e.ctrlKey || e.metaKey) return; // native browser zoom wins
        if (!root.contains(document.activeElement)) return;
        e.preventDefault();
        this.pan = { x: this.pan.x - e.deltaX, y: this.pan.y - e.deltaY };
        this.transform();
      },
      { passive: false },
    );
  }
  dispose(): void {
    this.observer.disconnect();
  }
  refresh(): void {
    this.renderPositions();
  }
  render(
    g: Blueprint,
    blocks: BlueprintBlock[],
    positions: Record<string, Point>,
    selected: string,
    diagnostics: BlueprintDiagnostic[],
  ): void {
    this.graph = g;
    this.positions = positions;
    this.selected = selected;
    this.invalidNodes = new Set(diagnostics.map((d) => d.node_id || ""));
    this.world.querySelector<HTMLElement>(".blueprint-nodes")!.innerHTML =
      g.nodes
        .map((n) => {
          const b = blockFor(blocks, n.type);
          const ds = diagnostics.filter((d) => d.node_id === n.id).length;
          return `<article class="blueprint-node${n.id === selected ? " selected" : ""}${ds ? " invalid" : ""}" data-canvas-node="${esc(n.id)}" aria-label="${esc(TITLES[n.type] || n.type)} ${esc(n.id)}">
        <button type="button" class="btn ghost blueprint-handle" data-drag-node="${esc(n.id)}" aria-pressed="${n.id === selected}">${esc(TITLES[n.type] || n.type)}</button>
        <div class="blueprint-node-body"><span class="mono">${esc(n.id)}</span><span class="mono">${esc(n.type)}</span>
        ${b ? `<span class="badge ${b.effects === "purchase" ? "warn" : ""}" data-effect="${b.effects}">${EFFECTS[b.effects]}</span>` : "Блок недоступен"}
        ${Object.entries(g.entries)
          .filter(([, id]) => id === n.id)
          .map(
            ([role]) => `<span class="badge info">${role === "initial" ? "Первичное создание" : "Наблюдение"}</span>`,
          )
          .join("")}
        ${ds ? `<span class="badge bad">Ошибок: ${ds}</span>` : ""}
        <div class="blueprint-ports"><button type="button" class="btn sm" id="bp-port-${esc(n.id)}-exec" data-exec aria-label="Соединить с ${esc(n.id)} · exec">→ exec</button>
        <div>${(b?.ports.control || []).map((p) => `<button type="button" class="btn sm" id="bp-port-${esc(n.id)}-out-${esc(p)}" data-output="${esc(p)}" aria-label="Соединить ${esc(n.id)} · ${esc(p)}">${esc(p)} →</button>`).join("")}</div></div>
        <div class="blueprint-data">${(b?.ports.inputs || []).map((p) => `<div>Вход ${esc(p.name)} · ${esc(p.type)}</div>`).join("")}${(b?.ports.outputs || []).map((p) => `<div>Выход ${esc(p.name)} · ${esc(p.type)}</div>`).join("")}</div>
        </div></article>`;
        })
        .join("") ||
      `<div class="blueprint-canvas-empty">Добавьте блок или выберите шаблон. Клавиатурный эквивалент схемы — список блоков и связей.</div>`;
    this.renderPositions();
  }
  select(id: string, focus = false): void {
    this.selected = id;
    this.root.querySelectorAll<HTMLElement>("[data-canvas-node]").forEach((el) => {
      const on = el.dataset.canvasNode === id;
      el.classList.toggle("selected", on);
      el.querySelector("[data-drag-node]")?.setAttribute("aria-pressed", String(on));
    });
    if (focus) {
      const p = this.positions[id];
      if (p) {
        this.pan = {
          x: this.root.clientWidth / 2 - (p.x + 120) * this.zoom,
          y: this.root.clientHeight / 2 - (p.y + 80) * this.zoom,
        };
        this.transform();
      }
      this.root.querySelector<HTMLElement>(`[data-drag-node="${CSS.escape(id)}"]`)?.focus({ preventScroll: true });
    }
  }
  startConnection(from: string, port: string): void {
    this.connection = { from, port };
    this.root.classList.add("connecting");
    this.opts.announce(`${from} · ${port}: выберите exec следующего блока. Escape — отмена.`);
  }
  private finishConnection(to: string): void {
    if (!this.connection) return;
    const { from, port } = this.connection;
    this.connection = null;
    this.root.classList.remove("connecting");
    this.opts.connect(from, port, to);
  }
  cancelConnection(): void {
    const from = this.connection?.from;
    this.connection = null;
    this.root.classList.remove("connecting");
    if (from) this.select(from, true);
  }
  nudge(x: number, y: number): void {
    const p = this.positions[this.selected];
    if (!p) return;
    this.opts.move({ ...this.positions, [this.selected]: { x: p.x + x, y: p.y + y } });
  }
  scale(factor: number): number {
    const z = Math.max(0.25, Math.min(2, this.zoom * factor));
    this.pan = {
      x: this.root.clientWidth / 2 - ((this.root.clientWidth / 2 - this.pan.x) * z) / this.zoom,
      y: this.root.clientHeight / 2 - ((this.root.clientHeight / 2 - this.pan.y) * z) / this.zoom,
    };
    this.zoom = z;
    this.transform();
    return Math.round(z * 100);
  }
  fit(): number {
    const ps = Object.values(this.positions);
    if (!ps.length) {
      this.pan = { x: 0, y: 0 };
      this.zoom = 1;
      this.transform();
      return 100;
    }
    const minX = Math.min(...ps.map((p) => p.x)),
      minY = Math.min(...ps.map((p) => p.y));
    const w = Math.max(...ps.map((p) => p.x)) - minX + 280,
      h = Math.max(...ps.map((p) => p.y)) - minY + 330;
    this.zoom = Math.max(0.25, Math.min(1, this.root.clientWidth / w, this.root.clientHeight / h));
    this.pan = { x: 20 - minX * this.zoom, y: 20 - minY * this.zoom };
    this.transform();
    return Math.round(this.zoom * 100);
  }
  private transform(): void {
    this.world.style.transform = `translate(${this.pan.x}px,${this.pan.y}px) scale(${this.zoom})`;
  }
  private renderPositions(): void {
    this.root.querySelectorAll<HTMLElement>("[data-canvas-node]").forEach((el) => {
      const p = this.positions[el.dataset.canvasNode!];
      if (p) {
        el.style.left = `${p.x}px`;
        el.style.top = `${p.y}px`;
      }
    });
    this.transform();
    this.wires();
  }
  private wires(): void {
    const point = (id: string, output?: string): Point | null => {
      const n = this.root.querySelector<HTMLElement>(`[data-canvas-node="${CSS.escape(id)}"]`);
      const el = n?.querySelector<HTMLElement>(
        output === undefined ? "[data-exec]" : `[data-output="${CSS.escape(output)}"]`,
      );
      if (!el) return null;
      const r = el.getBoundingClientRect(),
        world = this.world.getBoundingClientRect();
      return {
        x: (r.left - world.left + (output === undefined ? 0 : r.width)) / this.zoom,
        y: (r.top - world.top + r.height / 2) / this.zoom,
      };
    };
    this.svg.innerHTML =
      `<defs><marker id="bp-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse"><path d="M0 0 L10 5 L0 10z" fill="currentColor"/></marker></defs>` +
      (this.graph?.edges || [])
        .map((e) => {
          const a = point(e.from, e.port),
            b = point(e.to);
          if (!a || !b) return "";
          const bend = Math.max(70, Math.abs(b.x - a.x) / 2);
          return `<path class="${this.invalidNodes.has(e.from) ? "invalid" : ""}" d="M${a.x},${a.y} C${a.x + bend},${a.y} ${b.x - bend},${b.y} ${b.x},${b.y}" marker-end="url(#bp-arrow)"/><text x="${a.x + 10}" y="${a.y - 8}">${esc(e.port)}</text>`;
        })
        .join("");
  }
  private down(e: PointerEvent): void {
    if (e.button !== 0) return;
    const t = e.target as HTMLElement;
    const node = t.closest<HTMLElement>("[data-drag-node]");
    const out = t.closest<HTMLElement>("[data-output]");
    if (out) {
      const id = out.closest<HTMLElement>("[data-canvas-node]")!.dataset.canvasNode!;
      this.startConnection(id, out.dataset.output!);
      return;
    }
    if (!node && t.closest("button,a,input,select")) return;
    const id = this.space ? "" : node?.dataset.dragNode || "";
    if (id) this.opts.select(id);
    this.drag = {
      pointer: e.pointerId,
      id,
      x: e.clientX,
      y: e.clientY,
      origin: id ? this.positions[id] : this.pan,
      moved: false,
    };
    this.root.setPointerCapture(e.pointerId);
  }
  private motion(e: PointerEvent): void {
    const d = this.drag;
    if (!d || e.pointerId !== d.pointer) return;
    const dx = e.clientX - d.x,
      dy = e.clientY - d.y;
    if (Math.abs(dx) + Math.abs(dy) > 3) d.moved = true;
    if (d.id)
      this.positions = {
        ...this.positions,
        [d.id]: { x: d.origin.x + dx / this.zoom, y: d.origin.y + dy / this.zoom },
      };
    else this.pan = { x: d.origin.x + dx, y: d.origin.y + dy };
    this.renderPositions();
  }
  private up(e: PointerEvent): void {
    if (this.connection) {
      const target = document
        .elementFromPoint(e.clientX, e.clientY)
        ?.closest<HTMLElement>("[data-exec]")
        ?.closest<HTMLElement>("[data-canvas-node]");
      if (target) this.finishConnection(target.dataset.canvasNode!);
    }
    const d = this.drag;
    this.drag = null;
    if (this.root.hasPointerCapture(e.pointerId)) this.root.releasePointerCapture(e.pointerId);
    if (d?.id && d.moved) this.opts.move(this.positions);
  }
}
