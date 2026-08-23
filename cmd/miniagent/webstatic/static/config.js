"use strict";

// config.js — WebUI 配置管理页面。
// 双模式：全字段表单 + JSON 高级编辑器。FORM_MODE 渲染可折叠分组 + providers 卡片编辑
// （增删 provider/model/header/thinking-map）；JSON_MODE 直接编辑原始 JSON 文本。
// 保存统一走 PUT /api/config，后端校验 + 原子写回。

import { api, authHeaders } from "./store.js";
import { eventsViewport } from "./views.js";

const $ = (id) => document.getElementById(id);

let configData = {};   // 当前编辑中的 config 对象
let dirty = false;
let writable = false;
let mode = "form";     // "form" | "json"

const MASK = "********"; // 后端掩码占位符（GET 时 secret 被替换为该值，PUT 原样保留→不覆盖）

// ── 字段 schema ──────────────────────────────────────────

// special:"providers" 走独立渲染（卡片列表），其余按 fields 通用渲染。
const CONFIG_SECTIONS = [
  { group: "providers", label: "提供商", special: "providers" },
  { group: "defaults", label: "默认参数", fields: [
    { key: "defaults.provider", label: "默认提供商", type: "text" },
    { key: "defaults.model", label: "默认模型", type: "text" },
    { key: "defaults.thinking", label: "默认思考级别", type: "text", help: "off / low / medium / high; 按 provider.thinking.map 声明" },
    { key: "defaults.system_prompt", label: "系统提示词", type: "text", help: "覆盖内置默认值" },
    { key: "defaults.rules_file", label: "规则文件", type: "text", help: "工作目录下的文件名（如 AGENTS.md），自动追加到系统提示词" },
    { key: "defaults.summary_request", label: "摘要请求提示", type: "text" },
    { key: "defaults.summarizer_prompt", label: "摘要器提示", type: "text" },
    { key: "defaults.subagent_guidance", label: "子代理引导", type: "text" },
    { key: "defaults.summary_create_instruction", label: "摘要创建指令", type: "text" },
    { key: "defaults.summary_update_instruction", label: "摘要更新指令", type: "text" },
    { key: "defaults.summary_template", label: "摘要模板", type: "text" },
  ]},
  { group: "run", label: "运行参数", fields: [
    { key: "run.max_tokens", label: "最大输出 Token", type: "number" },
    { key: "run.max_iterations", label: "最大迭代次数", type: "number" },
    { key: "run.max_tokens_total", label: "总 Token 预算（0=不限）", type: "number" },
    { key: "run.context_window", label: "上下文窗口", type: "number" },
    { key: "run.max_duration", label: "最大运行时长", type: "duration", help: "如 30m / 2h" },
    { key: "run.shell_timeout", label: "Shell 超时", type: "duration" },
    { key: "run.file_op_timeout", label: "文件操作超时", type: "duration" },
    { key: "run.write_timeout", label: "写入超时", type: "duration" },
    { key: "run.http_timeout", label: "HTTP 超时", type: "duration" },
    { key: "run.web_timeout", label: "网页抓取超时", type: "duration" },
    { key: "run.stream", label: "流式输出", type: "bool" },
    { key: "run.confirm_destructive", label: "确认破坏性操作", type: "bool", help: "开启后写/编辑/危险 shell 需确认" },
    { key: "run.context_use_real_usage", label: "使用真实用量", type: "bool", help: "为空则启用" },
    { key: "run.max_tool_result_chars", label: "工具结果最大字符", type: "number" },
    { key: "run.max_file_result_chars", label: "文件结果最大字符", type: "number" },
    { key: "run.max_parallel_tools", label: "最大并行工具数", type: "number" },
    { key: "run.context_keep_recent", label: "保留最近轮数", type: "number" },
    { key: "run.summary_max_chars", label: "摘要最大字符", type: "number" },
    { key: "run.max_read_file_bytes", label: "最大读取文件字节", type: "number" },
    { key: "run.max_shell_output_chars", label: "Shell 输出最大字符", type: "number" },
    { key: "run.shell_stream_window_bytes", label: "Shell 流窗口字节", type: "number" },
    { key: "run.max_session_bytes", label: "会话最大字节", type: "number" },
    { key: "run.summary_max_tokens", label: "摘要最大 Token", type: "number" },
    { key: "run.grep_max_matches", label: "Grep 最大匹配数", type: "number" },
    { key: "run.context_trim_tool_chars", label: "上下文修剪工具字符", type: "number" },
    { key: "run.context_keep_reasoning", label: "保留推理轮数", type: "number" },
    { key: "run.context_keep_tool_args", label: "保留工具参数轮数", type: "number" },
    { key: "run.context_keep_reasoning_chars", label: "保留推理字符数", type: "number" },
    { key: "run.preserve_recent_tokens", label: "保留最近 Token 预算", type: "number" },
    { key: "run.tool_output_dir", label: "工具输出目录", type: "text" },
    { key: "run.tool_output_retention", label: "工具输出保留时长", type: "duration", help: "如 168h" },
  ]},
  { group: "compaction", label: "摘要压缩", fields: [
    { key: "compaction.provider", label: "提供商", type: "text", help: "为空则使用默认提供商" },
    { key: "compaction.model", label: "模型", type: "text" },
    { key: "compaction.auto", label: "自动压缩", type: "bool", help: "检测用量溢出时自动触发" },
    { key: "compaction.reserved", label: "保留 Token", type: "number" },
  ]},
  { group: "web", label: "Web 服务", fields: [
    { key: "web.listen", label: "监听地址", type: "text", default: "127.0.0.1:8787" },
    { key: "web.key", label: "API 密钥", type: "secret", help: "留空=无认证(仅限回环地址)；******** 表示保持原值不变" },
    { key: "web.max_concurrent_turns", label: "最大并发轮次", type: "number", default: "0", help: "0=不限" },
    { key: "web.allowed_hosts", label: "允许 Host 白名单", type: "array", help: "每行一个，反代域名/外部 IP" },
  ]},
  { group: "session", label: "会话", fields: [
    { key: "session.dir", label: "会话目录", type: "text" },
  ]},
];

