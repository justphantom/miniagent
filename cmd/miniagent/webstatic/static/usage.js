"use strict";

// usage.js — Token usage visualization: summary bar + per-step detail list.

// renderUsageBar appends a .usage-bar block to container showing input/output token proportions.
// budget is the max_tokens_total from config (0 = no budget).
export function renderUsageBar(container, { in: tin, out: tout, budget }) {
  const total = tin + tout;
  const bar = document.createElement("div");
  bar.className = "usage-bar";
  bar.title = `in=${tin} out=${tout}${budget ? ` / 预算 ${budget}` : ""}`;
  if (total === 0) {
    bar.classList.add("usage-empty");
  } else {
    const inEl = document.createElement("div");
    inEl.className = "usage-in";
    inEl.style.width = (tin / total * 100) + "%";
    const outEl = document.createElement("div");
    outEl.className = "usage-out";
    outEl.style.width = (tout / total * 100) + "%";
    bar.append(inEl, outEl);
    if (budget > 0 && total > budget) {
      const mark = document.createElement("div");
      mark.className = "usage-budget-mark";
      mark.style.left = (budget / total * 100) + "%";
      bar.appendChild(mark);
    }
  }
  container.appendChild(bar);
}

// renderStepUsageList appends a collapsible <details> showing per-step token breakdown.
export function renderStepUsageList(container, steps) {
  if (!steps || steps.length === 0) return;
  const det = document.createElement("details");
  det.className = "usage-steps";
  const sum = document.createElement("summary");
  sum.textContent = `用量详情（${steps.length} 步）`;
  det.appendChild(sum);
  for (const s of steps) {
    const row = document.createElement("div");
    row.className = "usage-step";
    row.title = `step ${s.step} · in=${s.in} out=${s.out} · tools=${s.toolCalls}`;
    const label = document.createElement("span");
    label.className = "usage-step-label";
    label.textContent = `step ${s.step}`;
    const stepBar = document.createElement("div");
    stepBar.className = "usage-step-bar";
    const sTotal = s.in + s.out;
    if (sTotal > 0) {
      const inEl = document.createElement("div");
      inEl.className = "usage-in";
      inEl.style.width = (s.in / sTotal * 100) + "%";
      const outEl = document.createElement("div");
      outEl.className = "usage-out";
      outEl.style.width = (s.out / sTotal * 100) + "%";
      stepBar.append(inEl, outEl);
    }
    const meta = document.createElement("span");
    meta.className = "usage-step-meta";
    meta.textContent = `in=${s.in} out=${s.out} · tools=${s.toolCalls}`;
    row.append(label, stepBar, meta);
    det.appendChild(row);
  }
  container.appendChild(det);
}
