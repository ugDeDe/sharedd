/* H1-регрессия: точка наведения должна лежать ровно на нарисованной линии.
   Запуск: node ui/lib/chart.check.mjs  (Node ≥22, type-stripping). Без фреймворков. */
import assert from "node:assert/strict";
import { chartTop, liveLineChart } from "./chart.ts";

const unesc = (s) =>
  s.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");

// y в единицах viewBox по формуле СТРАНИЦЫ (node-detail.liveShow).
const pageY = (v, p) => p.pad + (1 - v / p.max) * (p.h - p.pad * 2);

function svgPointY(d, idx) {
  const pts = [...d.matchAll(/[ML]([\d.]+) ([\d.]+)/g)].map((m) => Number(m[2]));
  return pts[idx];
}

function cases(name, series, opts) {
  const x = series[0].values.map((_, i) => "t" + i);
  const html = liveLineChart(x, series, opts);
  const payload = JSON.parse(unesc(html.match(/data-live="([^"]*)"/)[1]));
  const d = html.match(/class="chart-line"[^>]*d="([^"]*)"/)[1];
  const nums = series.flatMap((s) => s.values);
  assert.equal(payload.max, chartTop(nums, opts?.max), `${name}: payload.max == отрисованная шкала`);

  const peak = series[0].values.indexOf(Math.max(...series[0].values));
  const v = series[0].values[peak];
  const yLine = svgPointY(d, peak);
  const yDot = pageY(v, payload);
  // допуск 0.02: путь в SVG округлён до 2 знаков (линия), точка — нет.
  assert.ok(Math.abs(yDot - yLine) < 0.02, `${name}: точка ${yDot.toFixed(3)} != линия ${yLine.toFixed(3)}`);
  return { payload, yLine, yDot };
}

// chart C: без opts.max → авто-максимум с запасом 8 % (именно тут ломалось H1).
const auto = cases("auto-max", [{ name: "c", values: [5, 12, 7] }], undefined);
assert.ok(Math.abs(auto.payload.max - 12 * 1.08) < 1e-9, "auto: max = dataMax * 1.08");

// chart D: явный max → шкала ровно max, запас не применяется.
cases("explicit-max", [{ name: "c", values: [3, 8, 4] }], { max: 100 });
// многосерийный: точка каждой серии на своей линии.
const s1 = [1, 9, 2], s2 = [4, 3, 8];
const html = liveLineChart(["a", "b", "c"], [
  { name: "A", values: s1 }, { name: "B", values: s2 },
]);
const payload = JSON.parse(unesc(html.match(/data-live="([^"]*)"/)[1]));
for (const [si, vals] of [s1, s2].entries()) {
  const d = [...html.matchAll(/class="chart-line"[^>]*d="([^"]*)"/g)][si][1];
  for (const [i, v] of vals.entries()) {
    assert.ok(Math.abs(pageY(v, payload) - svgPointY(d, i)) < 0.02, `multi S${si} i${i}`);
  }
}

console.log("OK: H1 dot-on-line (auto x1.08, explicit, multi-series)");
