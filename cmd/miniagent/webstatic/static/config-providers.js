"use strict";

// config-providers.js — providers card rendering (renderProviders, renderKv, renderKvMap).

import { $, MASK, state } from "./config-state.js";
import { CONFIG_SECTIONS, getNested, setNested, deleteNested, markDirty, renderField } from "./config-form.js";

// renderProviders 渲染提供商卡片列表（增删 provider/model/header/map）。
function renderProviders(container) {
  const provs = Array.isArray(state.configData.providers) ? state.configData.providers : [];
  state.configData.providers = provs;

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
      const root = container.closest("#config-page") || container;
      renderFormInto(root);
    });
    head.append(title, delBtn);
    card.appendChild(head);

    const body = document.createElement("div");
    body.className = "cfg-provider-body";

    const addField = (label, path, type, help, def) => {
      body.appendChild(renderField(label, type, getNested(state.configData, `providers.${pi}.${path}`), (v) => {
        setNested(state.configData, `providers.${pi}.${path}`, v);
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
    const tcur = getNested(state.configData, `providers.${pi}.thinking.field`);
    tf.value = tcur || "";
    tf.placeholder = "字段名（如 reasoning_effort）";
    tf.addEventListener("input", () => {
      let t = getNested(state.configData, `providers.${pi}.thinking`);
      if (!t) t = {}; setNested(state.configData, `providers.${pi}.thinking`, t);
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
      modelsEl.querySelectorAll(".cfg-model").forEach((el) => el.remove());
      models.forEach((m, mi) => {
        const row = document.createElement("div");
        row.className = "cfg-model";
        const addM = (label, path, type) => {
          row.appendChild(renderField(label, type, getNested(state.configData, `providers.${pi}.models.${mi}.${path}`), (v) => {
            setNested(state.configData, `providers.${pi}.models.${mi}.${path}`, v);
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
    const root = container.closest("#config-page") || container;
    renderFormInto(root);
  });
  container.appendChild(addProvBtn);
}

// renderKv 渲染键值对集合（对象类型：headers）。
function renderKv(path, labelText) {
  const wrap = document.createElement("div");
  wrap.className = "cfg-field";
  wrap.style.flexDirection = "column";
  const label = document.createElement("label");
  label.className = "cfg-label";
  label.textContent = labelText;
  wrap.appendChild(label);
  const obj = getNested(state.configData, path) || {};
  const list = document.createElement("div");
  list.className = "cfg-array";
  const sync = () => {
    const entries = [...list.querySelectorAll(".cfg-array-row")].map((r) => {
      const [k, v] = r.querySelectorAll("input");
      return [k.value.trim(), v.value];
    }).filter(([k]) => k);
    if (entries.length === 0) { deleteNested(state.configData, path); markDirty(); return; }
    const o = {};
    for (const [k, v] of entries) o[k] = v;
    setNested(state.configData, path, o);
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
  const cur = getNested(state.configData, path) || {};
  const list = document.createElement("div");
  list.className = "cfg-array";
  const sync = () => {
    const entries = [...list.querySelectorAll(".cfg-array-row")].map((r) => {
      const [k, v] = r.querySelectorAll("input");
      return [k.value.trim(), v.value];
    }).filter(([k]) => k);
    if (entries.length === 0) { deleteNested(state.configData, path); markDirty(); return; }
    const o = {};
    for (const [k, v] of entries) o[k] = v;
    setNested(state.configData, path, o);
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

// renderFormInto renders the form-mode sections (providers + field groups).
// Imported by config.js for the form mode path.
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
        const currentVal = getNested(state.configData, f.key);
        const fe = renderField(f.label, f.type, currentVal, (v) => {
          setNested(state.configData, f.key, v);
          markDirty();
        }, f.help, f.default);
        if (state.divergedPaths.has(f.key)) fe.classList.add("cfg-diff");
        details.appendChild(fe);
      }
    }
    container.appendChild(details);
  }
}

export { renderProviders, renderKv, renderKvMap, renderFormInto };