// Themis UI. Every value from the API is untrusted, evidence most of all:
// elements are built with textContent only, never innerHTML.
//
// The words follow docs/writing.md. This file holds no less-than sign: the
// test that keeps internal words off the page reads markup-like spans.
"use strict";

const TOKEN_KEY = "themis-token";
const POLL_MS = 3000;
const main = document.getElementById("main");
let timer = null;
// me is who's signed in, from /v1/me: a person, whoever holds the break-glass
// token, or, until setup finishes, whoever opened Themis through kubectl
// port-forward.
let me = null;
// flash is what the last action did, said once on the page it came from.
let flash = null;

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false || c === "") continue;
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
  if (token()) out.Authorization = "Bearer " + token();
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

// render replaces the page, keeping what the reader opened, closed or had in
// focus when the same page refreshes.
let rendered = null;
function render(...nodes) {
  const same = rendered === location.hash;
  const folds = new Map();
  let focus = null;
  if (same) {
    for (const d of main.querySelectorAll("details[data-key]")) folds.set(d.dataset.key, d.open);
    const active = document.activeElement;
    if (active && main.contains(active) && active.dataset.key) focus = active.dataset.key;
  }
  main.replaceChildren(...nodes.flat(Infinity).filter((n) => n !== null && n !== undefined && n !== false));
  rendered = location.hash;
  for (const d of main.querySelectorAll("details[data-key]")) {
    if (folds.has(d.dataset.key)) d.open = folds.get(d.dataset.key);
  }
  if (focus) {
    const el = main.querySelector("[data-key=" + JSON.stringify(focus) + "]");
    if (el) el.focus({ preventScroll: true });
  }
}

// Words -------------------------------------------------------------------

const sentence = (s) => {
  const t = String(s || "").trim();
  if (!t) return "";
  const first = t.charAt(0).toUpperCase() + t.slice(1);
  return /[.?!:]$/.test(first) ? first : first + ".";
};
const cap = (s) => String(s || "").charAt(0).toUpperCase() + String(s || "").slice(1);
const humanize = (v) => sentence(String(v || "").replaceAll("_", " ")).replace(/\.$/, "");
const label = (labels, v) => labels[v] || humanize(v);
const plural = (n, one, many) => n + " " + (n === 1 ? one : many);

const DAY = new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short" });
const CLOCK = new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
const sameDay = (a, b) => a.toDateString() === b.toDateString();
// when is a moment in full, such as 13 Nov, 11:05.
function when(t) {
  if (!t) return "—";
  const d = new Date(t);
  const year = d.getFullYear() === new Date().getFullYear() ? "" : " " + d.getFullYear();
  return DAY.format(d) + year + ", " + CLOCK.format(d);
}
// moment is a time for a list: the clock today, the day before that.
function moment(t) {
  const d = new Date(t);
  return h("time", { datetime: d.toISOString(), title: when(t) }, sameDay(d, new Date()) ? CLOCK.format(d) : DAY.format(d));
}
// ago says how long ago t was.
function ago(t) {
  const s = Math.max(0, Math.round((Date.now() - new Date(t)) / 1000));
  if (90 > s) return plural(s, "second", "seconds") + " ago";
  if (90 * 60 > s) return plural(Math.round(s / 60), "minute", "minutes") + " ago";
  if (36 * 3600 > s) return plural(Math.round(s / 3600), "hour", "hours") + " ago";
  return "on " + when(t);
}

const workload = (t) => ({
  kind: t.Kind || t.kind, namespace: t.Namespace || t.namespace, name: t.Name || t.name,
  uid: t.UID || t.uid, group: t.Group || t.group,
});

// Roles, least trusted first: each can do what the ones before it can.
const ROLES = ["viewer", "responder", "approver", "admin"];
const ROLE_LABEL = { viewer: "Viewer", responder: "Responder", approver: "Approver", admin: "Admin" };
const can = (role) => me && ROLES.indexOf(me.role) >= ROLES.indexOf(role);

// A fix, in the words of each thing a page says about it.
const ROLLBACK = "deployment.rollback-to-revision";
function fixWords(name, params, target) {
  const p = params || {};
  if (p.restores) {
    return { noun: "restore", short: "Restore the previous state", question: "Restore the previous state of " + target + "?",
      doing: "Restoring the previous state of " + target + ".", done: "Restored the previous state of " + target + "." };
  }
  if (name === ROLLBACK) {
    return { noun: "rollback", short: "Roll back to revision " + p.revision, question: "Roll " + target + " back to revision " + p.revision + "?",
      doing: "Rolling " + target + " back to revision " + p.revision + ".", done: "Rolled " + target + " back to revision " + p.revision + "." };
  }
  if (name === "deployment.set-probes") {
    return { noun: "probe change", short: "Change the probe timing", question: "Change the probe timing of " + target + "?",
      doing: "Changing the probe timing of " + target + ".", done: "Changed the probe timing of " + target + "." };
  }
  if (name === "deployment.set-resources") {
    return { noun: "memory and CPU change", short: "Change the memory and CPU settings", question: "Change the memory and CPU settings of " + target + "?",
      doing: "Changing the memory and CPU settings of " + target + ".", done: "Changed the memory and CPU settings of " + target + "." };
  }
  return { noun: "change", short: "Apply a change", question: "Apply this change to " + target + "?",
    doing: "Applying a change to " + target + ".", done: "Changed " + target + "." };
}

// States. One name per state, everywhere: docs/writing.md lists them.
const TERMINAL = ["resolved", "contained_escalated", "escalated", "closed_no_action", "self_resolved", "rejected", "canceled"];
const REFERRED = ["escalated", "contained_escalated"];
const WORKING = ["received", "investigating", "diagnosed", "proposed"];
// SUGGESTABLE are the states in which a person may suggest a fix.
const SUGGESTABLE = ["investigating", "awaiting_suggestion", "inconclusive"];

// stateOf names an incident's state, with its tone. how says what the state
// alone can't: whether the fix was undone, or only recommended.
function stateOf(state, how = {}) {
  if (state === "awaiting_approval") return [can("approver") ? "Waiting for your approval" : "Waiting for approval", "wait"];
  if (state === "awaiting_suggestion" || state === "inconclusive") return ["Waiting for a suggested fix", "wait"];
  if (WORKING.includes(state)) return ["Investigating", ""];
  if (state === "authorized" || state === "executing") return ["Applying", ""];
  if (state === "verifying") return ["Verifying", ""];
  if (state === "restoring") return ["Restoring", "bad"];
  if (state === "resolved") return ["Fixed", "good"];
  if (state === "escalated" && how.restored) return ["Restored", "bad"];
  if (REFERRED.includes(state)) return ["Referred to a person", "bad"];
  if (state === "recommended" || (state === "closed_no_action" && how.recommended)) return ["Recommended", "done"];
  if (state === "closed_no_action" || state === "rejected") return ["Closed, no change", "done"];
  if (state === "self_resolved") return ["Recovered on its own", "done"];
  if (state === "canceled") return ["Canceled", "done"];
  return [humanize(state), ""];
}

// causeText is a root cause in its pack's words, as a sentence.
function causeText(name, description) {
  if (!name || name === "other") return null;
  if (name === "insufficient_evidence") return "The evidence doesn't show the cause.";
  return sentence(description || name.replaceAll("_", " "));
}

