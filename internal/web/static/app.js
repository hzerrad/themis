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
  for (const c of children.flat(Infinity)) {
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

// Words for the values the API returns. Anything unlisted is shown as it is.
const STATE_LABEL = {
  received: "alert received", investigating: "investigating", diagnosed: "diagnosed",
  inconclusive: "no clear cause", recommended: "fix suggested, not applied", proposed: "fix proposed",
  awaiting_approval: "needs your approval", authorized: "approved", executing: "applying the fix",
  verifying: "checking the fix", resolved: "fixed", contained_escalated: "partly fixed, needs a person",
  escalated: "needs a person", closed_no_action: "closed, nothing changed", self_resolved: "resolved on its own",
  rejected: "not handled", canceled: "canceled",
  succeeded: "applied", failed: "failed", ambiguous: "unknown if applied", stale: "not applied: the workload changed",
  denied: "denied", expired: "approval ran out",
  accepted: "accepted", superseded: "replaced", deferred: "waiting: another fix is in progress",
};
// A suggestion's status, which shares words with an incident's state.
const SUGGESTION_LABEL = {
  accepted: "accepted", rejected: "refused", superseded: "replaced", deferred: "waiting: another fix is in progress",
};
const OUTCOME_LABEL = {
  resolved: "it worked", contained: "it helped, not fully", unchanged: "it didn't help",
  worsened: "it made things worse", inconclusive: "can't tell",
};
const CHECK_LABEL = {
  rollout_recent: "A rollout happened recently",
  failure_onset_after_rollout: "The failures started after the rollout",
  prior_revision_was_healthy: "The previous version was healthy",
  oom_killed_new_revision: "The new version runs out of memory",
  gitops_managed: "Managed by GitOps (Argo CD or Flux)",
};
const ANSWER_LABEL = { true: "yes", false: "no", unknown: "can't tell" };
const REJECTION_LABEL = {
  REJECTION_REASON_TARGET_MISMATCH: "It named a different workload than the incident's.",
  REJECTION_REASON_CAPABILITY_NOT_ALLOWED: "This kind of fix isn't allowed for this incident.",
  REJECTION_REASON_INVALID_PARAMS: "Its details were invalid.",
  REJECTION_REASON_UNKNOWN_EVIDENCE: "It cited evidence Themis never collected.",
  REJECTION_REASON_UNKNOWN_PREDICATE: "It cited a check that doesn't exist.",
};
const EVIDENCE_LABEL = {
  rollout_history: "Rollout history", pod_states: "Pod states", target_events: "Events", container_logs_tail: "Container logs",
};
const ACTOR_LABEL = {
  intake: "alert intake", investigation: "Themis", investigator: "the investigation", governance: "policy check",
  approval: "approval timer", execution: "Themis", executor: "executor", verify: "fix check", reconcile: "cluster watch",
};
const DECISION_LABEL = { allow: "allowed", require_approval: "needs approval", deny: "denied" };
const INVESTIGATION_LABEL = { pending: "waiting to start", leased: "in progress", completed: "finished", failed: "failed" };
const CAUSE_LABEL = {
  HYPOTHESIS_STATUS_SUPPORTED: "supported by the evidence", HYPOTHESIS_STATUS_REFUTED: "ruled out", HYPOTHESIS_STATUS_OPEN: "still open",
};

const label = (labels, v) => labels[v] || String(v || "").replaceAll("_", " ");
const state = (s) => h("span", { class: `state ${STATE_TONE[s] || "neutral"}` }, label(STATE_LABEL, s));
const fix = (capability, params) => (capability === "deployment.rollback-to-revision"
  ? `Roll back to revision ${params.revision}` : capability);
const time = (t) => (t ? new Date(t).toLocaleString() : "—");
const target = (t) => `${t.Kind || t.kind} ${t.Namespace || t.namespace}/${t.Name || t.name}`;
const short = (id) => (id || "").slice(-12);

function render(...nodes) {
  main.replaceChildren(...nodes.flat(Infinity).filter((n) => n !== null && n !== undefined && n !== false));
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
        h("thead", {}, h("tr", {}, ["State", "Workload", "Profile", "Alerts", "Opened", "ID"].map((c) => h("th", {}, c)))),
        h("tbody", {}, rows))));
}

