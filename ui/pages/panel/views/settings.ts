/* Экран «Настройки»: секции конфига + редактор пользователей MTProto.

   Логика (что именно уходит в каждый saveSection, модель usersModel,
   randHex, разбор ответа сервера) перенесена из прежней версии ДОСЛОВНО.
   Разметку секций рисует этот модуль — в каркасе (index.html) под них есть
   только пустой контейнер #settings-grid. */

import { $, esc, icon, ICONS } from "../../../lib/dom";
import { api, getToken, showToast, saveSection, LS_KEY } from "../state";

/* иконка «глаз» для полей-секретов — своего нет в общем наборе ui/lib/dom.ts */
const EYE = `<path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/>`;

/* ── разметка секций ──────────────────────────────────────────────── */

function renderGrid(): void {
  $("settings-grid").innerHTML = `
    <div class="cfg" data-settings-group="proxy">
      <div class="cfg-head">${icon(ICONS.link)}<span>Прокси · SNI</span></div>
      <div class="cfg-body">
        <div class="field">
          <label for="s-sni">tls_domain</label>
          <input type="text" id="s-sni" class="mono" placeholder="www.microsoft.com">
          <span class="help">Домен маскировки; уходит нодам в tls_domains.</span>
        </div>
		<div class="field">
		  <label for="s-proxy-port">Общий порт прокси</label>
		  <input type="number" id="s-proxy-port" min="1" max="65535">
		  <span class="help">Нода допускается в пул и включает antiscan только при совпадении порта.</span>
		</div>
        <div class="note">Ноды подтянут на ближайшем sync (sync_ms) и обновят telemt.</div>
      </div>
      <div class="cfg-foot"><button class="btn primary" id="s-sni-save">Сохранить SNI</button></div>
    </div>

    <div class="cfg" data-settings-group="connect">
      <div class="cfg-head">${icon(ICONS.health)}<span>Подключить ноду</span></div>
      <div class="cfg-body">
        <div class="field">
          <label for="noc-node-command">Команда установки</label>
          <textarea id="noc-node-command" class="mono" readonly></textarea>
          <span class="help">Запустите на новом сервере от root.</span>
        </div>
        <div class="field">
          <label for="noc-node-token">Токен нод</label>
          <input id="noc-node-token" type="password" class="mono" readonly>
        </div>
      </div>
      <div class="cfg-foot">
        <button class="btn" id="noc-node-token-reveal">Показать токен</button>
        <button class="btn primary" id="noc-node-command-copy">Копировать команду</button>
      </div>
    </div>

    <div class="cfg" data-settings-group="proxy">
      <div class="cfg-head">${icon(ICONS.users)}<span>Пользователи MTProto</span></div>
      <div class="cfg-body">
        <table id="users-tbl"><tbody id="users-body"></tbody></table>
        <div class="note">Добавления и новые secret применяются на нодах автоматически. Удалённые здесь пользователи остаются на нодах до ручной чистки.</div>
      </div>
      <div class="cfg-foot">
        <button class="btn" id="s-user-add">+ пользователь</button>
        <button class="btn" id="s-user-gen">сгенерить имя+secret</button>
        <button class="btn primary" id="s-users-save" style="margin-left:auto">Сохранить пользователей</button>
      </div>
    </div>

    <div class="cfg" data-settings-group="pool">
      <div class="cfg-head">${icon(ICONS.dns)}<span>Cloudflare DNS</span></div>
      <div class="cfg-body">
        <div class="field">
          <label for="s-cf-token">api_token</label>
          <div class="input-group">
            <input type="password" id="s-cf-token" class="mono" autocomplete="off">
            <button class="in-btn" type="button" data-reveal="s-cf-token" title="Показать/скрыть">${icon(EYE)}</button>
          </div>
        </div>
        <div class="field">
          <label for="s-cf-zone">zone_id</label>
          <input type="text" id="s-cf-zone" class="mono">
        </div>
        <div class="field">
          <label for="s-cf-domains">managed-домены</label>
          <textarea id="s-cf-domains" class="mono" spellcheck="false"></textarea>
          <span class="help">По одному домену в строке.</span>
        </div>
        <div class="row">
          <div class="field" style="flex:1 1 130px">
            <label for="s-cf-ttl">dns_ttl</label>
            <input type="number" id="s-cf-ttl" min="1" max="86400">
          </div>
          <label class="check"><input type="checkbox" id="s-cf-proxied"><span class="box"></span> proxied</label>
        </div>
      </div>
      <div class="cfg-foot">
        <button class="btn" id="s-dns-push">Перезаписать DNS сейчас</button>
        <button class="btn primary" id="s-cf-save" style="margin-left:auto">Сохранить Cloudflare</button>
      </div>
    </div>

    <div class="cfg" data-settings-group="pool">
      <div class="cfg-head">${icon(ICONS.clock)}<span>Интервалы нод (мс)</span></div>
      <div class="cfg-body">
        <div class="field"><label for="s-nd-heartbeat">heartbeat</label><input type="number" id="s-nd-heartbeat"></div>
        <div class="field"><label for="s-nd-globalping">globalping</label><input type="number" id="s-nd-globalping"></div>
        <div class="field"><label for="s-nd-metrics">metrics</label><input type="number" id="s-nd-metrics"></div>
        <div class="field"><label for="s-nd-sync">sync</label><input type="number" id="s-nd-sync"></div>
        <div class="note">Ноды получают интервалы в GET /config и применяют без рестарта.</div>
      </div>
      <div class="cfg-foot"><button class="btn primary" id="s-nd-save">Сохранить интервалы</button></div>
    </div>

    <div class="cfg" data-settings-group="pool">
      <div class="cfg-head">${icon(ICONS.swap)}<span>Ротация мастерства</span></div>
      <div class="cfg-body">
        <div class="field">
          <label for="s-rot-ttl">master_ttl_minutes</label>
          <input type="number" id="s-rot-ttl" min="0" max="43200">
          <span class="help">Максимум времени мастером одного домена, мин. 0 = без лимита.</span>
        </div>
        <div class="note">По истечении домен уходит следующей здоровой ноде очереди, бывший мастер — в конец очереди (round-robin). 0..43200 мин.</div>
      </div>
      <div class="cfg-foot"><button class="btn primary" id="s-rot-save">Сохранить ротацию</button></div>
    </div>

    <div class="cfg" data-settings-group="pool">
      <div class="cfg-head">${icon(ICONS.masters)}<span>Мастера на домен</span></div>
      <div class="cfg-body">
        <div class="field">
          <label for="s-sel-masters">Мастеров на домен</label>
          <input type="number" id="s-sel-masters" min="1" max="8">
          <span class="help">Число слотов-мастеров для каждого managed-домена (1–8). Это НЕ
            <span class="mono">[srmd] max_nodes_per_domain</span>: слоты селекции и создание доменов СРМД не связаны.</span>
        </div>
        <div class="note">При изменении набор A-записей домена будет перестроен под новое число слотов. DNS клиентов обновится сам; прокси-ссылки не меняются.</div>
        <div class="banner bad" id="s-sel-err" role="alert" hidden style="margin:0"></div>
      </div>
      <div class="cfg-foot"><button class="btn primary" id="s-sel-save">Сохранить слоты</button></div>
    </div>

    <div class="cfg" data-settings-group="globalping">
      <div class="cfg-head">${icon(ICONS.health)}<span>Globalping</span></div>
      <div class="cfg-body">
        <div class="field">
          <label for="s-gp-base">api_base</label>
          <input type="text" id="s-gp-base" class="mono">
        </div>
        <div class="note">Measurement'ы создают сами ноды; API-токен не нужен.</div>
      </div>
      <div class="cfg-foot"><button class="btn primary" id="s-gp-save">Сохранить Globalping</button></div>
    </div>

    <div class="cfg" data-settings-group="globalping">
      <div class="cfg-head">${icon(ICONS.health)}<span>MTProto-проверка</span></div>
      <div class="cfg-body">
        <label class="check"><input type="checkbox" id="s-mt-enabled"><span class="box"></span> Проверять ноды по MTProto вместе с Globalping</label>
        <div class="field">
          <label for="s-mt-token">API-ключ</label>
          <input type="password" id="s-mt-token" class="mono" spellcheck="false" autocomplete="off" placeholder="rf_…">
        </div>
        <div class="field">
          <label for="s-mt-base">api_base</label>
          <input type="text" id="s-mt-base" class="mono">
        </div>
        <div class="note">Регистратор делает настоящий MTProto-хендшейк общим секретом пула из РФ (за ТСПУ) и снаружи. Нода в карантин идёт, только если отрицательны обе проверки: Globalping провалена, но MTProto «OK» — нода считается доступной. «UNKNOWN» или сбой API — решает Globalping.</div>
      </div>
      <div class="cfg-foot"><button class="btn primary" id="s-mt-save">Сохранить MTProto-проверку</button></div>
    </div>

    <div class="cfg" data-settings-group="globalping">
      <div class="cfg-head">${icon(ICONS.ban)}<span>Карантин GP</span></div>
      <div class="cfg-body">
        <div class="field">
          <label for="s-hc-quar">quarantine_attempts</label>
          <input type="number" id="s-hc-quar" min="1" max="20">
          <span class="help">Неудачных проверок globalping до бана по IP.</span>
        </div>
		<div class="field">
		  <label for="s-hc-gp-validity">Срок годности GP, мин</label>
		  <input type="number" id="s-hc-gp-validity" min="1" max="525600">
		  <span class="help">Должен быть строго больше globalping_ms. Просрочка запрашивает внеочередную проверку и переводит ноду в карантин без штрафной попытки.</span>
		</div>
        <div class="note">Попытка = независимо верифицированный measurement. Применяется на лету, 1..20. Выход из карантина любым путём, кроме восстановления, — бан.</div>
      </div>
      <div class="cfg-foot"><button class="btn primary" id="s-hc-quar-save">Сохранить карантин</button></div>
    </div>

    <div class="cfg" data-settings-group="antiscan">
      <div class="cfg-head">${icon(ICONS.ban)}<span>Антискан · сканеры из abuse-листов</span></div>
      <div class="cfg-body">
        <label class="check"><input type="checkbox" id="s-as-enabled"><span class="box"></span> Блокировать сканеры на нодах</label>
        <div class="field">
          <label for="s-as-sources">Источники</label>
          <textarea id="s-as-sources" class="mono" spellcheck="false"></textarea>
          <span class="help">По одному URL в строке: IPv4-адреса, подсети CIDR или диапазоны a-b, по записи на строку (формат ipsum подходит).</span>
        </div>
        <div class="field">
          <label for="s-as-interval">Интервал обновления, мин</label>
          <input type="number" id="s-as-interval" min="5" max="1440">
        </div>
        <div class="field">
          <label for="s-as-geo">Российские диапазоны (geo_url)</label>
          <input type="text" id="s-as-geo" class="mono" spellcheck="false">
          <span class="help">RIPEstat country-resource-list или свой текстовый список CIDR. Пусто — RIPEstat по умолчанию.</span>
        </div>
        <div class="note">Регистратор оставляет из источников только российские адреса и подсети и раздаёт список нодам; ноды дропают их на общем порту прокси. Если источник недоступен, используется его последняя удачная копия.</div>
        <div class="as-status" id="s-as-status"></div>
      </div>
      <div class="cfg-foot"><button class="btn primary" id="s-as-save">Сохранить антискан</button></div>
    </div>

    <div class="cfg" data-settings-group="adpause">
      <div class="cfg-head">${icon(ICONS.swap)}<span>Рекламная пауза пула</span></div>
      <div class="cfg-body">
        <label class="check"><input type="checkbox" id="s-ad-enabled"><span class="box"></span> Показывать рекламу пула на нодах</label>
        <div class="field">
          <label for="s-ad-tag">Рекламный тег пула</label>
          <input type="text" id="s-ad-tag" class="mono" spellcheck="false" maxlength="32" placeholder="32 hex от @MTProxybot">
        </div>
        <div class="field">
          <label for="s-ad-minutes">Минут рекламы в сутки на весь пул</label>
          <input type="number" id="s-ad-minutes" min="1" max="1440">
          <span class="help">Делится поровну между нодами, умеющими подмену тега, с округлением вверх до целых окон master_ttl (не меньше одного окна на ноду). Реклама идёт только пока нода мастер, окна мастерства ноды чередуются (и через ротацию в очередь): рекламное, своё, рекламное… Когда пул выбрал бюджет, до полуночи (по часам регистратора) рекламы нет. Ноды добавились или ушли — доли пересчитываются.</span>
        </div>
        <div class="note">Считается только время, подтверждённое агентом; каждое включение и снятие тега пишется в журнал событий. Поддерживают ноды mtdd (тег меняется сразу у всех клиентов) и MTProxyL ≥ 1.4.9 с ME (тег пула ставится только общим пользователям пула, клиенты получают его при переподключении). Прочие ноды отмечены «не поддерживает» и рекламу не показывают.</div>
        <div class="as-status" id="s-ad-status"></div>
      </div>
      <div class="cfg-foot"><button class="btn primary" id="s-ad-save">Сохранить рекламу</button></div>
    </div>

    <div class="cfg" data-settings-group="system">
      <div class="cfg-head">${icon(ICONS.sliders)}<span>Панель</span></div>
      <div class="cfg-body">
        <div class="field">
          <label for="s-panel-token">token</label>
          <div class="input-group">
            <input type="password" id="s-panel-token" class="mono" autocomplete="off">
            <button class="in-btn" type="button" data-reveal="s-panel-token" title="Показать/скрыть">${icon(EYE)}</button>
          </div>
          <span class="help">Bearer для API панели и /status.</span>
        </div>
        <div class="field">
          <label for="s-panel-events">events_max</label>
          <input type="number" id="s-panel-events" min="50" max="10000">
          <span class="help">Размер журнала.</span>
        </div>
        <div class="note">После смены токена сессия слетит — войдите с новым.</div>
      </div>
      <div class="cfg-foot"><button class="btn primary" id="s-panel-save">Сохранить параметры панели</button></div>
    </div>
  `;
}

