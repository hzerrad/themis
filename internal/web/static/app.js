// Themis UI. Every value from the API is untrusted, evidence most of all:
// elements are built with textContent only, never innerHTML.
"use strict";

const TOKEN_KEY = "themis-token";
const POLL_MS = 3000;
const main = document.getElementById("main");
let timer = null;

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

function token() {
  try { return sessionStorage.getItem(TOKEN_KEY); } catch { return null; }
}

async function api(path, options = {}) {
  const resp = await fetch(path, {
    ...options,
    headers: { "Authorization": `Bearer ${token()}`, "Content-Type": "application/json" },
  });
  if (resp.status === 401) {
    try { sessionStorage.removeItem(TOKEN_KEY); } catch {}
    route();
    throw new Error("unauthorized");
  }
  if (!resp.ok) throw new Error((await resp.text()).trim() || resp.statusText);
  return resp.json();
}

const STATE_TONE = {
  resolved: "good", self_resolved: "good", closed_no_action: "neutral", canceled: "neutral", rejected: "neutral",
  awaiting_approval: "warn", escalated: "bad", contained_escalated: "warn",
  succeeded: "good", failed: "bad", stale: "bad", ambiguous: "bad", denied: "neutral", expired: "neutral", authorized: "warn",
};

const state = (s) => h("span", { class: `state ${STATE_TONE[s] || "neutral"}` }, s.replaceAll("_", " "));
const time = (t) => (t ? new Date(t).toLocaleString() : "—");
const target = (t) => `${t.Kind || t.kind} ${t.Namespace || t.namespace}/${t.Name || t.name}`;
const short = (id) => (id || "").slice(-12);

function render(...nodes) {
  main.replaceChildren(...nodes);
}

function login() {
  const input = h("input", { type: "password", placeholder: "admin token", autocomplete: "off" });
  const submit = (e) => {
    e.preventDefault();
    try { sessionStorage.setItem(TOKEN_KEY, input.value.trim()); } catch {}
    route();
  };
  render(h("form", { class: "panel", onsubmit: submit },
    h("h1", {}, "Sign in"),
    h("p", { class: "muted" }, "Enter the admin token. It is kept for this browser tab only."),
    h("div", { class: "actions" }, input, h("button", { class: "primary", type: "submit" }, "Continue"))));
  input.focus();
}

async function list() {
  const incidents = await api("/v1/incidents");
  const rows = incidents.map((i) => h("tr", { class: "link", onclick: () => { location.hash = `#/incidents/${i.id}`; } },
    h("td", {}, state(i.state)),
    h("td", {}, target(i.target)),
    h("td", {}, i.profile),
    h("td", {}, i.occurrenceCount),
    h("td", {}, time(i.createdAt)),
    h("td", { class: "mono muted" }, short(i.id))));
  render(
    h("h1", {}, "Incidents"),
    h("div", { class: "panel" }, incidents.length === 0
      ? h("p", { class: "muted" }, "No incidents yet.")
      : h("table", {},
        h("thead", {}, h("tr", {}, ["State", "Target", "Profile", "Alerts", "Opened", "ID"].map((c) => h("th", {}, c)))),
        h("tbody", {}, rows))));
}

function containers(template) {
  return ((template && template.spec && template.spec.containers) || []).map((c) => ({
    name: c.name,
    image: c.image,
    limits: JSON.stringify((c.resources && c.resources.limits) || {}),
    requests: JSON.stringify((c.resources && c.resources.requests) || {}),
  }));
}

function templateDiff(pre, desired) {
  const now = containers(pre);
  return h("table", {},
    h("thead", {}, h("tr", {}, ["Container", "Field", "Now", "After"].map((c) => h("th", {}, c)))),
    h("tbody", {}, containers(desired).flatMap((d) => {
      const n = now.find((c) => c.name === d.name) || {};
      return ["image", "limits", "requests"].map((f) => h("tr", {},
        h("td", {}, d.name), h("td", { class: "muted" }, f),
        h("td", { class: `mono ${n[f] !== d[f] ? "diff-old" : ""}` }, n[f] || "—"),
        h("td", { class: `mono ${n[f] !== d[f] ? "diff-new" : ""}` }, d[f])));
    })));
}

