import { esc } from "../../lib/dom";
import { api, newIdemKey } from "./api";
import type {
  APIError,
  BlueprintBlock,
  ProfileRun,
  ProfileRunBody,
  ProfileRunCheckpoint,
  ProfileVersion,
  ResumeDecision,
} from "./api";
import { blockFor, bytes, EFFECTS, reachable, runtimeGate, TITLES, versionByPK } from "./blueprint-model";
import { mountInstance, mountVersionPicker } from "./blueprint-instance";

const TERMINAL = new Set(["succeeded", "failed", "cancelled"]);
const STATES: Record<string, [string, string]> = {
  pending: ["Ожидает", "info"],
  running: ["Выполняется", "info"],
  reconciling: ["Сверка", "info"],
  waiting_event: ["Ожидает события", "warn"],
  waiting_retry: ["Ожидает безопасного повтора", "warn"],
  paused: ["Приостановлен", "warn"],
  needs_attention: ["Нужен ручной разбор", "special"],
  failed: ["Ошибка", "bad"],
  succeeded: ["Завершён", "ok"],
  cancelled: ["Остановлен", ""],
  ready: ["Готов к шагу", "info"],
  claimed: ["Зарезервирован", "warn"],
  dispatched: ["Отправлен", "info"],
  confirmed: ["Подтверждён", "ok"],
  rejected: ["Отклонён", "bad"],
  unknown: ["Результат неизвестен — сверка/разбор", "special"],
  waiting: ["Ожидает", "warn"],
};
const stateBadge = (state: string): string => {
  const [label, cls] = STATES[state] || [state, ""];
  return `<span class="badge ${cls}">${esc(label)}</span> <span class="mono">${esc(state)}</span>`;
};
const time = (ts?: number): string => (ts ? new Date(ts * 1000).toLocaleString("ru") : "—");
let review: HTMLDialogElement | null = null;
let uncertainRun: { body: ProfileRunBody; key: string } | null = null;
const uncertainCommands = new Set<number>();
export function leaveRunReview(): boolean {
  if (review?.dataset.busy === "true") return false;
  review?.close();
  review?.remove();
  review = null;
  return true;
}
window.addEventListener("beforeunload", (event) => {
  if (uncertainRun || review?.dataset.busy === "true") {
    event.preventDefault();
    event.returnValue = "";
  }
});