/* ── пользователи MTProto ─────────────────────────────────────────── */

let usersModel: Record<string, string> = {};
let settingsGroup = "connect";

function selectSettingsGroup(group: string): void {
  settingsGroup = group;
  document.querySelectorAll<HTMLElement>("#settings-nav [data-settings-tab]").forEach((button) => {
    button.classList.toggle("active", button.dataset.settingsTab === group);
  });
  document.querySelectorAll<HTMLElement>("#settings-grid [data-settings-group]").forEach((card) => {
    card.hidden = card.dataset.settingsGroup !== group;
  });
  $("s-static").hidden = group !== "system";
}

function renderUsersEditor(): void {
  const body = $("users-body");
  const names = Object.keys(usersModel).sort();
  if (names.length === 0) {
    body.innerHTML = `<tr><td class="muted">нет пользователей</td></tr>`;
    return;
  }
  body.innerHTML = names.map((n) => `<tr>
    <td class="mono">${esc(n)}</td>
    <td><input type="text" class="mono" data-user="${esc(n)}" value="${esc(usersModel[n])}" spellcheck="false"></td>
    <td style="width:40px"><button class="btn icon sm danger" data-del="${esc(n)}" title="Удалить">✕</button></td>
  </tr>`).join("");
}

function randHex(n: number): string {
  const b = new Uint8Array(n);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

/* ── загрузка ──────────────────────────────────────────────────────── */

export async function load(): Promise<void> {
  const data = await api("/panel/api/config");
  const c = data.config, st = data.static;
	const onboarding = data.node_onboarding || {};
	($("noc-node-command") as HTMLTextAreaElement).value = onboarding.install_command || "Публичный URL регистратора не настроен";
	($("noc-node-token") as HTMLInputElement).value = onboarding.token || "";
  ($("s-sni") as HTMLInputElement).value = c.shared_proxy.tls_domain;
  ($("s-proxy-port") as HTMLInputElement).value = c.shared_proxy.port || 443;
  usersModel = { ...(c.shared_proxy.users || {}) };
  renderUsersEditor();
  ($("s-cf-token") as HTMLInputElement).value = c.cloudflare.api_token;
  ($("s-cf-zone") as HTMLInputElement).value = c.cloudflare.zone_id;
  ($("s-cf-domains") as HTMLTextAreaElement).value = (c.cloudflare.domains || []).join("\n");
  ($("s-cf-ttl") as HTMLInputElement).value = c.cloudflare.dns_ttl;
  ($("s-cf-proxied") as HTMLInputElement).checked = !!c.cloudflare.proxied;
  ($("s-nd-heartbeat") as HTMLInputElement).value = c.node_defaults.heartbeat_ms;
  ($("s-nd-globalping") as HTMLInputElement).value = c.node_defaults.globalping_ms;
  ($("s-nd-metrics") as HTMLInputElement).value = c.node_defaults.metrics_ms;
  ($("s-nd-sync") as HTMLInputElement).value = c.node_defaults.sync_ms;
  ($("s-sel-masters") as HTMLInputElement).value = c.selection ? c.selection.masters_per_domain : 1;
  ($("s-rot-ttl") as HTMLInputElement).value = c.rotation ? c.rotation.master_ttl_minutes : 30;
  ($("s-hc-quar") as HTMLInputElement).value = c.healthcheck ? c.healthcheck.quarantine_attempts : 3;
  ($("s-hc-gp-validity") as HTMLInputElement).value = c.healthcheck ? c.healthcheck.globalping_validity_min : 15;
  ($("s-gp-base") as HTMLInputElement).value = c.globalping.api_base;
  const mt = c.mtproto_check || {};
  ($("s-mt-enabled") as HTMLInputElement).checked = !!mt.enabled;
  ($("s-mt-token") as HTMLInputElement).value = mt.token || "";
  ($("s-mt-base") as HTMLInputElement).value = mt.api_base || "";
  const as = c.antiscan || {};
  ($("s-as-enabled") as HTMLInputElement).checked = as.enabled !== false;
  ($("s-as-sources") as HTMLTextAreaElement).value = (as.sources || []).join("\n");
  ($("s-as-interval") as HTMLInputElement).value = as.interval_min || 30;
  ($("s-as-geo") as HTMLInputElement).value = as.geo_url || "";
  renderAntiscanStatus(data.antiscan_status);
  const ad = c.ad_pause || {};
  ($("s-ad-enabled") as HTMLInputElement).checked = !!ad.enabled;
  ($("s-ad-tag") as HTMLInputElement).value = ad.tag || "";
  ($("s-ad-minutes") as HTMLInputElement).value = ad.minutes_per_day || 60;
  renderAdPauseStatus(data.ad_pause_status, data.ad_pause_summary);
  ($("s-panel-token") as HTMLInputElement).value = c.panel.token;
  ($("s-panel-events") as HTMLInputElement).value = c.panel.events_max;
  // Раньше это был моноширинный дамп в рамке. Те же значения читаются
  // куда легче парами «ключ — значение»; правки тут невозможны — секции
  // требуют рестарта регистратора.
  const row = (k: string, v: unknown, mono = true) =>
    `<div class="static-row"><span class="static-k">${k}</span>` +
    `<span class="static-v${mono ? " mono" : ""}" title="${esc(v)}">${esc(v)}</span></div>`;
  $("s-static").innerHTML =
    `<div class="static-head">Требуют рестарта регистратора</div>` +
    `<div class="static-grid">` +
      row("http.addr", st.http_addr) +
      row("state.file", st.state_file) +
      row("probe_interval", st.probe_interval_ms + " мс") +
      row("probe_timeout", st.probe_timeout_ms + " мс") +
      row("selection_interval", st.selection_interval_ms + " мс") +
      row("heartbeat_ttl", st.heartbeat_ttl_sec + " с") +
      row("fail_threshold", st.fail_threshold) +
      row("recover_threshold", st.recover_threshold) +
      row("report_freshness", st.report_freshness_min + " мин") +
      row("prune_unhealthy", st.prune_unhealthy_min + " мин") +
    `</div>`;
}

/* ── антискан: состояние сборки списка ────────────────────────────── */

function fmtTime(iso: string | undefined): string {
  return iso ? new Date(iso).toLocaleString("ru-RU") : "—";
}

/* Короткое имя источника: домен + имя файла (полный URL — в подсказке). */
function shortSource(u: string): string {
  try {
    const url = new URL(u);
    const file = url.pathname.split("/").filter(Boolean).pop() || "";
    return file ? `${url.host} · ${file}` : url.host;
  } catch {
    return u;
  }
}

function renderAntiscanStatus(st: any): void {
  if (!st) { $("s-as-status").innerHTML = ""; return; }
  const row = (k: string, v: unknown, bad = false) =>
    `<div class="static-row"><span class="static-k">${esc(k)}</span>` +
    `<span class="static-v mono${bad ? " as-bad" : ""}" title="${esc(v)}">${esc(v)}</span></div>`;
  const rev = st.revision === "disabled" ? "выключен" : (st.revision || "ещё не собран");
  let html =
    `<div class="static-grid as-summary">` +
      row("ревизия", rev) +
      row("записей RU", st.entries) +
      row("собран", fmtTime(st.built_at)) +
      row("последняя попытка", fmtTime(st.last_attempt)) +
      row("RU-диапазонов", st.geo_ranges + (st.geo_at ? " · " + fmtTime(st.geo_at) : "")) +
      (st.geo_error ? row("geo", st.geo_error, true) : "") +
    `</div>`;
  const sources = st.sources || [];
  if (sources.length) {
    html += `<div class="as-sources">` + sources.map((s: any) => {
      const status = s.error ? "ошибка: " + s.error : `${s.entries} записей · ${fmtTime(s.fetched_at)}`;
      return `<div class="as-src${s.error ? " as-bad" : ""}">` +
        `<span class="as-src-name mono" title="${esc(s.url)}">${esc(shortSource(s.url))}</span>` +
        `<span class="as-src-stat" title="${esc(status)}">${esc(status)}</span></div>`;
    }).join("") + `</div>`;
  }
  if (st.last_error) html += `<div class="as-src as-bad"><span class="as-src-stat as-wrap">${esc(st.last_error)}</span></div>`;
  $("s-as-status").innerHTML = html;
}

/* ── рекламная пауза: фактическая доля по нодам ──────────────────── */

function fmtDur(sec: number): string {
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60);
  return h > 0 ? `${h} ч ${m} мин` : `${m} мин`;
}

