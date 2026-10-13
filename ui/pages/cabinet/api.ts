/* Слой кабинета: типы ответов и запросы к /api/* (docs/api-contract.md §2.1).
   Формы ответов сняты с registry/internal/server/cabinet_api.go и
   cabinet_beget.go — код авторитетнее прозы.

   CSRF: double-submit. Cookie __Host-sharedd_csrf (читаемая JS, имя — как в
   cabinet_api.go: csrfCookieName) + заголовок X-CSRF-Token (csrfHeaderName)
   на всех мутирующих запросах. 401 → /signin. */

export interface Me { username: string; role: string; max_nodes: number; used_nodes: number; }

export interface Quarantine { attempts: number; entered_at: number; }

export interface NodeInfo {
  node_id: string;
  ip?: string;
  status: "healthy" | "unhealthy" | "quarantined" | "deleting" | "offline";
  quarantine?: Quarantine;
  master_domains?: string[];
  availability: number;
  dc2_ok: boolean;
}

export interface Authkey { prefix: string; created_at: number; active: boolean; }

// addendum-manual-servers.md §2: свой сервер (уже существующий хост). Секреты
// (пароль/ключ) наружу не отдаются ни одним read-эндпоинтом.
export interface ManualServer {
  id: number; label: string; host: string; port: number; ssh_user: string;
  auth_type: "password" | "key";
  created_at: number; verified_at?: number; last_error?: string;
  node_id?: string; operation?: Operation;
}

export interface ManualServerPayload {
  label: string; host: string; port?: number; ssh_user?: string;
  password?: string; private_key?: string;
}

export interface ManualServerCheck { verified: boolean; last_error?: string; }

export interface Profile { id: number; name: string; description: string; vars_schema?: string; }

// addendum-profiles-vps.md §3: свой профиль (полный, для правки). Правка
// шагов создаёт новую версию (append-only).
export interface OwnProfile {
  id: number; name: string; description: string; published: boolean;
  created_at: number; updated_at: number;
  steps?: string; vars_schema?: string; versions?: number;
}

export interface MyProfiles {
  profiles: OwnProfile[];  // свои (черновики + опубликованные)
  published: Profile[];    // опубликованные (для выбора при создании VPS)
  enabled: boolean;        // [provisioning] user_profiles_enabled
  max: number;             // max_user_profiles
}

export interface ProfilePayload {
  name?: string; description?: string; steps?: string; vars_schema?: string;
}

export interface BegetLink { verified: boolean; last_error?: string; }

export interface Region { id: string; name: string; name_en: string; country: string; available: boolean; }

// addendum §8.4: тариф провайдера. price — null, если провайдер не отдал
// цену (НЕ 0); UI обязан показать «цена не указана», а не выдумывать.
export interface Configuration {
  id: string; name: string;
  cpu?: number; ram?: number; disk?: number; // RAM/disk в МБ
  price: number | null;
  period?: string; region?: string; available: boolean;
}

export interface Catalog { regions: Region[]; configurations: Configuration[]; }

// Пошаговый журнал установки (plan-ui-batch.md Фаза 3). output — хвост вывода
// шага, секреты замаскированы сервером; duration_ms=0, пока шаг не завершён.
export interface OperationStep {
  idx: number;
  name: string;
  status: "running" | "ok" | "error" | string;
  duration_ms: number;
  output?: string;
}

export interface Operation {
  id: number;
  profile_id?: number;
  node_id?: string;
  kind: string;
  state: "pending" | "sent" | "confirmed" | "installing" | "done" | "unknown" | "failed" | "deleting" | "deleted" | "waiting_vps";
  // Добавлено backend'ом: у операции есть созданный VPS (provider_vps_id непустой).
  // Отсутствует у старого backend'а — тогда повтор гейтится прежним поведением.
  has_vps?: boolean;
  last_error?: string;
  created_at: number;
  updated_at: number;
  steps?: OperationStep[];
}

// ── детальная карточка ноды (plan-ui-batch.md Фаза 2, `GET …/detail`) ──
// Формы сняты с registry/internal/server/node_detail_api.go. Разделы без
// источника приходят null, а их имена перечислены в absent[] — UI обязан
// рисовать «нет данных», не выдумывая значения.

export interface NodeMasterInfo {
  is_master: boolean;
  domains: string[];
  slots_filled: number;
  slots_total: number;
  ttl_remaining_sec?: number;
  master_stints: number;
  master_time_sec: number;
}

