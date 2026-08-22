"use strict";

// ---- markdown subset renderer (vanilla, no deps) ----
// XSS defense: escape HTML entities FIRST, then apply markdown replacements; innerHTML only
// ever receives escaped content. Blocks: # headings, ``` code fences, -/* lists, > quote, ---, tables.
// Inline: **bold**, *italic*, `code`, ~~strike~~.

export function esc(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

// safeLink renders an escaped markdown link; http(s)/relative/mailto only — anything else
// (javascript:, data:, vbscript:) degrades to escaped literal text, keeping href injection-proof.
function safeLink(t, u) {
  if (/^(https?:|mailto:|\/|#|\.)/i.test(u)) {
    return `<a href="${u}" target="_blank" rel="noopener noreferrer">${t}</a>`;
  }
  return `[${t}](${u})`;
}

function mdInline(s) {
  return esc(s)
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/\*([^*\n]+)\*/g, "<em>$1</em>")
    .replace(/~~([^~]+)~~/g, "<del>$1</del>")
    // L7: markdown links. Input is already HTML-escaped above, so a javascript: href can only
    // break out via a quote — esc() neutralized it; still strip the scheme allowlist misses.
    .replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_, t, u) => safeLink(t, u));
}

// splitRow splits a table row on unescaped pipes, trimming the outer empty cells produced
// by the leading/trailing pipe ("| a | b |" → ["a","b"]).
function splitRow(line) {
  const cells = line.trim().replace(/^\||\|$/g, "").split(/(?<!\\)\|/);
  return cells.map(c => c.trim().replace(/\\\|/g, "|"));
}

function isTableSep(line) {
  return /^\s*\|?[\s:|-]*-[\s:|-]*\|?\s*$/.test(line) && line.includes("|");
}

function alignOf(cell) {
  const c = cell.trim();
  const left = c.startsWith(":"), right = c.endsWith(":");
  return left && right ? "center" : right ? "right" : left ? "left" : "";
}

export function mdRender(src) {
  const lines = src.split("\n");
  const out = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (/^```/.test(line)) {
      const buf = [];
      i++;
      while (i < lines.length && !/^```/.test(lines[i])) { buf.push(lines[i]); i++; }
      if (i < lines.length) i++; // skip closing fence (EOF without fence = unterminated, body still rendered)
      out.push(`<pre><code>${esc(buf.join("\n"))}</code></pre>`);
      continue;
    }
    let m;
    if ((m = /^(#{1,3})\s+(.*)$/.exec(line))) {
      out.push(`<h${m[1].length}>${mdInline(m[2])}</h${m[1].length}>`);
    } else if (line.includes("|") && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      const heads = splitRow(line);
      const aligns = splitRow(lines[i + 1]).map(alignOf);
      i += 2;
      const rows = [];
      while (i < lines.length && lines[i].includes("|") && lines[i].trim() !== "") {
        rows.push(splitRow(lines[i]));
        i++;
      }
      const th = heads.map((h, k) => `<th${aligns[k] ? ` style="text-align:${aligns[k]}"` : ""}>${mdInline(h)}</th>`).join("");
      const trs = rows.map(r => `<tr>${r.map((c, k) => `<td${aligns[k] ? ` style="text-align:${aligns[k]}"` : ""}>${mdInline(c)}</td>`).join("")}</tr>`).join("");
      out.push(`<table><thead><tr>${th}</tr></thead><tbody>${trs}</tbody></table>`);
      continue;
    } else if (/^\s*[-*]\s+/.test(line)) {
      const items = [];
      while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) {
        items.push(`<li>${mdInline(lines[i].replace(/^\s*[-*]\s+/, ""))}</li>`);
        i++;
      }
      out.push(`<ul>${items.join("")}</ul>`);
      continue;
    } else if (/^>\s?/.test(line)) {
      const buf = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) { buf.push(lines[i].replace(/^>\s?/, "")); i++; }
      out.push(`<blockquote>${mdInline(buf.join(" "))}</blockquote>`);
      continue;
    } else if (/^---+\s*$/.test(line)) {
      out.push("<hr>");
    } else if (line.trim() !== "") {
      out.push(`<p>${mdInline(line)}</p>`);
    }
    i++;
  }
  return out.join("");
}
