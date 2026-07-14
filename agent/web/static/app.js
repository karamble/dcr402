"use strict";
// dcr402-agent dashboard — vanilla JS, no build step. Renders the live
// snapshot streamed from /api/events (SSE), with a polling fallback, and
// posts approve/deny to /api/approvals/resolve.

const $ = (id) => document.getElementById(id);
const DCR = (atoms) => (Number(atoms || 0) / 1e8);
const fmtDCR = (atoms) => DCR(atoms).toLocaleString(undefined, { maximumFractionDigits: 8 });

// The JSON API is authenticated. The token is printed in the daemon's startup
// log (or set via DCR402_AGENT_HTTP_TOKEN); it is kept in localStorage.
let TOKEN = localStorage.getItem("dcr402_token") || "";
function setToken(t) { TOKEN = t; localStorage.setItem("dcr402_token", t); }
function askToken(msg) {
  const t = prompt(msg || "dcr402-agent API token (from the daemon startup log):");
  if (t) setToken(t.trim());
  return TOKEN;
}
// api wraps fetch with the bearer token and re-prompts once on 401.
async function api(path, opts = {}) {
  if (!TOKEN) askToken();
  const headers = Object.assign({}, opts.headers, { "Authorization": "Bearer " + TOKEN });
  let resp = await fetch(path, Object.assign({}, opts, { headers }));
  if (resp.status === 401) {
    if (askToken("token rejected — enter the dcr402-agent API token:")) {
      const h2 = Object.assign({}, opts.headers, { "Authorization": "Bearer " + TOKEN });
      resp = await fetch(path, Object.assign({}, opts, { headers: h2 }));
    }
  }
  return resp;
}

function toast(msg, bad) {
  const t = $("toast");
  t.textContent = msg;
  t.classList.toggle("bad", !!bad);
  t.classList.add("show");
  setTimeout(() => t.classList.remove("show"), 2600);
}

function pct(spent, budget) {
  const b = Number(budget || 0);
  if (b <= 0) return 0;
  return Math.min(100, Math.round((Number(spent || 0) / b) * 100));
}

function renderStatus(st) {
  if (!st) return;
  $("network").textContent = st.network || "—";
  $("mode").textContent = st.policy_mode || "—";
  $("syncdot").classList.toggle("ok", !!st.synced);
  const node = st.node_pubkey || "";
  $("node").textContent = node ? node.slice(0, 10) + "…" : "—";

  $("ln-balance").textContent = fmtDCR(st.ln_spendable_atoms);
  if (st.onchain_enabled) {
    $("onchain-card").style.display = "";
    $("onchain-balance").textContent = fmtDCR(st.onchain_atoms);
  } else {
    $("onchain-card").style.display = "none";
  }

  const dayPct = pct(st.day_spent_atoms, st.daily_budget_atoms);
  $("day-lbl").textContent = st.daily_budget_atoms > 0
    ? `${fmtDCR(st.day_spent_atoms)} / ${fmtDCR(st.daily_budget_atoms)} DCR` : "no cap";
  $("day-bar").style.width = dayPct + "%";
  $("day-bar").className = dayPct >= 85 ? "hot" : "";

  const weekPct = pct(st.week_spent_atoms, st.weekly_budget_atoms);
  $("week-lbl").textContent = st.weekly_budget_atoms > 0
    ? `${fmtDCR(st.week_spent_atoms)} / ${fmtDCR(st.weekly_budget_atoms)} DCR` : "no cap";
  $("week-bar").style.width = weekPct + "%";
  $("week-bar").className = weekPct >= 85 ? "hot" : "";

  $("hour-lbl").textContent = `${st.hour_payment_count} payment${st.hour_payment_count === 1 ? "" : "s"}`;
  $("hour-bar").style.width = Math.min(100, st.hour_payment_count * 10) + "%";
}

function renderApprovals(list) {
  const wrap = $("approvals");
  const box = $("approval-list");
  box.textContent = "";
  if (!list || list.length === 0) { wrap.hidden = true; return; }
  wrap.hidden = false;
  for (const a of list) {
    const el = document.createElement("div");
    el.className = "approval";
    const expires = new Date(a.expires_at);
    el.innerHTML = `
      <div class="what">
        <div class="dest">approval <b>${a.id}</b> · attempt #${a.attempt_id}</div>
        <div class="memo">channel: ${a.channel}</div>
        <div class="idline">reply over Bison Relay: <b>yes ${a.id}</b> / <b>no ${a.id}</b></div>
      </div>
      <div class="act">
        <span class="clock">${expires.toLocaleTimeString()}</span>
        <button class="btn-approve">approve</button>
        <button class="btn-deny">deny</button>
      </div>`;
    el.querySelector(".btn-approve").addEventListener("click", () => resolve(a.id, true));
    el.querySelector(".btn-deny").addEventListener("click", () => resolve(a.id, false));
    box.appendChild(el);
  }
}