// ── 工具函数 ──────────────────────────────────────────

function getNested(obj, path) {
  return path.split(".").reduce((o, k) => (o != null ? o[k] : undefined), obj);
}

function setNested(obj, path, val) {
  const parts = path.split(".");
  let cur = obj;
  for (let i = 0; i < parts.length - 1; i++) {
    if (cur[parts[i]] == null) cur[parts[i]] = {};
    cur = cur[parts[i]];
  }
  cur[parts[parts.length - 1]] = val;
}

// deleteNested 删除路径指向的键（omitempty 字段删掉后保存时不再序列化）。
function deleteNested(obj, path) {
  const parts = path.split(".");
  let cur = obj;
  for (let i = 0; i < parts.length - 1; i++) {
    if (cur[parts[i]] == null) return;
    cur = cur[parts[i]];
  }
  delete cur[parts[parts.length - 1]];
}

function markDirty() {
  dirty = true;
  updateSaveBtn();
}

// renderField 通用字段渲染器。
function renderField(labelText, type, currentVal, onChange, help, def) {
  const wrap = document.createElement("div");
  wrap.className = "cfg-field";
  const label = document.createElement("label");
  label.className = "cfg-label";
  label.textContent = labelText;
  label.title = help || "";
  wrap.appendChild(label);
  if (help) {
    const h = document.createElement("span");
    h.className = "cfg-help muted";
    h.textContent = help;
    wrap.appendChild(h);
  }
  const input = document.createElement("div");
  input.className = "cfg-input";
  const val = currentVal !== undefined && currentVal !== null ? currentVal : def ?? "";

  switch (type) {
    case "bool": {
      const cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = val === true;
      cb.indeterminate = currentVal === null || currentVal === undefined;
      cb.addEventListener("change", () => {
        if (cb.indeterminate) { cb.checked = true; cb.indeterminate = false; onChange(true); return; }
        if (!cb.checked) { cb.checked = false; cb.indeterminate = true; onChange(null); return; }
        onChange(cb.checked);
      });
      input.appendChild(cb);
      break;
    }
    case "number": {
      const inp = document.createElement("input");
      inp.type = "number";
      inp.value = val !== "" ? val : "";
      inp.placeholder = def || "";
      inp.addEventListener("input", () => {
        const v = inp.value.trim();
        onChange(v === "" ? null : Number(v));
      });
      input.appendChild(inp);
      break;
    }
    case "secret": {
      const inp = document.createElement("input");
      inp.type = "text";
      inp.value = val !== "" && val !== MASK ? MASK : (val === MASK ? MASK : "");
      inp.placeholder = def || "";
      inp.onfocus = () => { if (inp.value === MASK) inp.value = ""; };
      inp.onblur = () => { if (inp.value === "") inp.value = MASK; };
      inp.addEventListener("input", () => { onChange(inp.value.trim() || null); });
      // 初始值：MASK 或空。真实当前值仅存在于后端。
      input.appendChild(inp);
      break;
    }
    case "duration":
    case "text": {
      const inp = document.createElement("input");
      inp.type = "text";
      inp.value = val !== "" ? val : "";
      inp.placeholder = type === "duration" ? "如 30s / 5m / 2h" : (def || "");
      if (type === "duration") inp.placeholder = "如 30s / 5m / 2h";
      inp.addEventListener("input", () => { onChange(inp.value.trim() || null); });
      input.appendChild(inp);
      break;
    }
    case "array": {
      const arr = Array.isArray(val) ? [...val] : [];
      const list = document.createElement("div");
      list.className = "cfg-array";
      const sync = () => onChange(arr.filter(Boolean));
      const addRow = (v) => {
        const row = document.createElement("div");
        row.className = "cfg-array-row";
        const inp = document.createElement("input");
        inp.type = "text";
        inp.value = v || "";
        const del = document.createElement("button");
        del.type = "button"; del.className = "ghost cfg-array-del"; del.textContent = "✕";
        del.addEventListener("click", () => { row.remove(); arr.splice([...list.children].indexOf(row), 1); sync(); });
        inp.addEventListener("input", () => {
          const idx = [...list.children].indexOf(row);
          arr[idx] = inp.value.trim();
          sync();
        });
        row.appendChild(inp); row.appendChild(del);
        list.appendChild(row);
      };
      arr.forEach(addRow);
      const addBtn = document.createElement("button");
      addBtn.type = "button"; addBtn.className = "ghost";
      addBtn.textContent = "+ 添加";
      addBtn.addEventListener("click", () => { arr.push(""); addRow(""); sync(); });
      input.appendChild(list); input.appendChild(addBtn);
      break;
    }
    default: {
      const span = document.createElement("span");
      span.textContent = String(val);
      input.appendChild(span);
    }
  }
  wrap.appendChild(input);
  return wrap;
}