const ANSWER = { true: "Yes", false: "No", unknown: "Unknown" };
// AGAINST_WHEN_TRUE are checks whose finding, when true, weighs against a fix.
const AGAINST_WHEN_TRUE = ["gitops_managed", "config_changed_before_failure", "oom_without_recent_rollout", "evidence_has_instructions"];
const EVIDENCE_LABEL = {
  rollout_history: "Rollout history", pod_states: "Pod states", target_events: "Events", container_logs_tail: "Container logs",
  referenced_config: "Referenced config", container_memory: "Container memory", container_cpu: "Container CPU",
};
const ACTOR_LABEL = {
  intake: "Alert intake", investigation: "Themis", investigator: "The investigator", governance: "Policy",
  approval: "Approval timer", execution: "Themis", executor: "Executor", verify: "Verification", reconcile: "Cluster watch",
};
const DECISION_LABEL = { allow: "Allowed by policy", require_approval: "A person must approve", deny: "Refused by policy" };
const ACTION_LABEL = {
  awaiting_approval: "Waiting for approval", authorized: "Approved", executing: "Applying", succeeded: "Applied",
  failed: "Not applied: it failed", ambiguous: "Unknown whether it was applied", stale: "Not applied: the workload changed first",
  denied: "Denied", expired: "Approval expired", canceled: "Canceled",
};
const OUTCOME_LABEL = {
  resolved: "It worked", contained: "It helped, not fully", unchanged: "It didn't help",
  worsened: "It made things worse", inconclusive: "Undetermined",
};
const INVESTIGATION_LABEL = { pending: "Waiting to start", leased: "In progress", completed: "Finished", failed: "Failed" };
const CAUSE_STATUS = {
  HYPOTHESIS_STATUS_SUPPORTED: "Supported by the evidence", HYPOTHESIS_STATUS_REFUTED: "Ruled out", HYPOTHESIS_STATUS_OPEN: "Open",
};
const SUGGESTION_LABEL = { accepted: "Accepted", rejected: "Refused", superseded: "Replaced", deferred: "Waiting: another fix is in progress" };
const REJECTION_LABEL = {
  REJECTION_REASON_TARGET_MISMATCH: "Refused: it names a different workload than the incident's.",
  REJECTION_REASON_CAPABILITY_NOT_ALLOWED: "Refused: this kind of fix isn't allowed for this incident.",
  REJECTION_REASON_INVALID_PARAMS: "Refused: its values are invalid.",
  REJECTION_REASON_UNKNOWN_EVIDENCE: "Refused: it cites evidence Themis didn't collect.",
  REJECTION_REASON_UNKNOWN_PREDICATE: "Refused: it cites a check that doesn't exist.",
  REJECTION_REASON_INCIDENT_CLOSED: "Refused: the incident no longer took suggestions.",
};

// Sign-in -----------------------------------------------------------------

function field(text, input, hint) {
  return h("label", { class: "field" }, text, input, hint ? h("span", { class: "hint" }, hint) : null);
}

// passwordForm signs in with a Themis account.
function passwordForm(primary) {
  const username = h("input", { type: "text", autocomplete: "username", autocapitalize: "none", required: true });
  const password = h("input", { type: "password", autocomplete: "current-password", required: true });
  const error = h("div", { class: "error", role: "alert" });
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
  const form = h("form", { class: "form", onsubmit: submit },
    field("Username", username), field("Password", password),
    h("div", {}, h("button", { class: "btn" + (primary ? " primary" : ""), type: "submit" }, "Sign in")),
    error);
  return { form, focus: () => username.focus() };
}

async function login() {
  let methods = { oidc: false, password: false };
  try { methods = await (await fetch("/v1/sign-in")).json(); } catch {}
  const input = h("input", { type: "password", autocomplete: "off", required: true });
  const submit = (e) => {
    e.preventDefault();
    try { sessionStorage.setItem(TOKEN_KEY, input.value.trim()); } catch {}
    route();
  };
  const breakGlass = h("details", { class: "minor" }, h("summary", {}, "Use the break-glass token"),
    h("form", { class: "form", onsubmit: submit },
      h("p", { class: "quiet small" }, "For scripts, and for when sign-in fails. Kept for this browser tab only."),
      field("Break-glass token", input),
      h("div", {}, h("button", { class: "btn", type: "submit" }, "Continue"))));
  if (!methods.oidc && !methods.password) {
    render(h("div", { class: "plate centre" },
      h("h1", {}, "Sign-in is not set up"),
      h("p", {}, "Open Themis through kubectl port-forward to set it up. Admin access lasts until someone signs in with an account."),
      h("pre", {}, "kubectl -n themis port-forward deploy/themis-core 8090:8083"),
      h("p", { class: "quiet small" }, "Then open http://localhost:8090. If Themis was installed under another name or namespace, use those."),
      breakGlass));
    return;
  }
  const pw = methods.password ? passwordForm(!methods.oidc) : null;
  render(h("div", { class: "plate centre" },
    h("h1", {}, "Sign in"),
    methods.oidc ? h("p", {}, h("a", { class: "btn primary", href: "/auth/login" }, "Sign in with your identity provider")) : null,
    pw ? [methods.oidc ? h("p", { class: "or" }, "Or with a Themis account") : null, pw.form] : null,
    breakGlass));
  if (pw && !methods.oidc) pw.focus();
}

function noAccess() {
  render(h("div", { class: "plate centre" },
    h("h1", {}, "No access"),
    h("p", {}, "Signed in as ", h("b", {}, me.username), ". This account has no role in Themis."),
    h("p", { class: "quiet" }, "A Themis admin can grant access to you or one of your groups.")));
}

// password lets someone signed in with an account change its password.
function changePassword() {
  const current = h("input", { type: "password", autocomplete: "current-password", required: true });
  const next = h("input", { type: "password", autocomplete: "new-password", minlength: "12", required: true });
  const error = h("div", { class: "error", role: "alert" });
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
  render(h("div", { class: "plate centre" },
    h("h1", {}, "Change your password"),
    h("p", { class: "quiet" }, "Changing it signs you out everywhere. Sign in again with the new password."),
    h("form", { class: "form", onsubmit: submit },
      field("Current password", current), field("New password", next, "12 characters or more."),
      h("div", {}, h("button", { class: "btn primary", type: "submit" }, "Change password")),
      error)));
}

// Accounts ----------------------------------------------------------------

