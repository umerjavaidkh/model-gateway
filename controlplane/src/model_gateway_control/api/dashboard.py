"""The operator console, as one self-contained page.

One file with no build step, no bundler and no second container. It is a
read-only view over data the admin API already holds, plus a scratchpad for
sending a request through the gateway, and a separate deployable for two tabs
would be more machinery than the page.

Two tabs, because watching traffic and generating traffic are the same session:
you send a prompt, switch to Traffic, and see the stages it went through.

No token is ever baked in. There are two of them and they are deliberately kept
apart:

* the **admin token** authenticates this API and can reconfigure the fleet;
* the **gateway key** is an ordinary tenant key that can only spend that
  tenant's budget.

The chat tab uses the second, so leaving the page open does not put an
administrative credential into a request the browser sends to the data plane.
Both live in session storage and neither outlives the tab.
"""

from __future__ import annotations

import json

#: Where the chat tab posts when the operator has not said otherwise. It is a
#: field on the page rather than a build-time constant, because the console is
#: served by the control plane and the data plane is a different process on a
#: different host in every deployment that is not a laptop.
DEFAULT_GATEWAY_URL = "http://localhost:18080"

#: Substituted with a JSON-encoded URL when the page is rendered. A marker
#: rather than a format placeholder because the page is full of CSS braces.
_GATEWAY_URL_MARKER = '"__GATEWAY_URL_DEFAULT__"'

_TEMPLATE = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Model Gateway — console</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: #fbfbfa; --fg: #1a1a19; --muted: #6b6b68; --line: #e3e3e0;
    --panel: #ffffff; --fail: #b4341f; --ok: #2d6a4f; --accent: #2f5bea;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #17171a; --fg: #e8e8e6; --muted: #97978f; --line: #2c2c30;
      --panel: #1e1e22; --fail: #f0806c; --ok: #78c79b; --accent: #8aa5ff;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: var(--bg); color: var(--fg);
    font: 14px/1.5 ui-sans-serif, system-ui, -apple-system, sans-serif;
  }
  header {
    display: flex; gap: 12px; align-items: center; flex-wrap: wrap;
    padding: 14px 20px; border-bottom: 1px solid var(--line); background: var(--panel);
    position: sticky; top: 0; z-index: 2;
  }
  h1 { font-size: 15px; margin: 0 8px 0 0; font-weight: 600; }
  input, button, select, textarea {
    font: inherit; padding: 6px 10px; border: 1px solid var(--line);
    border-radius: 6px; background: var(--bg); color: var(--fg);
  }
  textarea { width: 100%; resize: vertical; font-size: 14px; }
  button { cursor: pointer; }
  button:disabled { opacity: .5; cursor: default; }
  button.on { border-color: var(--accent); color: var(--accent); }
  .tabs { display: flex; gap: 6px; margin-right: 4px; }
  .tabs button { border-radius: 999px; padding: 5px 14px; }
  .spacer { flex: 1; }
  main { padding: 20px; display: grid; gap: 20px; }
  /* An explicit display beats the user agent's [hidden] rule, so the
     inactive tab would otherwise stay on the page. */
  [hidden] { display: none !important; }
  .cards { display: flex; gap: 10px; flex-wrap: wrap; }
  .card {
    background: var(--panel); border: 1px solid var(--line); border-radius: 8px;
    padding: 10px 14px; min-width: 120px;
  }
  .card b { display: block; font-size: 20px; font-weight: 600; }
  .card span { color: var(--muted); font-size: 12px; }
  table { width: 100%; border-collapse: collapse; background: var(--panel); }
  th, td { text-align: left; padding: 8px 10px; border-bottom: 1px solid var(--line); }
  th { color: var(--muted); font-weight: 500; font-size: 12px; }
  tbody tr { cursor: pointer; }
  tbody tr:hover { background: color-mix(in srgb, var(--accent) 8%, transparent); }
  .wrap { border: 1px solid var(--line); border-radius: 8px; overflow: auto; }
  code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; }
  .fail { color: var(--fail); }
  .ok { color: var(--ok); }
  .muted { color: var(--muted); }
  dialog {
    border: 1px solid var(--line); border-radius: 10px; background: var(--panel);
    color: var(--fg); max-width: 720px; width: 92vw; padding: 0;
  }
  dialog header { position: static; }
  .body { padding: 16px 20px 20px; }
  .kv { display: grid; grid-template-columns: max-content 1fr; gap: 4px 16px; margin-bottom: 18px; }
  .kv dt { color: var(--muted); }
  .kv dd { margin: 0; }
  .bar { height: 20px; border-radius: 4px; background: var(--accent); min-width: 2px; }
  .bar.bad { background: var(--fail); }
  .empty { padding: 28px; text-align: center; color: var(--muted); }
  .panel {
    background: var(--panel); border: 1px solid var(--line);
    border-radius: 8px; padding: 16px 18px;
  }
  .row { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; margin-bottom: 12px; }
  .row label { color: var(--muted); font-size: 12px; }
  .turn { border-top: 1px solid var(--line); padding: 14px 0; }
  .turn:first-child { border-top: 0; }
  .turn .said { white-space: pre-wrap; margin: 0 0 8px; }
  .turn .said.me { color: var(--muted); }
  .turn pre {
    white-space: pre-wrap; margin: 0; background: var(--bg);
    border: 1px solid var(--line); border-radius: 6px; padding: 10px;
  }
  .meta { font-size: 12px; color: var(--muted); margin-top: 6px; }
  .meta button { padding: 2px 8px; font-size: 12px; }