// ── providers 专用渲染（卡片列表，增删 provider/model/header/map）──────────────────

function renderProviders(container) {
  const provs = Array.isArray(configData.providers) ? configData.providers : [];
  configData.providers = provs; // 保证数组存在

  // 每个 provider 一张卡片
  provs.forEach((p, pi) => {
    const card = document.createElement("div");
    card.className = "cfg-provider";
    const head = document.createElement("div");
    head.className = "cfg-provider-head";
    const title = document.createElement("span");
    title.className = "cfg-provider-title";
    title.textContent = p.name || `提供商 ${pi + 1}`;
    const delBtn = document.createElement("button");
    delBtn.type = "button"; delBtn.className = "ghost cfg-array-del";
    delBtn.textContent = "删除";
    delBtn.addEventListener("click", () => {
      provs.splice(pi, 1);
      markDirty();
      renderFormInto(container);
    });
    head.append(title, delBtn);
    card.appendChild(head);

    const body = document.createElement("div");
    body.className = "cfg-provider-body";

    const addField = (label, path, type, help, def) => {
      body.appendChild(renderField(label, type, getNested(configData, `providers.${pi}.${path}`), (v) => {
        setNested(configData, `providers.${pi}.${path}`, v);
        markDirty();
      }, help, def));
    };

    addField("名称", "name", "text");
    addField("Chat URL", "chat_url", "text");
    addField("Models URL", "models_url", "text");
    addField("API 密钥", "key", "secret", "******** 保持原值；留空=无 key（用 $MINIAGENT_API_KEY）");
    addField("最大输出 Token", "max_tokens", "number");
    addField("上下文窗口", "context_window", "number");
    addField("HTTP 超时", "http_timeout", "duration");
    addField("思考级别", "thinking_level", "text");
    addField("接受未终止流", "stream_allow_unterminated", "bool", "opt-in：接受无 [DONE] 的流（vLLM/Ollama）");

    // headers 键值对
    const headersBox = renderKv(`providers.${pi}.headers`, "自定义请求头（键=值）");
    body.appendChild(headersBox);

    // thinking 映射（field + map）
    const thinkWrap = document.createElement("div");
    thinkWrap.className = "cfg-field";
    const tl = document.createElement("label"); tl.className = "cfg-label"; tl.textContent = "思考映射";
    thinkWrap.appendChild(tl);
    const tin = document.createElement("div"); tin.className = "cfg-input";
    const tf = document.createElement("input"); tf.type = "text";
    const tcur = getNested(configData, `providers.${pi}.thinking.field`);
    tf.value = tcur || "";
    tf.placeholder = "字段名（如 reasoning_effort）";
    tf.addEventListener("input", () => {
      let t = getNested(configData, `providers.${pi}.thinking`);
      if (!t) t = {}; setNested(configData, `providers.${pi}.thinking`, t);
      t.field = tf.value.trim() || null;
      markDirty();
    });
    tin.appendChild(tf);
    thinkWrap.appendChild(tin);
    body.appendChild(thinkWrap);
    body.appendChild(renderKvMap(`providers.${pi}.thinking.map`, "思考级别映射（级别→wire 值）"));

    // models 子列表
    const modelsEl = document.createElement("div");
    modelsEl.className = "cfg-models";
    const mt = document.createElement("div"); mt.className = "cfg-models-title"; mt.textContent = "模型列表";
    modelsEl.appendChild(mt);
    const models = Array.isArray(p.models) ? p.models : [];
    p.models = models;
    const renderModels = () => {
      // 重新渲染模型行
      modelsEl.querySelectorAll(".cfg-model").forEach((el) => el.remove());
      models.forEach((m, mi) => {
        const row = document.createElement("div");
        row.className = "cfg-model";
        const addM = (label, path, type) => {
          row.appendChild(renderField(label, type, getNested(configData, `providers.${pi}.models.${mi}.${path}`), (v) => {
            setNested(configData, `providers.${pi}.models.${mi}.${path}`, v);
            markDirty();
          }, undefined, undefined));
        };
        addM("模型名", "name", "text");
        addM("最大 Token", "max_tokens", "number");
        addM("上下文窗口", "context_window", "number");
        addM("思考级别", "thinking", "text");
        const del = document.createElement("button");
        del.type = "button"; del.className = "ghost cfg-array-del";
        del.textContent = "✕";
        del.addEventListener("click", () => { models.splice(mi, 1); markDirty(); renderModels(); });
        row.appendChild(del);
        modelsEl.appendChild(row);
      });
    };
    renderModels();
    const addModelBtn = document.createElement("button");
    addModelBtn.type = "button"; addModelBtn.className = "ghost";
    addModelBtn.textContent = "+ 添加模型";
    addModelBtn.addEventListener("click", () => { models.push({ name: "" }); markDirty(); renderModels(); });
    modelsEl.appendChild(addModelBtn);
    body.appendChild(modelsEl);

    card.appendChild(body);
    container.appendChild(card);
  });

  const addProvBtn = document.createElement("button");
  addProvBtn.type = "button"; addProvBtn.className = "ghost";
  addProvBtn.textContent = "+ 添加提供商";
  addProvBtn.addEventListener("click", () => {
    provs.push({ name: "", chat_url: "", models: [] });
    markDirty();
    renderFormInto(container);
  });
  container.appendChild(addProvBtn);
}

