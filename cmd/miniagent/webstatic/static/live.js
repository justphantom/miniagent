"use strict";

// live.js — cross-browser sync client: the global lifecycle stream (/api/events) and per-view
// live attach (/api/sessions/{id}/live). Plain fetch NDJSON on purpose: EventSource cannot
// carry the x-api-key header, and the fetch reader loop already exists for /api/turn.

import { authHeaders } from "./store.js";

const RETRY_MS = 2000; // reconnect backoff after a dropped stream (D3: rebuild on reconnect)

// startEvents opens the global lifecycle feed and dispatches every non-keepalive event to
// onEvent. The stream self-heals: any drop (proxy idle timeout, server restart) reconnects
// after RETRY_MS; state resyncs via the sessions list on the next lifecycle event.
export function startEvents(onEvent) {
  let stopped = false;
  let ctrl = null;
  (async function run() {
    while (!stopped) {
      ctrl = new AbortController();
      try {
        const r = await fetch("/api/events", { headers: authHeaders(), signal: ctrl.signal });
        if (!r.ok || !r.body) throw new Error(`events ${r.status}`);
        const reader = r.body.getReader();
        const dec = new TextDecoder();
        let buf = "";
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          let nl;
          while ((nl = buf.indexOf("\n")) >= 0) {
            const line = buf.slice(0, nl).trim();
            buf = buf.slice(nl + 1);
            if (!line) continue;
            try {
              const ev = JSON.parse(line);
              if (ev.type !== "ping" && ev.type !== "hello") onEvent(ev);
            } catch { /* skip bad line */ }
          }
        }
      } catch { /* dropped — retry below */ }
      if (stopped) return;
      await new Promise(res => setTimeout(res, RETRY_MS));
    }
  })();
  return () => { stopped = true; ctrl?.abort(); };
}

// attachLive follows one session's in-flight turn into a sink object:
//   sink.event(ev)  — each NDJSON event (replay + live, from event zero)
//   sink.end()      — the turn finished (live_end received or stream dropped)
// Returns a detach() that aborts the fetch. Per D3 a dropped live stream is NOT re-attached
// by this module: the caller rebuilds its view (replay + live) instead.
export function attachLive(sessionID, sink) {
  const ctrl = new AbortController();
  (async function run() {
    try {
      const r = await fetch(`/api/sessions/${encodeURIComponent(sessionID)}/live`, { headers: authHeaders(), signal: ctrl.signal });
      if (!r.ok || !r.body) throw new Error(`live ${r.status}`);
      const reader = r.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let nl;
        while ((nl = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, nl).trim();
          buf = buf.slice(nl + 1);
          if (!line) continue;
          try {
            const ev = JSON.parse(line);
            if (ev.type === "live_end") { sink.end(); return; }
            if (ev.type === "live_truncated") continue; // marker already reflected by the replay itself
            sink.event(ev);
          } catch { /* skip bad line */ }
        }
      }
      sink.end(); // stream ended without live_end (server restart / proxy drop)
    } catch {
      sink.end();
    }
  })();
  return () => ctrl.abort();
}
