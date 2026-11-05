// Themis UI. Every value from the API is untrusted, evidence most of all:
// elements are built with textContent only, never innerHTML.
"use strict";

const TOKEN_KEY = "themis-token";
const POLL_MS = 3000;
const main = document.getElementById("main");
let timer = null;
// me is who's signed in, from /v1/me: a person, whoever holds the admin token,
// or, until setup finishes, whoever opened Themis through kubectl port-forward.
let me = null;

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

// headers go with every request. The request header proves a change comes
// from this page, which a form on another site can't add.
function headers() {
  const out = { "Content-Type": "application/json", "X-Themis-Request": "1" };
  if (token()) out.Authorization = `Bearer ${token()}`;
  return out;
}

async function api(path, options = {}) {
  const resp = await fetch(path, { ...options, headers: headers() });
  if (resp.status === 401) {
    try { sessionStorage.removeItem(TOKEN_KEY); } catch {}
    me = null;
    route();
    throw new Error("unauthorized");
  }
  if (!resp.ok) throw new Error((await resp.text()).trim() || resp.statusText);
  // Changes such as creating an account answer with no body.
  const text = await resp.text();
  return text ? JSON.parse(text) : null;
}

const STATE_TONE = {
  resolved: "good", self_resolved: "good", closed_no_action: "neutral", canceled: "neutral", rejected: "neutral",
  awaiting_approval: "warn", awaiting_suggestion: "warn", escalated: "bad", contained_escalated: "warn",
  succeeded: "good", failed: "bad", stale: "bad", ambiguous: "bad", denied: "neutral", expired: "neutral", authorized: "warn",
};

