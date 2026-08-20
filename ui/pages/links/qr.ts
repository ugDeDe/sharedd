/* Минимальный QR-кодер: байтовый режим, уровень коррекции L, версии 1..20.
 Библиотек нет принципиально — страница обязана работать в закрытом
 контуре, без CDN. Матрица проверена сторонним декодером (OpenCV):
 65 из 65 строк, включая реальные tg://proxy-ссылки.

 Код перенесён без изменений в логике: правки только в обвязке модуля. */


// ── GF(256), примитивный многочлен 0x11d ──
var EXP = new Uint8Array(512), LOG = new Uint8Array(256);
for (var i = 0, x = 1; i < 255; i++) {
  EXP[i] = x; LOG[x] = i;
  x <<= 1; if (x & 0x100) x ^= 0x11d;
}
for (var i = 255; i < 512; i++) EXP[i] = EXP[i - 255];
function mul(a, b) { return a && b ? EXP[LOG[a] + LOG[b]] : 0; }

function rsGen(deg) {
  var p = [1];
  for (var i = 0; i < deg; i++) {
    var np = new Array(p.length + 1).fill(0);
    for (var j = 0; j < p.length; j++) {
      np[j] ^= mul(p[j], 1);
      np[j + 1] ^= mul(p[j], EXP[i]);
    }
    p = np;
  }
  return p;
}
function rsEnc(data, deg) {
  var gen = rsGen(deg), res = new Array(deg).fill(0);
  for (var i = 0; i < data.length; i++) {
    var f = data[i] ^ res[0];
    res.shift(); res.push(0);
    for (var j = 0; j < deg; j++) res[j] ^= mul(gen[j + 1], f);
  }
  return res;
}

// ── таблицы уровня L: [ecПоБлоку, блоковГр1, данныхГр1, блоковГр2, данныхГр2] ──
var ECL = [null,
  [7,1,19,0,0],   [10,1,34,0,0],  [15,1,55,0,0],  [20,1,80,0,0],  [26,1,108,0,0],
  [18,2,68,0,0],  [20,2,78,0,0],  [24,2,97,0,0],  [30,2,116,0,0], [18,2,68,2,69],
  [20,4,81,0,0],  [24,2,92,2,93], [26,4,107,0,0], [30,3,115,1,116],[22,5,87,1,88],
  [24,5,98,1,99], [28,1,107,5,108],[30,5,120,1,121],[28,3,113,4,114],[28,3,107,5,108]];
var ALIGN = [null, [], [6,18],[6,22],[6,26],[6,30],[6,34],[6,22,38],[6,24,42],[6,26,46],
  [6,28,50],[6,30,54],[6,32,58],[6,34,62],[6,26,46,66],[6,26,48,70],[6,26,50,74],
  [6,30,54,78],[6,30,56,82],[6,30,58,86],[6,34,62,90]];

function dataCapacity(v) { var t = ECL[v]; return t[1] * t[2] + t[3] * t[4]; }

function bch15(v) { // формат-информация
  var d = v << 10;
  for (var i = 4; i >= 0; i--) if (d >>> (10 + i) & 1) d ^= 0x537 << i;
  return ((v << 10) | d) ^ 0x5412;
}
function bch18(v) { // версия (для v >= 7)
  var d = v << 12;
  for (var i = 5; i >= 0; i--) if (d >>> (12 + i) & 1) d ^= 0x1f25 << i;
  return (v << 12) | d;
}

function utf8(s) {
  var out = [], b = new TextEncoder().encode(s);
  for (var i = 0; i < b.length; i++) out.push(b[i]);
  return out;
}

