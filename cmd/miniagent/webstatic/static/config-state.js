"use strict";

// config-state.js — shared state and DOM helpers for the config management UI.

const $ = (id) => document.getElementById(id);
const MASK = "********";

// Shared mutable state: read/written by config-form.js, config-providers.js, config.js.
const state = {
  configData: {},
  dirty: false,
  writable: false,
  mode: "form",        // "form" | "json"
  cfgFilePath: "",
  divergedPaths: new Set(),
  cfgFileError: "",
};

export { $, MASK, state };