// renderKv 渲染键值对集合（对象类型：headers）。条目按序排列，对象内 key 直接作为属性名。
function renderKv(path, labelText) {
  const wrap = document.createElement("div");
  wrap.className = "cfg-field";
  wrap.style.flexDirection = "column";
  const label = document.createElement("label");
  label.className = "cfg-label";
  label.textContent = labelText;
  wrap.appendChild(label);
  const obj = getNested(configData, path) || {}; // 渲染读，不写回——避免保存时空字段污染
  const list = document.createElement("div");
  list.className = "cfg-array";
  const sync = () => {
    // 清理空键（键为必填，空键行忽略）
    const entries = [...list.querySelectorAll(".cfg-array-row")].map((r) => {
      const [k, v] = r.querySelectorAll("input");
      return [k.value.trim(), v.value];
    }).filter(([k]) => k);
    if (entries.length === 0) { deleteNested(configData, path); markDirty(); return; } // 空则删字段，omitempty 不序列化
    const o = {};
    for (const [k, v] of entries) o[k] = v;
    setNested(configData, path, o);
    markDirty();
  };
  const addRow = (k, v) => {
    const row = document.createElement("div");
    row.className = "cfg-array-row";
    const kInp = document.createElement("input"); kInp.type = "text"; kInp.placeholder = "键"; kInp.value = k || "";
    const vInp = document.createElement("input"); vInp.type = "text"; vInp.placeholder = "值"; vInp.value = v || "";
    const del = document.createElement("button"); del.type = "button"; del.className = "ghost cfg-array-del"; del.textContent = "✕";
    kInp.addEventListener("input", sync);
    vInp.addEventListener("input", sync);
    del.addEventListener("click", () => { row.remove(); sync(); });
    row.append(kInp, vInp, del);
    list.appendChild(row);
  };
  Object.entries(obj).forEach(([k, v]) => addRow(k, v));
  const addBtn = document.createElement("button");
  addBtn.type = "button"; addBtn.className = "ghost";
  addBtn.textContent = "+ 添加条目";
  addBtn.addEventListener("click", () => { addRow("", ""); });
  wrap.append(list, addBtn);
  return wrap;
}