// Words for the values the API returns. Anything unlisted is shown as it is.
const STATE_LABEL = {
  received: "alert received", investigating: "investigating", awaiting_suggestion: "waiting for a suggestion", diagnosed: "diagnosed",
  inconclusive: "no clear cause", recommended: "fix suggested, not applied", proposed: "fix proposed",
  awaiting_approval: "needs your approval", authorized: "approved", executing: "applying the fix",
  verifying: "checking the fix", restoring: "putting things back", resolved: "fixed", contained_escalated: "partly fixed, needs a person",
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
const ROOT_CAUSE_LABEL = {
  memory_limit_regression: "The new version's memory limit is too low",
  bad_configuration: "The new version's settings are wrong",
  image_pull_failure: "The image can't be pulled",
  application_bug: "A bug in the application",
  configuration_change: "A config change outside the rollout",
  dependency_failure: "Something it depends on is failing",
  insufficient_capacity: "The cluster has no room for it",
  node_problem: "A node is unhealthy",
  storage_full: "A disk or volume is full",
  traffic_spike: "More traffic than it can take",
  insufficient_evidence: "Not enough evidence to tell",
  other: "Something else",
};
const ANSWER_LABEL = { true: "yes", false: "no", unknown: "can't tell" };
const REJECTION_LABEL = {
  REJECTION_REASON_TARGET_MISMATCH: "It named a different workload than the incident's.",
  REJECTION_REASON_CAPABILITY_NOT_ALLOWED: "This kind of fix isn't allowed for this incident.",
  REJECTION_REASON_INVALID_PARAMS: "Its details were invalid.",
  REJECTION_REASON_UNKNOWN_EVIDENCE: "It cited evidence Themis never collected.",
  REJECTION_REASON_UNKNOWN_PREDICATE: "It cited a check that doesn't exist.",
  REJECTION_REASON_INCIDENT_CLOSED: "The incident was already past taking suggestions.",
};
const EVIDENCE_LABEL = {
  rollout_history: "Rollout history", pod_states: "Pod states", target_events: "Events", container_logs_tail: "Container logs", referenced_config: "Config it refers to",
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

// Roles, least trusted first: each can do what the ones before it can.
const ROLES = ["viewer", "responder", "approver", "admin"];
const can = (role) => me && ROLES.indexOf(me.role) >= ROLES.indexOf(role);

// passwordForm signs in with a Themis account.
function passwordForm(primary) {
  const username = h("input", { type: "text", placeholder: "username", autocomplete: "username", autocapitalize: "none" });
  const password = h("input", { type: "password", placeholder: "password", autocomplete: "current-password" });
  const error = h("div", { class: "error" });
  const submit = async (e) => {
    e.preventDefault();
    error.textContent = "";
    const resp = await fetch("/auth/password", { method: "POST", headers: headers(), body: JSON.stringify({ username: username.value.trim(), password: password.value }) });
    if (!resp.ok) {
      error.textContent = (await resp.text()).trim();
      password.value = "";
      return;
    }
    // Signing in during setup finishes it: Setup shows what's left.
    if (me && me.setup) location.hash = "#/setup";
    me = null;
    route();
  };
  const form = h("form", { onsubmit: submit },
    h("div", { class: "actions" }, username, password, h("button", { class: primary ? "primary" : "", type: "submit" }, "Sign in")),
    error);
  return { form, focus: () => username.focus() };
}

async function login() {
  let methods = { oidc: false, password: false };
  try { methods = await (await fetch("/v1/sign-in")).json(); } catch {}
  const input = h("input", { type: "password", placeholder: "admin token", autocomplete: "off" });
  const submit = (e) => {
    e.preventDefault();
    try { sessionStorage.setItem(TOKEN_KEY, input.value.trim()); } catch {}
    route();
  };
  const tokenForm = h("form", { onsubmit: submit },
    h("p", { class: "muted" }, "The emergency token is for scripts and for when sign-in is broken. It is kept for this browser tab only."),
    h("div", { class: "actions" }, input, h("button", { type: "submit" }, "Continue")));
  const emergency = h("details", {}, h("summary", { class: "muted" }, "Use the emergency token"), tokenForm);
  if (!methods.oidc && !methods.password) {
    render(h("div", { class: "panel" },
      h("h1", {}, "Sign-in isn't set up yet"),
      h("p", {}, "Open Themis through kubectl port-forward to set it up. You'll be an admin until someone signs in for real:"),
      h("pre", {}, "kubectl -n themis port-forward deploy/themis-core 8090:8083"),
      h("p", { class: "muted" }, "Then go to http://localhost:8090. If you installed Themis under another name or namespace, use those."),
      emergency));
    return;
  }
  const pw = methods.password ? passwordForm(!methods.oidc) : null;
  render(h("div", { class: "panel" },
    h("h1", {}, "Sign in"),
    methods.oidc ? h("p", {}, h("a", { class: "button primary", href: "/auth/login" }, "Sign in with your identity provider")) : null,
    pw ? [methods.oidc ? h("p", { class: "muted" }, "Or with a Themis account:") : null, pw.form] : null,
    emergency));
  if (pw && !methods.oidc) pw.focus();
}

// accounts lets admins manage who signs in with a password. During setup,
// it's where the first admin account is made.
async function accounts() {
  const list = await api("/v1/accounts");
  const error = h("div", { class: "error" });
  const act = async (path, options) => {
    error.textContent = "";
    try {
      await api(path, options);
      route();
    } catch (e) {
      error.textContent = e.message;
    }
  };
  const roleSelect = (value) => h("select", {}, ROLES.map((r) => h("option", { value: r, selected: r === value }, r)));
  const username = h("input", { type: "text", placeholder: "username", autocomplete: "off", autocapitalize: "none" });
  const role = roleSelect(me.setup && !list.length ? "admin" : "approver");
  const password = h("input", { type: "password", placeholder: "password, 12 characters or more", autocomplete: "new-password" });
  const create = (e) => {
    e.preventDefault();
    act("/v1/accounts", { method: "POST", body: JSON.stringify({ username: username.value.trim(), role: role.value, password: password.value }) });
  };
  const hasAdmin = list.some((a) => a.role === "admin");
  const first = me.setup && !hasAdmin;
  // During setup, once an admin account exists, signing in with it finishes setup.
  const finish = me.setup && hasAdmin ? passwordForm(true) : null;
  const rows = list.map((a) => {
    const self = me.account && me.username === a.username;
    const r = roleSelect(a.role);
    r.disabled = self;
    r.addEventListener("change", () => act(`/v1/accounts/${encodeURIComponent(a.username)}`, { method: "PATCH", body: JSON.stringify({ role: r.value }) }));
    const pw = h("input", { type: "password", placeholder: "new password", autocomplete: "new-password" });
    const reset = h("button", { onclick: () => {
      if (pw.value) act(`/v1/accounts/${encodeURIComponent(a.username)}`, { method: "PATCH", body: JSON.stringify({ password: pw.value }) });
    } }, "Set password");
    const remove = h("button", { class: "danger", disabled: self, onclick: () => act(`/v1/accounts/${encodeURIComponent(a.username)}`, { method: "DELETE" }) }, "Delete");
    return h("tr", {},
      h("td", {}, a.username, a.locked ? h("div", { class: "muted" }, "locked after wrong passwords") : null),
      h("td", {}, r),
      h("td", { class: "muted" }, `${a.createdBy} · ${time(a.createdAt)}`),
      h("td", {}, h("div", { class: "actions" }, pw, reset, remove)));
  });
  render(
    finish ? h("div", { class: "panel" },
      h("h1", {}, "Now sign in with your admin account"),
      h("p", { class: "muted" }, "That finishes setup: from then on, kubectl port-forward alone no longer makes anyone an admin."),
      finish.form) : null,
    h("h1", {}, first ? "Create your admin account" : "Accounts"),
    h("p", { class: "muted" }, first
      ? "Then sign in with it here. That finishes setup: from then on, kubectl port-forward alone no longer makes anyone an admin."
      : "People who sign in with a username and password. People who sign in through your identity provider get their roles from Themis's settings instead."),
    list.length ? h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, "Username"), h("th", {}, "Role"), h("th", {}, "Created"), h("th", {}))), h("tbody", {}, rows)) : null,
    h("div", { class: "panel" },
      h("h3", {}, "New account"),
      h("form", { onsubmit: create }, h("div", { class: "actions" }, username, role, password, h("button", { class: "primary", type: "submit" }, "Create")))),
    error);
}

