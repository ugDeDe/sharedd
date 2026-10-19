import { api } from "./api";
import type { APIError, CloudResource, ManagementBody, ManagementPolicy, ProfileVersion } from "./api";
import { BEGET_GATE, bytes, runtimeGate } from "./blueprint-model";
import { mountInstance, mountVersionPicker } from "./blueprint-instance";
import type { InstanceForm } from "./blueprint-instance";

export function mountBlueprintManagement(
  root: HTMLElement,
  nodeID: string,
  initial: ManagementPolicy,
  resource: CloudResource | null,
  onSave: (m: ManagementPolicy) => void,
): void {
  if (root.dataset.mounted) return;
  root.dataset.mounted = nodeID;
  let current = initial,
    mode = initial.mode,
    version: ProfileVersion | null = null,
    instance: InstanceForm | null = null,
    busy = false,
    uncertain = false;
  /* Ресурс, найденный по node_id (передан вызывающим из GET /api/me/cloud-resources),
     — источник префиллов, даже когда политика ещё не связана (current_resource_id
     = null). PUT management свяжет его автоматически (backend-компаньон). */
  const bound = (): CloudResource | null => resource;
  root.innerHTML = `<div class="blueprint-facts"><p>Текущий ресурс: ${initial.current_resource_id ? `<a href="#/resources/${initial.current_resource_id}">#${initial.current_resource_id}</a>` : bound() ? `не связан · по узлу найден <a href="#/resources/${bound()!.id}">#${bound()!.id}</a> — свяжется автоматически` : `не связан · <a href="#/resources">хостинг-ресурсы</a>`}</p><p>Активный запуск: не предоставляется API. <a href="#/profiles">Открыть запуск по известному ID</a></p><p>Сохранённый PK версии: ${initial.profile_version_id ?? "—"} · ревизия <span data-policy-revision>${initial.revision}</span></p></div>
    <form><fieldset><legend>Режим управления</legend><label class="check"><input type="radio" name="blueprint-mode" value="manual"${mode === "manual" ? " checked" : ""}><span class="box"></span>Ручной</label><label class="check"><input type="radio" name="blueprint-mode" value="profile"${mode === "profile" ? " checked" : ""}><span class="box"></span>Профильный</label></fieldset>
    <p data-mode-note class="banner info"></p><fieldset data-policy-profile${mode === "manual" ? " hidden disabled" : ""}><legend>Профиль и входы политики</legend><div data-policy-picker></div><div data-policy-gate class="banner warn"></div><div data-policy-inputs></div>
    <label class="check"><input type="checkbox" data-current-ban${initial.process_current_ban && initial.mode === "profile" ? " checked" : ""}><span class="box"></span>Обработать текущий эпизод блокировки</label><p class="help">По умолчанию выключено при новом включении. Разрешает действие сейчас для текущего эпизода; прошлые не переигрываются.</p>
    <label class="check"><input type="checkbox" data-policy-consent><span class="box"></span>Разрешить действия этой версии при новых блокировках, включая платную замену VPS, если она есть в схеме.</label><p class="help">Сохранение самого профиля этого не делает. Политика не принимает vars: используются значения текстового профиля/регистратора, а не переопределения первоначального запуска. Допуск — ограниченная проверка реестра, не обещание независимых GP/metrics/TCP-постусловий.</p></fieldset>
    <div class="banner bad" role="alert" id="bp-policy-error" hidden></div><div class="blueprint-toolbar"><button class="btn primary" type="submit">Сохранить режим</button><button class="btn" type="button" data-reload-policy>Обновить GET политики</button></div></form>`;
  const profile = root.querySelector<HTMLElement>("[data-policy-profile]")!,
    note = root.querySelector<HTMLElement>("[data-mode-note]")!,
    gate = root.querySelector<HTMLElement>("[data-policy-gate]")!,
    errorBox = root.querySelector<HTMLElement>("#bp-policy-error")!,
    form = root.querySelector<HTMLFormElement>("form")!,
    submit = form.querySelector<HTMLButtonElement>('[type="submit"]')!;
  const consent = root.querySelector<HTMLInputElement>("[data-policy-consent]")!,
    ban = root.querySelector<HTMLInputElement>("[data-current-ban]")!;
  const error = (text: string): void => {
    errorBox.textContent = text;
    errorBox.hidden = false;
  };
  const update = (): void => {
    profile.hidden = mode === "manual";
    (profile as HTMLFieldSetElement).disabled = mode === "manual" || busy;
    note.textContent =
      mode === "manual"
        ? "Ручной режим не разрешает автоматическую покупку при бане. Ресурсы не удаляются."
        : "Профильный режим разрешает действия выбранной версии при новых блокировках, включая платную замену VPS. Обработка текущего эпизода может действовать сразу.";
    const graph = version?.blueprint;
    const res = bound();
    /* Блокирует только реально недостающее: нет ресурса (нечего автоматизировать),
       beget, не загружен graph или runtime-гейт. Связь с ресурсом — не блокер:
       префиллы из resource + автосвязь на PUT. */
    const reason = !res
      ? `Узел не связан с облачным ресурсом Nodehost — автоматизировать нечего. Добавьте ресурс и установите на него узел либо привяжите текущий в разделе <a href="#/resources">хостинг-ресурсов</a>.`
      : res.provider === "beget"
        ? BEGET_GATE
        : !graph
          ? "Загрузите числовую версию graph-профиля и разрешите её действия явно."
          : runtimeGate(graph);
    const textPicked = !!instance?.installedTextVersion?.();
    if (mode === "profile" && !reason && graph)
      gate.innerHTML = `Чек-лист · Подключение: из ресурса #${res!.connection_id} ✓ · Конфигурация: ${
        root.querySelector<HTMLInputElement>("[data-config]")?.value
          ? "из формы ✓"
          : "заполните параметры VPS в форме"
      } · Профиль установки: ${textPicked ? "выбран ✓" : "выберите текстовую версию"}`;
    else
      gate.textContent =
        reason ||
        "Nodehost · версия разрешена для настройки. Сохранение режима может разрешить платные действия при бане.";
    submit.textContent = mode === "profile" ? "Включить автоматизацию" : "Сохранить ручной режим";
    submit.disabled =
      busy || uncertain || (mode === "profile" && (!!reason || !textPicked || !consent.checked));
  };
  root.addEventListener("input", (event) => {
    root.dataset.edited = "true";
    if (event.target !== consent) consent.checked = false;
    update();
  });
  root.querySelectorAll<HTMLInputElement>('[name="blueprint-mode"]').forEach((el) => {
    el.onchange = () => {
      mode = el.value as ManagementPolicy["mode"];
      if (mode === "manual") consent.checked = false;
      if (mode === "manual" || current.mode !== "profile") ban.checked = false;
      update();
    };
  });
  mountVersionPicker(root.querySelector<HTMLElement>("[data-policy-picker]")!, "graph", (v) => {
    version = v;
    consent.checked = false;
    ban.checked =
      v?.profile_version_id === current.profile_version_id && current.mode === "profile"
        ? current.process_current_ban
        : false;
    const inputs = root.querySelector<HTMLElement>("[data-policy-inputs]")!;
    inputs.replaceChildren();
    /* Префилл: сохранённые inputs политики приоритетны; connection_id из ресурса
       узла (найден по node_id), даже когда политика ещё не связана. */
    instance = v?.blueprint
      ? mountInstance(inputs, v.blueprint, true, {
          ...(v.profile_version_id === current.profile_version_id ? current.inputs : {}),
          connection_id: bound()?.connection_id,
        })
      : null;
    update();
  });
  const loadResource = async (): Promise<void> => {
    if (current.current_resource_id && resource?.id !== current.current_resource_id) {
      try {
        resource = await api.cloudResource(current.current_resource_id);
        update();
      } catch (e) {
        error((e as Error).message);
      }
    }
  };
  void loadResource();
  root.querySelector<HTMLButtonElement>("[data-reload-policy]")!.onclick = async () => {
    if (
      busy ||
      !window.confirm(
        "Обновить ревизию политики? После чтения повторно подтвердите выбранный режим; неизвестная команда автоматически не повторяется.",
      )
    )
      return;
    try {
      current = await api.nodeManagement(nodeID);
      uncertain = false;
      consent.checked = false;
      root.querySelector<HTMLElement>("[data-policy-revision]")!.textContent = String(current.revision);
      error("Политика перечитана. Проверьте ресурс и настройки и подтвердите выбор заново.");
      await loadResource();
      update();
    } catch (e) {
      error((e as Error).message);
    }
  };
  form.onsubmit = async (e) => {
    e.preventDefault();
    if (busy || uncertain || !form.reportValidity()) return;
    let body: ManagementBody;
    try {
      if (mode === "profile") {
        if (!version?.blueprint || !instance || !consent.checked || !bound() || bound()!.provider !== "nodehost")
          throw new Error("Загрузите graph-версию Nodehost, проверьте подключение и подтвердите разрешение действий.");
        const values = instance.collect();
        body = {
          mode,
          profile_version_id: version.profile_version_id,
          inputs: values.inputs,
          process_current_ban: ban.checked,
          expected_revision: current.revision,
        };
      } else body = { mode: "manual", expected_revision: current.revision };
      if (bytes(body) > 256 * 1024) throw new Error("Тело политики превышает 256 КиБ.");
    } catch (e) {
      error((e as Error).message);
      return;
    }
    busy = true;
    update();
    form.querySelectorAll<HTMLInputElement | HTMLSelectElement>("input,select").forEach((el) => {
      el.disabled = true;
    });
    try {
      const result = await api.putNodeManagement(nodeID, body);
      current = result;
      onSave(result);
      root.dataset.edited = "false";
      root.querySelector<HTMLElement>("[data-policy-revision]")!.textContent = String(result.revision);
      error(
        mode === "manual"
          ? "Ручной режим сохранён. Серверы не удалены."
          : "Профильный режим сохранён. Разрешения и текущий эпизод применяются сервером; запуск не считается завершённым.",
      );
    } catch (errorValue) {
      const ex = errorValue as APIError;
      uncertain = !ex.status || ex.status >= 500 || ex.code === "revision_conflict";
      error(
        `${ex.code === "revision_conflict" ? "Настройки изменились. Выбор сохранён; обновите GET и подтвердите заново." : ex.message}${ex.diagnostics?.length ? " · " + ex.diagnostics.map((d) => `${d.node_id || "граф"} ${d.port || ""}: ${d.message}`).join("; ") : ""}`,
      );
      root
        .querySelector<HTMLElement>("[data-policy-picker] select")
        ?.setAttribute("aria-describedby", "bp-policy-error");
      root.querySelector<HTMLElement>("[data-policy-picker] select")?.focus();
    } finally {
      busy = false;
      consent.checked = false;
      form.querySelectorAll<HTMLInputElement | HTMLSelectElement>("input,select").forEach((el) => {
        el.disabled = false;
      });
      update();
    }
  };
  update();
}
