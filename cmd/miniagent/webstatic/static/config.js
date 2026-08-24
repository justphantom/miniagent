"use strict";

// config.js — WebUI 配置管理页面入口。
// 表单渲染→config-form.js / config-providers.js；状态/工具函数→config-state.js。

import { api, authHeaders } from "./store.js";
import { $, MASK, state } from "./config-state.js";
import { CONFIG_SECTIONS, getNested, setNested, deleteNested, markDirty, updateSaveBtn, renderField, clientValidate } from "./config-form.js";
import { renderProviders, renderKv, renderKvMap, renderFormInto } from "./config-providers.js";

export function openConfigModal() {
  $("config-modal").hidden = false;
  const body = $("config-body");
  body.innerHTML = "";
  const page = document.createElement("div");
  page.id = "config-page";
  page.innerHTML = `<div id="cfg-msg" class="cfg-msg" hidden></div>
  <div class="cfg-loading">加载中…</div>`;
  body.appendChild(page);
  loadConfig();
}

export function closeConfigModal() {
  $("config-modal").hidden = true;
  $("config-body").innerHTML = "";
}

async function loadConfig() {
  const loadEl = document.querySelector(".cfg-loading");
  const msgEl = $("cfg-msg");
  try {
    const r = await api("/api/config");
    const resp = await r.json();
    if (!resp.config) throw new Error("config 为空");
    state.configData = resp.config;
    state.dirty = false;
    state.writable = resp.writable;
    state.cfgFilePath = resp.path || "";
    state.divergedPaths = new Set(resp.diff || []);
    state.cfgFileError = resp.file_error || "";
    renderAll(resp.path);
    loadEl?.remove();
  } catch (e) {
    msgEl.hidden = false;
    msgEl.className = "cfg-msg err";
    msgEl.textContent = "加载配置失败：" + e.message;
    if (loadEl) loadEl.textContent = "加载失败";
  }
}

// renderAll builds the entire config page (mode bar + form/json editor + action buttons).
function renderAll(filePath) {
  const container = document.querySelector("#config-page");
  container.querySelectorAll("#cfg-form, .cfg-loading").forEach((el) => el.remove());

  const form = document.createElement("div");
  form.id = "cfg-form";
  form.className = "cfg-form";

  if (!state.writable) {
    const note = document.createElement("div");
    note.className = "cfg-note muted";
    note.textContent = "当前配置无法写回文件（无配置文件路径），以下为只读视图。";
    form.appendChild(note);
  } else if (state.cfgFileError) {
    const note = document.createElement("div");
    note.className = "cfg-note warn";
    note.textContent = `配置文件读取失败，以下显示运行中的配置；保存将覆盖损坏的文件。(${state.cfgFileError})`;
    form.appendChild(note);
  } else if (state.divergedPaths.size > 0) {
    const note = document.createElement("div");
    note.className = "cfg-note warn";
    note.textContent = `文件配置与运行中配置有 ${state.divergedPaths.size} 处差异，重启服务后文件值才生效。标记 ● 的字段不一致。`;
    form.appendChild(note);
  } else {
    const note = document.createElement("div");
    note.className = "cfg-note muted";
    note.textContent = `配置文件路径：${filePath}。保存后需重启服务生效。`;
    form.appendChild(note);
  }

  // 模式切换按钮
  const modeBar = document.createElement("div");
  modeBar.className = "cfg-modebar";
  const btnForm = document.createElement("button");
  btnForm.type = "button";
  btnForm.textContent = "表单模式";
  btnForm.className = state.mode === "form" ? "cfg-mode active" : "cfg-mode";
  btnForm.addEventListener("click", () => { state.mode = "form"; renderAll(filePath); });
  const btnJson = document.createElement("button");
  btnJson.type = "button";
  btnJson.textContent = "JSON 编辑器";
  btnJson.className = state.mode === "json" ? "cfg-mode active" : "cfg-mode";
  btnJson.addEventListener("click", () => { state.mode = "json"; renderAll(filePath); });
  modeBar.append(btnForm, btnJson);
  form.appendChild(modeBar);

  if (state.mode === "json") {
    renderJsonEditor(form);
  } else {
    renderFormInto(form);
  }

  // 提交按钮
  const btnRow = document.createElement("div");
  btnRow.className = "cfg-actions";
  const saveBtn = document.createElement("button");
  saveBtn.id = "cfg-save";
  saveBtn.className = "cfg-save";
  saveBtn.textContent = "保存配置";
  saveBtn.disabled = !state.writable;
  saveBtn.addEventListener("click", saveConfig);
  btnRow.appendChild(saveBtn);

  if (state.writable) {
    const reloadBtn = document.createElement("button");
    reloadBtn.id = "cfg-reload";
    reloadBtn.className = "cfg-reload";
    reloadBtn.textContent = "重载服务";
    reloadBtn.addEventListener("click", reloadService);
    btnRow.appendChild(reloadBtn);
  }
  form.appendChild(btnRow);
  container.appendChild(form);
  updateSaveBtn();
}