const AD_STATE: Record<string, string> = {
  pool: "тег пула", own: "свой тег", unsupported: "не поддерживает", failed: "ошибка", "": "нет данных",
};

function renderAdPauseStatus(list: any[], sum: any): void {
  if (!list || list.length === 0) { $("s-ad-status").innerHTML = ""; return; }
  const head = sum
    ? `<div class="as-src"><span class="as-src-name">Сегодня (${esc(sum.day)})</span>` +
      `<span class="as-src-stat">пул ${fmtDur(sum.used_sec)} из ${fmtDur(sum.budget_sec)} · нод с рекламой: ${sum.nodes} · окно ${fmtDur(sum.period_sec)}</span></div>`
    : "";
  $("s-ad-status").innerHTML = `<div class="as-sources">` + head + list.map((n: any) => {
    const state = AD_STATE[n.state ?? ""] ?? n.state;
    const now = n.active ? " · сейчас окно рекламы" : (n.master ? " · мастер" : "");
    const quota = n.quota_sec ? ` из ${fmtDur(n.quota_sec)}` : "";
    const stat = `${state}${now} · сегодня ${fmtDur(n.today_sec)}${quota} · всего ${fmtDur(n.ad_sec)}`;
    return `<div class="as-src${n.state === "failed" ? " as-bad" : ""}">` +
      `<span class="as-src-name mono" title="${esc(n.ip)}">${esc(n.node_id)}${n.node_type ? " · " + esc(n.node_type) : ""}</span>` +
      `<span class="as-src-stat" title="${esc(stat)}">${esc(stat)}</span></div>`;
  }).join("") + `</div>`;
}