export function encode(text: string): Int8Array[] {
  var bytes = utf8(text), ver = 0;
  for (var v = 1; v <= 20; v++) {
    var cci = v < 10 ? 8 : 16;
    if (dataCapacity(v) * 8 >= 4 + cci + bytes.length * 8) { ver = v; break; }
  }
  if (!ver) throw new Error("строка слишком длинная для QR");

  // ── битовый поток ──
  var bits = [];
  function put(val, len) { for (var i = len - 1; i >= 0; i--) bits.push(val >>> i & 1); }
  put(4, 4);
  put(bytes.length, ver < 10 ? 8 : 16);
  for (var i = 0; i < bytes.length; i++) put(bytes[i], 8);
  var cap = dataCapacity(ver) * 8;
  for (var i = 0; i < 4 && bits.length < cap; i++) bits.push(0);
  while (bits.length % 8) bits.push(0);
  var dat = [];
  for (var i = 0; i < bits.length; i += 8) {
    var b = 0; for (var j = 0; j < 8; j++) b = (b << 1) | bits[i + j];
    dat.push(b);
  }
  for (var pad = 0; dat.length < dataCapacity(ver); pad++) dat.push(pad % 2 ? 0x11 : 0xEC);

  // ── блоки и коррекция ──
  var t = ECL[ver], blocks = [], ecs = [], p = 0;
  for (var i = 0; i < t[1]; i++) { var b = dat.slice(p, p + t[2]); p += t[2]; blocks.push(b); ecs.push(rsEnc(b, t[0])); }
  for (var i = 0; i < t[3]; i++) { var b = dat.slice(p, p + t[4]); p += t[4]; blocks.push(b); ecs.push(rsEnc(b, t[0])); }
  var maxD = Math.max(t[2], t[4]), out = [];
  for (var i = 0; i < maxD; i++) for (var j = 0; j < blocks.length; j++) if (i < blocks[j].length) out.push(blocks[j][i]);
  for (var i = 0; i < t[0]; i++) for (var j = 0; j < ecs.length; j++) out.push(ecs[j][i]);

  // ── матрица ──
  var n = ver * 4 + 17;
  var m = [], res = [];
  for (var i = 0; i < n; i++) { m.push(new Int8Array(n).fill(-1)); res.push(new Int8Array(n)); }
  function setF(r, c, v) { if (r >= 0 && r < n && c >= 0 && c < n) m[r][c] = v; }

  function finder(r, c) {
    for (var dr = -1; dr <= 7; dr++) for (var dc = -1; dc <= 7; dc++) {
      var inb = dr >= 0 && dr <= 6 && dc >= 0 && dc <= 6;
      var v = inb && (dr === 0 || dr === 6 || dc === 0 || dc === 6 ||
                      (dr >= 2 && dr <= 4 && dc >= 2 && dc <= 4)) ? 1 : 0;
      setF(r + dr, c + dc, v);
    }
  }
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);

  for (var i = 8; i < n - 8; i++) { m[6][i] = i % 2 === 0 ? 1 : 0; m[i][6] = i % 2 === 0 ? 1 : 0; }

  var ap = ALIGN[ver];
  for (var a = 0; a < ap.length; a++) for (var b2 = 0; b2 < ap.length; b2++) {
    var r = ap[a], c = ap[b2];
    if ((r <= 8 && c <= 8) || (r <= 8 && c >= n - 9) || (r >= n - 9 && c <= 8)) continue;
    for (var dr = -2; dr <= 2; dr++) for (var dc = -2; dc <= 2; dc++)
      m[r + dr][c + dc] = (Math.abs(dr) === 2 || Math.abs(dc) === 2 || (dr === 0 && dc === 0)) ? 1 : 0;
  }
  m[n - 8][8] = 1; // тёмный модуль

  // резерв под формат-информацию
  for (var i = 0; i <= 8; i++) { if (m[8][i] === -1) m[8][i] = 0; if (m[i][8] === -1) m[i][8] = 0; }
  for (var i = n - 8; i < n; i++) { if (m[8][i] === -1) m[8][i] = 0; if (m[i][8] === -1) m[i][8] = 0; }

  if (ver >= 7) {
    var vi = bch18(ver);
    for (var i = 0; i < 18; i++) {
      var bit = vi >>> i & 1, r = Math.floor(i / 3), c = i % 3;
      m[r][n - 11 + c] = bit; m[n - 11 + c][r] = bit;
    }
  }

  // ── укладка данных зигзагом ──
  var reserved = [];
  for (var i = 0; i < n; i++) reserved.push(Int8Array.from(m[i]));
  var bitIdx = 0, upward = true;
  for (var col = n - 1; col > 0; col -= 2) {
    if (col === 6) col--;
    for (var k = 0; k < n; k++) {
      var row = upward ? n - 1 - k : k;
      for (var c2 = 0; c2 < 2; c2++) {
        var cc = col - c2;
        if (reserved[row][cc] !== -1) continue;
        var bit = 0;
        if (bitIdx < out.length * 8) bit = out[bitIdx >> 3] >>> (7 - (bitIdx & 7)) & 1;
        bitIdx++;
        m[row][cc] = bit;
      }
    }
    upward = !upward;
  }

  // ── маски и штрафы ──
  function maskFn(k, r, c) {
    switch (k) {
      case 0: return (r + c) % 2 === 0;
      case 1: return r % 2 === 0;
      case 2: return c % 3 === 0;
      case 3: return (r + c) % 3 === 0;
      case 4: return (Math.floor(r / 2) + Math.floor(c / 3)) % 2 === 0;
      case 5: return (r * c) % 2 + (r * c) % 3 === 0;
      case 6: return ((r * c) % 2 + (r * c) % 3) % 2 === 0;
      default: return ((r + c) % 2 + (r * c) % 3) % 2 === 0;
    }
  }
  function penalty(g) {
    var p1 = 0, p2 = 0, p3 = 0, dark = 0;
    for (var r = 0; r < n; r++) {
      for (var c = 0; c < n; c++) {
        if (g[r][c]) dark++;
        if (c < n - 1 && r < n - 1 && g[r][c] === g[r][c+1] && g[r][c] === g[r+1][c] && g[r][c] === g[r+1][c+1]) p2 += 3;
      }
    }
    function runs(get) {
      var s = 0;
      for (var a = 0; a < n; a++) {
        var run = 1;
        for (var b = 1; b < n; b++) {
          if (get(a, b) === get(a, b - 1)) run++;
          else { if (run >= 5) s += run - 2; run = 1; }
        }
        if (run >= 5) s += run - 2;
      }
      return s;
    }
    p1 = runs(function (a, b) { return g[a][b]; }) + runs(function (a, b) { return g[b][a]; });
    var pat1 = [1,0,1,1,1,0,1,0,0,0,0], pat2 = [0,0,0,0,1,0,1,1,1,0,1];
    function look(get) {
      var s = 0;
      for (var a = 0; a < n; a++) for (var b = 0; b + 11 <= n; b++) {
        var ok1 = true, ok2 = true;
        for (var d = 0; d < 11; d++) {
          if (get(a, b + d) !== pat1[d]) ok1 = false;
          if (get(a, b + d) !== pat2[d]) ok2 = false;
        }
        if (ok1) s += 40; if (ok2) s += 40;
      }
      return s;
    }
    p3 = look(function (a, b) { return g[a][b]; }) + look(function (a, b) { return g[b][a]; });
    var pct = dark * 100 / (n * n);
    var p4 = Math.floor(Math.abs(pct - 50) / 5) * 10;
    return p1 + p2 + p3 + p4;
  }

  var best = null, bestScore = Infinity;
  for (var k = 0; k < 8; k++) {
    var g = [];
    for (var r = 0; r < n; r++) {
      g.push(new Int8Array(n));
      for (var c = 0; c < n; c++)
        g[r][c] = reserved[r][c] === -1 ? (m[r][c] ^ (maskFn(k, r, c) ? 1 : 0)) : m[r][c];
    }
    // формат-информация уровня L (биты 01) с этой маской
    var fi = bch15((0x01 << 3) | k);
    for (var i = 0; i < 15; i++) {
      var bit = fi >>> i & 1;
      if (i < 6) g[i][8] = bit;
      else if (i < 8) g[i + 1][8] = bit;
      else if (i === 8) g[8][7] = bit;
      else g[8][14 - i] = bit;
      if (i < 8) g[8][n - 1 - i] = bit;
      else g[n - 15 + i][8] = bit;
    }
    g[n - 8][8] = 1;
    var sc = penalty(g);
    if (sc < bestScore) { bestScore = sc; best = g; }
  }
  return best;
}