function changesTable(changes) {
  if (!changes || changes.length === 0) {
    return h("p", { class: "muted" }, "Rolling back changes nothing visible in the pod spec.");
  }
  return h("table", {},
    h("thead", {}, h("tr", {}, ["Container", "Field", "Now", "After"].map((c) => h("th", {}, c)))),
    h("tbody", {}, changes.map((c) => h("tr", {},
      h("td", {}, c.container), h("td", { class: "mono" }, c.field),
      h("td", { class: "mono diff-old" }, c.before), h("td", { class: "mono diff-new" }, c.after)))),
    h("caption", { class: "muted" }, "Values that look like secrets are hidden. For commands and arguments, only the fact that they change is shown."));
}

function proposalPanel(p) {
  return h("div", { class: "panel" },
    h("h3", {}, "What the model suggested"),
    h("dl", {},
      h("dt", {}, "Status"), h("dd", {}, h("span", { class: `state ${p.Status === "rejected" ? "bad" : "neutral"}` }, label(SUGGESTION_LABEL, p.Status)), p.RejectionReason ? h("div", {}, label(REJECTION_LABEL, p.RejectionReason)) : null),
      h("dt", {}, "Fix"), h("dd", {}, fix(p.Capability, p.Params)),
      h("dt", {}, "Workload"), h("dd", {}, target(p.Target), h("div", { class: "mono muted" }, p.Target.UID)),
      h("dt", {}, "Expected effect"), h("dd", {}, p.ExpectedEffect || "—"),
      h("dt", {}, "Why"), h("dd", {}, p.Rationale || "—"),
      h("dt", {}, "Evidence cited"), h("dd", { class: "mono" }, (p.CitedEvidenceIDs || []).map(short).join(", ") || "—"),
      h("dt", {}, "Checks cited"), h("dd", {}, (p.CitedPredicates || []).map((c) => label(CHECK_LABEL, c)).join("; ") || "—"),
      h("dt", {}, "Model"), h("dd", {}, (p.Provenance && p.Provenance.model) || "—"),
      p.Message ? [h("dt", {}, "Note"), h("dd", {}, p.Message)] : null));
}

// observed says what the fix check last saw, in pods.
function observed(o) {
  if (!o) return null;
  return `${o.ReadyReplicas} of ${o.DesiredReplicas} pods ready · ${o.RestartDelta} restarts · ${o.NewOOMKills} out-of-memory kills`;
}

function actionPanel(a, reload) {
  const error = h("div", { class: "error" });
  const reason = h("input", { type: "text", placeholder: "Why you deny it (optional)" });
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
  const approve = h("button", { class: "primary", onclick: () => decide("approve", { digest: shown }, approve) }, "Approve this change");
  const deny = h("button", { class: "danger", onclick: () => decide("deny", { reason: reason.value }, deny) }, "Deny");
  const t = a.Target;
  return h("div", { class: "panel" },
    h("h3", {}, "The change Themis would make, read from the cluster"),
    h("dl", {},
      h("dt", {}, "State"), h("dd", {}, state(a.State)),
      h("dt", {}, "Policy"), h("dd", {}, label(DECISION_LABEL, a.Decision), h("div", { class: "muted" }, a.DecisionReason)),
      h("dt", {}, "Change"), h("dd", {}, fix(a.Capability, a.Params)),
      h("dt", {}, "Workload"), h("dd", {}, target(t), h("div", { class: "mono muted" }, `uid ${t.uid} · generation ${t.generation} · last changed by ${t.manager || "—"}`)),
      h("dt", {}, "Affects"), h("dd", {}, `${a.BlastRadius.replicas} pods · risk ${a.Risk}`),
      a.ExpiresAt ? [h("dt", {}, "Approve before"), h("dd", {}, time(a.ExpiresAt))] : null,
      h("dt", {}, "Change ID"), h("dd", {}, h("span", { class: "mono" }, shown), h("div", { class: "muted" }, "Approving approves exactly this change. If the workload changes first, the approval no longer applies.")),
      a.Execution ? [h("dt", {}, "Result"), h("dd", {}, state(a.Execution.Status), h("div", {}, a.Execution.Message))] : null,
      a.Verification ? [h("dt", {}, "Did it work?"), h("dd", {}, a.Verification.Outcome ? label(OUTCOME_LABEL, a.Verification.Outcome) : "still checking",
        h("div", { class: "muted" }, observed(a.Verification.Observation)))] : null),
    h("h2", {}, "What changes"),
    changesTable(a.Changes),
    a.State === "awaiting_approval" ? h("div", { class: "actions" }, approve, reason, deny) : null,
    error);
}