// accounts lets admins manage who signs in with a password. During setup,
// it's where the first admin account is made.
async function accounts() {
  const list = await api("/v1/accounts");
  const error = h("div", { class: "error", role: "alert" });
  const act = async (path, options, done) => {
    error.textContent = "";
    try {
      await api(path, options);
      flash = done;
      route();
    } catch (e) {
      error.textContent = e.message;
    }
  };
  const roleSelect = (value, name) => h("select", { "aria-label": name }, ROLES.map((r) => h("option", { value: r, selected: r === value }, ROLE_LABEL[r])));
  const username = h("input", { type: "text", autocomplete: "off", autocapitalize: "none", required: true });
  const role = roleSelect(me.setup && !list.length ? "admin" : "approver", "Role");
  const password = h("input", { type: "password", autocomplete: "new-password", minlength: "12", required: true });
  const create = (e) => {
    e.preventDefault();
    const name = username.value.trim();
    act("/v1/accounts", { method: "POST", body: JSON.stringify({ username: name, role: role.value, password: password.value }) }, "Account " + name + " created.");
  };
  const hasAdmin = list.some((a) => a.role === "admin");
  const first = me.setup && !hasAdmin;
  // During setup, once an admin account exists, signing in with it finishes setup.
  const finish = me.setup && hasAdmin ? passwordForm(true) : null;
  const path = (a) => "/v1/accounts/" + encodeURIComponent(a.username);
  const rows = list.map((a) => {
    const self = me.account && me.username === a.username;
    const r = roleSelect(a.role, "Role of " + a.username);
    r.disabled = self;
    r.addEventListener("change", () => act(path(a), { method: "PATCH", body: JSON.stringify({ role: r.value }) }, a.username + " is now " + ROLE_LABEL[r.value] + "."));
    const pw = h("input", { type: "password", autocomplete: "new-password", "aria-label": "New password for " + a.username, placeholder: "New password" });
    const reset = h("button", { class: "btn small", type: "button", onclick: () => {
      if (pw.value) act(path(a), { method: "PATCH", body: JSON.stringify({ password: pw.value }) }, "Password of " + a.username + " set.");
    } }, "Set password");
    const remove = h("button", { class: "btn small deny", type: "button", disabled: self,
      onclick: () => act(path(a), { method: "DELETE" }, "Account " + a.username + " deleted.") }, "Delete");
    return h("tr", {},
      h("td", {}, h("b", {}, a.username), a.locked ? h("div", { class: "quiet small" }, "Locked: too many wrong passwords") : null),
      h("td", {}, r),
      h("td", { class: "quiet small" }, "By " + a.createdBy + ", " + when(a.createdAt)),
      h("td", {}, h("div", { class: "inline" }, pw, reset, remove)));
  });
  const said = flash;
  flash = null;
  render(
    finish ? h("div", { class: "plate" },
      h("h1", {}, "Sign in with the admin account"),
      h("p", { class: "quiet" }, "Signing in finishes setup. From then on, kubectl port-forward alone grants no admin access."),
      finish.form) : null,
    h("h1", { class: finish ? "sub" : "" }, first ? "Create the admin account" : "Accounts"),
    h("p", { class: "lede" }, first
      ? "Then sign in with it here. Signing in finishes setup. From then on, kubectl port-forward alone grants no admin access."
      : "People who sign in with a username and password. People who sign in through your identity provider get their roles from Themis's settings."),
    said ? h("p", { class: "flash", role: "status" }, said) : null,
    list.length ? h("div", { class: "plate" }, h("table", { class: "list" },
      h("thead", {}, h("tr", {}, h("th", {}, "Username"), h("th", {}, "Role"), h("th", {}, "Created"), h("th", {}, h("span", { class: "quiet" }, "Password and removal")))),
      h("tbody", {}, rows))) : null,
    h("div", { class: "plate" },
      h("h2", {}, "New account"),
      h("form", { class: "inline", onsubmit: create },
        field("Username", username), field("Role", role), field("Password, 12 characters or more", password),
        h("button", { class: "btn primary", type: "submit" }, "Create account"))),
    error);
}

// Status and setup --------------------------------------------------------

// CHECK_STATE names how each part of the install is doing, and its tone.
const CHECK_STATE = {
  ok: ["Working", "good"], warn: ["Needs attention", "wait"], problem: ["Not working", "bad"],
  waiting: ["Waiting", ""], off: ["Off", "done"],
};

