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
    <div class="cfg">
      <div class="cfg-head">${icon(ICONS.link)}<span>Прокси · SNI</span></div>
      <div class="cfg-body">
        <div class="field">
          <label for="s-sni">tls_domain</label>
          <input type="text" id="s-sni" class="mono" placeholder="www.microsoft.com">
          <span class="help">Домен маскировки; уходит нодам в tls_domains.</span>
        </div>
        <div class="note">Ноды подтянут на ближайшем sync (sync_ms) и обновят telemt.</div>
      </div>
      <div class="cfg-foot"><button class="btn primary" id="s-sni-save">Сохранить SNI</button></div>
    </div>

    <div class="cfg">
      <div class="cfg-head">${icon(ICONS.users)}<span>Пользователи MTProto</span></div>
      <div class="cfg-body">
        <table id="users-tbl"><tbody id="users-body"></tbody></table>
        <div class="note">Ноды только ДОБАВЛЯЮТ пользователей в telemt — удалённые здесь остаются на нодах до ручной чистки.</div>
      </div>
      <div class="cfg-foot">
        <button class="btn" id="s-user-add">+ пользователь</button>
        <button class="btn" id="s-user-gen">сгенерить имя+secret</button>
        <button class="btn primary" id="s-users-save" style="margin-left:auto">Сохранить пользователей</button>
      </div>
    </div>

    <div class="cfg">
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

    <div class="cfg">
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

    <div class="cfg">
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

    <div class="cfg">
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

    <div class="cfg">
      <div class="cfg-head">${icon(ICONS.ban)}<span>Карантин GP</span></div>
      <div class="cfg-body">
        <div class="field">
          <label for="s-hc-quar">quarantine_attempts</label>
          <input type="number" id="s-hc-quar" min="1" max="20">
          <span class="help">Неудачных проверок globalping до бана по IP.</span>
        </div>
        <div class="note">Попытка = независимо верифицированный measurement. Применяется на лету, 1..20. Выход из карантина любым путём, кроме восстановления, — бан.</div>
      </div>
      <div class="cfg-foot"><button class="btn primary" id="s-hc-quar-save">Сохранить карантин</button></div>
    </div>

    <div class="cfg">
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
  ($("s-sni") as HTMLInputElement).value = c.shared_proxy.tls_domain;
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
  ($("s-rot-ttl") as HTMLInputElement).value = c.rotation ? c.rotation.master_ttl_minutes : 30;
  ($("s-hc-quar") as HTMLInputElement).value = c.healthcheck ? c.healthcheck.quarantine_attempts : 3;
  ($("s-gp-base") as HTMLInputElement).value = c.globalping.api_base;
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

  $("s-sni-save").onclick = () =>
    saveSection({ shared_proxy: { tls_domain: ($("s-sni") as HTMLInputElement).value.trim(), users: usersModel } }, "SNI сохранён")
      .catch((e) => showToast(e.message, true));

  $("s-users-save").onclick = () =>
    saveSection({ shared_proxy: { tls_domain: ($("s-sni") as HTMLInputElement).value.trim(), users: usersModel } }, "Пользователи сохранены")
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

  $("s-rot-save").onclick = () =>
    saveSection({ rotation: { master_ttl_minutes: Math.max(0, parseInt(($("s-rot-ttl") as HTMLInputElement).value, 10) || 0) } }, "Лимит мастерства сохранён")
      .catch((e) => showToast(e.message, true));

  $("s-hc-quar-save").onclick = () =>
    saveSection({ healthcheck: { quarantine_attempts: Math.max(1, parseInt(($("s-hc-quar") as HTMLInputElement).value, 10) || 3) } }, "Карантин сохранён")
      .catch((e) => showToast(e.message, true));

  $("s-gp-save").onclick = () =>
    saveSection({ globalping: { api_base: ($("s-gp-base") as HTMLInputElement).value.trim() } }, "Globalping сохранён")
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
