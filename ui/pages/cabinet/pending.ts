/* Идентичность платной команды между отправкой и повтором.
 *
 * Зачем: если сервер принял заказ, а ответ потерялся, повтор с новым
 * Idempotency-Key создаёт второе намерение покупки. Здесь хранится ровно
 * один ключ и каноничное тело до получения определённого ответа.
 *
 * В sessionStorage попадают только несекретные тела облачных команд:
 * конфигурация VPS, имя, профиль и переменные установки. Пароли, SSH-ключи,
 * токены и node_secret сюда не передаются.
 */

const PREFIX = "sharedd.pending.";

export type PendingState = "submitting" | "unknown";

export interface PendingCommand<T> {
  key: string;
  body: T;
  hash: string;
  state: PendingState;
  createdAt: number;
}

/** Стабильная сериализация: порядок ключей не зависит от порядка вставки. */
export function stableStringify(value: unknown): string {
  if (value === undefined) return "null";
  if (value === null || typeof value !== "object") {
    return JSON.stringify(value) ?? "null";
  }
  if (Array.isArray(value)) return `[${value.map(stableStringify).join(",")}]`;
  const obj = value as Record<string, unknown>;
  const keys = Object.keys(obj).filter((k) => obj[k] !== undefined).sort();
  return `{${keys.map((k) => `${JSON.stringify(k)}:${stableStringify(obj[k])}`).join(",")}}`;
}

export function bodyHash(body: unknown): string {
  return stableStringify(body);
}

function storage(): Storage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

export function loadPending<T>(name: string): PendingCommand<T> | null {
  const s = storage();
  if (!s) return null;
  try {
    const raw = s.getItem(PREFIX + name);
    if (!raw) return null;
    const p = JSON.parse(raw) as Partial<PendingCommand<T>>;
    if (!p || typeof p.key !== "string" || !p.key || typeof p.hash !== "string" || !p.body) return null;
    return {
      key: p.key,
      body: p.body,
      hash: p.hash,
      state: p.state === "unknown" ? "unknown" : "submitting",
      createdAt: typeof p.createdAt === "number" ? p.createdAt : 0,
    };
  } catch {
    return null;
  }
}

export function savePending<T>(name: string, p: PendingCommand<T>): void {
  const s = storage();
  if (!s) return;
  try {
    s.setItem(PREFIX + name, JSON.stringify(p));
  } catch {
    /* private mode / quota: команда остаётся в памяти вызывающего кода */
  }
}

export function clearPending(name: string): void {
  const s = storage();
  if (!s) return;
  try {
    s.removeItem(PREFIX + name);
  } catch {
    /* ignore */
  }
}

/** New intent is allowed only after the user explicitly resolved the old one. */
export function beginPending<T>(name: string, key: string, body: T): PendingCommand<T> {
  const p: PendingCommand<T> = {
    key,
    body,
    hash: bodyHash(body),
    state: "submitting",
    createdAt: Date.now(),
  };
  savePending(name, p);
  return p;
}

export function markPendingUnknown<T>(name: string, p: PendingCommand<T>): PendingCommand<T> {
  const next = { ...p, state: "unknown" as const };
  savePending(name, next);
  return next;
}

export function clearAllPending(): void {
  const s = storage();
  if (!s) return;
  try {
    for (let i = s.length - 1; i >= 0; i--) {
      const k = s.key(i);
      if (k && k.startsWith(PREFIX)) s.removeItem(k);
    }
  } catch {
    /* ignore */
  }
}
