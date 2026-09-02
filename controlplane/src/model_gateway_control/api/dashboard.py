"""The traffic dashboard, as one self-contained page.

One file with no build step, no bundler and no second container. It is a
read-only view over data the admin API already holds, and a separate deployable
for one page would be more machinery than the page.

The token is never baked in. The operator pastes it, the browser keeps it in
session storage, and every fetch carries it — so the page can be served
unauthenticated without being a credential that leaks into every cache that
ever held it.
"""

from __future__ import annotations

DASHBOARD_HTML = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Model Gateway — traffic</title>
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
  h1 { font-size: 15px; margin: 0 12px 0 0; font-weight: 600; }
  input, button, select {
    font: inherit; padding: 6px 10px; border: 1px solid var(--line);
    border-radius: 6px; background: var(--bg); color: var(--fg);
  }
  button { cursor: pointer; }
  button.on { border-color: var(--accent); color: var(--accent); }
  main { padding: 20px; display: grid; gap: 20px; }
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
  code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; }
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
</style>
</head>
<body>
<header>
  <h1>Model Gateway</h1>
  <input id="token" type="password" placeholder="admin token" size="28">
  <button id="all" class="on">Latest 100</button>
  <button id="failed">Failures only</button>
  <label class="muted"><input type="checkbox" id="shadow"> show shadow</label>
  <button id="refresh">Refresh</button>
  <span id="status" class="muted"></span>
</header>

<main>
  <div class="cards" id="cards"></div>
  <div class="wrap">
    <table>
      <thead><tr>
        <th>when</th><th>outcome</th><th>model</th><th>tenant</th>
        <th>latency</th><th>tokens</th><th>cost</th><th>request</th>
      </tr></thead>
      <tbody id="rows"></tbody>
    </table>
    <div class="empty" id="empty" hidden>Nothing yet. Send a request through the gateway.</div>
  </div>
</main>

<dialog id="detail"><header><b id="d-title"></b></header><div class="body" id="d-body"></div></dialog>

<script>
const $ = (id) => document.getElementById(id);
// Session storage, not local: an operator's token should not outlive the tab
// they pasted it into.
const token = () => $("token").value || sessionStorage.getItem("gw-token") || "";
$("token").value = sessionStorage.getItem("gw-token") || "";
$("token").addEventListener("change", () => sessionStorage.setItem("gw-token", $("token").value));

let failedOnly = false;

async function api(path) {
  const res = await fetch(path, { headers: { Authorization: "Bearer " + token() } });
  if (!res.ok) throw new Error(res.status === 401 ? "bad or missing admin token" : "HTTP " + res.status);
  return res.json();
}

const escape = (s) => String(s ?? "").replace(/[&<>"]/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const usd = (micro) => "$" + (micro / 1e6).toFixed(6);
const when = (iso) => new Date(iso).toLocaleTimeString();

async function load() {
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

$("all").onclick = () => { failedOnly = false; $("all").classList.add("on"); $("failed").classList.remove("on"); load(); };
$("failed").onclick = () => { failedOnly = true; $("failed").classList.add("on"); $("all").classList.remove("on"); load(); };
$("refresh").onclick = load;
$("shadow").onchange = load;
$("token").addEventListener("change", load);
load();
setInterval(load, 10000);
</script>
</body>
</html>
"""