// CHECK_STATE says how each part of the install is doing, and in what tone.
const CHECK_STATE = {
  ok: ["Working", "good"], warn: ["Needs attention", "warn"], problem: ["Not working", "bad"],
  waiting: ["Waiting", "neutral"], off: ["Off", "neutral"],
};

// ago says how long ago t was, roughly.
function ago(t) {
  const s = Math.max(0, Math.round((Date.now() - new Date(t)) / 1000));
  if (90 > s) return `${s} seconds ago`;
  if (90 * 60 > s) return `${Math.round(s / 60)} minutes ago`;
  if (36 * 3600 > s) return `${Math.round(s / 3600)} hours ago`;
  return time(t);
}

// copyable is code to copy and run, with a button that copies it.
function copyable(code) {
  const button = h("button", { onclick: async () => {
    try {
      await navigator.clipboard.writeText(code);
      button.textContent = "Copied";
    } catch {
      button.textContent = "Select it and copy";
    }
  } }, "Copy");
  return h("div", { class: "copyable" }, h("pre", {}, code), button);
}

// alertmanagerSetup shows how to send Alertmanager's alerts to Themis.
function alertmanagerSetup() {
  const ns = h("input", { type: "text", value: "monitoring", autocapitalize: "none", "aria-label": "Alertmanager's namespace" });
  const out = h("div", {});
  const show = async () => {
    try {
      const { steps } = await api(`/v1/setup/alertmanager?namespace=${encodeURIComponent(ns.value.trim())}`);
      out.replaceChildren(...steps.map((s, i) => h("div", { class: "step" },
        h("h3", {}, `${i === 0 ? "1" : "2"}. ${s.title}`), h("p", { class: "muted" }, s.text), copyable(s.code))));
    } catch (e) {
      out.replaceChildren(h("div", { class: "error" }, e.message));
    }
  };
  ns.addEventListener("change", show);
  show();
  return h("div", { class: "panel" },
    h("h3", {}, "Connect Alertmanager"),
    h("div", { class: "actions" }, h("label", {}, "Alertmanager's namespace ", ns)),
    out,
    h("p", { class: "muted" }, "Once a crash-loop or out-of-memory alert fires, this page shows alerts arriving."));
}

// modelStep sets the model the install left unset. What the install set is
// shown locked: a change here would be undone by the next upgrade.
// modelSaved says the model was just saved, so Setup can say so once.
let modelSaved = false;

function modelStep(v, done) {
  const saved = modelSaved;
  modelSaved = false;
  const byInstall = (name) => v.byInstall.includes(name);
  const locked = h("span", { class: "muted" }, " Set by Helm: change it in the chart's values.");
  const field = (name, input) => byInstall(name) ? [input, locked] : input;
  const url = h("input", { type: "text", value: v.baseURL, placeholder: "https://api.openai.com/v1", disabled: byInstall("model.baseURL"), "aria-label": "Model address" });
  const name = h("input", { type: "text", value: v.name, placeholder: "gpt-4.1 or qwen3:8b", disabled: byInstall("model.name"), "aria-label": "Model name" });
  const key = h("input", { type: "password", autocomplete: "off", disabled: byInstall("model.apiKey"),
    placeholder: v.apiKeySet ? "set: leave empty to keep it" : "if the model needs one", "aria-label": "API key" });
  const strict = h("input", { type: "checkbox", checked: v.strictOutput, disabled: byInstall("model.strictOutput") });
  const error = h("div", { class: "error" });
  const save = async (e) => {
    e.preventDefault();
    error.textContent = "";
    const change = {};
    if (!byInstall("model.baseURL")) { change.baseURL = url.value.trim(); change.strictOutput = strict.checked; }
    if (!byInstall("model.name")) change.name = name.value.trim();
    if (!byInstall("model.apiKey") && key.value) change.apiKey = key.value;
    try {
      await api("/v1/settings/model", { method: "PUT", body: JSON.stringify(change) });
      modelSaved = true;
      route();
    } catch (err) {
      error.textContent = err.message;
    }
  };
  const all = ["model.baseURL", "model.name"].every(byInstall);
  return h("div", { class: "panel step" },
    h("h3", {}, `2. The model ${done ? "· done" : v.complete ? "· set" : ""}`),
    saved ? h("p", { class: "fix" }, "Saved. The investigator uses it from its next investigation; this step shows done once the model has answered one.") : null,
    all ? h("p", {}, `Set by Helm: ${v.name} at ${v.baseURL}. Change it in the chart's values.`)
      : h("form", { onsubmit: save },
        h("p", { class: "muted" }, "Any OpenAI-compatible endpoint: OpenAI, OpenRouter, Ollama, vLLM. The investigator uses it from its next investigation."),
        h("dl", {},
          h("dt", {}, "Address"), h("dd", {}, field("model.baseURL", url)),
          h("dt", {}, "Model"), h("dd", {}, field("model.name", name)),
          h("dt", {}, "API key"), h("dd", {}, field("model.apiKey", key),
            h("div", { class: "muted" }, "Kept encrypted, and never shown again.")),
          h("dt", {}, "Strict format"), h("dd", {}, h("label", {}, field("model.strictOutput", strict),
            " Ask the model to follow Themis's reply format exactly. Turn it off for OpenAI, which refuses it."))),
        h("div", { class: "actions" }, h("button", { class: "primary", type: "submit" }, "Save")),
        error));
}

