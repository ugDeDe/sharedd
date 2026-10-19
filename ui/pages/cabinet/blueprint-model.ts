import nodehostTemplate from "../../../registry/internal/blueprint/testdata/nodehost_replacement.json";
import begetTemplate from "../../../registry/internal/blueprint/testdata/beget_ip_or_replacement.json";
import { api } from "./api";
import type { Blueprint, BlueprintBlock, BlueprintDiagnostic, BlueprintEdge, MyProfiles } from "./api";

export const BEGET_GATE =
  "Только проектирование. Смена IP Beget не выполняется: блоки ip.* не реализованы; проверка primary IP не закрыта. Создание VPS через граф Beget также недоступно.";
export const TYPES = ["string", "number", "bool", "config", "server", "operation", "ssh_target", "secretref"] as const;
export const EFFECTS = { read: "чтение", mutation: "изменение", purchase: "платное действие", control: "завершение" };
export const TITLES: Record<string, string> = {
  "server.create": "Создать VPS",
  "server.ready": "Дождаться VPS",
  "install.text": "Установить профиль",
  "pool.admit": "Допустить в пул",
  "event.ip_ban": "Дождаться блокировки",
  "policy.guard": "Проверить политику",
  "server.distinct_ip": "Проверить новый IP",
  "resource.fence": "Оградить старый ресурс",
  "resource.switch": "Переключить ресурс",
  "server.delete": "Удалить VPS",
  "server.absent": "Подтвердить удаление",
  "observe.arm": "Завершить и наблюдать",
  "terminal.stop": "Остановить ветку",
  "ip.order": "Заказать IPv4",
  "ip.attach": "Подключить IPv4",
  "ip.switch_advertised": "Переключить IP",
  "ip.cleanup": "Очистить IP",
  "ip.verify": "Проверить IP",
};
export interface Point {
  x: number;
  y: number;
}
export interface Draft {
  graph: Blueprint;
  name: string;
  description: string;
  positions: Record<string, Point>;
}
export const clone = <T>(value: T): T => JSON.parse(JSON.stringify(value)) as T;
export const bytes = (value: unknown): number => new TextEncoder().encode(JSON.stringify(value)).length;
export const stableJSON = (value: unknown): string =>
  JSON.stringify(value, (_, v: unknown) =>
    v && typeof v === "object" && !Array.isArray(v)
      ? Object.fromEntries(Object.entries(v).sort(([a], [b]) => a.localeCompare(b)))
      : v,
  );
export const positiveID = (s: string): number =>
  /^\d+$/.test(s) && Number.isSafeInteger(Number(s)) && Number(s) > 0 ? Number(s) : 0;
