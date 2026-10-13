/* Слежение за свежестью интерфейса.
 *
 * Страницы — SPA: открытая вкладка держит старый бандл и не замечает, что
 * регистратор отдал новую сборку. Инструмент сборки (tools/uigen) запекает в
 * каждую страницу <meta name="sharedd-build" content="<hash>">. Здесь мы
 * изредка перезапрашиваем собственную HTML (no-store), сравниваем маркер и при
 * расхождении показываем ненавязчивую полоску «Обновить». Автоперезагрузки нет
 * — только по клику, поэтому открытая модалка или незаконченная форма данные не
 * теряет. При обновлении сохраняются hash и прокрутка. */

const MARKER_RE = /<meta\s+name="sharedd-build"\s+content="([^"]+)"/;
const SCROLL_KEY = "sharedd.restore-scroll";
const CHECK_MS = 60_000;

function loadedMarker(): string {
  return document.querySelector<HTMLMetaElement>('meta[name="sharedd-build"]')?.content || "";
}

async function checkForUpdate(): Promise<void> {
  const mine = loadedMarker();
  if (!mine) return;
  let html: string;
  try {
    const resp = await fetch(location.pathname + location.search, { cache: "no-store" });
    if (!resp.ok) return;
    html = await resp.text();
  } catch {
    return; // нет связи/файл — тихо ждём следующей проверки
  }
  const m = MARKER_RE.exec(html);
  if (!m || m[1] === mine) return;
  const bar = document.getElementById("update-bar");
  if (bar) bar.hidden = false;
}

function reloadPreservingView(): void {
  try { sessionStorage.setItem(SCROLL_KEY, String(window.scrollY)); } catch { /* ignore */ }
  location.reload(); // hash сохраняется самим браузером
}

export function initBuildWatch(): void {
  const bar = document.createElement("div");
  bar.id = "update-bar";
  bar.hidden = true;
  bar.setAttribute("role", "status");
  bar.innerHTML =
    `<span>Доступно обновление интерфейса —</span>` +
    `<button type="button" class="btn sm primary" id="update-reload">Обновить</button>`;
  document.body.appendChild(bar);
  document.getElementById("update-reload")?.addEventListener("click", reloadPreservingView);

  // Восстановить прокрутку после ручного обновления (одноразово).
  let saved: string | null = null;
  try { saved = sessionStorage.getItem(SCROLL_KEY); } catch { /* ignore */ }
  if (saved !== null) {
    try { sessionStorage.removeItem(SCROLL_KEY); } catch { /* ignore */ }
    const y = Number(saved);
    if (Number.isFinite(y)) requestAnimationFrame(() => window.scrollTo(0, y));
  }

  window.addEventListener("focus", () => { checkForUpdate().catch(() => {}); });
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) checkForUpdate().catch(() => {});
  });
  setInterval(() => { if (!document.hidden) checkForUpdate().catch(() => {}); }, CHECK_MS);
}
