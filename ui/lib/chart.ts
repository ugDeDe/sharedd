/* Ванильный inline-SVG для графиков страницы ноды (specs/node-stats.md §4,
   DESIGN.md §13.5). Без зависимостей, без canvas — глазу хватает 7–31 точки,
   а SVG бесплатно даёт текстовый sr-only-эквивалент и CSP-безопасен.

   Функции возвращают строку <svg> либо "" — пустой ряд вызывающий рисует
   текстом («накапливается — пока мало точек»), не пустым холстом.
   Подписи осей — HTML рядом с SVG: при preserveAspectRatio="none" буквы
   внутри растянулись бы по горизонтали. */

const VB_W = 100;

export interface LineSeries {
  /** null/NaN — пропуск сэмпла: линия рвётся (как mtplane «нет сэмпла»). */
  values: (number | null)[];
  /** CSS-значение цвета; по умолчанию var(--acc). */
  color?: string;
}

export interface ChartOpts {
  /** высота viewBox (px-эквивалент); по умолчанию 26, как у .metric-spark. */
  height?: number;
  /** фиксированный максимум оси Y (например 100 %); иначе — max данных. */
  max?: number;
}

/** Линия: одна или несколько серий, разрыв на null. Y от 0 до max. */
export function lineChart(series: LineSeries[], opts: ChartOpts = {}): string {
  const nums = series
    .flatMap((s) => s.values)
    .filter((v): v is number => typeof v === "number" && isFinite(v));
  if (nums.length < 2) return "";
  const height = opts.height ?? 26;
  const max = (opts.max ?? Math.max(...nums, 1)) * (opts.max != null ? 1 : 1.08); // запас, чтобы линия не липла к верху
  const pad = 3;
  const plot = height - pad * 2;
  const paths = series.map((s) => {
    const n = s.values.length;
    if (n < 2) return "";
    let d = "";
    let pen = false;
    s.values.forEach((v, i) => {
      if (typeof v !== "number" || !isFinite(v)) { pen = false; return; }
      const x = (i / (n - 1)) * VB_W;
      const y = pad + (1 - v / max) * plot;
      d += `${pen ? "L" : "M"}${x.toFixed(2)} ${y.toFixed(2)} `;
      pen = true;
    });
    return `<path class="chart-line" d="${d.trim()}" stroke="${s.color || "var(--acc)"}"/>`;
  }).join("");
  return `<svg class="chart-svg" viewBox="0 0 ${VB_W} ${height}" preserveAspectRatio="none" aria-hidden="true">${paths}</svg>`;
}

export interface Bar {
  value: number;
  /** класс цвета (ok/warn/bad) — fill задаётся в CSS. */
  cls?: string;
  /** явный цвет (CSS-значение), если класса нет. */
  color?: string;
}
/** Слот x-метки: 1 столбик (A, E) или N сгруппированных (F). */
export interface BarRow { bars: Bar[]; }

/** Столбики (вертикальные). Y от 0 до max, max — общий по всем столбикам. */
export function barChart(rows: BarRow[], opts: ChartOpts = {}): string {
  const all = rows.flatMap((r) => r.bars.map((b) => b.value)).filter((v) => isFinite(v));
  if (!rows.length || !all.length) return "";
  const height = opts.height ?? 26;
  const max = opts.max ?? Math.max(...all, 1);
  const pad = 3;
  const plot = height - pad * 2;
  const slot = VB_W / rows.length;
  const inner = slot * 0.72;
  const rects = rows.map((row, ri) => {
    const k = Math.max(row.bars.length, 1);
    const bw = inner / k;
    return row.bars.map((b, bi) => {
      const h = Math.max(0, (b.value / max) * plot);
      const x = ri * slot + (slot - inner) / 2 + bi * bw;
      const y = pad + plot - h;
      const cls = b.cls ? `bar ${b.cls}` : "bar";
      // Инлайн-цвет перебивает класс .bar (fill по умолчанию) только когда он нужен.
      const style = b.color ? ` style="fill:${b.color}"` : "";
      return `<rect class="${cls}" x="${x.toFixed(2)}" y="${y.toFixed(2)}" ` +
        `width="${Math.max(bw - 0.4, 0.3).toFixed(2)}" height="${h.toFixed(2)}"${style}/>`;
    }).join("");
  }).join("");
  return `<svg class="chart-svg" viewBox="0 0 ${VB_W} ${height}" preserveAspectRatio="none" aria-hidden="true">${rects}</svg>`;
}
