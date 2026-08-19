/* Тема. Тёмная — по умолчанию; светлая включается кнопкой и запоминается.
   Ключ хранения отдельный от токена панели, ничего чужого не трогаем. */

const KEY = "sharedd.theme";

/** Вызывается инлайном в <head> ДО первой отрисовки, иначе мигает кадр. */
export function applyStoredTheme(): void {
  try {
    document.documentElement.dataset.theme = localStorage.getItem(KEY) || "dark";
  } catch {
    document.documentElement.dataset.theme = "dark";
  }
}

export function initTheme(): void {
  const btn = document.getElementById("theme-toggle");
  if (!btn) return;
  btn.addEventListener("click", () => {
    const root = document.documentElement;
    const next = root.dataset.theme === "light" ? "dark" : "light";
    root.dataset.theme = next;
    try { localStorage.setItem(KEY, next); } catch { /* приватный режим */ }
    document.dispatchEvent(new CustomEvent("themechange"));
  });
}

/** Значение токена темы — нужно там, где рисуем на canvas/SVG вручную. */
export function token(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}