export const emptyGraph = (): Blueprint => ({
  schema_version: 1,
  name: "Новая схема",
  provider: "nodehost",
  inputs: {},
  entries: { initial: "", observe: "" },
  nodes: [],
  edges: [],
});
export function template(name: string): Blueprint {
  // Trusted backend fixtures; JSON-module inference widens enum strings and empty objects.
  return name === "nodehost"
    ? (clone(nodehostTemplate) as unknown as Blueprint)
    : name === "beget"
      ? (clone(begetTemplate) as unknown as Blueprint)
      : emptyGraph();
}
export function blockFor(blocks: BlueprintBlock[], type: string): BlueprintBlock | undefined {
  return blocks.find((b) => b.type === type);
}
export function edgeCount(g: Blueprint): number {
  return (
    g.edges.length +
    g.nodes.reduce(
      (n, node) =>
        n +
        Object.values(node.in || {}).filter((b) => b.ref && !b.ref.startsWith("input.") && !b.ref.startsWith("run."))
          .length,
      0,
    )
  );
}
export function localDiagnostics(g: Blueprint): BlueprintDiagnostic[] {
  const ds: BlueprintDiagnostic[] = [];
  const add = (code: string, message: string, node_id?: string, port?: string): void => {
    ds.push({ code, message, node_id, port });
  };
  if (!g.name.trim() || [...g.name].length > 100) add("name", "Имя: от 1 до 100 символов.");
  if (g.nodes.length > 128 || edgeCount(g) > 256 || Object.keys(g.inputs).length > 32 || bytes(g) > 256 * 1024)
    add("limits", "Лимиты: 128 блоков, 256 связей, 32 входа, 256 КиБ UTF-8.");
  for (const n of g.nodes) {
    for (const [port, b] of Object.entries(n.in || {})) {
      if (b.ref === "run.policy_revision" || b.ref === "run.episode_watermark")
        add("runtime_unavailable", "Эта runtime-ссылка не разрешается текущим движком.", n.id, port);
      if ("literal" in b && /secret|password|token|key|credential/i.test(port))
        add("secret_literal", "Секретные константы запрещены.", n.id, port);
    }
  }
  return ds;
}
export function runtimeGate(g: Blueprint): string {
  if (g.provider !== "nodehost" || g.nodes.some((n) => n.type.startsWith("ip."))) return BEGET_GATE;
  if (localDiagnostics(g).some((d) => d.code === "runtime_unavailable"))
    return "Недоступная runtime-ссылка: исправьте привязку перед выполнением.";
  for (const n of g.nodes) {
    if (n.retry && !["server.ready", "pool.admit", "server.distinct_ip", "server.absent"].includes(n.type))
      return "В загруженной схеме задан retry для блока без safe-read. Удалите эту настройку в свойствах блока перед запуском.";
    if (
      n.type === "install.text" &&
      (n.in.version?.ref !== "input.installation_profile_version_id" ||
        g.inputs.installation_profile_version_id?.type !== "number")
    )
      return "Для запуска install.text привяжите version к input.installation_profile_version_id: вход передаёт id текстового профиля до платных действий.";
    if (
      n.type === "server.create" &&
      (n.in.connection?.ref !== "input.connection_id" || n.in.configuration?.ref !== "input.configuration_ref")
    )
      return "Для запуска server.create используйте input.connection_id и input.configuration_ref: подключение выбирается из своих, конфигурация задаётся структурированной формой. Проектирование и сохранение доступны.";
  }
  return "";
}
export function reachable(g: Blueprint, start: string): Set<string> {
  const found = new Set<string>();
  const todo = [start];
  while (todo.length) {
    const id = todo.pop()!;
    if (!id || found.has(id)) continue;
    found.add(id);
    todo.push(...g.edges.filter((e) => e.from === id).map((e) => e.to));
  }
  return found;
}
export function connectError(g: Blueprint, e: BlueprintEdge): string {
  if (e.from === e.to || reachable(g, e.to).has(e.from)) return "Циклы и связь блока с собой запрещены.";
  if (edgeCount(g) >= 256 && !g.edges.some((x) => x.from === e.from && x.port === e.port))
    return "Достигнут лимит 256 связей.";
  return "";
}
export function layout(g: Blueprint): Record<string, Point> {
  const observe = reachable(g, g.entries.observe);
  const initial = reachable(g, g.entries.initial);
  const levels: Record<string, number> = {};
  // ponytail: bounded relaxation handles incomplete/cyclic drafts without hanging.
  for (let i = 0; i < g.nodes.length; i++)
    for (const e of g.edges) {
      if (e.to === g.entries.initial || e.to === g.entries.observe) continue;
      levels[e.to] = Math.min(g.nodes.length, Math.max(levels[e.to] || 0, (levels[e.from] || 0) + 1));
    }
  const occupied: Record<string, number> = {};
  const initialLevels = g.nodes.filter((n) => !observe.has(n.id) || initial.has(n.id)).map((n) => levels[n.id] || 0);
  const laneHeight = Math.max(1, ...initialLevels.map((l) => initialLevels.filter((v) => v === l).length)) * 425 + 60;
  return Object.fromEntries(
    [...g.nodes]
      .sort((a, b) => a.id.localeCompare(b.id))
      .map((n) => {
        const lane = observe.has(n.id) && !initial.has(n.id) ? 1 : 0;
        const level = levels[n.id] || 0;
        const key = `${lane}-${level}`;
        const row = occupied[key] || 0;
        occupied[key] = row + 1;
        return [n.id, { x: 40 + level * 310, y: 40 + lane * laneHeight + row * 425 }];
      }),
  );
}

// Профиль — единое содержимое: blueprint/steps приходят в GET /api/me/profiles.
export async function textProfiles(): Promise<MyProfiles> {
  const m = await api.myProfiles();
  const isText = (p: { format?: string }): boolean => p.format === "text";
  return { ...m, profiles: m.profiles.filter(isText), published: m.published.filter(isText) };
}