</style>
</head>
<body>
<header>
  <h1>Model Gateway</h1>
  <div class="tabs">
    <button id="tab-traffic" class="on">Traffic</button>
    <button id="tab-chat">Chat</button>
  </div>
  <input id="token" type="password" placeholder="admin token" size="26"
         autocomplete="off" spellcheck="false">
  <span class="spacer"></span>
  <span id="status" class="muted"></span>
</header>

<main id="traffic">
  <div class="row">
    <button id="all" class="on">Latest 100</button>
    <button id="failed">Failures only</button>
    <label><input type="checkbox" id="shadow"> show shadow</label>
    <button id="refresh">Refresh</button>
    <label><input type="checkbox" id="auto" checked> auto-refresh</label>
  </div>
  <div class="cards" id="cards"></div>
  <div class="wrap">
    <table>
      <thead><tr>
        <th>when</th><th>outcome</th><th>model</th><th>tenant</th>
        <th>latency</th><th>tokens</th><th>cost</th><th>request</th>
      </tr></thead>
      <tbody id="rows"></tbody>
    </table>
    <div class="empty" id="empty" hidden></div>
  </div>
</main>

<main id="chat" hidden>
  <div class="panel">
    <div class="row">
      <label for="gw-url">gateway</label>
      <input id="gw-url" size="26" placeholder="http://localhost:18080"
             autocomplete="off" spellcheck="false">
      <label for="gw-key">key</label>
      <input id="gw-key" type="password" size="24" placeholder="gw_tenant_secret"
             autocomplete="off" spellcheck="false">
      <label for="model">model</label>
      <input id="model" list="models" size="22" placeholder="model or alias"
             autocomplete="off" spellcheck="false">
      <datalist id="models"></datalist>
    </div>
    <p class="muted" style="margin:0 0 10px;font-size:12px">
      This posts through the gateway with a tenant key, not the admin token —
      the same path any client takes. Every send shows up in Traffic.
    </p>
    <textarea id="prompt" rows="4" placeholder="Ask the model something. ⌘/Ctrl+Enter sends."></textarea>
    <div class="row" style="margin:10px 0 0">
      <button id="send">Send</button>
      <span id="chat-status" class="muted"></span>
    </div>
  </div>
  <div class="panel" id="transcript"><p class="muted">Nothing sent yet.</p></div>
</main>

<dialog id="detail"><header><b id="d-title"></b></header><div class="body" id="d-body"></div></dialog>

<script>
const GATEWAY_URL_DEFAULT = "__GATEWAY_URL_DEFAULT__";
const $ = (id) => document.getElementById(id);