export interface NodeParam {
  node_id: string;
  ip: string;
  port: number;
  is_master: boolean;
  master_domains?: string[];
  queue_position: number;
  healthy: boolean;
  fully_healthy: boolean;
  globalping_ok: boolean;
  metrics_ok: boolean;
  metrics_healthy: boolean;
  metrics_fail_streak?: number;
  globalping_ratio: number;
  unhealthy_reason?: string;
  registered_at: string;
  heartbeat_age_sec: number;
  report_age_sec: number; // -1 = отчётов не было
  report_error?: string;
  heartbeats_total: number;
  reports_total: number;
  reports_ok: number;
  availability_pct: number;
  clients_unique_ips?: number;
  gp_checks_total: number;
  gp_verified_pct: number;
  master_stints: number;
  master_time_sec: number;
  node_type?: string;
  quarantine?: { attempts: number; entered_at: number };
  region?: string;
  uptime_sec: number;
  status: string;
  master: NodeMasterInfo;
}

export interface NodeDaily {
  date: string; tcp_ok: number; tcp_total: number;
  reports_ok: number; reports_total: number; ok_pct: number;
}
export interface NodeAvailability {
  window_days: number; overall_pct: number; reports_ok: number; reports_total: number;
  gp_checks_ok: number; gp_checks_total: number; gp_verified_pct: number;
  daily: NodeDaily[];
  tcp_hist: { at: string; ok: boolean }[];
  gp_hist: { at: string; ok: boolean; ratio: number; probes_ok: number; probes_total: number }[];
  report_hist: { at: string; metrics_ok: boolean; clients: number; writers: number }[];
}
export interface NodeCheck { at: string; source: string; ok: boolean; detail?: string; }
export interface NodeHosting {
  cpu_percent: number | null;
  ram_percent: number | null;
  net_ingress_bytes_total?: number;
  net_egress_bytes_total?: number;
}
export interface NodeConnections {
  current_writers?: number; current_unique_ips?: number; current_user_connections?: number;
  series: { at: string; clients: number; writers: number }[];
  note?: string;
}
export interface NodeUserUniqueIPs { user: string; unique_ips: number; }
// Агрегат трафика пользователя за 24ч. null, когда данных нет; имя раздела в
// absent[] равнозначно «нет данных».
export interface NodeUserTraffic { user: string; ingress_bytes: number; egress_bytes: number; }
export interface NodeUsers {
  unique_ips_by_user: NodeUserUniqueIPs[];
  traffic_by_user: NodeUserTraffic[] | null;
}
export interface NodeEvent { at: string; type: string; domain?: string; detail?: string; }
export interface IPHistoryRow { ip: string; first_seen: string; last_seen: string; events: number; }

// Снимок telemt Control API (DC/quality). null/отсутствует, когда данных нет;
// имена telemt_control_api/dc_endpoints/quality_health приходят в absent[].
export interface TelemtDc {
  dc: number;
  available_endpoints: number;
  alive_writers: number;
  fresh_alive_writers: number;
  rtt_ms: number;
}
export interface TelemtControlApi {
  ok: boolean;
  generated_at_epoch_secs: number;
  middle_proxy_enabled: boolean;
  dcs: TelemtDc[];
}
export interface TelemtSnapshot {
  metrics_snapshot?: Record<string, number>;
  collected_at?: string;
  control_api?: TelemtControlApi | null;
}

export interface NodeDetail {
  node: NodeParam | null;
  availability: NodeAvailability | null;
  ip_history: IPHistoryRow[] | null;
  last_checks: NodeCheck[];
  hosting: NodeHosting | null;
  connections_24h: NodeConnections | null;
  users: NodeUsers | null;
  telemt: TelemtSnapshot | null;
  events: NodeEvent[];
  absent: string[];
}

export interface CreateVps {
  region: string;
  configuration_id: string;
  profile_id: number;
  vars: Record<string, unknown>;
  confirm: true;
  idem_key: string;
}

let redirecting = false;

function gotoSignin(): void {
  if (redirecting) return;
  redirecting = true;
  location.replace("/signin");
}

/* Имя cookie должно совпадать с server csrfCookieName (cabinet_api.go).
   __Host-префикс: Secure/Path=/ без Domain — доступен JS. Точное совпадение;
   старый sharedd_csrf сервер не ставит и не принимает — фолбэк не нужен. */
function csrf(): string {
  const m = document.cookie.match(/(?:^|;\s*)__Host-sharedd_csrf=([^;]+)/);
  return m ? decodeURIComponent(m[1]).trim() : "";
}

/* Сессия/CSRF отсутствуют или не совпали: сервер ответит csrf_invalid (403).
   Показываем понятную подсказку вместо непрозрачного «csrf token mismatch». */
function csrfError(): Error & { code: string; status: number } {
  const e = new Error("Сессия устарела или истекла. Войдите заново.") as Error & { code: string; status: number };
  e.code = "csrf_invalid";
  e.status = 403;
  return e;
}