// renderKvMap 渲染对象值（map[string]string），路径指向对象本身。
function renderKvMap(path, labelText) {
  const wrap = document.createElement("div");
  wrap.className = "cfg-field";
  wrap.style.flexDirection = "column";
  const label = document.createElement("label");
  label.className = "cfg-label";
  label.textContent = labelText;
  wrap.appendChild(label);
  const cur = getNested(configData, path) || {}; // 渲染读，不写回——避免保存时空字段污染
  const list = document.createElement("div");
  list.className = "cfg-array";
  const sync = () => {
    const entries = [...list.querySelectorAll(".cfg-array-row")].map((r) => {
      const [k, v] = r.querySelectorAll("input");
      return [k.value.trim(), v.value];
    }).filter(([k]) => k);
    if (entries.length === 0) { deleteNested(configData, path); markDirty(); return; } // 空则删字段
    const o = {};
    for (const [k, v] of entries) o[k] = v;
    setNested(configData, path, o);
    markDirty();
  };
  const addRow = (k, v) => {
    const row = document.createElement("div");
    row.className = "cfg-array-row";
    const kInp = document.createElement("input"); kInp.type = "text"; kInp.placeholder = "级别"; kInp.value = k || "";
    const vInp = document.createElement("input"); vInp.type = "text"; vInp.placeholder = "wire 值"; vInp.value = v || "";
    const del = document.createElement("button"); del.type = "button"; del.className = "ghost cfg-array-del"; del.textContent = "✕";
    kInp.addEventListener("input", sync);
    vInp.addEventListener("input", sync);
    del.addEventListener("click", () => { row.remove(); sync(); });
    row.append(kInp, vInp, del);
    list.appendChild(row);
  };
  Object.entries(cur || {}).forEach(([k, v]) => addRow(k, v));
  const addBtn = document.createElement("button");
  addBtn.type = "button"; addBtn.className = "ghost";
  addBtn.textContent = "+ 添加映射";
  addBtn.addEventListener("click", () => { addRow("", ""); });
  wrap.append(list, addBtn);
  return wrap;
}

// ── 主渲染函数 ──────────────────────────────────────────

export function renderConfigPage() {
  const vp = eventsViewport();
  vp.innerHTML = "";
  const page = document.createElement("div");
  page.id = "config-page";
  page.innerHTML = `<div class="cfg-header">
    <h2>配置管理</h2>
    <span id="cfg-status" class="cfg-status"></span>
  </div>
  <div id="cfg-msg" class="cfg-msg" hidden></div>
  <div class="cfg-loading">加载中…</div>`;
  vp.appendChild(page);
  loadConfig();
}

async function loadConfig() {
  const loadEl = document.querySelector(".cfg-loading");
  const msgEl = $("cfg-msg");
  try {
    const r = await api("/api/config");
    const resp = await r.json();
    if (!resp.config) throw new Error("config 为空");
    configData = resp.config;
    dirty = false;
    writable = resp.writable;
    renderAll(resp.path);
    loadEl?.remove();
  } catch (e) {
    msgEl.hidden = false;
    msgEl.className = "cfg-msg err";
    msgEl.textContent = "加载配置失败：" + e.message;
    if (loadEl) loadEl.textContent = "加载失败";
  }
}