// setup walks through what an install needs, in order, each step marked
// done when the status page says so.
async function setup() {
  const { checks } = await api("/v1/status");
  const check = (name) => checks.find((c) => c.name === name) || { state: "waiting" };
  const done = (name) => check(name).state === "ok";
  let model = null;
  try { model = await api("/v1/settings/model"); } catch {}
  const signIn = check("Sign-in");
  const alerts = check("Alerts");
  render(
    h("h1", {}, "Setup"),
    h("p", { class: "muted" }, "What Helm already set is skipped or shown locked. Each step is marked done when it works."),
    h("div", { class: "panel step" },
      h("h3", {}, `1. Sign-in ${done("Sign-in") ? "· done" : ""}`),
      h("p", {}, signIn.summary),
      done("Sign-in") ? null : h("p", {}, h("a", { href: "#/accounts" }, "Create accounts"), ", or set the chart's oidc values for your identity provider.")),
    check("Investigator").state === "off"
      ? h("div", { class: "panel step" }, h("h3", {}, "2. The model · not used"), h("p", {}, check("Investigator").summary))
      : model ? modelStep(model, done("Model")) : null,
    h("div", { class: "panel step" },
      h("h3", {}, `3. Alerts ${done("Alerts") ? "· done" : ""}`),
      h("p", {}, alerts.summary),
      done("Alerts") ? null : h("p", { class: "muted" }, "Already connected Alertmanager? This step shows done when the first alert arrives."),
      done("Alerts") ? null : alertmanagerSetup()),
    h("div", { class: "panel step" },
      h("h3", {}, "4. Check"),
      h("p", {}, h("a", { href: "#/status" }, "Status"), " checks every part, and says how to fix what doesn't work.")));
}