// copyable is code to copy and run, with a button that copies it.
function copyable(code) {
  const button = h("button", { class: "btn small", type: "button", onclick: async () => {
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
  const ns = h("input", { type: "text", value: "monitoring", autocapitalize: "none" });
  const out = h("div", {});
  const show = async () => {
    try {
      const { steps } = await api("/v1/setup/alertmanager?namespace=" + encodeURIComponent(ns.value.trim()));
      out.replaceChildren(...steps.map((s, i) => h("div", { class: "sub" },
        h("h3", {}, (i + 1) + ". " + s.title), h("p", { class: "quiet" }, s.text), copyable(s.code))));
    } catch (e) {
      out.replaceChildren(h("div", { class: "error" }, e.message));
    }
  };
  ns.addEventListener("change", show);
  show();
  return h("div", { class: "sub" },
    h("h2", {}, "Connect Alertmanager"),
    h("div", { class: "inline" }, field("Alertmanager's namespace", ns)),
    out,
    h("p", { class: "quiet small sub" }, "Status shows alerts arriving once a crash-loop or out-of-memory alert fires."));
}

// modelSaved says the model was just saved, so Setup says so once.
let modelSaved = false;

// modelStep sets the model the install left unset. What the install set is
// shown locked: a change here would be undone by the next upgrade.
function modelStep(v) {
  const saved = modelSaved;
  modelSaved = false;
  const byInstall = (name) => v.byInstall.includes(name);
  const locked = "Set through Helm. Change it in the chart's values.";
  const url = h("input", { type: "text", value: v.baseURL, placeholder: "https://api.openai.com/v1", disabled: byInstall("model.baseURL") });
  const name = h("input", { type: "text", value: v.name, placeholder: "gpt-4.1 or qwen3:8b", disabled: byInstall("model.name") });
  const key = h("input", { type: "password", autocomplete: "off", disabled: byInstall("model.apiKey"),
    placeholder: v.apiKeySet ? "Set. Leave empty to keep it." : "If the model needs one" });
  const strict = h("input", { type: "checkbox", checked: v.strictOutput, disabled: byInstall("model.strictOutput") });
  const error = h("div", { class: "error", role: "alert" });
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
  if (["model.baseURL", "model.name"].every(byInstall)) {
    return [h("p", {}, "Set through Helm: ", h("code", {}, v.name), " at ", h("code", {}, v.baseURL), ". Change it in the chart's values.")];
  }
  return [
    saved ? h("p", { class: "flash", role: "status" }, "Model saved. The investigator uses it from its next investigation. This step shows done once the model has answered.") : null,
    h("p", { class: "quiet" }, "Any OpenAI-compatible endpoint: OpenAI, OpenRouter, Ollama, vLLM. The investigator uses it from its next investigation."),
    h("form", { class: "form", onsubmit: save },
      field("Address", url, byInstall("model.baseURL") ? locked : null),
      field("Model", name, byInstall("model.name") ? locked : null),
      field("API key", key, byInstall("model.apiKey") ? locked : "Stored encrypted. Never shown again."),
      h("label", { class: "check" }, strict,
        h("span", {}, "Strict format: the model follows Themis's reply format exactly. For OpenAI, turn it off, or install with ",
          h("code", {}, "model.schemaDialect=openai-strict"), ".", byInstall("model.strictOutput") ? " " + locked : "")),
      h("div", {}, h("button", { class: "btn primary", type: "submit" }, "Save the model")),
      error)];
}

// setup walks through what an install needs, in order, each step marked
// done when the status page says so.
async function setup() {
  const { checks } = await api("/v1/status");
  const check = (name) => checks.find((c) => c.name === name) || { state: "waiting" };
  const done = (name) => check(name).state === "ok";
  let model = null;
  try { model = await api("/v1/settings/model"); } catch {}
  const investigatorOff = check("Investigator").state === "off";
  const steps = [
    { title: "Sign-in", status: done("Sign-in") ? "Done" : "Not done", body: [
      h("p", {}, check("Sign-in").summary),
      done("Sign-in") ? null : h("p", {}, h("a", { href: "#/accounts" }, "Create accounts"), ", or set the chart's oidc values for your identity provider.")] },
    investigatorOff
      ? { title: "The model", status: "Not used", body: [h("p", {}, check("Investigator").summary)] }
      : model ? { title: "The model", status: done("Model") ? "Done" : model.complete ? "Set" : "Not done", body: modelStep(model) } : null,
    { title: "Alerts", status: done("Alerts") ? "Done" : "Not done", body: [
      h("p", {}, check("Alerts").summary),
      done("Alerts") ? null : [h("p", { class: "quiet" }, "Done when Themis receives its first alert."), alertmanagerSetup()]] },
    { title: "Check", status: "", body: [h("p", {}, h("a", { href: "#/status" }, "Status"), " checks each part and says how to fix what doesn't work.")] },
  ].filter(Boolean);
  render(
    h("h1", {}, "Setup"),
    h("p", { class: "lede" }, "Each step shows done once it works. Values set through Helm are locked here."),
    steps.map((s, i) => h("section", { class: "plate" },
      h("div", { class: "step-head" },
        h("h2", {}, h("span", { class: "num" }, String(i + 1)), s.title),
        s.status ? h("span", { class: "state " + (s.status === "Done" ? "good" : "done") }, s.status) : null),
      s.body)));
}

// status says whether each part of the install works, and how to fix what doesn't.
async function status() {
  const { checks } = await api("/v1/status");
  const broken = checks.filter((c) => c.state === "problem").length;
  render(
    h("h1", {}, "Status"),
    h("p", { class: "lede" }, broken
      ? plural(broken, "part is", "parts are") + " not working. Each says how to fix it."
      : "Every part, checked at " + CLOCK.format(new Date()) + "."),
    h("div", { class: "plate" }, h("table", { class: "list" },
      h("thead", {}, h("tr", {}, h("th", {}, "Part"), h("th", {}, "State"), h("th", {}, "Finding"))),
      h("tbody", {}, checks.map((c) => {
        const [word, tone] = CHECK_STATE[c.state] || [humanize(c.state), ""];
        return h("tr", {},
          h("td", {}, h("b", {}, c.name)),
          h("td", {}, h("span", { class: "state " + tone }, word)),
          h("td", {}, c.summary,
            c.at ? h("div", { class: "quiet small" }, "Last heard " + ago(c.at) + ".") : null,
            c.fix && c.state !== "ok" ? h("div", { class: "fix" }, c.fix) : null,
            c.name === "Alerts" && c.state !== "ok" && can("admin") ? h("button", { class: "btn small sub", type: "button", onclick: (e) => {
              e.target.replaceWith(alertmanagerSetup());
            } }, "Show how to connect Alertmanager") : null));
      })))),
    h("p", { class: "sub" }, h("button", { class: "btn", type: "button", onclick: route }, "Check again")));
}

// The queue ---------------------------------------------------------------

// queueGroup places an incident: what waits for the reader and they can act
// on, what is still open, and what is closed.
function queueGroup(i) {
  if (i.supersededBy) return "closed";
  if (i.state === "awaiting_approval") return can("approver") ? "needs" : "open";
  if (i.state === "awaiting_suggestion" || i.state === "inconclusive") return can("approver") ? "needs" : "open";
  if (REFERRED.includes(i.state)) return can("responder") ? "needs" : "open";
  return TERMINAL.includes(i.state) ? "closed" : "open";
}

function listHow(i) {
  const s = i.summary || {};
  return { restored: i.state === "escalated" && s.from === "restoring", recommended: s.from === "recommended" };
}

// rowWhat is one plain sentence of what happened and what Themis proposes.
function rowWhat(i) {
  const s = i.summary || {};
  const t = workload(i.target);
  const cause = s.cause ? causeText(s.cause.name, s.cause.description) : null;
  const fx = s.fix ? fixWords(s.fix.capability, s.fix.params, t.name) : null;
  const em = (text) => (text ? h("em", {}, text) : null);
  if (i.supersededBy) return ["Changed outside Themis and still failing. ", em("A newer incident covers it.")];
  if (i.state === "awaiting_approval" && fx) return [cause ? cause + " " : null, em(fx.short + "?")];
  if (WORKING.includes(i.state)) return "Collecting and weighing evidence.";
  if (i.state === "awaiting_suggestion" || i.state === "inconclusive") return [cause ? cause + " " : null, em("No fix proposed yet.")];
  if (i.state === "closed_no_action" && !listHow(i).recommended && cause) return [cause + " ", em(s.reason)];
  return s.reason || cause || "";
}

function row(i) {
  const t = workload(i.target);
  const [word, tone] = stateOf(i.state, listHow(i));
  return h("a", { class: "row", href: "#/incidents/" + i.id, "data-key": i.id },
    h("span", { class: "wl" }, t.name, h("small", {}, t.namespace)),
    h("span", { class: "what" }, rowWhat(i)),
    h("span", { class: "state " + tone }, word),
    h("span", { class: "t" }, moment(i.createdAt)));
}

async function list() {
  const incidents = await api("/v1/incidents");
  const groups = { needs: [], open: [], closed: [] };
  for (const i of incidents) groups[queueGroup(i)].push(i);
  const n = (k) => groups[k].length;
  const lede = !incidents.length
    ? ["No incidents on record. One opens when Alertmanager sends an alert about a workload Themis watches. ",
      h("a", { href: "#/status" }, "Status"), " shows whether alerts arrive."]
    : (n("needs") ? plural(n("needs"), "incident needs", "incidents need") + " you. " : "No incident needs you. ")
      + (n("open") ? plural(n("open"), "other is", "others are") + " open." : "");
  render(
    h("h1", {}, "Incidents"),
    h("p", { class: "lede" }, lede),
    n("needs") ? h("section", { class: "group needs" },
      h("h2", {}, "Needs you", h("span", { class: "n" }, n("needs"))), h("div", { class: "rows" }, groups.needs.map(row))) : null,
    n("open") ? h("section", { class: "group" },
      h("h2", {}, "Open", h("span", { class: "n" }, n("open"))), h("div", { class: "rows" }, groups.open.map(row))) : null,
    n("closed") ? h("details", { class: "group", "data-key": "closed" },
      h("summary", {}, "Closed", h("span", { class: "n" }, n("closed"))), h("div", { class: "rows" }, groups.closed.map(row))) : null);
}

// The incident ------------------------------------------------------------

// leadCause is the first cause the investigator didn't rule out.
function leadCause(inv) {
  return ((inv && inv.Hypotheses) || []).find((x) => !String(x.status).endsWith("_REFUTED")) || null;
}

const evidenceName = (d, id) => {
  const e = (d.Evidence || []).find((x) => x.ID === id);
  return e ? label(EVIDENCE_LABEL, e.Template) : "evidence " + String(id).slice(-8);
};
const sources = (d, ids) => [...new Set((ids || []).map((id) => evidenceName(d, id)))].join(", ");

function changesTable(changes) {
  if (!changes || changes.length === 0) return h("p", { class: "quiet" }, "No field of the pod template changes.");
  return [
    h("table", { class: "diff" },
      h("thead", {}, h("tr", {}, h("th", {}, "Container"), h("th", {}, "Field"), h("th", {}, "Now"), h("th", {}, "After"))),
      h("tbody", {}, changes.map((c) => h("tr", {},
        h("td", {}, h("code", {}, c.container)), h("td", {}, h("code", {}, c.field)),
        h("td", { class: "val from" }, c.before), h("td", { class: "val to" }, c.after))))),
    h("p", { class: "quiet small sub" }, "Values that look like secrets are hidden. For commands and arguments, only the fact that they change is shown."),
  ];
}

// suspicion warns that evidence holds text addressed to Themis, such as
// instructions or forged tool output. Viewers learn where, not what it says.
function suspicion(d, approving) {
  const items = (d.Evidence || []).filter((e) => e.Suspicious && e.Suspicious.length);
  if (!items.length) return null;
  const where = items.map((e) => label(EVIDENCE_LABEL, e.Template));
  const excerpt = items.flatMap((e) => e.Suspicious).map((s) => s.excerpt).find(Boolean);
  return h("div", { class: "alert", role: "note" },
    h("span", { class: "sign", "aria-hidden": "true" }, "!"),
    h("div", {},
      h("b", {}, where.join(" and ") + (where.length === 1 && !where[0].endsWith("s") ? " contains" : " contain") + " text addressed to Themis"),
      excerpt ? [": ", h("code", {}, excerpt)] : null, ". ",
      "It is material, not a finding; the checks do not rely on it.",
      approving ? " Check the change against the cluster before approving." : ""));
}

// result says what became of a change once decided.
function result(a) {
  const rows = [];
  const add = (k, ...v) => rows.push(h("dt", {}, k), h("dd", {}, v));
  add("Policy", label(DECISION_LABEL, a.Decision));
  if (!a.Execution) add("State", label(ACTION_LABEL, a.State));
  if (a.Execution) {
    add(label(ACTION_LABEL, a.Execution.Status), a.Execution.Message,
      a.Execution.FinishedAt ? h("div", { class: "quiet small" }, when(a.Execution.FinishedAt)) : null);
  }
  if (a.Verification) {
    const o = a.Verification.Observation;
    const facts = o ? (o.Facts || (o.ReadyReplicas + " of " + o.DesiredReplicas + " pods ready.")) : null;
    add("Did it work?", a.Verification.Outcome ? label(OUTCOME_LABEL, a.Verification.Outcome) : "Verifying",
      facts ? h("div", { class: "quiet small" }, facts) : null);
  }
  return h("dl", { class: "result" }, rows);
}

// decision is a change: exactly what it does, and the decision on it, or
// what became of it.
function decision(a, d, ctx, latest, reload) {
  const t = workload(d.Incident.target);
  const fx = fixWords(a.Capability, a.Params, t.name);
  const awaiting = a.State === "awaiting_approval";
  const gitOps = a.PolicyID === "gitops-owns-the-target";
  const replicas = a.BlastRadius && a.BlastRadius.replicas;
  const facts = [
    replicas !== undefined ? ["Affects ", h("b", {}, plural(Number(replicas), "pod", "pods")), ". "] : null,
    a.Risk ? ["Risk: ", a.Risk, ". "] : null,
    awaiting && a.ExpiresAt ? ["Approve before ", h("b", {}, when(a.ExpiresAt)), ". "] : null,
    awaiting ? "Applies exactly this change. Void if " + t.name + " changes before it is applied." : null,
  ];
  let side;
  if (awaiting && can("approver")) {
    const error = h("div", { class: "error", role: "alert" });
    const reason = h("textarea", { rows: "2" });
    // The approval carries the change shown here, never the server's current one.
    const shown = a.Digest;
    const decide = async (path, body, button, said) => {
      button.disabled = true;
      error.textContent = "";
      try {
        await api("/v1/actions/" + a.ID + "/" + path, { method: "POST", body: JSON.stringify(body) });
        flash = said;
        reload();
      } catch (e) {
        error.textContent = e.message;
        button.disabled = false;
      }
    };
    const approve = h("button", { class: "btn primary", type: "button",
      onclick: () => decide("approve", { digest: shown }, approve, cap(fx.noun) + " approved.") }, "Approve the " + fx.noun);
    const deny = h("button", { class: "btn deny", type: "button",
      onclick: () => decide("deny", { reason: reason.value }, deny, cap(fx.noun) + " denied.") }, "Deny the " + fx.noun);
    side = h("div", { class: "act" }, approve, field("Grounds for denying (optional)", reason), deny,
      h("p", { class: "fine" }, "Themis applies exactly this change, then verifies it worked."), error);
  } else if (awaiting) {
    side = h("div", { class: "act" }, h("p", { class: "quiet" }, "Only approvers can approve or deny this change."));
  } else if (gitOps) {
    side = h("div", { class: "act" }, h("h3", {}, "Next step"), h("p", {}, "Apply this change in Git."));
  } else {
    side = h("div", {}, h("h3", {}, "Decision"), result(a));
  }
  return h("section", { class: "decide", "aria-label": latest ? "The change" : "An earlier change" },
    h("div", {},
      h("h3", {}, latest ? (gitOps ? "The recommended change" : "The exact change") : cap(fx.short)),
      changesTable(a.Changes),
      h("p", { class: "facts" }, facts),
      latest ? suspicion(d, awaiting) : null),
    side);
}

// finding is one check: Themis's answer, what it means, and on what grounds.
function finding(p, d, contrary) {
  return h("li", {},
    h("span", { class: "k" + (contrary ? " no" : "") }, ANSWER[p.Value] || humanize(p.Value)),
    h("span", {}, label(d.Checks || {}, p.Name),
      h("span", { class: "src" }, [p.Reason, sources(d, p.EvidenceIDs)].filter(Boolean).join(" · "))));
}

// findings weighs Themis's checks: for and against the fix when there is
// one, each check once.
function findings(d, cited, noun) {
  const all = d.Predicates || [];
  if (!all.length) return null;
  const forFix = [], against = [], other = [];
  for (const p of all) {
    const flips = AGAINST_WHEN_TRUE.includes(p.Name);
    const supports = p.Value === (flips ? "false" : "true");
    const contrary = p.Value === (flips ? "true" : "false");
    if (p.Value === "unknown") against.push(finding(p, d, false));
    else if (cited && cited.includes(p.Name)) (supports ? forFix : against).push(finding(p, d, contrary));
    else if (contrary && flips) against.push(finding(p, d, true));
    else if (!cited && p.Value === "true") forFix.push(finding(p, d, false));
    else other.push(finding(p, d, false));
  }
  const pan = (cls, title, items, empty) => h("div", { class: "pan " + cls },
    h("h3", {}, title), items.length ? h("ul", {}, items) : h("p", { class: "empty" }, empty));
  const blind = d.Missing && d.Missing.length ? h("div", { class: "blind" },
    h("b", {}, "What Themis couldn't see"), h("ul", {}, d.Missing.map((m) => h("li", {}, m)))) : null;
  return h("section", { class: "findings", "aria-label": "Findings" },
    h("div", { class: "pans" },
      cited ? pan("for", "Supports the " + noun, forFix, "No finding supports it.") : pan("for", "Found", forFix, "No check came out true."),
      cited ? pan("against", "Against, or unknown", against, "No finding is against it.") : pan("against", "Unknown, or against", against, "Every check has an answer.")),
    cited ? h("p", { class: "weigh" }, plural(forFix.length, "finding supports", "findings support") + " the " + noun + "; "
      + against.length + (against.length === 1 ? " is" : " are") + " against or unknown.") : null,
    blind,
    other.length ? h("details", { class: "fold sub", "data-key": "other-checks" },
      h("summary", {}, "Other checks", h("span", { class: "n" }, other.length)),
      h("div", { class: "pan against" }, h("ul", {}, other))) : null);
}

// sourceRows say where a suggested fix came from, as fully as Themis knows:
// who sent it, what identity Themis verified, and what the sender says
// about itself. An outside tool's name and model are its own claims.
function sourceRows(p) {
  const prov = p.Provenance || {};
  const src = prov.source || "";
  const rows = [];
  const row = (k, v) => rows.push(h("dt", {}, k), h("dd", {}, v));
  let claimed = false;
  if (p.LeaseID) {
    row("From", "The investigator");
    row("Identity", "Verified: it held this incident's investigation.");
  } else if (src === "person:setup") {
    row("From", "A person");
    row("Identity", "Someone with kubectl access, through port-forward before sign-in was set up. Kubernetes verified them; Themis doesn't know who.");
  } else if (src === "admin-token") {
    row("From", "A person");
    row("Identity", "The break-glass token. It proves the token, not the person.");
  } else if (src.startsWith("person:")) {
    row("From", "A person");
    row("Identity", "Signed in as " + src.slice("person:".length) + ".");
  } else if (src.startsWith("serviceaccount:")) {
    const [ns, name] = src.slice("serviceaccount:".length).split("/");
    claimed = true;
    row("From", prov.claimedSource ? "An outside tool that calls itself " + JSON.stringify(prov.claimedSource) : "An outside tool");
    row("Identity", p.Authenticated ? "Verified: Kubernetes service account " + name + " in namespace " + ns + "." : "Not verified: " + src);
  } else {
    row("From", src || "Unknown");
    row("Identity", p.Authenticated ? "Verified" : "Not verified");
  }
  const model = [prov.model, prov.promptVersion && "prompt " + prov.promptVersion,
    prov.route && prov.route !== "default" && "route " + prov.route].filter(Boolean).join(" · ");
  row(claimed ? "Model, as it reports" : "Model", model || "Not reported");
  row("Sent", when(p.CreatedAt));
  return rows;
}

// suggestionText is what a suggester wrote: Themis decides nothing on it.
function suggestionText(p, d) {
  const cited = (p.CitedPredicates || []).map((c) => {
    const found = (d.Predicates || []).find((x) => x.Name === c);
    return label(d.Checks || {}, c) + ": " + (found ? (ANSWER[found.Value] || found.Value).toLowerCase() : "not checked");
  });
  return [
    p.Rationale ? [h("h3", { class: "sub" }, "Grounds it gave"), h("p", {}, p.Rationale)] : null,
    p.ExpectedEffect ? [h("h3", {}, "Expected effect"), h("p", {}, p.ExpectedEffect)] : null,
    p.Message ? [h("h3", {}, "Message"), h("p", {}, p.Message)] : null,
    cited.length ? [h("h3", {}, "Checks it cites, with Themis's findings"), h("ul", {}, cited.map((c) => h("li", {}, c)))] : null,
    p.Notes ? h("details", { class: "minor", "data-key": "notes-" + p.ID }, h("summary", {}, "What it says it looked at"), h("pre", {}, p.Notes)) : null,
  ];
}

function investigatorNote(inv, p, d) {
  if (!inv || !(inv.Summary || (inv.Hypotheses || []).length || inv.Failure || p)) return null;
  const causes = inv.Hypotheses || [];
  return h("details", { class: "fold", "data-key": "note" },
    h("summary", {}, "The investigator's note", h("span", { class: "claim" }, "Written by the model, not checked by Themis")),
    inv.Failure ? h("p", { class: "fix" }, "The investigation failed: " + inv.Failure) : null,
    inv.Summary ? h("p", {}, inv.Summary) : null,
    p ? suggestionText(p, d) : null,
    causes.length ? [h("h3", { class: "sub" }, "Causes it considered"), causes.map((x) => h("div", { class: "cause" },
      h("b", {}, causeText(x.rootCauseClass, (d.Causes || {})[x.rootCauseClass]) || "Another cause."), " ",
      h("span", { class: "quiet small" }, label(CAUSE_STATUS, x.status)),
      x.summary ? h("p", {}, x.summary) : null,
      h("p", { class: "quiet small" },
        "Evidence for it: " + (sources(d, x.supportingEvidenceIds) || "none") + ". Against it: " + (sources(d, x.contradictingEvidenceIds) || "none") + ".",
        (x.missing || []).length ? " Still missing: " + x.missing.join("; ") + "." : "")))] : null);
}

function otherSuggestion(p, d) {
  return h("div", { class: "cause" },
    h("b", {}, fixWords(p.Capability, p.Params, workload(d.Incident.target).name).short), " ",
    h("span", { class: "state " + (p.Status === "rejected" ? "bad" : "done") }, label(SUGGESTION_LABEL, p.Status)),
    p.RejectionReason ? h("p", {}, label(REJECTION_LABEL, p.RejectionReason)) : null,
    h("dl", { class: "tech sub" }, sourceRows(p)),
    suggestionText(p, d));
}

function evidenceFold(d) {
  const items = d.Evidence || [];
  if (!items.length) return null;
  return h("details", { class: "fold", "data-key": "evidence" },
    h("summary", {}, "Evidence", h("span", { class: "n" }, items.length)),
    h("p", { class: "quiet small" }, "Material read from the cluster. It is not a finding until a check weighs it."),
    items.map((e) => h("details", { class: "item", "data-key": "evidence-" + e.ID },
      h("summary", {}, h("b", {}, label(EVIDENCE_LABEL, e.Template)), " ",
        h("span", { class: "quiet small" }, [when(e.CollectedAt),
          e.Classification === "sensitive" ? "may hold sensitive data" : "",
          e.Truncated ? "shortened" : "", e.Redactions ? plural(e.Redactions, "secret", "secrets") + " hidden" : "",
          e.Suspicious ? "contains text addressed to Themis" : ""].filter(Boolean).join(" · "))),
      e.Suspicious ? h("ul", {}, e.Suspicious.map((s) => h("li", {},
        cap(s.reason), ", at ", h("code", {}, s.at || "/"), s.excerpt ? [": ", h("code", {}, s.excerpt)] : null))) : null,
      e.Data === undefined
        ? h("p", { class: "quiet" }, "Only responders can read this evidence.")
        : h("pre", {}, JSON.stringify(e.Data, null, 2)))));
}

function timelineFold(d) {
  const ts = d.Transitions || [];
  if (!ts.length) return null;
  return h("details", { class: "fold", "data-key": "timeline" },
    h("summary", {}, "Timeline", h("span", { class: "n" }, plural(ts.length, "event", "events"))),
    h("ol", { class: "timeline" }, ts.map((t) => h("li", {},
      h("time", { datetime: t.At }, when(t.At)),
      h("span", {}, sentence(t.Reason) || stateOf(t.To)[0] + ".", " ", h("span", { class: "by" }, label(ACTOR_LABEL, t.Actor)))))));
}

function usage(u) {
  if (!u || !u.stepsUsed) return null;
  const n = (v) => Number(v || 0).toLocaleString("en-GB");
  return plural(u.stepsUsed, "model step", "model steps") + " · " + n(u.inputTokens) + " tokens sent · " + n(u.outputTokens) + " received";
}

function technical(d, action) {
  const inc = d.Incident;
  const t = workload(inc.target);
  const inv = d.Investigation;
  const rows = [];
  const add = (k, v) => { if (v !== null && v !== undefined && v !== "") rows.push(h("dt", {}, k), h("dd", {}, v)); };
  const code = (v) => (v ? h("code", {}, v) : null);
  add("Incident ID", code(inc.id));
  add("Workload", code(t.kind + " " + t.namespace + "/" + t.name));
  add("Workload UID", code(t.uid));
  add("Opened", when(inc.createdAt));
  add("Alerts", String(inc.occurrenceCount));
  add("Scenario packs", inc.profiles.length === 1 ? inc.profile : inc.profiles.join(", ") + ", combined");
  if (action) {
    add("Change ID", code(action.Digest));
    add("Action ID", code(action.ID));
    const at = action.Target || {};
    add("Generation", at.generation !== undefined ? String(at.generation) : null);
    add("Last changed by", code(at.manager));
    add("Policy rule", code(action.PolicyID));
    add("Policy", action.DecisionReason);
  }
  if (inv) {
    add("Investigation", label(INVESTIGATION_LABEL, inv.State) + (inv.Attempts ? ", " + plural(inv.Attempts, "attempt", "attempts") : ""));
    const prov = inv.Provenance || {};
    add("Model", prov.model ? [code(prov.model), prov.promptVersion ? " · prompt " + prov.promptVersion : ""] : null);
    add("Usage", usage(inv.Usage));
  }
  return h("details", { class: "fold", "data-key": "technical" },
    h("summary", {}, "Technical details"), h("dl", { class: "tech" }, rows));
}

// suggestForm lets a person suggest a rollback. Themis checks it exactly as
// it checks the investigator's: against the cluster, then the policy.
function suggestForm(inc, evidence, reload) {
  const t = workload(inc.target);
  const history = evidence.find((e) => e.Template === "rollout_history" && e.Data);
  const earlier = history ? (history.Data.revisions || []).filter((r) => !r.current)
    .sort((a, b) => Number(b.revision) - Number(a.revision)) : [];
  const revision = earlier.length
    ? h("select", {}, earlier.map((r) => h("option", { value: r.revision },
      "Revision " + r.revision + " · " + (r.containers || []).map((c) => c.image).join(", ") + (r.createdAt ? " · " + when(r.createdAt) : ""))))
    : h("input", { type: "number", min: "1", required: true });
  const why = h("textarea", { rows: "3", placeholder: "Why this fixes it. The approver reads this." });
  const effect = h("input", { type: "text", placeholder: "For example: pods stop crash-looping" });
  const error = h("div", { class: "error", role: "alert" });
  const submit = async (e) => {
    e.preventDefault();
    error.textContent = "";
    if (!why.value.trim()) {
      error.textContent = "State the grounds: the approver reads them.";
      return;
    }
    const body = {
      clientProposalId: "ui-" + Date.now(),
      target: { group: t.group || "apps", kind: t.kind, namespace: t.namespace, name: t.name, uid: t.uid },
      capabilityName: ROLLBACK,
      params: { revision: Number(revision.value) },
      rationale: why.value.trim(),
      expectedEffect: effect.value.trim(),
      citedEvidenceIds: evidence.map((x) => x.ID),
    };
    try {
      const r = await api("/v1/incidents/" + encodeURIComponent(inc.id) + "/proposals", { method: "POST", body: JSON.stringify(body) });
      // A refusal ends its status name with _REJECTED.
      if (r && String(r.status).endsWith("_REJECTED")) {
        error.textContent = r.message || "Themis refused this fix.";
        return;
      }
      flash = "Rollback suggested. Themis is checking it against the cluster and the policy.";
      reload();
    } catch (err) {
      error.textContent = err.message;
    }
  };
  const waiting = inc.state !== "investigating";
  return h("details", { class: "fold", open: waiting, "data-key": "suggest" },
    h("summary", {}, waiting ? "Suggest a fix" : "Suggest a fix while Themis investigates"),
    h("form", { class: "form", onsubmit: submit },
      field("Roll back to", revision),
      field("Grounds", why),
      field("Expected effect", effect),
      h("div", {}, h("button", { class: "btn primary", type: "submit" }, "Suggest the rollback")),
      h("p", { class: "quiet small" }, "Themis checks it against the cluster and the policy, as it checks its own. It cites the evidence on this page."),
      error));
}

// headline is the decision first: the question when a fix waits, or else
// what happened; then one sentence of why.
function headline(d, ctx) {
  const inc = d.Incident;
  const ts = d.Transitions || [];
  const last = ts[ts.length - 1];
  const reason = last ? sentence(last.Reason) : "";
  const { fx, cause, action } = ctx;
  const s = inc.state;
  if (s === "awaiting_approval" && fx) return [fx.question, cause || reason];
  if (ctx.recommended && fx && !["resolved", "self_resolved", "canceled"].includes(s)) {
    return [sentence(fx.short + " in Git"), action ? sentence(action.DecisionReason) : reason];
  }
  if (s === "received" || s === "investigating") {
    const got = [...new Set((d.Evidence || []).map((e) => label(EVIDENCE_LABEL, e.Template).toLowerCase()))];
    return ["Collecting and weighing evidence.", got.length ? "Collected so far: " + got.join(", ") + "." : "Nothing collected yet."];
  }
  if (s === "diagnosed" || s === "proposed") return ["Checking the suggested fix against the cluster and the policy.", fx ? "Suggested: " + fx.short.toLowerCase() + "." : reason];
  if (s === "awaiting_suggestion") {
    return ["Waiting for a suggested fix.", "No investigator is installed. A person, or a tool such as HolmesGPT, can suggest a fix; Themis checks it against the cluster and the policy, as it would its own."];
  }
  if (s === "inconclusive") return [d.Headline || "No clear cause found.", "A person can suggest a fix. Themis checks it against the cluster and the policy."];
  if ((s === "authorized" || s === "executing") && fx) {
    const approved = ts.filter((t) => t.To === "authorized").pop();
    return [fx.doing, approved ? sentence(approved.Reason) : reason];
  }
  if (s === "verifying" && fx) return ["Verifying the " + fx.noun + ".", reason];
  if (s === "restoring" && fx) return ["The " + fx.noun + " didn't help. Restoring the previous state.", reason];
  if (s === "resolved" && fx) return [fx.done, reason];
  if (s === "escalated" && ctx.restored && fx) return ["The " + fx.noun + " didn't help and was undone.", reason];
  if (REFERRED.includes(s)) return [reason || "Referred to a person.", "Themis takes no further action on this incident. A person takes over."];
  if (s === "closed_no_action") {
    const leading = cause ? "The investigator's leading cause: " + cause.charAt(0).toLowerCase() + cause.slice(1) : null;
    return [d.Headline || "No fix met the checks and policy.", leading || (d.Headline ? "" : reason)];
  }
  if (s === "self_resolved") return ["Recovered before Themis acted.", reason];
  return [reason || stateOf(s)[0] + ".", ""];
}

async function detail(id) {
  const reload = () => detail(id).catch(showError);
  const d = await api("/v1/incidents/" + encodeURIComponent(id));
  const inc = d.Incident;
  const inv = d.Investigation;
  const proposals = d.Proposals || [];
  const actions = d.Actions || [];
  const ts = d.Transitions || [];
  const t = workload(inc.target);
  const action = actions[actions.length - 1] || null;
  const accepted = proposals.filter((p) => p.Status === "accepted").pop() || null;
  const fixed = action || accepted;
  const fx = fixed ? fixWords(fixed.Capability, fixed.Params, t.name) : null;
  const lead = leadCause(inv);
  const ctx = {
    fx, action,
    cause: lead ? causeText(lead.rootCauseClass, (d.Causes || {})[lead.rootCauseClass]) : null,
    restored: ts.some((x) => x.To === "restoring"),
    recommended: ts.some((x) => x.To === "recommended") || actions.some((a) => a.PolicyID === "gitops-owns-the-target"),
  };
  const [word, tone] = stateOf(inc.state, ctx);
  const [title, because] = headline(d, ctx);
  // The fix the findings weigh: the one the latest change came from.
  const source = action ? proposals.find((p) => p.ID === action.ProposalID) : accepted;
  const mine = new Set([...actions.map((a) => a.ProposalID), accepted && accepted.ID].filter(Boolean));
  const others = proposals.filter((p) => !mine.has(p.ID));
  const fromPerson = source && !source.LeaseID ? source : null;
  const said = flash;
  flash = null;

  render(
    h("a", { class: "back", href: "#" }, "Incidents"),
    h("div", { class: "head" },
      h("span", { class: "wl2" }, h("b", {}, t.name), " in " + t.namespace + " · " + t.kind),
      h("span", { class: "tag state " + tone }, word)),
    h("h1", { class: "question" }, title),
    because ? h("p", { class: "because" }, because) : null,
    said ? h("p", { class: "flash", role: "status" }, said) : null,
    inc.supersededBy ? h("p", { class: "note" }, "The workload changed outside Themis and still fails. A newer incident covers it as it is now: ",
      h("a", { href: "#/incidents/" + inc.supersededBy }, "open the newer incident"), ".") : null,

    action ? decision(action, d, ctx, true, reload) : suspicion(d, false),
    SUGGESTABLE.includes(inc.state) && can("approver") ? h("div", { class: "more" }, suggestForm(inc, d.Evidence || [], reload)) : null,

    findings(d, source ? source.CitedPredicates || [] : null, fx ? fx.noun : "fix"),

    h("div", { class: "more" },
      actions.length > 1 ? h("details", { class: "fold", "data-key": "earlier" },
        h("summary", {}, "Earlier changes", h("span", { class: "n" }, actions.length - 1)),
        actions.slice(0, -1).reverse().map((a) => decision(a, d, ctx, false, reload))) : null,
      investigatorNote(inv, source && source.LeaseID ? source : null, d),
      fromPerson ? h("details", { class: "fold", "data-key": "suggestion" },
        h("summary", {}, "The suggested fix", h("span", { class: "claim" }, "Written by the suggester, not checked by Themis")),
        h("dl", { class: "tech" }, sourceRows(fromPerson)), suggestionText(fromPerson, d)) : null,
      others.length ? h("details", { class: "fold", "data-key": "others" },
        h("summary", {}, "Other suggestions", h("span", { class: "n" }, others.length)),
        others.map((p) => otherSuggestion(p, d))) : null,
      evidenceFold(d),
      timelineFold(d),
      technical(d, action)));

  // Refreshing would discard what an approver or suggester is typing.
  const typing = actions.some((a) => a.State === "awaiting_approval") || (SUGGESTABLE.includes(inc.state) && can("approver"));
  if (!typing) timer = setTimeout(reload, POLL_MS * 2);
}

// Routing -----------------------------------------------------------------

function showError(e) {
  if (e.message === "unauthorized") return;
  render(h("div", { class: "alert", role: "alert" }, h("span", { class: "sign", "aria-hidden": "true" }, "!"),
    h("div", {}, h("b", {}, "This page couldn't be loaded. "), e.message)));
}

// whoami asks who's signed in, by the break-glass token or a session cookie.
async function whoami() {
  const resp = await fetch("/v1/me", { headers: headers() });
  if (resp.status === 401) {
    try { sessionStorage.removeItem(TOKEN_KEY); } catch {}
    return null;
  }
  if (!resp.ok) throw new Error((await resp.text()).trim() || resp.statusText);
  return resp.json();
}

function markNav(id) {
  for (const a of document.querySelectorAll(".bar nav a")) {
    if (a.id === id) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
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
    : me.setup ? "Admin through kubectl port-forward"
    : me.token ? "Signed in with the break-glass token"
    : me.username + (me.role ? " · " + ROLE_LABEL[me.role] : "");
  document.getElementById("setup").hidden = !(me && me.setup);
  // Coming in through port-forward isn't a sign-in, so there's nothing to sign out of.
  document.getElementById("logout").hidden = !me || me.setup;
  document.querySelector(".bar nav").hidden = !me || !me.role;
  if (!me) return login();
  if (!me.role) return noAccess();
  document.getElementById("nav-accounts").hidden = !can("admin");
  document.getElementById("nav-setup").hidden = !can("admin");
  document.getElementById("nav-password").hidden = !me.account;
  const hash = location.hash;
  const m = hash.match(/^#\/incidents\/([\w-]+)$/);
  let run, nav;
  if (m) [run, nav] = [() => detail(m[1]), "nav-incidents"];
  else if (hash === "#/accounts" && can("admin")) [run, nav] = [accounts, "nav-accounts"];
  else if (hash === "#/status") [run, nav] = [status, "nav-status"];
  else if ((hash === "#/setup" || (hash === "" && me.setup)) && can("admin")) [run, nav] = [setup, "nav-setup"];
  else if (hash === "#/password" && me.account) [run, nav] = [async () => changePassword(), "nav-password"];
  else [run, nav] = [async () => { await list(); timer = setTimeout(route, POLL_MS); }, "nav-incidents"];
  markNav(nav);
  const title = { "nav-incidents": "Incidents", "nav-accounts": "Accounts", "nav-status": "Status", "nav-setup": "Setup", "nav-password": "Password" }[nav];
  document.title = title + " · Themis";
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