function renderAll(filePath) {
  const container = document.querySelector("#config-page");
  // 清空既有内容（保留 header/msg）
  container.querySelectorAll("#cfg-form, .cfg-loading").forEach((el) => el.remove());

  const form = document.createElement("div");
  form.id = "cfg-form";
  form.className = "cfg-form";

  if (!writable) {
    const note = document.createElement("div");
    note.className = "cfg-note muted";
    note.textContent = "当前配置无法写回文件（无配置文件路径），以下为只读视图。";
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
  btnForm.className = mode === "form" ? "cfg-mode active" : "cfg-mode";
  btnForm.addEventListener("click", () => { mode = "form"; renderAll(filePath); });
  const btnJson = document.createElement("button");
  btnJson.type = "button";
  btnJson.textContent = "JSON 编辑器";
  btnJson.className = mode === "json" ? "cfg-mode active" : "cfg-mode";
  btnJson.addEventListener("click", () => { mode = "json"; renderAll(filePath); });
  modeBar.append(btnForm, btnJson);
  form.appendChild(modeBar);

  if (mode === "json") {
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
  saveBtn.disabled = !writable;
  saveBtn.addEventListener("click", saveConfig);
  btnRow.appendChild(saveBtn);
  form.appendChild(btnRow);

  container.appendChild(form);
}

function renderFormInto(container) {
  container.querySelectorAll(".cfg-section, .cfg-provider, .cfg-providers-add").forEach((el) => el.remove());
  for (const sec of CONFIG_SECTIONS) {
    const details = document.createElement("details");
    details.className = "cfg-section";
    details.open = sec.group !== "providers";
    const summary = document.createElement("summary");
    const h3 = document.createElement("h3");
    h3.textContent = sec.label;
    summary.appendChild(h3);
    details.appendChild(summary);

    if (sec.special === "providers") {
      const provBox = document.createElement("div");
      provBox.className = "cfg-providers";
      details.appendChild(provBox);
      renderProviders(provBox);
    } else {
      for (const f of sec.fields) {
        const currentVal = getNested(configData, f.key);
        const fe = renderField(f.label, f.type, currentVal, (v) => {
          setNested(configData, f.key, v);
          markDirty();
        }, f.help, f.default);
        details.appendChild(fe);
      }
    }
    container.appendChild(details);
  }
}

function renderJsonEditor(form) {
  const box = document.createElement("div");
  box.className = "cfg-json";
  const hint = document.createElement("div");
  hint.className = "cfg-note muted";
  hint.textContent = "直接编辑配置 JSON。保存后端会校验结构，非法 JSON 或未知字段将被拒绝。";
  const ta = document.createElement("textarea");
  ta.id = "cfg-json-text";
  ta.spellcheck = false;
  ta.value = JSON.stringify(configData, null, 2);
  ta.addEventListener("input", () => {
    try {
      const parsed = JSON.parse(ta.value);
      configData = parsed;
      dirty = true;
      updateSaveBtn();
    } catch { /* 非法 JSON：保存时再报 */ }
  });
  box.append(hint, ta);
  form.appendChild(box);
}

function updateSaveBtn() {
  const btn = $("cfg-save");
  if (btn) btn.textContent = dirty ? "保存配置（有未保存更改）" : "保存配置";
}

async function saveConfig() {
  const msgEl = $("cfg-msg");
  const saveBtn = $("cfg-save");
  if (!saveBtn) return;
  saveBtn.disabled = true;
  saveBtn.textContent = "保存中…";
  try {
    // JSON 模式：从 textarea 重新解析
    if (mode === "json") {
      const ta = $("cfg-json-text");
      if (ta) configData = JSON.parse(ta.value);
    }
    if (!configData || typeof configData !== "object") throw new Error("配置内容无效");
    const r = await fetch("/api/config", {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...authHeaders() },
      body: JSON.stringify(configData),
    });
    const resp = await r.json();
    msgEl.hidden = false;
    if (r.ok) {
      msgEl.className = "cfg-msg ok";
      msgEl.textContent = resp.message || "配置已保存";
      dirty = false;
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