function usage(u) {
  if (!u || !u.stepsUsed) return "—";
  const n = (v) => Number(v || 0).toLocaleString();
  return `${u.stepsUsed} model steps · ${n(u.inputTokens)} tokens sent · ${n(u.outputTokens)} received`;
}

function hypotheses(inv) {
  const hs = (inv && inv.Hypotheses) || [];
  if (hs.length === 0) return h("p", { class: "muted" }, "No possible causes recorded.");
  return hs.map((x) => h("div", { class: "cause" },
    h("strong", {}, label({}, x.rootCauseClass || "unclassified")), " ",
    h("span", { class: "muted" }, label(CAUSE_LABEL, x.status)),
    h("div", {}, x.summary),
    h("div", { class: "muted" },
      `Evidence for it: ${(x.supportingEvidenceIds || []).map(short).join(", ") || "—"}`,
      h("br"), `Evidence against it: ${(x.contradictingEvidenceIds || []).map(short).join(", ") || "—"}`,
      h("br"), `Still missing: ${(x.missing || []).join("; ") || "—"}`)));
}

const NEXT = {
  received: "Themis received the alert and is collecting evidence.",
  investigating: "Themis is investigating with the model.",
  diagnosed: "The investigation suggested a fix. Themis is checking it against the cluster and your policy.",
  proposed: "Themis is checking the suggested fix against your policy.",
  awaiting_approval: "A fix needs your approval. Check what it changes, then approve or deny it.",
  authorized: "Approved. Themis is about to apply the fix.",
  executing: "Themis is applying the fix.",
  verifying: "The fix was applied. Themis is watching whether the workload recovers.",
  resolved: "The fix worked: the workload recovered and stayed healthy.",
  contained_escalated: "The fix helped but the workload is not fully healthy. A human should take over.",
  escalated: "Themis could not resolve this. A human should take over.",
  closed_no_action: "Closed without acting: no fix was justified or allowed.",
  self_resolved: "The alert resolved on its own before Themis acted.",
  rejected: "Themis did not act on this alert.",
  inconclusive: "Themis couldn't find a clear cause.",
  recommended: "Themis suggested a fix but won't apply it. The timeline says why.",
  canceled: "Canceled. The timeline says why.",
};