// status says whether each part of the install works, and how to fix what doesn't.
async function status() {
  const { checks } = await api("/v1/status");
  const broken = checks.filter((c) => c.state === "problem").length;
  render(
    h("h1", {}, "Status"),
    h("p", { class: "muted" }, broken ? `${broken === 1 ? "One part isn't" : `${broken} parts aren't`} working. Each says how to fix it.` : "Each part of Themis, checked just now."),
    h("table", {}, h("tbody", {}, checks.map((c) => {
      const [word, tone] = CHECK_STATE[c.state] || [c.state, "neutral"];
      return h("tr", {},
        h("td", {}, h("strong", {}, c.name)),
        h("td", {}, h("span", { class: `state ${tone}` }, word)),
        h("td", {}, c.summary,
          c.at ? h("div", { class: "muted" }, `Last heard from ${ago(c.at)}.`) : null,
          c.fix && c.state !== "ok" ? h("div", { class: "fix" }, c.fix) : null,
          c.name === "Alerts" && c.state !== "ok" && can("admin") ? h("button", { onclick: (e) => {
            e.target.replaceWith(alertmanagerSetup());
          } }, "Show how to connect Alertmanager") : null));
    }))),
    h("div", { class: "actions" }, h("button", { onclick: route }, "Check again")));
}

// password lets someone signed in with an account change its password.
function changePassword() {
  const current = h("input", { type: "password", placeholder: "current password", autocomplete: "current-password" });
  const next = h("input", { type: "password", placeholder: "new password, 12 characters or more", autocomplete: "new-password" });
  const error = h("div", { class: "error" });
  const submit = async (e) => {
    e.preventDefault();
    error.textContent = "";
    try {
      await api("/v1/me/password", { method: "POST", body: JSON.stringify({ current: current.value, new: next.value }) });
      // A new password ends every session, this one too.
      me = null;
      route();
    } catch (err) {
      error.textContent = err.message;
    }
  };
  render(h("div", { class: "panel" },
    h("h1", {}, "Change your password"),
    h("p", { class: "muted" }, "You'll be signed out everywhere, then sign in again with the new one."),
    h("form", { onsubmit: submit }, h("div", { class: "actions" }, current, next, h("button", { class: "primary", type: "submit" }, "Change password"))),
    error));
}

function noAccess() {
  render(h("div", { class: "panel" },
    h("h1", {}, "No access yet"),
    h("p", {}, `You're signed in as ${me.username}, but you haven't been given access to Themis yet.`),
    h("p", { class: "muted" }, "Ask a Themis admin to add you or one of your groups.")));
}

// profiles names every profile an incident matched, its primary first.
function profiles(inc) {
  return inc.profiles.join(", ");
}

// matched says which profiles an incident matched: with several, it got
// what each brings.
function matched(inc) {
  return inc.profiles.length === 1 ? `profile ${inc.profile}` : `profiles ${profiles(inc)}, combined`;
}

async function list() {
  const incidents = await api("/v1/incidents");
  const rows = incidents.map((i) => h("tr", { class: "link", onclick: () => { location.hash = `#/incidents/${i.id}`; } },
    h("td", {}, state(i.state)),
    h("td", {}, target(i.target)),
    h("td", {}, profiles(i)),
    h("td", {}, i.occurrenceCount),
    h("td", {}, time(i.createdAt)),
    h("td", { class: "mono muted" }, short(i.id))));
  render(
    h("h1", {}, "Incidents"),
    h("div", { class: "panel" }, incidents.length === 0
      ? h("p", { class: "muted" }, "No incidents yet. One opens when Alertmanager sends Themis an alert about a workload it watches. ",
        h("a", { href: "#/status" }, "Status"), " shows whether alerts arrive.")
      : h("table", {},
        h("thead", {}, h("tr", {}, ["State", "Workload", "Profiles", "Alerts", "Opened", "ID"].map((c) => h("th", {}, c)))),
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

// sourceRows say where a fix came from, as fully as Themis knows: who sent it,
// what identity Themis checked, and what the sender says about itself. An
// outside tool's name, model and prompt are its own claims; the identity is
// what Themis verified.
function sourceRows(p) {
  const prov = p.Provenance || {};
  const src = prov.source || "";
  const rows = [];
  const row = (k, v) => rows.push(h("dt", {}, k), h("dd", {}, v));
  let claimed = false;
  if (p.LeaseID) {
    row("From", "Themis's own investigator");
    row("Identity", "Checked: it held this incident's investigation");
  } else if (src === "person:setup") {
    row("From", "A person");
    row("Identity", "Someone with kubectl access, through port-forward before sign-in was set up. Kubernetes checked them; Themis doesn't know who");
  } else if (src === "admin-token") {
    row("From", "A person");
    row("Identity", "The shared admin token, which proves the token, not which person");
  } else if (src.startsWith("person:")) {
    row("From", "A person");
    row("Identity", `Signed in as ${src.slice("person:".length)}`);
  } else if (src.startsWith("serviceaccount:")) {
    const [ns, name] = src.slice("serviceaccount:".length).split("/");
    claimed = true;
    row("From", prov.claimedSource ? `An outside tool that calls itself "${prov.claimedSource}"` : "An outside tool");
    row("Identity", p.Authenticated
      ? `Checked: Kubernetes service account ${name} in namespace ${ns}`
      : `Not checked: ${src}`);
  } else {
    row("From", src || "Unknown");
    row("Identity", p.Authenticated ? "Checked" : "Not checked");
  }
  const model = [prov.model, prov.promptVersion && `prompt ${prov.promptVersion}`,
    prov.route && prov.route !== "default" && `route ${prov.route}`].filter(Boolean).join(" · ");
  row(claimed ? "Model, as it reports" : "Model", model || "Not reported");
  row("Sent", time(p.CreatedAt));
  return rows;
}

// citedChecks names the checks a suggestion cites, each with what Themis
// found: a check's name alone reads as a claim, true or not. d is the
// incident's detail, with its checks' answers and descriptions.
function citedChecks(names, d) {
  return (names || []).map((c) => {
    const found = (d.Predicates || []).find((x) => x.Name === c);
    return `${label(d.Checks || {}, c)}: ${found ? label(ANSWER_LABEL, found.Value) : "not checked"}`;
  }).join("; ");
}

function proposalPanel(p, d) {
  return h("div", { class: "panel" },
    h("h3", {}, "What was suggested"),
    h("dl", { class: "source" }, sourceRows(p)),
    h("dl", {},
      h("dt", {}, "Status"), h("dd", {}, h("span", { class: `state ${p.Status === "rejected" ? "bad" : "neutral"}` }, label(SUGGESTION_LABEL, p.Status)), p.RejectionReason ? h("div", {}, label(REJECTION_LABEL, p.RejectionReason)) : null),
      h("dt", {}, "Fix"), h("dd", {}, fix(p.Capability, p.Params)),
      h("dt", {}, "Workload"), h("dd", {}, target(p.Target), h("div", { class: "mono muted" }, p.Target.UID)),
      h("dt", {}, "Expected effect"), h("dd", {}, p.ExpectedEffect || "—"),
      h("dt", {}, "Why"), h("dd", {}, p.Rationale || "—"),
      h("dt", {}, "Evidence cited"), h("dd", { class: "mono" }, (p.CitedEvidenceIDs || []).map(short).join(", ") || "—"),
      h("dt", {}, "Checks cited"), h("dd", {}, citedChecks(p.CitedPredicates, d) || "—"),
      p.Message ? [h("dt", {}, "Note"), h("dd", {}, p.Message)] : null,
      // What the suggester says it looked at: for people, nothing decides on it.
      p.Notes ? [h("dt", {}, "What it looked at"), h("dd", {}, h("details", {},
        h("summary", { class: "muted" }, "As it reports, unchecked"), h("pre", {}, p.Notes)))] : null));
}

// observed says what the fix check last saw, in the fix's own words. Checks
// from before fixes described themselves counted pods.
function observed(o) {
  if (!o) return null;
  if (o.Facts) return o.Facts;
  return `${o.ReadyReplicas} of ${o.DesiredReplicas} pods ready · ${o.RestartDelta} restarts · ${o.NewOOMKills} out-of-memory kills`;
}

// suspicious warns that evidence holds text that looks written for the
// investigator, such as instructions or forged tool output, with what to do.
function suspicious(warning, advice) {
  return warning ? h("div", { class: "callout" }, h("p", { class: "lead" }, `${warning} ${advice}`)) : null;
}

function actionPanel(a, reload, warning) {
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
    suspicious(warning, "Check the fix against the cluster before approving."),
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
    a.State !== "awaiting_approval" ? null
      : can("approver") ? h("div", { class: "actions" }, approve, reason, deny)
        : h("p", { class: "muted" }, "Only approvers can approve or deny this change."),
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
    h("strong", {}, label(ROOT_CAUSE_LABEL, x.rootCauseClass || "unclassified")), " ",
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
  awaiting_suggestion: "No investigator is installed, so Themis is waiting for a person or a tool such as HolmesGPT to suggest a fix. It checks any suggestion against the cluster and your policy, like its own.",
  diagnosed: "The investigation suggested a fix. Themis is checking it against the cluster and your policy.",
  proposed: "Themis is checking the suggested fix against your policy.",
  awaiting_approval: "A fix needs your approval. Check what it changes, then approve or deny it.",
  authorized: "Approved. Themis is about to apply the fix.",
  executing: "Themis is applying the fix.",
  verifying: "The fix was applied. Themis is watching whether the workload recovers.",
  restoring: "The fix didn't help, so Themis is putting the workload back as it was before it acted.",
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
function summary(inc, inv, actions, proposals, headline, missed) {
  const facts = [];
  const accepted = proposals.find((p) => p.Status === "accepted");
  const action = actions[actions.length - 1];
  if (inv && inv.Summary) facts.push(h("li", {}, h("strong", {}, "Diagnosis: "), inv.Summary));
  // Themis never changes a workload GitOps manages: its fix is a
  // recommendation, to apply in Git.
  const gitOps = action && action.PolicyID === "gitops-owns-the-target";
  if (action) {
    const changed = (action.Changes || []).map((c) => c.field).join(", ") || "nothing visible";
    facts.push(h("li", {}, h("strong", {}, gitOps ? "Recommended fix: " : "Proposed fix: "),
      `${fix(action.Capability, action.Params)}, which changes ${changed} on ${action.BlastRadius.replicas} pods.`));
    facts.push(h("li", {}, h("strong", {}, "Policy: "), action.DecisionReason));
  } else if (accepted) {
    facts.push(h("li", {}, h("strong", {}, "Proposed fix: "), accepted.Rationale));
  }
  const rejected = proposals.filter((p) => p.Status === "rejected").length;
  if (rejected) facts.push(h("li", {}, h("strong", {}, "Refused suggestions: "), `${rejected}, listed below.`));
  const lead = inc.supersededBy
    ? ["The workload was changed outside Themis and still fails, so a new incident investigates it as it is now: ",
      h("a", { href: `#/incidents/${inc.supersededBy}` }, "open the new incident"), "."]
    : headline || (gitOps ? "Themis recommends this fix but won't apply it: GitOps manages, or may manage, this workload." : null)
      || NEXT[inc.state] || inc.state;
  // A headline promises the list of what Themis couldn't see: it follows.
  const seen = headline && missed && missed.length ? h("ul", {}, missed.map((m) => h("li", {}, m))) : null;
  return h("div", { class: `callout ${inc.state === "awaiting_approval" ? "" : "quiet"}` },
    h("p", { class: "lead" }, lead), seen, facts.length ? h("ul", {}, facts) : null);
}

// missing lists what Themis couldn't see, why, and how to let it see next time.
function missing(messages) {
  if (!messages || messages.length === 0) return null;
  return h("div", { class: "panel" },
    h("h3", {}, "What Themis couldn't see"),
    h("ul", {}, messages.map((m) => h("li", {}, m))));
}

function rejectedAttempts(proposals, d) {
  if (proposals.length === 0) return null;
  return h("details", { class: "panel rejected" },
    h("summary", {}, `Suggestions Themis refused (${proposals.length})`),
    proposals.map((p) => h("div", { class: "grid" }, proposalPanel(p, d))));
}

// SUGGESTABLE are the states in which a person may suggest a fix.
const SUGGESTABLE = ["investigating", "awaiting_suggestion", "inconclusive"];

// suggestForm lets a person suggest a rollback. The core checks it exactly as
// it checks the investigator's: against the cluster, then your policy.
function suggestForm(inc, evidence, reload) {
  const t = inc.target;
  const history = evidence.find((e) => e.Template === "rollout_history" && e.Data);
  const earlier = history ? (history.Data.revisions || []).filter((r) => !r.current)
    .sort((a, b) => Number(b.revision) - Number(a.revision)) : [];
  const revision = earlier.length
    ? h("select", { "aria-label": "Revision to roll back to" }, earlier.map((r) => h("option", { value: r.revision },
      `Revision ${r.revision} · ${(r.containers || []).map((c) => c.image).join(", ")}${r.createdAt ? ` · from ${time(r.createdAt)}` : ""}`)))
    : h("input", { type: "number", min: "1", placeholder: "revision", "aria-label": "Revision to roll back to" });
  const why = h("textarea", { rows: "3", placeholder: "Why this fixes it. The approver reads this." });
  const effect = h("input", { type: "text", placeholder: "What should happen, for example: pods stop crash-looping" });
  const error = h("div", { class: "error" });
  const submit = async (e) => {
    e.preventDefault();
    error.textContent = "";
    if (!why.value.trim()) {
      error.textContent = "Say why this fixes it: whoever approves it reads that.";
      return;
    }
    const body = {
      clientProposalId: `ui-${Date.now()}`,
      target: { group: t.Group || t.group || "apps", kind: t.Kind || t.kind, namespace: t.Namespace || t.namespace, name: t.Name || t.name, uid: t.UID || t.uid },
      capabilityName: "deployment.rollback-to-revision",
      params: { revision: Number(revision.value) },
      rationale: why.value.trim(),
      expectedEffect: effect.value.trim(),
      citedEvidenceIds: evidence.map((x) => x.ID),
    };
    try {
      const r = await api(`/v1/incidents/${encodeURIComponent(inc.id)}/proposals`, { method: "POST", body: JSON.stringify(body) });
      // The API says REJECTED at the end of a longer status name.
      if (r && String(r.status).endsWith("_REJECTED")) {
        error.textContent = r.message || "Themis didn't accept this fix.";
        return;
      }
      reload();
    } catch (err) {
      error.textContent = err.message;
    }
  };
  const form = h("form", { onsubmit: submit },
    h("dl", {},
      h("dt", {}, "Fix"), h("dd", {}, "Roll back to ", revision),
      h("dt", {}, "Why"), h("dd", {}, why),
      h("dt", {}, "Expected effect"), h("dd", {}, effect)),
    h("div", { class: "actions" }, h("button", { class: "primary", type: "submit" }, "Suggest this fix")),
    h("p", { class: "muted" }, "Themis checks it against the cluster and your policy, as it does its own. It cites the evidence above."),
    error);
  const waiting = inc.state !== "investigating";
  return h("details", { class: "panel suggest", open: waiting },
    h("summary", {}, h("strong", {}, waiting ? "Suggest a fix" : "Suggest a fix while Themis investigates")), form);
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
    h("p", { class: "muted mono" }, `${inc.id} · ${matched(inc)} · uid ${inc.target.UID} · opened ${time(inc.createdAt)}`),

    summary(inc, inv, actions, proposals, d.Headline, d.Missing),
    d.Headline ? null : missing(d.Missing),

    actions.map((a) => {
      const p = proposals.find((x) => x.ID === a.ProposalID);
      return h("div", { class: "grid" }, p ? proposalPanel(p, d) : h("div"), actionPanel(a, reload, d.EvidenceWarning));
    }),
    rejectedAttempts(proposals.filter((p) => !actions.some((a) => a.ProposalID === p.ID)), d),
    SUGGESTABLE.includes(inc.state) && can("approver") ? suggestForm(inc, d.Evidence || [], reload) : null,

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
        h("td", {}, label(d.Checks || {}, p.Name)),
        h("td", { class: `truth-${p.Value}` }, label(ANSWER_LABEL, p.Value)),
        h("td", {}, p.Reason),
        h("td", { class: "mono muted" }, (p.EvidenceIDs || []).map(short).join(", "))))))),

    h("h2", {}, "Timeline"),
    h("div", { class: "panel" }, h("ol", { class: "timeline" }, (d.Transitions || []).map((t) => h("li", {},
      h("strong", {}, label(STATE_LABEL, t.To)), " ", h("span", { class: "muted" }, `${time(t.At)} · ${label(ACTOR_LABEL, t.Actor)}`),
      h("div", {}, t.Reason))))),

    h("h2", {}, "Evidence"),
    suspicious(d.EvidenceWarning, "Check what it claims against the cluster."),
    h("div", { class: "panel" }, (d.Evidence || []).map((e) => h("details", {},
      h("summary", {}, h("span", { class: "mono" }, short(e.ID)), ` ${label(EVIDENCE_LABEL, e.Template)} · ${time(e.CollectedAt)}`,
        e.Classification === "sensitive" ? " · may contain sensitive data" : "",
        e.Truncated ? " · shortened" : "", e.Redactions ? ` · ${e.Redactions} secrets hidden` : "",
        e.Suspicious ? " · looks like instructions" : ""),
      e.Suspicious ? h("ul", {}, e.Suspicious.map((s) => h("li", {},
        `Holds ${s.reason}, at ${s.at || "the top"}`, s.excerpt ? [": ", h("span", { class: "mono" }, s.excerpt)] : null))) : null,
      e.Data === undefined
        ? h("p", { class: "muted" }, "Only responders can read this evidence.")
        : h("pre", {}, JSON.stringify(e.Data, null, 2))))));
  // Refreshing would discard what an approver or suggester is typing.
  const typing = actions.some((a) => a.State === "awaiting_approval") || (SUGGESTABLE.includes(inc.state) && can("approver"));
  if (!typing) timer = setTimeout(reload, POLL_MS * 2);
}