const TOOL_CHIP = (t) => `<span class="chip tool">${escapeHTML(t)}</span>`;

function renderFeed(attempts) {
  const feed = $("feed");
  feed.textContent = "";
  if (!attempts || attempts.length === 0) {
    feed.innerHTML = `<tr><td class="empty" colspan="5">waiting for activity…</td></tr>`;
    return;
  }
  for (const a of attempts) {
    const decision = a.decision;
    const dest = a.destination || "";
    const memo = a.memo || "";
    const atoms = a.amount_atoms || 0;
    const when = new Date(a.created_at);
    const chipClass = decision === "allow" ? "allow" : decision === "deny" ? "deny" : "escalate";
    const glyph = decision === "allow" ? "✓" : decision === "deny" ? "✕" : "◌";

    const row = document.createElement("tr");
    row.innerHTML = `
      <td>${when.toLocaleTimeString()}</td>
      <td>${TOOL_CHIP(a.tool)}</td>
      <td class="dest">${escapeHTML(dest.length > 40 ? dest.slice(0, 20) + "…" + dest.slice(-8) : dest)}
        ${memo ? `<div class="memo">${escapeHTML(memo)}</div>` : ""}</td>
      <td class="amt">${atoms ? fmtDCR(atoms) : "—"}</td>
      <td><span class="chip ${chipClass}">${glyph} ${decision}</span></td>`;
    feed.appendChild(row);

    if (decision === "deny") {
      const trace = parseTrace(a.rule_trace);
      if (trace) {
        const tr = document.createElement("tr");
        const td = document.createElement("td");
        td.colSpan = 5; td.style.padding = "0"; td.style.border = "0";
        td.appendChild(trace);
        tr.appendChild(td);
        feed.appendChild(tr);
      }
    }
  }
}

function parseTrace(rules) {
  // rule_trace is an embedded JSON array of {rule, pass, detail}.
  if (!Array.isArray(rules) || !rules.length) return null;
  const div = document.createElement("div");
  div.className = "trace";
  div.innerHTML = rules.map((r) =>
    r.pass
      ? `<span class="ok">✓</span> ${r.rule}${r.detail ? " — " + escapeHTML(r.detail) : ""}`
      : `<span class="bad">✕ ${r.rule}${r.detail ? ": " + escapeHTML(r.detail) : ""}</span> — short-circuit deny`
  ).join("<br>") + `<br><span style="opacity:.7">nothing signed, nothing sent</span>`;
  return div;
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

async function resolve(id, approved) {
  try {
    const resp = await api("/api/approvals/resolve", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id, approved }),
    });
    if (resp.ok) {
      toast(`${approved ? "Approved" : "Denied"} ${id}`, !approved);
    } else {
      const body = await resp.json().catch(() => ({}));
      toast(body.error || `failed (${resp.status})`, true);
    }
  } catch (e) {
    toast("network error", true);
  }
  refresh();
}

function apply(snapshot) {
  renderStatus(snapshot.status);
  renderApprovals(snapshot.approvals);
  renderFeed(snapshot.attempts);
}

async function refresh() {
  try {
    const [st, ap, at] = await Promise.all([
      api("/api/status").then((r) => r.json()),
      api("/api/approvals").then((r) => r.json()),
      api("/api/attempts?limit=12").then((r) => r.json()),
    ]);
    apply({ status: st, approvals: ap, attempts: at });
  } catch (_) { /* transient */ }
}

function connect() {
  const live = $("live");
  let es;
  try {
    if (!TOKEN) askToken();
    // EventSource cannot set headers, so the token rides as a query param.
    es = new EventSource("/api/events?token=" + encodeURIComponent(TOKEN));
  } catch (_) {
    setInterval(refresh, 3000); refresh(); return;
  }
  es.onmessage = (ev) => {
    live.classList.remove("stale");
    try { apply(JSON.parse(ev.data)); } catch (_) {}
  };
  es.onerror = () => {
    live.classList.add("stale");
    // EventSource auto-reconnects; keep a slow poll as a safety net.
  };
}

connect();
refresh();
