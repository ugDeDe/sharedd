/* Форматирование. Функции перенесены из прежней версии ДОСЛОВНО:
   на них завязан весь текст интерфейса, и переписывать их не нужно —
   меняется представление, а не поведение. */

export function fmtDur(sec: number): string {
  sec = Math.max(0, Math.floor(sec));
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600),
        m = Math.floor((sec % 3600) / 60), s = sec % 60;
  if (d > 0) return `${d}д ${h}ч`;
  if (h > 0) return `${h}ч ${m}м`;
  if (m > 0) return `${m}м ${s}с`;
  return `${s}с`;
}

export function fmtAgo(iso: string | null | undefined): string {
  if (!iso) return "—";
  const sec = (Date.now() - new Date(iso).getTime()) / 1000;
  if (sec < 10) return "только что";
  if (sec < 3600) return `${Math.floor(sec / 60)} мин назад`;
  if (sec < 86400) return `${Math.floor(sec / 3600)} ч назад`;
  return `${Math.floor(sec / 86400)} дн назад`;
}

export function fmtClock(iso: string): string {
  const d = new Date(iso);
  return d.toLocaleString("ru-RU", {
    day: "2-digit", month: "2-digit",
    hour: "2-digit", minute: "2-digit", second: "2-digit",
  });
}

// средний промежуток: null/undefined (мало данных) — прочерк
export function fmtInterval(sec: number | null | undefined): string {
  if (sec === null || sec === undefined) return "—";
  return fmtDur(Math.round(sec));
}

// компактная длительность из минут (для чипа TTL мастерства)
export function fmtTtlMin(m: number): string {
  m = Math.round(m);
  if (m < 120) return m + " мин";
  if (m < 4320) return (Math.round(m / 6) / 10 + " ч").replace(/\.0 ч$/, " ч");
  return (Math.round(m / 144) / 10 + " д").replace(/\.0 д$/, " д");
}

// Русское склонение: 1 домен / 2 домена / 5 доменов.
export function plural(n: number, one: string, few: string, many: string): string {
  const m10 = n % 10, m100 = n % 100;
  if (m10 === 1 && m100 !== 11) return one;
  if (m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14)) return few;
  return many;
}