export function openRunReview(version: ProfileVersion, blocks: BlueprintBlock[]): void {
  const graph = version.blueprint;
  if (!graph || runtimeGate(graph)) return;
  if (review?.isConnected) {
    if (!review.open) review.showModal();
    return;
  }
  const dialog = document.createElement("dialog");
  dialog.className = "blueprint-dialog blueprint-review";
  dialog.setAttribute("aria-label", "Проверить и разрешить запуск графа");
  review = dialog;
  const path = reachable(graph, graph.entries.initial);
  dialog.innerHTML = `<h2>Проверить и разрешить запуск · версия ${version.version}</h2><p class="mono">Профиль #${version.profile_id} · PK версии ${version.profile_version_id} · initial</p>
    <p>Сохранение профиля не запускает его и не включает автоматические расходы. Этот запуск разрешает перечисленные платные действия в подключении Nodehost. Цена API не предоставлена. Ошибка или неизвестный результат не разрешают повторную покупку.</p>
    <details open><summary>Действия первичного пути и условные переходы</summary><ul>${graph.nodes
      .filter((n) => path.has(n.id))
      .map(
        (n) =>
          `<li>${esc(n.id)} · ${esc(TITLES[n.type] || n.type)}<span class="mono"> ${esc(n.type)}</span> · ${blockFor(blocks, n.type) ? EFFECTS[blockFor(blocks, n.type)!.effects] : "Класс не загружен"}</li>`,
      )
      .join("")}</ul><p>${graph.edges
      .filter((e) => path.has(e.from))
      .map((e) => `${esc(e.from)} · ${esc(e.port)} → ${esc(e.to)}`)
      .join("; ")}</p></details>
    <form><fieldset data-review-fields><div data-instance-root></div><div class="field"><label for="bp-run-node-id">node_id · необязательная постоянная идентичность развёртывания</label><input id="bp-run-node-id" maxlength="128" pattern="[A-Za-z0-9_-]+" aria-describedby="bp-run-node-help"><span class="help" id="bp-run-node-help">Это не имя VPS. При deployment_unresolved не меняйте ID для обхода защиты: сначала разберите существующее развёртывание.</span></div>
    <label class="check"><input type="checkbox" data-run-consent required><span class="box"></span>Разрешить этот запуск и его платные действия. Цена не предоставлена API.</label></fieldset>
    <div data-run-error class="banner bad" role="alert" hidden></div><div class="blueprint-toolbar"><button type="button" class="btn" data-close>Закрыть</button><button class="btn primary" type="submit">${graph.nodes.some((n) => path.has(n.id) && n.type === "server.create") ? "Создать VPS и запустить" : "Запустить граф"}</button><button class="btn" type="button" data-same-key hidden>Повторить тот же запрос с тем же ключом</button></div></form>`;
  document.body.appendChild(dialog);
  dialog.showModal();
  const instance = mountInstance(dialog.querySelector<HTMLElement>("[data-instance-root]")!, graph);
  const form = dialog.querySelector<HTMLFormElement>("form")!,
    fields = dialog.querySelector<HTMLFieldSetElement>("fieldset")!,
    submit = form.querySelector<HTMLButtonElement>('[type="submit"]')!,
    retry = form.querySelector<HTMLButtonElement>("[data-same-key]")!,
    errorBox = form.querySelector<HTMLElement>("[data-run-error]")!;
  let pending = false;
  const close = (): void => {
    if (pending) return;
    dialog.close();
    dialog.remove();
    review = null;
  };
  dialog.oncancel = (e) => {
    e.preventDefault();
    close();
  };
  dialog.querySelector("[data-close]")!.addEventListener("click", close);
  const send = async (intent: { body: ProfileRunBody; key: string }): Promise<void> => {
    const unresolved = uncertainRun;
    if (pending || (unresolved && unresolved !== intent)) return;
    pending = true;
    dialog.dataset.busy = "true";
    fields.disabled = true;
    submit.disabled = true;
    retry.disabled = true;
    errorBox.hidden = true;
    try {
      const ack = await api.createProfileRun(intent.body, intent.key);
      if (
        !Number.isSafeInteger(ack.run_id) ||
        ack.run_id <= 0 ||
        !Number.isSafeInteger(ack.operation_id) ||
        ack.operation_id <= 0
      )
        throw new Error(
          "Подтверждение запуска не содержит корректных run_id/operation_id. Нужна сверка исходного запроса.",
        );
      // This response belongs to the exact locked body/key sent above, never a fresh intent.
      uncertainRun = null;
      pending = false;
      close();
      location.hash = `#/profiles/runs/${ack.run_id}`;
    } catch (error) {
      const e = error as APIError;
      // A rejected replay (including local/server 403) says nothing about the original paid request.
      const ambiguous = !!unresolved || !e.status || e.status >= 500 || e.code === "idempotency_conflict";
      if (ambiguous) uncertainRun = unresolved || intent;
      errorBox.textContent = ambiguous
        ? `Результат запроса неизвестен: ${e.message}. Не создавайте новый запуск. Допустим только тот же запрос/ключ; можно открыть известный run ID из профилей.`
        : `${e.message}${e.code === "deployment_unresolved" ? " Не меняйте node_id для обхода защиты." : ""}`;
      errorBox.hidden = false;
      retry.hidden = !ambiguous;
      fields.disabled = ambiguous;
      submit.disabled = ambiguous;
      retry.disabled = false;
    } finally {
      pending = false;
      dialog.dataset.busy = "false";
    }
  };
  if (uncertainRun) {
    fields.disabled = true;
    submit.disabled = true;
    retry.hidden = false;
    errorBox.hidden = false;
    errorBox.textContent =
      "Предыдущий запуск не подтверждён. Сохранены исходные body и Idempotency-Key; повтор отправит только их, независимо от открытого профиля.";
  }
  retry.onclick = () => {
    if (uncertainRun) void send(uncertainRun);
  };
  form.onsubmit = async (e) => {
    e.preventDefault();
    if (pending || uncertainRun || !form.reportValidity()) return;
    pending = true;
    dialog.dataset.busy = "true";
    submit.disabled = true;
    try {
      const values = instance.collect();
      const node_id = dialog.querySelector<HTMLInputElement>("#bp-run-node-id")!.value.trim();
      const body: ProfileRunBody = {
        profile_version_id: version.profile_version_id,
        entry: "initial",
        inputs: values.inputs,
        ...(Object.keys(values.vars).length ? { vars: values.vars } : {}),
        ...(node_id ? { node_id } : {}),
        confirm: true,
      };
      if (bytes(body) > 256 * 1024) throw new Error("Тело запуска превышает 256 КиБ UTF-8.");
      fields.disabled = true;
      if (bytes({ format: "graph", blueprint: graph, inputs: body.inputs }) > 256 * 1024)
        throw new Error("Тело проверки схемы с входами превышает 256 КиБ UTF-8.");
      // Validate instance before the first mutation; no provider action here.
      const validation = await api.validateBlueprint(graph, body.inputs);
      if (!validation.valid)
        throw new Error(
          validation.diagnostics.map((d) => `${d.node_id || "вход"} ${d.port || ""}: ${d.message}`).join("; "),
        );
      pending = false;
      await send({ body, key: newIdemKey() });
    } catch (error) {
      errorBox.textContent = (error as Error).message;
      errorBox.hidden = false;
    } finally {
      pending = false;
      dialog.dataset.busy = "false";
      if (!uncertainRun) {
        fields.disabled = false;
        submit.disabled = false;
      }
    }
  };
}

