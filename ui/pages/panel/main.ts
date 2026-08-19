/* Панель регистратора: маршрутизация, цикл обновления, вход.

   Логика перенесена из прежней версии ДОСЛОВНО — маршруты, состав
   запросов, интервал опроса, поведение при 401. Переписан только слой
   представления, он живёт в ./views/*. */

import { $, esc } from "../../lib/dom";
import { initTheme } from "../../lib/theme";
import { api, state, showLogin, hideLogin, showToast, LS_KEY } from "./state";

import * as overview from "./views/overview";
import * as nodes from "./views/nodes";
import * as node from "./views/node";
import * as dns from "./views/dns";
import * as srmd from "./views/srmd";
import * as events from "./views/events";
import * as settings from "./views/settings";

const VIEW_META: Record<string, { title: string }> = {
  overview: { title: "Обзор" },
  nodes:    { title: "Ноды пула" },
  dns:      { title: "Мастера и DNS" },
  srmd:     { title: "СРМД — распределение и масштабирование доменов" },
  events:   { title: "Журнал событий" },
  settings: { title: "Настройки" },
};

interface Route { view: string; id?: string; }

function currentRoute(): Route {
  const parts = (location.hash || "").replace(/^#\/?/, "").split("/").filter(Boolean);
  if (parts[0] === "nodes" && parts[1]) return { view: "node", id: decodeURIComponent(parts[1]) };
  return { view: VIEW_META[parts[0]] ? parts[0] : "overview" };
}

function route(): void {
  const r = currentRoute();
  document.querySelectorAll<HTMLElement>(".view").forEach((v) => { v.hidden = v.id !== "view-" + r.view; });
  document.querySelectorAll<HTMLElement>(".nav a").forEach((a) =>
    a.classList.toggle("active", a.dataset.view === (r.view === "node" ? "nodes" : r.view)));
  $("view-title").textContent = r.view === "node" ? "Нода · " + r.id : VIEW_META[r.view].title;

  if (r.view === "node") {
    if (state.nodeId !== r.id) {
      state.nodeId = r.id!;
      state.node = null;
      $("node-detail").innerHTML = `<div class="table-empty">загрузка…</div>`;
    }
    node.load(r.id!).catch((e: Error) => {
      $("node-detail").innerHTML =
        `<div class="table-empty">не удалось загрузить ноду: ${esc(e.message)}<br>возможно, она уже удалена из пула</div>`;
    });
  }
  if (r.view === "settings" && !state.settingsLoaded) {
    settings.load().then(() => { state.settingsLoaded = true; }).catch(() => {});
  }
  if (r.view === "srmd" && !state.srmdLoaded) {
    srmd.loadSettings().then(() => { state.srmdLoaded = true; }).catch(() => {});
  }
  renderAll();
}

function renderAll(): void {
  const o = state.ov;
  if (o) {
    renderHeader(o);
    renderBanner(o);
    const r = currentRoute();
    if (r.view === "overview") {
      overview.render(o);
      events.renderInto($("events-mini"), state.events.slice(0, 8));
    } else if (r.view === "nodes") {
      nodes.render(o);
    } else if (r.view === "dns") {
      dns.render(o);
    } else if (r.view === "srmd") {
      srmd.render(o);
    } else if (r.view === "events") {
      events.renderView();
    }
  }
  const r = currentRoute();
  if (r.view === "node" && state.node && state.nodeId === r.id) node.render(state.node);
}

/* ── шапка и баннер тревог ──────────────────────────────────────────── */
function renderHeader(o: any): void {
  $("hdr-sub").textContent =
    `доменов: ${o.domains.length} · SNI: ${o.tls_domain} · аптайм: ${fmtUptime(o.uptime_sec)}`;
  $("nav-nodes-count").textContent = o.nodes_total || "";
}

function fmtUptime(sec: number): string {
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60);
  return h > 0 ? `${h} ч ${m} м` : `${m} м`;
}

function renderBanner(o: any): void {
  const masters = o.masters || [];
  // домены, свёрнутые СРМД в CNAME, не «без мастера» — у них просто нет
  // своей A-записи по определению, предупреждение им не положено
  const unassigned = masters.filter((m: any) => !m.node_id && !m.cname_target).map((m: any) => m.domain);
  const dead = masters.filter((m: any) => m.node_id && m.dead).map((m: any) => m.domain);
  const activeCount = masters.filter((m: any) => !m.cname_target).length;
  const banner = $("alert-banner");
  const set = (text: string) => { banner.textContent = "⚠ " + text; banner.classList.add("show"); };

  if (o.nodes_fully_healthy === 0) {
    set("В пуле нет здоровых нод — DNS остался на последних мастерах.");
  } else if (o.srmd && o.srmd.alert) {
    set(o.srmd.alert);
  } else if (unassigned.length > 0) {
    set("Домены без мастера: " + unassigned.join(", ") + " — A-записи для них не выставлены.");
  } else if (dead.length > 0) {
    set("Мастер нездоров у доменов: " + dead.join(", ") + " — скоро переключатся.");
  } else if (o.nodes_fully_healthy < activeCount) {
    set(`Нод меньше, чем доменов (${o.nodes_fully_healthy}/${activeCount}): резерва нет.`);
  } else if (o.nodes_fully_healthy === activeCount) {
    set("Резерва нет: нод ровно столько, сколько доменов. При блокировке любой переключать будет некуда.");
  } else {
    banner.classList.remove("show");
  }
}

/* ── цикл обновления ────────────────────────────────────────────────── */
async function refresh(): Promise<void> {
  const [ov, ev] = await Promise.all([
    api("/panel/api/overview"),
    api("/panel/api/events?limit=1000"),
  ]);
  hideLogin();
  state.ov = ov;
  state.events = ev.events || [];
  const r = currentRoute();
  if (r.view === "node") await node.load(r.id!).catch(() => {});
  $("last-upd").textContent = "обновлено " + new Date().toLocaleTimeString("ru-RU");
  $("live-text").textContent = "live";
  $("live-pill").className = "status-pill live";
  renderAll();
}

/* ── вход ───────────────────────────────────────────────────────────── */
function submitToken(): void {
  const t = ($("token-input") as HTMLInputElement).value.trim();
  if (!t) return;
  localStorage.setItem(LS_KEY, t);
  refresh().catch(() => {});
}
$("login-btn").onclick = submitToken;
$("token-input").addEventListener("keydown", (e) => { if ((e as KeyboardEvent).key === "Enter") submitToken(); });
$("logout-btn").onclick = () => { localStorage.removeItem(LS_KEY); showLogin(false); };

/* Показать/скрыть значение поля-секрета: читать его глазами всё-таки нужно. */
document.addEventListener("click", (e) => {
  const b = (e.target as HTMLElement).closest<HTMLElement>("button[data-reveal]");
  if (!b) return;
  const inp = $(b.dataset.reveal!) as HTMLInputElement;
  if (inp) inp.type = inp.type === "password" ? "text" : "password";
});

window.addEventListener("hashchange", route);

/* ── старт ──────────────────────────────────────────────────────────── */
initTheme();
settings.init();
srmd.init();
events.init();
nodes.init();

if (!localStorage.getItem(LS_KEY)) showLogin(false);
route();
refresh().catch((err: Error) => { if (err.message !== "unauthorized") console.warn(err); });
setInterval(() => {
  refresh().catch(() => {
    $("live-text").textContent = "нет связи";
    $("live-pill").className = "status-pill off";
  });
}, 5000);

export { showToast };