// summary says, in plain words, what happened and what is expected of you.
function summary(inc, inv, actions, proposals) {
  const facts = [];
  const accepted = proposals.find((p) => p.Status === "accepted");
  const action = actions[actions.length - 1];
  if (inv && inv.Summary) facts.push(h("li", {}, h("strong", {}, "Diagnosis: "), inv.Summary));
  if (action) {
    const changed = (action.Changes || []).map((c) => c.field).join(", ") || "nothing visible";
    facts.push(h("li", {}, h("strong", {}, "Proposed fix: "),
      `roll back to revision ${action.Params.revision}, which changes ${changed} on ${action.BlastRadius.replicas} pods.`));
    facts.push(h("li", {}, h("strong", {}, "Policy: "), action.DecisionReason));
  } else if (accepted) {
    facts.push(h("li", {}, h("strong", {}, "Proposed fix: "), accepted.Rationale));
  }
  const rejected = proposals.filter((p) => p.Status === "rejected").length;
  if (rejected) facts.push(h("li", {}, h("strong", {}, "Refused suggestions: "), `${rejected}, listed below.`));
  const lead = inc.supersededBy
    ? ["The workload was changed outside Themis and still fails, so a new incident investigates it as it is now: ",
      h("a", { href: `#/incidents/${inc.supersededBy}` }, "open the new incident"), "."]
    : NEXT[inc.state] || inc.state;
  return h("div", { class: `callout ${inc.state === "awaiting_approval" ? "" : "quiet"}` },
    h("p", { class: "lead" }, lead), facts.length ? h("ul", {}, facts) : null);
}

function rejectedAttempts(proposals) {
  if (proposals.length === 0) return null;
  return h("details", { class: "panel rejected" },
    h("summary", {}, `Suggestions Themis refused (${proposals.length})`),
    proposals.map((p) => h("div", { class: "grid" }, proposalPanel(p))));
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

    summary(inc, inv, actions, proposals),

    actions.map((a) => {
      const p = proposals.find((x) => x.ID === a.ProposalID);
      return h("div", { class: "grid" }, p ? proposalPanel(p) : h("div"), actionPanel(a, reload));
    }),
    rejectedAttempts(proposals.filter((p) => !actions.some((a) => a.ProposalID === p.ID))),

    h("h2", {}, "Investigation"),
    h("div", { class: "panel" }, inv
      ? [h("dl", {},
        h("dt", {}, "State"), h("dd", {}, label(INVESTIGATION_LABEL, inv.State), inv.Failure ? h("div", {}, inv.Failure) : null),
        h("dt", {}, "Outcome"), h("dd", {}, label({ proposed: "suggested a fix", inconclusive: "couldn't decide" }, inv.Conclusion) || "—"),
        h("dt", {}, "Summary"), h("dd", {}, inv.Summary || "—"),
        h("dt", {}, "Model"), h("dd", {}, (inv.Provenance && inv.Provenance.model) || "—"),
        h("dt", {}, "Usage"), h("dd", {}, usage(inv.Usage))),
      h("h2", {}, "Possible causes"), hypotheses(inv)]
      : h("p", { class: "muted" }, "Not investigated yet.")),

    h("h2", {}, "What Themis checked"),
    h("div", { class: "panel" }, h("table", {},
      h("thead", {}, h("tr", {}, ["Check", "Answer", "Why", "Evidence"].map((c) => h("th", {}, c)))),
      h("tbody", {}, (d.Predicates || []).map((p) => h("tr", {},
        h("td", {}, label(CHECK_LABEL, p.Name)),
        h("td", { class: `truth-${p.Value}` }, label(ANSWER_LABEL, p.Value)),
        h("td", {}, p.Reason),
        h("td", { class: "mono muted" }, (p.EvidenceIDs || []).map(short).join(", "))))))),

    h("h2", {}, "Timeline"),
    h("div", { class: "panel" }, h("ol", { class: "timeline" }, (d.Transitions || []).map((t) => h("li", {},
      h("strong", {}, label(STATE_LABEL, t.To)), " ", h("span", { class: "muted" }, `${time(t.At)} · ${label(ACTOR_LABEL, t.Actor)}`),
      h("div", {}, t.Reason))))),

    h("h2", {}, "Evidence"),
    h("div", { class: "panel" }, (d.Evidence || []).map((e) => h("details", {},
      h("summary", {}, h("span", { class: "mono" }, short(e.ID)), ` ${label(EVIDENCE_LABEL, e.Template)} · ${time(e.CollectedAt)}`,
        e.Classification === "sensitive" ? " · may contain sensitive data" : "",
        e.Truncated ? " · shortened" : "", e.Redactions ? ` · ${e.Redactions} secrets hidden` : ""),
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