function proposalPanel(p) {
  return h("div", { class: "panel" },
    h("h3", {}, "Proposal (what the investigator suggested)"),
    h("dl", {},
      h("dt", {}, "Status"), h("dd", {}, state(p.Status), p.RejectionReason ? ` ${p.RejectionReason}` : ""),
      h("dt", {}, "Capability"), h("dd", { class: "mono" }, p.Capability, " ", JSON.stringify(p.Params)),
      h("dt", {}, "Target"), h("dd", {}, target(p.Target), h("div", { class: "mono muted" }, p.Target.UID)),
      h("dt", {}, "Expected effect"), h("dd", {}, p.ExpectedEffect || "—"),
      h("dt", {}, "Rationale"), h("dd", {}, p.Rationale || "—"),
      h("dt", {}, "Evidence"), h("dd", { class: "mono" }, (p.CitedEvidenceIDs || []).map(short).join(", ") || "—"),
      h("dt", {}, "Predicates"), h("dd", { class: "mono" }, (p.CitedPredicates || []).join(", ") || "—"),
      h("dt", {}, "Model"), h("dd", {}, (p.Provenance && p.Provenance.model) || "—"),
      p.Message ? [h("dt", {}, "Note"), h("dd", {}, p.Message)] : null));
}

function actionPanel(a, reload) {
  const error = h("div", { class: "error" });
  const reason = h("input", { type: "text", placeholder: "reason (optional)" });
  // The approval carries the digest shown here, never the server's current one.
  const shown = a.Digest;
  const decide = async (path, body, button) => {
    button.disabled = true;
    try {
      await api(`/v1/actions/${a.ID}/${path}`, { method: "POST", body: JSON.stringify(body) });
      reload();
    } catch (e) {
      error.textContent = e.message;
      button.disabled = false;
    }
  };
  const approve = h("button", { class: "primary", onclick: () => decide("approve", { digest: shown }, approve) }, "Approve this digest");
  const deny = h("button", { class: "danger", onclick: () => decide("deny", { reason: reason.value }, deny) }, "Deny");
  const t = a.Target;
  return h("div", { class: "panel" },
    h("h3", {}, "Governed action (derived from the cluster)"),
    h("dl", {},
      h("dt", {}, "State"), h("dd", {}, state(a.State)),
      h("dt", {}, "Decision"), h("dd", {}, a.Decision.replaceAll("_", " "), h("div", { class: "muted" }, a.DecisionReason)),
      h("dt", {}, "Capability"), h("dd", { class: "mono" }, `${a.Capability} v${a.CapabilityVersion}`, " ", `revision ${a.Params.revision}`),
      h("dt", {}, "Target"), h("dd", {}, target(t), h("div", { class: "mono muted" }, `uid ${t.uid} · generation ${t.generation} · manager ${t.manager || "—"}`)),
      h("dt", {}, "Blast radius"), h("dd", {}, `${a.BlastRadius.replicas} replicas · risk ${a.Risk}`),
      h("dt", {}, "Policy"), h("dd", { class: "mono" }, a.PolicyID || "—"),
      a.ExpiresAt ? [h("dt", {}, "Expires"), h("dd", {}, time(a.ExpiresAt))] : null,
      h("dt", {}, "Digest"), h("dd", { class: "mono" }, shown),
      a.Execution ? [h("dt", {}, "Execution"), h("dd", {}, state(a.Execution.Status), " ", a.Execution.Message)] : null,
      a.Verification ? [h("dt", {}, "Verification"), h("dd", {}, a.Verification.Outcome ? state(a.Verification.Outcome) : "in progress",
        h("pre", {}, JSON.stringify(a.Verification.Observation, null, 2)))] : null),
    h("h2", {}, "What changes"),
    templateDiff(a.PreState, a.Desired),
    a.State === "awaiting_approval" ? h("div", { class: "actions" }, approve, reason, deny) : null,
    error);
}

function hypotheses(inv) {
  const hs = (inv && inv.Hypotheses) || [];
  if (hs.length === 0) return h("p", { class: "muted" }, "No hypotheses recorded.");
  return hs.map((x) => h("div", { class: "hypothesis" },
    h("strong", {}, x.rootCauseClass || "unclassified"), " ",
    h("span", { class: "muted" }, (x.status || "").replace("HYPOTHESIS_STATUS_", "").toLowerCase()),
    h("div", {}, x.summary),
    h("div", { class: "mono muted" },
      `supporting: ${(x.supportingEvidenceIds || []).map(short).join(", ") || "—"}`,
      h("br"), `contradicting: ${(x.contradictingEvidenceIds || []).map(short).join(", ") || "—"}`,
      h("br"), `missing: ${(x.missing || []).join("; ") || "—"}`)));
}