// Session storage, not local: a credential should not outlive the tab it was
// pasted into. The admin token and the gateway key are stored under separate
// keys so that neither can be sent where the other belongs.
const remembered = (name, el, fallback = "") => {
  el.value = sessionStorage.getItem(name) || fallback;
  el.addEventListener("input", () => sessionStorage.setItem(name, el.value.trim()));
};
const value = (el) => el.value.trim();

remembered("gw-admin-token", $("token"));
remembered("gw-key", $("gw-key"));
remembered("gw-url", $("gw-url"), GATEWAY_URL_DEFAULT);

const escape = (s) => String(s ?? "").replace(/[&<>"]/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const usd = (micro) => "$" + (micro / 1e6).toFixed(6);
const when = (iso) => new Date(iso).toLocaleTimeString();

// --- tabs ---------------------------------------------------------------

let tab = "traffic";
function showTab(name) {
  tab = name;
  for (const other of ["traffic", "chat"]) {
    $(other).hidden = other !== name;
    $("tab-" + other).classList.toggle("on", other === name);
  }
  if (name === "traffic") load();
  else loadModels();
}
$("tab-traffic").onclick = () => showTab("traffic");
$("tab-chat").onclick = () => showTab("chat");

// --- traffic ------------------------------------------------------------

let failedOnly = false;

async function api(path) {
  if (!value($("token"))) throw new Error("no admin token");
  const res = await fetch(path, { headers: { Authorization: "Bearer " + value($("token")) } });
  if (!res.ok) throw new Error(res.status === 401 ? "the admin token was rejected" : "HTTP " + res.status);
  return res.json();
}

async function load() {
  if (tab !== "traffic") return;
  if (!value($("token"))) {
    $("rows").innerHTML = "";
    $("cards").innerHTML = "";
    $("empty").hidden = false;
    $("empty").textContent = "Paste your admin token above to load traffic.";
    $("status").textContent = "";
    return;
  }
  $("status").textContent = "loading…";
  try {
    const params = new URLSearchParams({ limit: "100" });
    if (failedOnly) params.set("failed", "true");
    if ($("shadow").checked) params.set("shadow", "true");

    const [list, summary] = await Promise.all([
      api("/v1/requests?" + params), api("/v1/requests/summary"),
    ]);
    render(list.requests, summary.failures);
    $("status").textContent = "updated " + new Date().toLocaleTimeString();
  } catch (err) {
    $("status").textContent = err.message;
    $("rows").innerHTML = "";
    $("empty").hidden = false;
    $("empty").textContent = err.message;
  }
}

function render(requests, failures) {
  const failed = requests.filter((r) => r.outcome).length;
  const slowest = requests.reduce((a, r) => Math.max(a, r.latency_ms), 0);
  const spend = requests.reduce((a, r) => a + r.cost_micro_usd, 0);

  const cards = [
    ["shown", requests.length], ["failed", failed],
    ["slowest", slowest + " ms"], ["spend", usd(spend)],
  ];
  // Failure kinds, most common first — "what is going wrong now" rather than
  // a lifetime total.
  for (const [kind, n] of Object.entries(failures).slice(0, 4)) cards.push([kind, n]);
  $("cards").innerHTML = cards
    .map(([k, v]) => `<div class="card"><b>${escape(v)}</b><span>${escape(k)}</span></div>`)
    .join("");

  $("empty").hidden = requests.length > 0;
  $("empty").textContent = failedOnly
    ? "No failures in the last 100 requests."
    : "Nothing yet. Send a request from the Chat tab.";
  $("rows").innerHTML = requests.map((r, i) => `
    <tr data-i="${i}">
      <td class="muted">${escape(when(r.occurred_at))}</td>
      <td class="${r.outcome ? "fail" : "ok"}">${escape(r.outcome || "ok")}${
        r.failed_at ? ` <span class="muted">at ${escape(r.failed_at)}</span>` : ""}</td>
      <td><code>${escape(r.adapter_id || r.base_model || "—")}</code></td>
      <td>${escape(r.tenant)}</td>
      <td>${r.latency_ms} ms</td>
      <td class="muted">${r.input_tokens}/${r.output_tokens}</td>
      <td class="muted">${escape(usd(r.cost_micro_usd))}</td>
      <td><code class="muted">${escape(r.request_id.slice(0, 12))}</code></td>
    </tr>`).join("");

  for (const row of $("rows").children) {
    row.onclick = () => show(requests[Number(row.dataset.i)]);
  }
}

function show(r) {
  $("d-title").textContent = (r.outcome || "ok") + " · " + r.request_id;
  const total = Math.max(1, r.stages.reduce((a, s) => a + s.duration_ms, 0));

  const facts = [
    ["when", new Date(r.occurred_at).toLocaleString()],
    ["tenant", r.tenant], ["key", r.key_id],
    ["model", r.adapter_id ? `${r.base_model} + ${r.adapter_id}` : r.base_model],
    ["deployment", `${r.deployment} (${r.provider})`],
    ["streamed", r.stream ? "yes" : "no"],
    ["shadow", r.shadow ? "yes — nobody was waiting for this" : "no"],
    ["latency", r.latency_ms + " ms"],
    ["time to first byte", r.time_to_first_byte_ms ? r.time_to_first_byte_ms + " ms" : "—"],
    ["tokens", `${r.input_tokens} in, ${r.output_tokens} out`],
    ["cost", usd(r.cost_micro_usd) + " (charged " + usd(r.price_micro_usd) + ")"],
    ["snapshot", r.snapshot_version],
  ];

  const stages = r.stages.length
    ? `<table><thead><tr><th>stage</th><th>took</th><th></th></tr></thead><tbody>` +
      r.stages.map((s) => `
        <tr>
          <td>${escape(s.name)}${s.outcome ? ` <span class="fail">${escape(s.outcome)}</span>` : ""}</td>
          <td>${s.duration_ms} ms</td>
          <td style="width:55%"><div class="bar ${s.outcome ? "bad" : ""}"
              style="width:${Math.max(2, (s.duration_ms / total) * 100)}%"></div></td>
        </tr>`).join("") + `</tbody></table>`
    : `<p class="muted">No stage timings — recorded before this gateway version.</p>`;

  $("d-body").innerHTML =
    `<dl class="kv">${facts.map(([k, v]) =>
      `<dt>${escape(k)}</dt><dd>${escape(v)}</dd>`).join("")}</dl>
     <b>Where the time went</b>${stages}
     <p style="margin-top:18px"><button onclick="detail.close()">Close</button></p>`;
  $("detail").showModal();
}

// --- chat ---------------------------------------------------------------

// The model names come from the admin API, so the operator picks a served
// model instead of guessing one and reading a 404 as an outage.
async function loadModels() {
  if (!value($("token"))) return;
  try {
    const { deployments } = await api("/v1/deployments");
    const names = [...new Set(deployments.map((d) => d.adapter_id || d.base_model))].sort();
    $("models").innerHTML = names.map((n) => `<option value="${escape(n)}">`).join("");
    if (!value($("model")) && names.length) $("model").value = names[0];
  } catch {
    // A missing model list is a smaller problem than a blocked chat tab: the
    // field takes any name typed into it.
  }
}

const turns = [];

async function send() {
  const prompt = $("prompt").value.trim();
  if (!prompt) return;
  const url = value($("gw-url")) || GATEWAY_URL_DEFAULT;
  const key = value($("gw-key"));
  const model = value($("model"));
  if (!key || !model) {
    $("chat-status").textContent = "a gateway key and a model are both needed";
    return;
  }

  $("send").disabled = true;
  $("chat-status").textContent = "sending…";
  const started = performance.now();
  const turn = { prompt, model, at: new Date() };
  turns.unshift(turn);
  drawTranscript();

  try {
    const res = await fetch(url.replace(/\\/$/, "") + "/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: "Bearer " + key },
      body: JSON.stringify({ model, messages: [{ role: "user", content: prompt }] }),
    });
    const body = await res.json().catch(() => null);
    // Round-trip measured here rather than taken from the response, because
    // this is the number the caller actually waits for; the gateway's own
    // view of it is on the Traffic row.
    turn.ms = Math.round(performance.now() - started);
    turn.requestId = res.headers.get("X-Request-Id") || "";
    turn.status = res.status;
    if (!res.ok) {
      turn.error = body?.error?.message || ("HTTP " + res.status);
    } else {
      turn.reply = body?.choices?.[0]?.message?.content ?? JSON.stringify(body, null, 2);
      turn.usage = body?.usage;
    }
    $("prompt").value = "";
    $("chat-status").textContent = "";
  } catch (err) {
    turn.ms = Math.round(performance.now() - started);
    // A cross-origin refusal reaches JavaScript as an opaque network error, so
    // name the likely cause rather than reporting "Failed to fetch".
    turn.error = err.message +
      " — is the gateway reachable at " + url + ", and is this page's origin in GATEWAY_CORS_ORIGINS?";
    $("chat-status").textContent = "";
  } finally {
    $("send").disabled = false;
    drawTranscript();
  }
}

function drawTranscript() {
  if (!turns.length) {
    $("transcript").innerHTML = `<p class="muted">Nothing sent yet.</p>`;
    return;
  }
  $("transcript").innerHTML = turns.map((t, i) => `
    <div class="turn">
      <p class="said me">${escape(t.prompt)}</p>
      ${t.error ? `<p class="fail" style="margin:0">${escape(t.error)}</p>`
                : t.reply !== undefined ? `<pre>${escape(t.reply)}</pre>`
                : `<p class="muted" style="margin:0">waiting…</p>`}
      <div class="meta">
        ${escape(t.model)} · ${t.ms !== undefined ? t.ms + " ms" : "…"}
        ${t.usage ? ` · ${t.usage.prompt_tokens ?? "?"} in, ${t.usage.completion_tokens ?? "?"} out` : ""}
        ${t.requestId ? ` · <code>${escape(t.requestId.slice(0, 12))}</code>
            <button data-trace="${i}">trace</button>` : ""}
      </div>
    </div>`).join("");

  for (const button of $("transcript").querySelectorAll("[data-trace]")) {
    button.onclick = () => trace(turns[Number(button.dataset.trace)].requestId);
  }
}

// Open the gateway's own record of a turn: the stages, not the reply.
async function trace(requestId) {
  try {
    show(await api("/v1/requests/" + encodeURIComponent(requestId)));
  } catch (err) {
    // The usage event travels through Redis and the accounting consumer, so a
    // just-sent request is briefly not there yet. That is not a failure.
    $("chat-status").textContent =
      err.message.includes("404") ? "not recorded yet — try again in a second" : err.message;
  }
}

$("send").onclick = send;
$("prompt").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) send();
});

// --- wiring -------------------------------------------------------------

$("all").onclick = () => { failedOnly = false; $("all").classList.add("on"); $("failed").classList.remove("on"); load(); };
$("failed").onclick = () => { failedOnly = true; $("failed").classList.add("on"); $("all").classList.remove("on"); load(); };
$("refresh").onclick = load;
$("shadow").onchange = load;

// Reload as the token is typed, not only on blur — pasting a token and seeing
// nothing happen reads as a broken page. Debounced so that a paste is one
// request rather than one per keystroke.
let typing;
$("token").addEventListener("input", () => {
  clearTimeout(typing);
  typing = setTimeout(() => { load(); loadModels(); }, 400);
});

load();
setInterval(() => { if ($("auto").checked) load(); }, 10000);
</script>
</body>
</html>
"""


def render_dashboard(gateway_url: str = DEFAULT_GATEWAY_URL) -> str:
    """Render the console with the gateway URL its chat tab should default to.

    JSON-encoded rather than interpolated: the value comes from deployment
    configuration, and configuration that lands inside a ``<script>`` block is
    still a place a quote can end a string early.
    """
    return _TEMPLATE.replace(_GATEWAY_URL_MARKER, json.dumps(gateway_url))