function renderJsonEditor(form) {
  const box = document.createElement("div");
  box.className = "cfg-json-editor";
  const hint = document.createElement("div");
  hint.className = "cfg-json-hint muted";
  hint.textContent = "JSON 模式下直接编辑配置内容，保存时校验。";
  const ta = document.createElement("textarea");
  ta.id = "cfg-json-text";
  ta.spellcheck = false;
  ta.value = JSON.stringify(state.configData, null, 2);
  ta.addEventListener("input", () => {
    try { JSON.parse(ta.value); ta.style.borderColor = ""; } catch { ta.style.borderColor = "#e06c75"; }
    updateSaveBtn();
  });
  box.append(hint, ta);
  form.appendChild(box);
}

// reloadService POSTs /api/reload and waits out the socket gap by polling /api/whoami
// (the handler answers, then runServe shuts the listener down and rebinds).
async function reloadService() {
  const msgEl = $("cfg-msg");
  const btn = $("cfg-reload");
  if (!btn) return;
  if (!confirm("重载服务将重建 Web 服务（连接短暂中断），未保存的修改将丢失。确定继续？")) return;
  btn.disabled = true;
  btn.textContent = "重载中…";
  try {
    const r = await fetch("/api/reload", { method: "POST", headers: authHeaders() });
    const resp = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(resp.error || `HTTP ${r.status}`);
    const deadline = Date.now() + 15000;
    for (;;) {
      try {
        const w = await (await fetch("/api/whoami")).json();
        if (w && (w.version !== undefined)) break;
      } catch { /* socket still rebinding */ }
      if (Date.now() > deadline) throw new Error("服务未在 15 秒内恢复");
      await new Promise((res) => setTimeout(res, 300));
    }
    msgEl.hidden = false;
    msgEl.className = "cfg-msg ok";
    msgEl.textContent = "服务已重载，配置已生效。";
    loadConfig();
  } catch (e) {
    msgEl.hidden = false;
    msgEl.className = "cfg-msg err";
    msgEl.textContent = "重载失败：" + e.message;
  }
  btn.disabled = false;
  btn.textContent = "重载服务";
}

async function saveConfig() {
  const msgEl = $("cfg-msg");
  const saveBtn = $("cfg-save");
  if (!saveBtn) return;
  saveBtn.disabled = true;
  saveBtn.textContent = "保存中…";
  try {
    if (state.mode === "json") {
      const ta = $("cfg-json-text");
      if (ta) state.configData = JSON.parse(ta.value);
    }
    if (!state.configData || typeof state.configData !== "object") throw new Error("配置内容无效");
    const preErrs = clientValidate(state.configData);
    if (preErrs.length > 0) {
      msgEl.hidden = false;
      msgEl.className = "cfg-msg err";
      msgEl.textContent = "配置校验未通过：" + preErrs.join("；");
      saveBtn.disabled = false;
      saveBtn.textContent = "保存配置";
      return;
    }
    const r = await fetch("/api/config", {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...authHeaders() },
      body: JSON.stringify(state.configData),
    });
    const resp = await r.json();
    msgEl.hidden = false;
    if (r.ok) {
      msgEl.className = "cfg-msg ok";
      msgEl.textContent = resp.message || "配置已保存";
      if (resp.config) {
        state.configData = resp.config;
        renderAll(state.cfgFilePath);
      }
      state.dirty = false;
      updateSaveBtn();
    } else {
      msgEl.className = "cfg-msg err";
      msgEl.textContent = resp.error || "保存失败";
    }
  } catch (e) {
    msgEl.hidden = false;
    msgEl.className = "cfg-msg err";
    msgEl.textContent = "保存失败：" + e.message;
  }
  saveBtn.disabled = false;
  saveBtn.textContent = "保存配置";
}