async function detail(id) {
  const reload = () => detail(id).catch(showError);
  const d = await api(`/v1/incidents/${encodeURIComponent(id)}`);
  const inc = d.Incident;
  const inv = d.Investigation;
  const proposals = d.Proposals || [];
  const actions = d.Actions || [];

  render(
    h("p", {}, h("a", { href: "#" }, "← Incidents")),
    h("h1", {}, target(inc.target), " ", state(inc.state)),
    h("p", { class: "muted mono" }, `${inc.id} · profile ${inc.profile} · uid ${inc.target.UID} · opened ${time(inc.createdAt)}`),

    actions.some((a) => a.State === "awaiting_approval")
      ? h("div", { class: "callout" }, "An action awaits your approval. Compare the proposal with the action derived from the cluster before approving.")
      : null,

    actions.map((a) => {
      const p = proposals.find((x) => x.ID === a.ProposalID);
      return h("div", { class: "grid" }, p ? proposalPanel(p) : h("div"), actionPanel(a, reload));
    }),
    proposals.filter((p) => !actions.some((a) => a.ProposalID === p.ID)).map((p) => h("div", { class: "grid" }, proposalPanel(p))),

    h("h2", {}, "Investigation"),
    h("div", { class: "panel" }, inv
      ? [h("dl", {},
        h("dt", {}, "State"), h("dd", {}, inv.State, inv.Failure ? ` — ${inv.Failure}` : ""),
        h("dt", {}, "Conclusion"), h("dd", {}, inv.Conclusion || "—"),
        h("dt", {}, "Summary"), h("dd", {}, inv.Summary || "—"),
        h("dt", {}, "Model"), h("dd", {}, (inv.Provenance && inv.Provenance.model) || "—"),
        h("dt", {}, "Usage"), h("dd", { class: "mono" }, JSON.stringify(inv.Usage || {}))),
      h("h2", {}, "Hypotheses"), hypotheses(inv)]
      : h("p", { class: "muted" }, "Not investigated yet.")),

    h("h2", {}, "Predicates"),
    h("div", { class: "panel" }, h("table", {},
      h("thead", {}, h("tr", {}, ["Predicate", "Value", "Reason", "Evidence"].map((c) => h("th", {}, c)))),
      h("tbody", {}, (d.Predicates || []).map((p) => h("tr", {},
        h("td", { class: "mono" }, p.Name),
        h("td", { class: `truth-${p.Value}` }, p.Value),
        h("td", {}, p.Reason),
        h("td", { class: "mono muted" }, (p.EvidenceIDs || []).map(short).join(", "))))))),

    h("h2", {}, "Timeline"),
    h("div", { class: "panel" }, h("ol", { class: "timeline" }, (d.Transitions || []).map((t) => h("li", {},
      h("strong", {}, `${t.From} → ${t.To}`), " ", h("span", { class: "muted" }, `${time(t.At)} · ${t.Actor}`),
      h("div", {}, t.Reason))))),

    h("h2", {}, "Evidence"),
    h("div", { class: "panel" }, (d.Evidence || []).map((e) => h("details", {},
      h("summary", {}, h("span", { class: "mono" }, short(e.ID)), ` ${e.Template} · ${e.Classification} · ${time(e.CollectedAt)}`,
        e.Truncated ? " · truncated" : "", e.Redactions ? ` · ${e.Redactions} redacted` : ""),
      h("pre", {}, JSON.stringify(e.Data, null, 2))))));
  // Refreshing would discard what an approver is typing.
  if (!actions.some((a) => a.State === "awaiting_approval")) timer = setTimeout(reload, POLL_MS * 2);
}

function showError(e) {
  if (e.message === "unauthorized") return;
  render(h("div", { class: "panel error" }, e.message));
}

function route() {
  clearTimeout(timer);
  document.getElementById("logout").hidden = !token();
  if (!token()) return login();
  const m = location.hash.match(/^#\/incidents\/([\w-]+)$/);
  const run = m ? () => detail(m[1]) : async () => { await list(); timer = setTimeout(route, POLL_MS); };
  run().catch(showError);
}

document.getElementById("logout").addEventListener("click", () => {
  try { sessionStorage.removeItem(TOKEN_KEY); } catch {}
  route();
});
window.addEventListener("hashchange", route);
route();