/* ── DNS-push (кнопка «Перезаписать DNS сейчас») ──────────────────── */

async function dnsPush(): Promise<void> {
  try {
    const resp = await fetch("/panel/api/dns-push", { method: "POST", headers: { "Authorization": "Bearer " + getToken() } });
    const data = await resp.json();
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    showToast(
      `DNS-push: ${(data.updated || []).join(", ") || "—"}${(data.errors || []).length ? " · ошибки: " + data.errors.join("; ") : ""}`,
      (data.errors || []).length > 0 && (data.updated || []).length === 0,
    );
  } catch (e) {
    showToast((e as Error).message, true);
  }
}

/* ── навешивание обработчиков (один раз) ──────────────────────────── */

export function init(): void {
  renderGrid();
	selectSettingsGroup(settingsGroup);
	$("settings-nav").addEventListener("click", (e) => {
	  const button = (e.target as HTMLElement).closest<HTMLElement>("button[data-settings-tab]");
	  if (button) selectSettingsGroup(button.dataset.settingsTab!);
	});

  $("users-body").addEventListener("click", (e) => {
    const del = (e.target as HTMLElement).closest<HTMLElement>("button[data-del]");
    if (del) { delete usersModel[del.dataset.del!]; renderUsersEditor(); }
  });
  $("users-body").addEventListener("input", (e) => {
    const inp = (e.target as HTMLElement).closest<HTMLInputElement>("input[data-user]");
    if (inp) usersModel[inp.dataset.user!] = inp.value.trim();
  });
  $("s-user-add").onclick = () => {
    let i = 1;
    while (usersModel["user" + i] !== undefined) i++;
    usersModel["user" + i] = randHex(16);
    renderUsersEditor();
  };
  $("s-user-gen").onclick = () => {
    const name = "u" + randHex(3);
    usersModel[name] = randHex(16);
    renderUsersEditor();
  };

  $("cfg-reload").onclick = () => load().then(() => showToast("Настройки перечитаны")).catch(() => {});

	$("noc-node-token-reveal").onclick = () => {
	  const input = $("noc-node-token") as HTMLInputElement;
	  input.type = input.type === "password" ? "text" : "password";
	};
	$("noc-node-command-copy").onclick = () => {
	  const command = ($("noc-node-command") as HTMLTextAreaElement).value;
	  navigator.clipboard.writeText(command).then(() => showToast("Команда скопирована"), () => showToast("Не удалось скопировать", true));
	};

  $("s-sni-save").onclick = () =>
    saveSection({ shared_proxy: { tls_domain: ($("s-sni") as HTMLInputElement).value.trim(), port: parseInt(($("s-proxy-port") as HTMLInputElement).value, 10), users: usersModel } }, "Прокси сохранён")
      .catch((e) => showToast(e.message, true));

  $("s-users-save").onclick = () =>
    saveSection({ shared_proxy: { tls_domain: ($("s-sni") as HTMLInputElement).value.trim(), port: parseInt(($("s-proxy-port") as HTMLInputElement).value, 10), users: usersModel } }, "Пользователи сохранены")
      .catch((e) => showToast(e.message, true));

  $("s-cf-save").onclick = () => {
    const domains = ($("s-cf-domains") as HTMLTextAreaElement).value.split("\n").map((s) => s.trim()).filter(Boolean);
    saveSection({
      cloudflare: {
        api_token: ($("s-cf-token") as HTMLInputElement).value.trim(),
        zone_id: ($("s-cf-zone") as HTMLInputElement).value.trim(),
        domains,
        dns_ttl: parseInt(($("s-cf-ttl") as HTMLInputElement).value, 10) || 60,
        proxied: ($("s-cf-proxied") as HTMLInputElement).checked,
      },
    }, "Cloudflare сохранён").catch((e) => showToast(e.message, true));
  };
  $("s-dns-push").onclick = dnsPush;

  $("s-nd-save").onclick = () =>
    saveSection({
      node_defaults: {
        heartbeat_ms: parseInt(($("s-nd-heartbeat") as HTMLInputElement).value, 10) || 0,
        globalping_ms: parseInt(($("s-nd-globalping") as HTMLInputElement).value, 10) || 0,
        metrics_ms: parseInt(($("s-nd-metrics") as HTMLInputElement).value, 10) || 0,
        sync_ms: parseInt(($("s-nd-sync") as HTMLInputElement).value, 10) || 0,
      },
    }, "Интервалы сохранены").catch((e) => showToast(e.message, true));

  $("s-sel-save").onclick = () => {
    const errBox = $("s-sel-err");
    errBox.hidden = true;
    const n = parseInt(($("s-sel-masters") as HTMLInputElement).value, 10);
    if (!Number.isFinite(n) || n < 1 || n > 8) {
      errBox.textContent = "Допустимо целое число 1..8.";
      errBox.hidden = false;
      return;
    }
    saveSection({ selection: { masters_per_domain: n } }, "Слоты мастеров сохранены")
      .catch((e) => { errBox.textContent = e.message; errBox.hidden = false; });
  };

  $("s-rot-save").onclick = () =>
    saveSection({ rotation: { master_ttl_minutes: Math.max(0, parseInt(($("s-rot-ttl") as HTMLInputElement).value, 10) || 0) } }, "Лимит мастерства сохранён")
      .catch((e) => showToast(e.message, true));

  $("s-hc-quar-save").onclick = () =>
    saveSection({ healthcheck: {
      quarantine_attempts: Math.max(1, parseInt(($("s-hc-quar") as HTMLInputElement).value, 10) || 3),
      globalping_validity_min: Math.max(1, parseInt(($("s-hc-gp-validity") as HTMLInputElement).value, 10) || 15),
    } }, "Карантин сохранён")
      .catch((e) => showToast(e.message, true));

  $("s-gp-save").onclick = () =>
    saveSection({ globalping: { api_base: ($("s-gp-base") as HTMLInputElement).value.trim() } }, "Globalping сохранён")
      .catch((e) => showToast(e.message, true));

  $("s-mt-save").onclick = () =>
    saveSection({ mtproto_check: {
      enabled: ($("s-mt-enabled") as HTMLInputElement).checked,
      token: ($("s-mt-token") as HTMLInputElement).value.trim(),
      api_base: ($("s-mt-base") as HTMLInputElement).value.trim(),
    } }, "MTProto-проверка сохранена")
      .catch((e) => showToast(e.message, true));

  $("s-as-save").onclick = () =>
    saveSection({ antiscan: {
      enabled: ($("s-as-enabled") as HTMLInputElement).checked,
      sources: ($("s-as-sources") as HTMLTextAreaElement).value.split("\n").map((s) => s.trim()).filter(Boolean),
      interval_min: parseInt(($("s-as-interval") as HTMLInputElement).value, 10) || 30,
      geo_url: ($("s-as-geo") as HTMLInputElement).value.trim(),
    } }, "Антискан сохранён — список пересобирается")
      // Пересборка идёт в фоне (скачивание источников); состояние подтянется
      // со следующим перечитыванием настроек.
      .then(() => setTimeout(() => load().catch(() => {}), 5000))
      .catch((e) => showToast(e.message, true));

  $("s-ad-save").onclick = () =>
    saveSection({ ad_pause: {
      enabled: ($("s-ad-enabled") as HTMLInputElement).checked,
      tag: ($("s-ad-tag") as HTMLInputElement).value.trim(),
      minutes_per_day: parseInt(($("s-ad-minutes") as HTMLInputElement).value, 10) || 60,
    } }, "Рекламная пауза сохранена")
      .then(() => load().catch(() => {}))
      .catch((e) => showToast(e.message, true));

  $("s-panel-save").onclick = () => {
    const newTok = ($("s-panel-token") as HTMLInputElement).value.trim();
    saveSection({ panel: { token: newTok, events_max: parseInt(($("s-panel-events") as HTMLInputElement).value, 10) || 500 } }, "Параметры панели сохранены")
      .then(() => {
        if (newTok && newTok !== getToken()) {
          localStorage.setItem("sharedd.panelToken", newTok);
          showToast("Токен обновлён — сессия переключена на новый токен");
        }
      })
      .catch((e) => showToast(e.message, true));
  };
}
