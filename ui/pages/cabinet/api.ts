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

export interface Profile {
  id: number; name: string; description: string;
  format?: "graph" | "text"; steps?: string; vars_schema?: string; blueprint?: Blueprint;
}

// Свой профиль (полный, для правки). Один профиль — одно содержимое,
// правка перезаписывает его на месте.
export interface OwnProfile {
  id: number; name: string; description: string; published: boolean;
  created_at: number; updated_at: number;
  format?: "graph" | "text"; steps?: string; vars_schema?: string; blueprint?: Blueprint;
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

export interface ProfileDefaults {
  profile_id: number;
  vars: Record<string, string>;
  revision: number;
  updated_at?: number;
}

// Frozen wire DTOs: blueprint/{model,registry}.go, blueprint_*_api.go.
export type BlueprintType = "string" | "number" | "bool" | "config" | "server" | "operation" | "ssh_target" | "secretref";
export interface BlueprintInput { type: BlueprintType; required: boolean; }
export type BlueprintBinding = { ref: string; literal?: never } | { literal: string | number | boolean; ref?: never };
export interface BlueprintNode {
  id: string; type: string; params: Record<string, string | number>;
  in: Record<string, BlueprintBinding>;
  retry?: { max_attempts: number; backoff_seconds: number };
}
export interface BlueprintEdge { from: string; port: string; to: string; }
export interface Blueprint {
  schema_version: number; name: string; provider: Provider;
  inputs: Record<string, BlueprintInput>; entries: { initial: string; observe: string };
  nodes: BlueprintNode[]; edges: BlueprintEdge[];
}
export interface BlueprintBlock {
  type: string;
  ports: { inputs: { name: string; type: BlueprintType }[] | null; outputs: { name: string; type: BlueprintType }[] | null; control: string[] | null };
  params_schema: { name: string; type: "string" | "integer"; required: boolean; enum?: string[] }[] | null;
  effects: "read" | "mutation" | "purchase" | "control";
  retry_class: "safe-read" | "none"; capabilities: Provider[];
}
export interface BlueprintDiagnostic { node_id?: string; port?: string; code: string; message: string; }
export interface BlueprintValidation { valid: boolean; diagnostics: BlueprintDiagnostic[]; }
export interface GraphProfilePayload { name: string; description: string; format: "graph"; blueprint: Blueprint; }
export interface APIError extends Error { code?: string; status?: number; reason?: string; diagnostics?: BlueprintDiagnostic[]; }
export interface ProfileRunCheckpoint {
  id: number; node_id: string; invocation: number; state: string; attempt: number;
  error?: string; output?: Record<string, unknown>; next_attempt_at: number;
}
export interface ProfileRun {
  id: number; operation_id?: number; profile_version_id?: number; profile_id: number;
  node_id?: string; entry: "initial" | "observe"; state: string; revision: number;
  resource_id: number | null; policy_revision: number; cancel_requested: boolean;
  next_wake_at?: number; last_error?: string;
  waiting?: { type: string; after_episode_id?: string }; checkpoints?: ProfileRunCheckpoint[];
}
export interface ProfileRunBody {
  profile_id: number; entry: "initial"; inputs: Record<string, unknown>;
  vars?: Record<string, string>; node_id?: string; confirm: true;
}
export interface ProfileRunAck { operation_id: number; run_id: number; state: string; resource_id: number | null; }
export type ResumeDecision = "reconcile" | "retry_safe" | "acknowledge";
export interface ManagementBody {
  mode: "manual" | "profile"; profile_id?: number; inputs?: Record<string, unknown>;
  process_current_ban?: boolean; expected_revision: number;
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
// Серии хостинга (24 ч, по возрастанию времени). Пустые массивы = «данные
// копятся»; cpu_percent/ram_percent внутри точки могут быть null (пропуск).
export interface NodeHostingCpuRam { at: string; cpu_percent: number | null; ram_percent: number | null; }
export interface NodeHostingNet { at: string; ingress_bytes: number; egress_bytes: number; }
export interface NodeHosting {
  cpu_percent: number | null;
  ram_percent: number | null;
  net_ingress_bytes_total?: number;
  net_egress_bytes_total?: number;
  cpu_ram_series?: NodeHostingCpuRam[];
  net_series?: NodeHostingNet[];
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

/* ── Хостинги: подключения и облачные ресурсы (docs/specs/hosting-blueprints) ──
   Формы сняты с registry/internal/server/cabinet_hosting.go и cabinet_cloud.go.
   Секретов в типах нет: read-эндпоинты plaintext никогда не отдают. */

export type Provider = "nodehost" | "beget";

export interface HostingCapabilities { create: boolean; change_ip: boolean; delete: boolean; }

export interface HostingBalance {
  amount: string | null; currency: string | null; scope: string;
  freshness: "fresh" | "stale" | "unavailable";
  checked_at: number | null; last_attempt_at: number | null; // unix-секунды (backend)
  error: { code: string } | null; // безопасный код: provider_error|provider_timeout|credentials_unavailable|…
}

export interface HostingConnection {
  id: number; provider: Provider; label: string;
  revision: number; credential_revision: number; credentials_configured: boolean;
  verified_at: number | null;
  capabilities: HostingCapabilities;
  balance: HostingBalance;
}

// POST/PATCH body (секреты write-only; пропуск — не менять бандл, объект — заменить целиком).
export interface HostingConnectionInput {
  provider?: Provider; label?: string;
  credentials?: Record<string, string>; // nodehost:{api_key}; beget:{cloud_login,cloud_password,hosting_api_login?,hosting_api_password?}
  expected_revision?: number; // PATCH
}

export interface CloudResource {
  id: number; connection_id: number; provider: Provider; label: string;
  state: string; revision: number; external_id?: string | null;
  ip?: string | null; current_ip?: string | null; // читаем оба поля защитно (stage-05)
  node_id?: string | null;
  replaces_resource_id?: number | null; replaced_by_resource_id?: number | null;
  active_operation_id?: number | null;
  last_error?: string;
}

export interface CloudCheckResult { ssh_ok: boolean; error?: string; }

// FROZEN: POST …/{id}/reconcile (CSRF) → 200 {resolved:"absent"} (ресурс станет
// deleted) | 200 {resolved:"found", external_id} | 409 provider_reconcile_required.
export interface CloudReconcileResult { resolved: "absent" | "found"; external_id?: string; }

export interface NodehostConfiguration {
  name: string; hostname?: string;
  cpu: string; cores: number; ram: number; disk: number; diskType: string;
  os: string; location: string; period: string; networkSpeed?: string;
}

export interface CloudCreateBody {
  connection_id: number; label: string;
  configuration: NodehostConfiguration;
  management?: { mode: "manual" }; // forward-compatible: stage-05 игнорирует
}

export interface CloudReplaceBody {
  connection_id: number; label: string;
  configuration: NodehostConfiguration;
  confirm: true; expected_revision: number;
  profile_id?: number; vars?: Record<string, unknown>;
}

/* ── флот/Autopilot (docs/specs/hosting-blueprints/fleet-autopilot.md) ──
   FROZEN wire DTOs: создание принимает ровно один платный заказ на N нод,
   статус опрашивается по fleet_id. Секретов в ответах нет. */
export type FleetState = "running" | "succeeded" | "partial" | "failed";
export type FleetItemState = "pending" | "creating" | "installing" | "verifying" | "admitted" | "error";

export interface FleetItem {
  idx: number;
  label: string;
  resource_id: number | null;
  node_id: string | null;
  state: FleetItemState;
  last_error: string;
  ip: string;
}

export interface Fleet {
  id: number;
  state: FleetState;
  count: number;
  auto_replace: boolean;
  created_at: number;
  items: FleetItem[];
}

export interface FleetSummary {
  id: number;
  state: FleetState;
  count: number;
  auto_replace: boolean;
  created_at: number;
  succeeded: number;
  errors: number;
}

export interface FleetConfiguration {
  name?: string;
  cpu: string;
  cores: number;
  ram: number;
  disk: number;
  diskType: string;
  os: string;
  location: string;
  period?: string;
  networkSpeed?: string;
}

export interface FleetCreateBody {
  connection_id: number;
  name?: string;
  count: number;
  configuration: FleetConfiguration;
  installation_profile_id: number;
  installation_vars?: Record<string, unknown>;
  auto_replace: boolean;
}

export interface FleetCreateAck { fleet_id: number; }

export interface NodeRuntimeSetting {
  state: "unmanaged" | "pending" | "applied" | "failed" | "unsupported";
  message?: string;
  mode?: "set" | "clear" | "unmanaged";
  value?: string;
  desired_revision?: number;
  applied_revision?: number;
}
export interface NodeSettings {
  node_id: string;
  slot_id: number;
  number: number;
  name: string;
  revision: number;
  runtime: Record<string, NodeRuntimeSetting>;
}
export interface NodeSettingsPatch {
  expected_revision: number;
  name?: string;
  runtime?: Record<string, unknown>;
}

export interface ManagementPolicy {
  mode: "manual" | "profile"; profile_id: number | null;  inputs: Record<string, unknown>; process_current_ban: boolean;
  revision: number; current_resource_id: number | null; active_run_id?: number | null;
}

export interface CommandAck { operation_id: number; state: string; resource_id: number | null; }

export interface HostingVerifyResult { verified: boolean; checked_at: number; capabilities: HostingCapabilities; }

/* ── FROZEN contract (backend реализует параллельно, формы не менять) ──────
   GET /api/me/hosting-connections/{id}/catalog?location=CODE — 200 nodehost,
   501 beget; network приходит только с location. */
export interface CatalogItem { id: string; name: string; }
export interface CatalogLocation { code: string; name: string; }
export interface CatalogLimit { min: number; max: number; }
export interface HostingCatalog {
  provider: Provider; checked_at: number;
  locations: CatalogLocation[];
  os: CatalogItem[];
  cpu_models: string[];
  disk_types: CatalogItem[];
  limits: { cores: CatalogLimit; ram_gb: CatalogLimit; disk_gb: CatalogLimit; types: { VPS: boolean } };
  network?: { speeds: string[]; available: boolean };
}

export interface EstimateBody {
  connection_id: number;
  configuration: { cores: number; ram: number; disk: number; networkSpeed?: string; period?: string; location?: string };
}
export interface EstimateResult {
  amount: string; currency: string; period: string; period_days: number;
  checked_at: number; scope: string;
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

async function req<T>(path: string, method = "GET", body?: unknown, opts?: { headers?: Record<string, string> }): Promise<T> {
  // opts.headers — дополнительные заголовки команд (Idempotency-Key,
  // X-Confirm-Action); Content-Type/CSRF добавляются ниже и не перетираются.
  const headers: Record<string, string> = { ...(opts?.headers || {}) };
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
    const data = await resp.json().catch(() => ({})) as { error?: string; message?: string; reason?: string; diagnostics?: BlueprintDiagnostic[] };
    if (data.error === "csrf_invalid") throw csrfError();
    // 503 control_unavailable: сервер отдаёт русское объяснение, но если
    // message отсутствует — подставляем то же слово в слово.
    const fallback = data.error === "control_unavailable"
      ? "Хранилище пользователей недоступно — обратитесь к администратору для проверки журнала регистратора"
      : undefined;
    const err = new Error(data.message || fallback || data.error || ("HTTP " + resp.status)) as APIError;
    err.code = data.error;
    err.status = resp.status;
    // reason — машиночитаемая причина (например, code_required для 2FA).
    err.reason = data.reason;
    err.diagnostics = data.diagnostics;
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
  blueprintBlocks: () => req<{ schema_version: number; blocks: BlueprintBlock[] }>("/api/me/blueprint-blocks"),
  validateBlueprint: (blueprint: Blueprint, inputs?: Record<string, unknown>) =>
    req<BlueprintValidation>("/api/me/profiles/validate", "POST", { format: "graph", blueprint, ...(inputs === undefined ? {} : { inputs }) }),
  createGraphProfile: (body: GraphProfilePayload) => req<OwnProfile>("/api/me/profiles", "POST", body),
  saveGraphProfile: (id: number, body: GraphProfilePayload) => req<OwnProfile>(`/api/me/profiles/${id}`, "PUT", body),
  createProfileRun: (body: ProfileRunBody, key: string) => req<ProfileRunAck>("/api/me/profile-runs", "POST", body, { headers: { "Idempotency-Key": key } }),
  profileRun: (id: number) => req<ProfileRun>(`/api/me/profile-runs/${id}`),
  cancelProfileRun: (id: number, expected_revision: number, key: string) => req<ProfileRunAck>(`/api/me/profile-runs/${id}/cancel`, "POST", { expected_revision }, { headers: { "Idempotency-Key": key } }),
  resumeProfileRun: (id: number, body: { expected_revision: number; checkpoint_id: number; decision: ResumeDecision }, key: string) => req<ProfileRunAck>(`/api/me/profile-runs/${id}/resume`, "POST", body, { headers: { "Idempotency-Key": key } }),
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
  // ── хостинги: подключения (cabinet_hosting.go) ──
  hostingConnections: () => req<{ connections: HostingConnection[] }>("/api/me/hosting-connections"),
  createHostingConnection: (body: HostingConnectionInput) =>
    req<HostingConnection>("/api/me/hosting-connections", "POST", body),
  updateHostingConnection: (id: number, body: HostingConnectionInput) =>
    req<HostingConnection>(`/api/me/hosting-connections/${id}`, "PATCH", body),
  deleteHostingConnection: (id: number) => req<void>(`/api/me/hosting-connections/${id}`, "DELETE"),
  verifyHostingConnection: (id: number, code?: string) =>
    req<HostingVerifyResult>(`/api/me/hosting-connections/${id}/verify`, "POST", { code: code || undefined }),
  // Снимок баланса всегда 200 (даже при ошибке провайдера); возвращает Connection.
  refreshHostingBalance: (id: number) =>
    req<HostingConnection>(`/api/me/hosting-connections/${id}/balance/refresh`, "POST", {}),
  hostingCatalog: (id: number, location?: string) =>
    req<HostingCatalog>(`/api/me/hosting-connections/${id}/catalog${location ? `?location=${encodeURIComponent(location)}` : ""}`),
  // ── облачные ресурсы (cabinet_cloud.go) ──
  cloudResources: () => req<{ resources: CloudResource[] }>("/api/me/cloud-resources"),
  cloudResource: (id: number) => req<CloudResource>(`/api/me/cloud-resources/${id}`),
  createCloudResource: (body: CloudCreateBody, key: string) =>
    req<CommandAck>("/api/me/cloud-resources", "POST", body, { headers: { "Idempotency-Key": key } }),
  deleteCloudResource: (id: number, key: string) =>
    req<CommandAck>(`/api/me/cloud-resources/${id}`, "DELETE", undefined,
      { headers: { "Idempotency-Key": key, "X-Confirm-Action": "delete" } }),
  // spec 202, impl 200 CloudResource — обрабатываем оба.
  changeCloudIP: (id: number, key: string, subnetId?: number) =>
    req<CloudResource | CommandAck>(`/api/me/cloud-resources/${id}/change-ip`, "POST",
      { subnet_id: subnetId || 0 }, { headers: { "Idempotency-Key": key } }),
  replaceCloudResource: (id: number, key: string, body: CloudReplaceBody) =>
    req<CommandAck>(`/api/me/cloud-resources/${id}/replace`, "POST", body, { headers: { "Idempotency-Key": key } }),
  // FROZEN: {profile_id, vars} — профиль адресуется id, содержимое одно.
  installCloudResource: (id: number, key: string, body: { profile_id?: number; vars?: Record<string, unknown> }) =>
    req<Operation>(`/api/me/cloud-resources/${id}/install`, "POST", body, { headers: { "Idempotency-Key": key } }),
  profileDefaults: (profileId: number) => req<ProfileDefaults>(`/api/me/profile-defaults?profile_id=${profileId}`),
  saveProfileDefaults: (body: { profile_id: number; vars: Record<string, string>; expected_revision: number }) =>
    req<ProfileDefaults>("/api/me/profile-defaults", "PUT", body),
  // FROZEN: 200 {ssh_ok,error}; 409 not_ready/ssh_unavailable; 501 beget.
  checkCloudResource: (id: number) =>
    req<CloudCheckResult>(`/api/me/cloud-resources/${id}/check`, "POST"),
  // FROZEN: сверка инвентаря провайдера при неясном платном исходе.
  reconcileCloudResource: (id: number) =>
    req<CloudReconcileResult>(`/api/me/cloud-resources/${id}/reconcile`, "POST", {}),
  estimateCloudResource: (body: EstimateBody) => req<EstimateResult>("/api/me/cloud-resources/estimate", "POST", body),
  cloudIPOptions: (id: number) => req<unknown>(`/api/me/cloud-resources/${id}/ip-options`),
  // ── управление нодой (cabinet_cloud.go §5) ──
  nodeManagement: (id: string) => req<ManagementPolicy>(`/api/me/nodes/${encodeURIComponent(id)}/management`),
  putNodeManagement: (id: string, body: ManagementBody) =>
    req<ManagementPolicy>(`/api/me/nodes/${encodeURIComponent(id)}/management`, "PUT", body),
  nodeSettings: (id: string) => req<NodeSettings>(`/api/me/nodes/${encodeURIComponent(id)}/settings`),
  patchNodeSettings: (id: string, body: NodeSettingsPatch) =>
    req<NodeSettings>(`/api/me/nodes/${encodeURIComponent(id)}/settings`, "PATCH", body),
  // ── флот/Autopilot ──
  createFleet: (body: FleetCreateBody, key: string) =>
    req<FleetCreateAck>("/api/me/fleets", "POST", body, { headers: { "Idempotency-Key": key } }),
  fleet: (id: number) => req<Fleet>(`/api/me/fleets/${id}`),
  fleets: () => req<{ fleets: FleetSummary[] }>("/api/me/fleets"),
  logout: () => req<void>("/api/auth/logout", "POST"),
};

/** UUID для idem_key: фиксируется на попытку, повтор запроса не создаёт второй заказ. */
export function newIdemKey(): string {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}