function showError(e) {
  if (e.message === "unauthorized") return;
  render(h("div", { class: "panel error" }, e.message));
}

// whoami asks who's signed in, by the admin token or a session cookie.
async function whoami() {
  const resp = await fetch("/v1/me", { headers: headers() });
  if (resp.status === 401) {
    try { sessionStorage.removeItem(TOKEN_KEY); } catch {}
    return null;
  }
  if (!resp.ok) throw new Error((await resp.text()).trim() || resp.statusText);
  return resp.json();
}

async function route() {
  clearTimeout(timer);
  try {
    if (!me) me = await whoami();
  } catch (e) {
    return showError(e);
  }
  const who = document.getElementById("whoami");
  who.textContent = !me ? ""
    : me.setup ? "Admin, through kubectl port-forward"
    : me.token ? "Signed in with the emergency token"
    : `${me.username}${me.role ? ` · ${me.role}` : ""}`;
  document.getElementById("setup").hidden = !(me && me.setup);
  // Coming in through port-forward isn't a sign-in, so there's nothing to sign out of.
  document.getElementById("logout").hidden = !me || me.setup;
  if (!me) return login();
  if (!me.role) return noAccess();
  document.getElementById("nav-accounts").hidden = !can("admin");
  document.getElementById("nav-setup").hidden = !can("admin");
  document.getElementById("nav-password").hidden = !me.account;
  const m = location.hash.match(/^#\/incidents\/([\w-]+)$/);
  const run = m ? () => detail(m[1])
    : location.hash === "#/accounts" && can("admin") ? accounts
    : location.hash === "#/status" ? status
    : (location.hash === "#/setup" || (location.hash === "" && me.setup)) && can("admin") ? setup
    : location.hash === "#/password" && me.account ? async () => changePassword()
    : async () => { await list(); timer = setTimeout(route, POLL_MS); };
  run().catch(showError);
}

document.getElementById("logout").addEventListener("click", async () => {
  if (me && !me.token) {
    try { await fetch("/auth/logout", { method: "POST", headers: headers() }); } catch {}
  }
  try { sessionStorage.removeItem(TOKEN_KEY); } catch {}
  me = null;
  route();
});
window.addEventListener("hashchange", route);
route();