let runView: RunView | null = null;
export function openBlueprintRun(root: HTMLElement, id: number): void {
  runView?.dispose();
  runView = new RunView(root, id);
}
export function closeBlueprintRun(): void {
  runView?.dispose();
  runView = null;
}
class RunView {
  private data: ProfileRun | null = null;
  private graphVersion: ProfileVersion | null = null;
  private blocks: BlueprintBlock[] = [];
  private timer: number | undefined;
  private fetching = false;
  private command = false;
  private disposed = false;
  private mutationUnknown: boolean;
  private awaitingRead = false;
  private notice = "";
  private visibility = (): void => {
    if (document.hidden) clearTimeout(this.timer);
    else if (!this.data || !TERMINAL.has(this.data.state)) void this.load();
  };
  constructor(
    private root: HTMLElement,
    private id: number,
  ) {
    this.mutationUnknown = uncertainCommands.has(id);
    root.innerHTML = `<div class="blueprint-toolbar"><a href="#/profiles" class="btn">← Профили</a><h2 tabindex="-1">Запуск #${id}</h2><button class="btn" data-refresh>Обновить GET</button><button class="btn danger" data-cancel disabled>Остановить дальнейшие шаги</button></div><p data-run-status role="status">Загрузка запуска…</p><div data-run-summary></div><section class="blueprint-panel"><h3>Закреплённая схема</h3><div data-run-version-note></div><div data-run-version-picker></div><div data-run-graph></div></section><section class="blueprint-panel"><h3>Журнал шагов</h3><div data-run-journal></div></section>`;
    root.querySelector<HTMLElement>("h2")!.focus();
    root.querySelector<HTMLButtonElement>("[data-refresh]")!.onclick = () => {
      void this.load();
    };
    root.querySelector<HTMLButtonElement>("[data-cancel]")!.onclick = () => {
      void this.cancel();
    };
    document.addEventListener("visibilitychange", this.visibility);
    void api
      .blueprintBlocks()
      .then((r) => {
        if (!this.disposed) {
          this.blocks = r.blocks;
          this.paint();
        }
      })
      .catch(() => {
        /* conservative: no safe retry until registry loaded */
      });
    void this.load();
  }
  dispose(): void {
    this.disposed = true;
    clearTimeout(this.timer);
    document.removeEventListener("visibilitychange", this.visibility);
    this.root.replaceChildren();
  }
  private el<T extends HTMLElement = HTMLElement>(selector: string): T {
    return this.root.querySelector<T>(selector)!;
  }
  private status(text: string): void {
    this.el("[data-run-status]").textContent = text;
  }
  private async load(): Promise<void> {
    if (this.fetching || this.disposed || document.hidden) return;
    clearTimeout(this.timer);
    this.fetching = true;
    try {
      const r = await api.profileRun(this.id);
      if (this.disposed) return;
      this.data = r;
      this.awaitingRead = false;
      this.graphVersion = versionByPK(r.profile_version_id) || this.graphVersion;
      this.paint();
      this.status(
        `Данные получены ${new Date().toLocaleTimeString("ru")}. ${this.notice}${this.mutationUnknown ? " Результат команды неизвестен; не повторяйте её с новым ключом. Сверьте журнал." : ""}`,
      );
      const picker = this.el("[data-run-version-picker]");
      if (!this.graphVersion && !picker.dataset.mounted) {
        picker.dataset.mounted = "true";
        mountVersionPicker(picker, "graph", (v) => {
          if (v && (v.profile_id !== r.profile_id || v.profile_version_id !== r.profile_version_id)) {
            this.status("Версия не совпадает с закреплённым PK запуска.");
            return;
          }
          this.graphVersion = v;
          this.paint();
        });
      }
      if (!TERMINAL.has(r.state) && !document.hidden)
        this.timer = window.setTimeout(() => {
          void this.load();
        }, 5000);
    } catch (error) {
      if (!this.disposed)
        this.status(
          `Не удалось обновить запуск. Последний журнал сохранён. ${(error as Error).message}. Нажмите «Обновить GET».`,
        );
    } finally {
      this.fetching = false;
    }
  }
  private paint(): void {
    const r = this.data;
    if (!r || this.disposed) return;
    this.el<HTMLButtonElement>("[data-cancel]").disabled =
      this.command || this.awaitingRead || this.mutationUnknown || TERMINAL.has(r.state) || r.cancel_requested;
    this.el("[data-run-summary]").innerHTML =
      `<div class="blueprint-panel"><p>${stateBadge(r.state)}${r.cancel_requested ? " · Запрошена остановка (cancel_requested)" : ""}</p><dl class="blueprint-facts"><dt>Операция</dt><dd>${r.operation_id ?? "—"}</dd><dt>Профиль / PK версии</dt><dd>#${r.profile_id} / ${r.profile_version_id}</dd><dt>Вход / ревизия</dt><dd>${esc(r.entry)} / ${r.revision}</dd><dt>Ревизия политики</dt><dd>${r.policy_revision}</dd><dt>Ресурс</dt><dd>${r.resource_id ? `<a href="#/resources/${r.resource_id}">#${r.resource_id}</a>` : "—"}</dd><dt>Нода</dt><dd>${r.node_id ? `<a href="#/nodes/${encodeURIComponent(r.node_id)}">${esc(r.node_id)}</a>` : "—"}</dd><dt>Стоимость</dt><dd>Не предоставлена API</dd><dt>Следующее пробуждение</dt><dd>${esc(time(r.next_wake_at))}</dd></dl>${r.waiting ? `<p>Ожидание ${esc(r.waiting.type)} · после эпизода ${esc(r.waiting.after_episode_id || "—")}</p>` : ""}${r.last_error ? `<p class="banner bad">${esc(r.last_error)}</p>` : ""}</div>`;
    const graph =
      this.graphVersion?.profile_version_id === r.profile_version_id ? this.graphVersion.blueprint : undefined;
    this.el("[data-run-version-note]").textContent = graph
      ? "Схема только для чтения. Показаны только подтверждённые журналом ветки; отсутствие шага не означает успех или пропуск."
      : "Закреплённая версия недоступна или удалена. Журнал остаётся доступен. Для схемы выберите известный числовой номер и сверьте PK.";
    this.el("[data-run-graph]").innerHTML = graph
      ? `<ol>${graph.nodes
          .map(
            (n) =>
              `<li><span class="mono">${esc(n.id)}</span> · ${esc(TITLES[n.type] || n.type)} · ${blockFor(this.blocks, n.type) ? EFFECTS[blockFor(this.blocks, n.type)!.effects] : "Класс не загружен"}${(
                r.checkpoints || []
              )
                .filter((c) => c.node_id === n.id)
                .map((cp) => ` · ${stateBadge(cp.state)}`)
                .join("")}</li>`,
          )
          .join(
            "",
          )}</ol><ul>${graph.edges.map((e) => `<li>${esc(e.from)} · ${esc(e.port)} → ${esc(e.to)} · exec</li>`).join("")}</ul>`
      : "";
    const journal = this.el("[data-run-journal]");
    // Preserve focused checkpoint controls across polling. Update after focus leaves.
    if (journal.contains(document.activeElement)) return;
    journal.innerHTML =
      (r.checkpoints || [])
        .map((cp) => {
          const node = graph?.nodes.find((n) => n.id === cp.node_id),
            block = node && blockFor(this.blocks, node.type);
          const retrySafe =
            !!graph && !runtimeGate(graph) && block?.effects === "read" && block.retry_class === "safe-read";
          const decisions: [ResumeDecision, string][] =
            cp.state === "confirmed"
              ? []
              : [
                  ...(["claimed", "dispatched", "unknown"].includes(cp.state)
                    ? [["reconcile", "Сверить отправленное действие"] as [ResumeDecision, string]]
                    : []),
                  ...(retrySafe ? [["retry_safe", "Повторить безопасное чтение"] as [ResumeDecision, string]] : []),
                  ["acknowledge", "Отметить просмотр · не восстановление"],
                ];
          return `<article class="blueprint-checkpoint"><h4 class="mono">${esc(cp.node_id)} · checkpoint #${cp.id}</h4><p>${stateBadge(cp.state)}</p><p>Invocation ${cp.invocation} · попытка ${cp.attempt} · следующий повтор ${esc(time(cp.next_attempt_at))}</p>${cp.error ? `<p class="banner bad">${esc(cp.error)}</p>` : ""}${outputHTML(cp)}<div class="blueprint-toolbar">${decisions.map(([decision, label]) => `<button class="btn sm" type="button" data-cp="${cp.id}" data-revision="${r.revision}" data-decision="${decision}"${this.command || this.mutationUnknown || TERMINAL.has(r.state) ? " disabled" : ""}>${label}</button>`).join("")}</div></article>`;
        })
        .join("") || "<p>Шаги ещё не зафиксированы.</p>";
    journal.onclick = (e) => {
      const btn = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-decision]");
      if (btn)
        void this.resume(Number(btn.dataset.cp), Number(btn.dataset.revision), btn.dataset.decision as ResumeDecision);
    };
  }
  private async cancel(): Promise<void> {
    if (
      !this.data ||
      this.command ||
      this.awaitingRead ||
      this.mutationUnknown ||
      !window.confirm(
        "Остановить дальнейшие шаги? Отправленные действия не отменяются; сервер и расходы могут остаться.",
      )
    )
      return;
    await this.send(() => api.cancelProfileRun(this.id, this.data!.revision, newIdemKey()));
  }
  private async resume(checkpoint: number, expected_revision: number, decision: ResumeDecision): Promise<void> {
    if (
      !this.data ||
      this.command ||
      this.awaitingRead ||
      this.mutationUnknown ||
      !window.confirm(
        `Применить ${decision} к checkpoint #${checkpoint}, ревизия ${expected_revision}? Это не разрешает повтор покупки/SSH.`,
      )
    )
      return;
    await this.send(() =>
      api.resumeProfileRun(this.id, { expected_revision, checkpoint_id: checkpoint, decision }, newIdemKey()),
    );
  }
  private async send(action: () => Promise<unknown>): Promise<void> {
    clearTimeout(this.timer);
    this.command = true;
    this.paint();
    try {
      await action();
      this.awaitingRead = true;
      this.notice = "Команда принята (202), ожидайте подтверждения в журнале.";
      this.status(this.notice);
    } catch (error) {
      const e = error as APIError;
      this.mutationUnknown = !e.status || e.status >= 500;
      if (this.mutationUnknown) uncertainCommands.add(this.id);
      this.notice = `${e.message}. Журнал сохранён; сначала обновите GET и подтвердите решение заново. Не повторяйте неизвестную команду с новым ключом.`;
      this.status(this.notice);
    } finally {
      this.command = false;
      if (!this.disposed) {
        this.paint();
        if (!this.mutationUnknown) void this.load();
      }
    }
  }
}
function outputHTML(cp: ProfileRunCheckpoint): string {
  const output = cp.output || {},
    ports = output.ports && typeof output.ports === "object" ? (output.ports as Record<string, unknown>) : {};
  const values: string[] = [];
  if (typeof output.__port === "string" && ["ok", "active", "stop", "fallback"].includes(output.__port))
    values.push(`Контрольный выход: ${output.__port}`);
  for (const name of ["server", "operation"]) {
    const handle = ports[name];
    if (!handle || typeof handle !== "object") continue;
    for (const key of ["resource_id", "operation_id", "connection_id"]) {
      const v = (handle as Record<string, unknown>)[key];
      if (typeof v === "number" && Number.isSafeInteger(v) && v > 0) values.push(`${name}.${key}: ${v}`);
    }
  }
  return values.length ? `<p class="mono">${values.map(esc).join(" · ")}</p>` : "";
}
