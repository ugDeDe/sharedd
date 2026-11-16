/* Тип ноды (менеджер прокси и его движок) — присылается агентом в /register.
   MTProxyL помечается движком: mtproxyl — telemt, mtproxyl_mtdd — mtdd. */
import { esc } from "./dom";

export const NODE_TYPE_META: Record<string, { label: string; cls: string; title: string }> = {
  classic:       { label: "Classic",          cls: "",        title: "классический telemt" },
  mtproxyl:      { label: "MTProxyL · telemt", cls: "special", title: "MTProxyL с движком telemt (/opt/mtproxyl/)" },
  mtproxyl_mtdd: { label: "MTProxyL · mtdd",   cls: "special", title: "MTProxyL с движком mtdd (/opt/mtproxyl/mtdd/)" },
  meko:          { label: "MEKO",             cls: "info",    title: "фикс MEKO (/opt/mtpr-simple/)" },
  mtdd:          { label: "mtdd",             cls: "info",    title: "прокси mtdd (/etc/mtdd/)" },
};

export function nodeTypeLabel(t: unknown): string {
  const m = NODE_TYPE_META[String(t ?? "")];
  return m ? m.label : "";
}

export function nodeTypeBadge(t: unknown): string {
  const m = NODE_TYPE_META[String(t ?? "")];
  if (!m) return "";
  return `<span class="badge ${m.cls}" title="${esc(m.title)}">${esc(m.label)}</span>`;
}
