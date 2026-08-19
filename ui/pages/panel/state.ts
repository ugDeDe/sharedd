/* Состояние, авторизация и слой API панели.

   Всё в этом файле перенесено из прежней версии ДОСЛОВНО: пути запросов,
   заголовок Authorization, ключ localStorage и поведение при 401.
   Дизайн этого не касается. */

import { $ } from "../../lib/dom";

export const LS_KEY = "sharedd.panelToken";

export interface PanelState {
  ov: any;
  events: any[];
  nodeId: string | null;
  node: any;
  settingsLoaded: boolean;
  srmdLoaded: boolean;
}

export const state: PanelState = {
  ov: null, events: [], nodeId: null, node: null,
  settingsLoaded: false, srmdLoaded: false,
};

export function getToken(): string {
  return localStorage.getItem(LS_KEY) || "";
}

export async function api(path: string): Promise<any> {
  const resp = await fetch(path, {
    headers: { "Authorization": "Bearer " + getToken() },
    cache: "no-store",
  });
  if (resp.status === 401) { showLogin(true); throw new Error("unauthorized"); }
  if (!resp.ok) throw new Error("HTTP " + resp.status);
  return resp.json();
}

export function showLogin(bad: boolean): void {
  $("login-overlay").classList.add("show");
  $("login-err").classList.toggle("show", !!bad);
  setTimeout(() => ($("token-input") as HTMLInputElement).focus(), 50);
}

export function hideLogin(): void {
  $("login-overlay").classList.remove("show");
}

/* ── тост ───────────────────────────────────────────────────────────── */
let toastTimer: number | undefined;
export function showToast(text: string, isErr?: boolean): void {
  const t = $("toast");
  t.textContent = text;
  t.classList.toggle("err", !!isErr);
  t.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => t.classList.remove("show"), 5000);
}

/* ── сохранение секции настроек ─────────────────────────────────────
   Разбор ответа перенесён дословно: сервер сообщает, удалось ли записать
   правку в файл и что произошло с DNS-push, и это надо показывать. */
export async function saveSection(payload: unknown, note?: string): Promise<any> {
  const resp = await fetch("/panel/api/config", {
    method: "PUT",
    headers: { "Authorization": "Bearer " + getToken(), "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  const data = await resp.json().catch(() => ({} as any));
  if (resp.status === 401) { showLogin(true); throw new Error("unauthorized"); }
  if (!resp.ok) throw new Error(data.error || ("HTTP " + resp.status));
  const msg = note || "Сохранено";
  const bits: string[] = [];
  if (data.persisted === false) bits.push("⚠ в файл НЕ записано: " + (data.persist_warning || ""));
  if (data.dns_push) {
    const up = (data.dns_push.updated || []).length, er = (data.dns_push.errors || []).length;
    bits.push(`DNS-push: ${up} ok / ${er} err`);
  }
  showToast(bits.length ? msg + " · " + bits.join(" · ") : msg, data.persisted === false);
  return data;
}