async function req<T>(path: string, method = "GET", body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET" && method !== "HEAD") {
    const token = csrf();
    // Нет cookie — не шлём пустой заголовок: сразу подсказка пере-входа.
    // (Secure-cookie: читается только на HTTPS-origin; на http:// её нет.)
    if (!token) throw csrfError();
    headers["X-CSRF-Token"] = token;
  }
  const resp = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    cache: "no-store",
  });
  if (resp.status === 401) { gotoSignin(); throw new Error("unauthorized"); }
  if (resp.status === 204) return undefined as T;
  if (!resp.ok) {
    const data = await resp.json().catch(() => ({} as any));
    if (data.error === "csrf_invalid") throw csrfError();
    // 503 control_unavailable: сервер отдаёт русское объяснение, но если
    // message отсутствует — подставляем то же слово в слово.
    const fallback = data.error === "control_unavailable"
      ? "Хранилище пользователей недоступно — обратитесь к администратору для проверки журнала регистратора"
      : undefined;
    const err = new Error(data.message || fallback || data.error || ("HTTP " + resp.status)) as Error & { code?: string; status?: number; reason?: string };
    err.code = data.error;
    err.status = resp.status;
    // reason — машиночитаемая причина (например, code_required для 2FA).
    err.reason = data.reason;
    throw err;
  }
  return resp.json() as Promise<T>;
}

export const api = {
  me: () => req<Me>("/api/me"),
  installDefaults: () => req<import("../../lib/vars").InstallDefaults>("/api/me/install-defaults"),
  nodes: () => req<{ nodes: NodeInfo[] }>("/api/me/nodes"),
  authkey: () => req<Authkey>("/api/me/authkey"),
  // Полный ключ — ОДИН раз; отзывает все прежние ключи пользователя.
  rotateAuthkey: () => req<{ authkey: string }>("/api/me/authkey/rotate", "POST"),
  revokeAuthkey: () => req<void>("/api/me/authkey", "DELETE"),
  servers: () => req<{ servers: ManualServer[] }>("/api/me/servers"),
  createServer: (body: ManualServerPayload) => req<ManualServer>("/api/me/servers", "POST", body),
  checkServer: (id: number) => req<ManualServerCheck>(`/api/me/servers/${id}/check`, "POST"),
  // Повтор установки — тот же сервер/нода (idem_key = manual-<id>), ноль
  // провайдерских вызовов; нового сервера не создаётся.
  installServer: (id: number, body?: { profile_id?: number; vars?: Record<string, unknown> }) =>
    req<Operation>(`/api/me/servers/${id}/install`, "POST", body ?? {}),
  deleteServer: (id: number) => req<void>(`/api/me/servers/${id}`, "DELETE"),
  myProfiles: () => req<MyProfiles>("/api/me/profiles"),
  createProfile: (body: ProfilePayload) => req<OwnProfile>("/api/me/profiles", "POST", body),
  // Копия видимого профиля (опубликованного или своего) в свой черновик:
  // «<имя> (моя копия)», лимит max_user_profiles; шаги/vars_schema — дословно.
  copyProfile: (id: number) => req<OwnProfile>(`/api/me/profiles/${id}/copy`, "POST", {}),
  updateProfile: (id: number, body: ProfilePayload) => req<OwnProfile>(`/api/me/profiles/${id}`, "PUT", body),
  deleteProfile: (id: number) => req<void>(`/api/me/profiles/${id}`, "DELETE"),
  begetLink: () => req<BegetLink>("/api/me/beget/link"),
  connectBeget: (login: string, password: string, code?: string) =>
    req<{ verified: boolean }>("/api/me/beget/link", "PUT", { login, password, code: code || undefined }),
  regions: () => req<Catalog>("/api/me/beget/regions"),
  createVps: (body: CreateVps) => req<Operation>("/api/me/vps", "POST", body),
  nodeDetail: (id: string) => req<NodeDetail>(`/api/me/nodes/${encodeURIComponent(id)}/detail`),
  operations: () => req<{ operations: Operation[] }>("/api/me/operations"),
  cancelOperation: (id: number) => req<Operation>(`/api/me/operations/${id}/cancel`, "POST"),
  // Повтор установки на УЖЕ созданный VPS: backend не делает CreateVps.
  retryInstall: (id: number, body?: { profile_id?: number; vars?: Record<string, unknown> }) =>
    req<Operation>(`/api/me/operations/${id}/retry-install`, "POST", body ?? {}),
  deleteNode: (id: string) => req<{ node_id: string; state: string }>(`/api/me/nodes/${encodeURIComponent(id)}`, "DELETE"),
  logout: () => req<void>("/api/auth/logout", "POST"),
};

/** UUID для idem_key: фиксируется на попытку, повтор запроса не создаёт второй заказ. */
export function newIdemKey(): string {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}
