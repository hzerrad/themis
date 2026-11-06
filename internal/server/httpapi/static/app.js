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
// token, or, until setup finishes, whoever signed in with the initial admin
// password.
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
// ago says how long ago t was, compactly: 4 s ago, 77 min ago.
function ago(t) {
  const s = Math.max(0, Math.round((Date.now() - new Date(t)) / 1000));
  if (90 > s) return s + " s ago";
  if (90 * 60 > s) return Math.round(s / 60) + " min ago";
  if (36 * 3600 > s) return Math.round(s / 3600) + " h ago";
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

// A finding's mark says what it means for the decision.
const MARK = { for: "✓", against: "✕", unknown: "?", found: "✓" };
const MARK_WORD = { for: "Supports", against: "Against", unknown: "Unknown", found: "Found" };
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
  failed: "Failed", ambiguous: "Result unknown", stale: "Not applied",
  denied: "Denied", expired: "Approval expired", canceled: "Canceled",
};
const OUTCOME_LABEL = {
  resolved: "Fixed", contained: "Partly recovered", unchanged: "Didn't help",
  worsened: "Worse after the fix", inconclusive: "Undetermined",
};
const INVESTIGATION_LABEL = { pending: "Waiting to start", leased: "In progress", completed: "Finished", failed: "Failed" };
const CAUSE_STATUS = {
  HYPOTHESIS_STATUS_SUPPORTED: "Supported by the evidence", HYPOTHESIS_STATUS_REFUTED: "Ruled out", HYPOTHESIS_STATUS_OPEN: "Open",
};
const SUGGESTION_LABEL = { accepted: "Accepted", rejected: "Refused", superseded: "Replaced", deferred: "Waiting for another fix" };
const REJECTION_LABEL = {
  REJECTION_REASON_TARGET_MISMATCH: "Refused. It names a different workload than the incident's.",
  REJECTION_REASON_CAPABILITY_NOT_ALLOWED: "Refused. This kind of fix isn't allowed for this incident.",
  REJECTION_REASON_INVALID_PARAMS: "Refused. Its values are invalid.",
  REJECTION_REASON_UNKNOWN_EVIDENCE: "Refused. It cites evidence Themis didn't collect.",
  REJECTION_REASON_UNKNOWN_PREDICATE: "Refused. It cites a check that didn't run for this incident.",
  REJECTION_REASON_INCIDENT_CLOSED: "Refused. The incident no longer takes suggestions.",
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
    if (me && me.initialAdmin) location.hash = "#/setup";
    me = null;
    route();
  };
  const form = h("form", { class: "form", onsubmit: submit },
    field("Username", username), field("Password", password),
    h("div", {}, h("button", { class: "btn" + (primary ? " primary" : ""), type: "submit" }, "Sign in")),
    error);
  return { form, focus: () => username.focus() };
}

// initialAdminForm signs in with the initial admin password, while no admin
// account exists, to create the first. where says how to read it, when
// Themis knows.
function initialAdminForm(where) {
  const password = h("input", { type: "password", autocomplete: "off", required: true });
  const error = h("div", { class: "error", role: "alert" });
  const submit = async (e) => {
    e.preventDefault();
    error.textContent = "";
    const resp = await fetch("/auth/initial-admin", { method: "POST", headers: headers(), body: JSON.stringify({ password: password.value }) });
    password.value = "";
    if (!resp.ok) {
      error.textContent = (await resp.text()).trim();
      return;
    }
    me = null;
    route();
  };
  const section = [
    h("p", {}, "No admin account. Sign in with the initial admin password to create the first one."),
    where.command ? [h("p", { class: "quiet small" }, "Read it with kubectl:"), copyable(where.command)] : null,
    h("form", { class: "form", onsubmit: submit },
      field("Initial admin password", password),
      h("div", {}, h("button", { class: "btn primary", type: "submit" }, "Sign in")),
      error)];
  return { section, focus: () => password.focus() };
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
  const first = methods.initialAdmin ? initialAdminForm(methods.initialAdmin) : null;
  const breakGlass = h("details", { class: "minor", open: !methods.oidc && !methods.password && !first },
    h("summary", {}, "Use the break-glass token"),
    h("form", { class: "form", onsubmit: submit },
      h("p", { class: "quiet small" }, "For scripts and failed sign-ins. Kept for this browser tab only."),
      field("Break-glass token", input),
      h("div", {}, h("button", { class: "btn", type: "submit" }, "Continue"))));
  if (!methods.oidc && !methods.password && !first) {
    render(h("div", { class: "plate centre" },
      h("h1", {}, "Sign-in is not set up"),
      h("p", {}, "No admin account. Sign in with the break-glass token to create the first one."),
      breakGlass));
    return;
  }
  const pw = methods.password ? passwordForm(!methods.oidc && !first) : null;
  render(h("div", { class: "plate centre" },
    h("h1", {}, "Sign in"),
    first ? first.section : null,
    methods.oidc ? [first ? h("p", { class: "or" }, "Or through the identity provider") : null,
      h("p", {}, h("a", { class: "btn" + (first ? "" : " primary"), href: "/auth/login" }, "Sign in with your identity provider"))] : null,
    pw ? [methods.oidc || first ? h("p", { class: "or" }, "Or with a Themis account") : null, pw.form] : null,
    breakGlass));
  if (first) first.focus();
  else if (pw && !methods.oidc) pw.focus();
}

// CLOSED is what Themis says while no admin account exists.
const CLOSED = "No admin account. Create the first admin account to start Themis.";

// firstAdmin is all Themis serves while no admin account exists: creating
// the first.
function firstAdmin() {
  if (!can("admin")) {
    render(h("div", { class: "plate centre" }, h("h1", {}, "Themis has not started"), h("p", {}, CLOSED)));
    return;
  }
  const username = h("input", { type: "text", autocomplete: "off", autocapitalize: "none", required: true });
  const password = h("input", { type: "password", autocomplete: "new-password", minlength: "12", required: true });
  const error = h("div", { class: "error", role: "alert" });
  const submit = async (e) => {
    e.preventDefault();
    error.textContent = "";
    const name = username.value.trim();
    try {
      await api("/v1/accounts", { method: "POST", body: JSON.stringify({ username: name, role: "admin", password: password.value }) });
      flash = "Account " + name + " created with a temporary password. " + name + " must choose a new one at first sign-in.";
      me = null;
      route();
    } catch (err) {
      error.textContent = err.message;
    }
  };
  render(h("div", { class: "plate centre" },
    h("h1", {}, "Create the first admin account"),
    h("p", {}, CLOSED),
    h("form", { class: "form", onsubmit: submit },
      field("Username", username),
      field("Temporary password", password, "12 characters or more. A new one is chosen at the first sign-in."),
      h("div", {}, h("button", { class: "btn primary", type: "submit" }, "Create the admin account")),
      error)));
  username.focus();
}

// signInAsAdmin follows the first admin account's making by the initial
// admin password, whose only use that was.
function signInAsAdmin() {
  const pw = passwordForm(true);
  const said = flash;
  flash = null;
  render(h("div", { class: "plate centre" },
    h("h1", {}, "Sign in with the admin account"),
    said ? h("p", { class: "flash", role: "status" }, said) : null,
    h("p", {}, "Then choose your own password. The initial admin password then stops working."),
    pw.form));
  pw.focus();
}

function noAccess() {
  render(h("div", { class: "plate centre" },
    h("h1", {}, "No access"),
    h("p", {}, "Signed in as ", h("b", {}, me.username), ". This account has no role in Themis."),
    h("p", { class: "quiet" }, "Ask an admin for a role.")));
}

// password lets someone signed in with an account change its password.
// A password an admin set is temporary: until its holder chooses their own,
// it is the only page they may open.
function changePassword() {
  const temporary = me.temporaryPassword;
  const current = h("input", { type: "password", autocomplete: "current-password", required: true });
  const next = h("input", { type: "password", autocomplete: "new-password", minlength: "12", required: true });
  const error = h("div", { class: "error", role: "alert" });
  const submit = async (e) => {
    e.preventDefault();
    error.textContent = "";
    try {
      await api("/v1/me/password", { method: "POST", body: JSON.stringify({ current: current.value, new: next.value }) });
      // A new password ends every session, this one too. Replacing a
      // temporary one signs in again with it, to continue.
      if (temporary) {
        await fetch("/auth/password", { method: "POST", headers: headers(), body: JSON.stringify({ username: me.username, password: next.value }) });
      }
      me = null;
      route();
    } catch (err) {
      error.textContent = err.message;
    }
  };
  render(h("div", { class: "plate centre" },
    h("h1", {}, temporary ? "Choose your password" : "Change your password"),
    h("p", { class: "quiet" }, temporary
      ? "Temporary password: an admin set it. Choose your own to continue."
      : "Changing it ends every session, this one included."),
    h("form", { class: "form", onsubmit: submit },
      field(temporary ? "Temporary password" : "Current password", current), field("New password", next, "12 characters or more."),
      h("div", {}, h("button", { class: "btn primary", type: "submit" }, temporary ? "Choose this password" : "Change password")),
      error)));
}

// Accounts ----------------------------------------------------------------

// recordLine says what an entry of the account record did, in the past tense
// of the one who did it.
function recordLine(e) {
  const role = ROLE_LABEL[e.role] || e.role;
  if (e.action === "account_created") return "Created " + e.username + " with the " + role + " role.";
  if (e.action === "account_deleted") return "Deleted " + e.username + ".";
  if (e.action === "role_changed") return "Gave " + e.username + " the " + role + " role.";
  if (e.action === "temporary_password_set") return "Set a temporary password for " + e.username + ".";
  if (e.action === "password_changed") return e.actor === e.username ? "Chose a new password." : "Changed the password of " + e.username + ".";
  if (e.action === "initial_admin_signed_in") return "Signed in with the initial admin password.";
  if (e.action === "initial_admin_retired") return "Signed in as an admin, which retired the initial admin password.";
  return humanize(e.action) + (e.username ? ": " + e.username : "") + ".";
}

// recordActor names who made an entry: a person, or a credential that proves
// itself, not who.
function recordActor(actor) {
  if (actor === "admin") return "The break-glass token";
  if (actor === "initial-admin") return "The initial admin password";
  if (actor === "helm-chart") return "The Helm chart";
  return actor;
}

// accounts lets admins manage who signs in with a password, and shows the
// record of changes to accounts.
async function accounts() {
  const list = await api("/v1/accounts");
  const { events } = await api("/v1/audit/accounts");
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
  const role = roleSelect("approver", "Role");
  const password = h("input", { type: "password", autocomplete: "new-password", minlength: "12", required: true });
  const create = (e) => {
    e.preventDefault();
    const name = username.value.trim();
    act("/v1/accounts", { method: "POST", body: JSON.stringify({ username: name, role: role.value, password: password.value }) },
      "Account " + name + " created with a temporary password. " + name + " must choose a new one at first sign-in.");
  };
  const path =(a) => "/v1/accounts/" + encodeURIComponent(a.username);
  const rows = list.map((a) => {
    const self = me.account && me.username === a.username;
    const r = roleSelect(a.role, "Role of " + a.username);
    r.disabled = self;
    r.addEventListener("change", () => act(path(a), { method: "PATCH", body: JSON.stringify({ role: r.value }) }, a.username + " is now " + ROLE_LABEL[r.value] + "."));
    const pw = h("input", { type: "password", autocomplete: "new-password", "aria-label": "Temporary password for " + a.username, placeholder: "Temporary password" });
    const reset = h("button", { class: "btn small", type: "button", onclick: () => {
      if (pw.value) act(path(a), { method: "PATCH", body: JSON.stringify({ password: pw.value }) }, "Temporary password set. " + a.username + " must choose a new one at next sign-in.");
    } }, "Set a temporary password");
    const remove = h("button", { class: "btn small deny", type: "button", disabled: self,
      onclick: () => act(path(a), { method: "DELETE" }, "Account " + a.username + " deleted.") }, "Delete");
    return h("tr", {},
      h("td", {}, h("b", {}, a.username),
        a.locked ? h("div", { class: "quiet small" }, "Locked: too many wrong passwords") : null,
        a.temporaryPassword ? h("div", { class: "quiet small" }, "Temporary password: a new one is due at the next sign-in") : null),
      h("td", {}, r),
      h("td", { class: "quiet small" }, "By " + a.createdBy + ", " + when(a.createdAt)),
      h("td", {}, h("div", { class: "inline" }, pw, reset, remove)));
  });
  const said = flash;
  flash = null;
  render(
    h("h1", {}, "Accounts"),
    h("p", { class: "lede" }, "Accounts sign in with a username and password. A password an admin sets is temporary. Its holder chooses a new one at the next sign-in. The last admin account stays."),
    said ? h("p", { class: "flash", role: "status" }, said) : null,
    list.length ? h("div", { class: "plate" }, h("table", { class: "list" },
      h("thead", {}, h("tr", {}, h("th", {}, "Username"), h("th", {}, "Role"), h("th", {}, "Created"), h("th", {}, h("span", { class: "quiet" }, "Password and removal")))),
      h("tbody", {}, rows))) : null,
    h("div", { class: "plate" },
      h("h2", {}, "New account"),
      h("form", { class: "inline", onsubmit: create },
        field("Username", username), field("Role", role), field("Temporary password, 12 characters or more", password),
        h("button", { class: "btn primary", type: "submit" }, "Create account"))),
    error,
    events.length ? h("details", { class: "fold sub", "data-key": "account-record" },
      h("summary", {}, "Record", h("span", { class: "n" }, events.length)),
      h("p", { class: "quiet small" }, "Changes to accounts and to the initial admin password, newest first."),
      h("table", { class: "list" },
        h("thead", {}, h("tr", {}, h("th", {}, "When"), h("th", {}, "By"), h("th", {}, "Change"))),
        h("tbody", {}, events.map((e) => h("tr", {},
          h("td", { class: "quiet small" }, when(e.at)),
          h("td", {}, recordActor(e.actor)),
          h("td", {}, recordLine(e))))))) : null);
}

// Status and setup --------------------------------------------------------

// CHECK_STATE names how each part of the install is doing, and its tone.
const CHECK_STATE = {
  ok: ["Operational", "good"], warn: ["Action required", "wait"], problem: ["Failing", "bad"],
  waiting: ["Waiting", ""], off: ["Off", "done"],
};
// URGENCY orders parts for acting on: failing first, then what requires action.
const URGENCY = { problem: 0, warn: 1, waiting: 2, off: 3, ok: 4 };
const needsAction = (c) => c.state === "problem" || c.state === "warn";

// partFinding is a part's finding, with when it was last heard from.
function partFinding(c) {
  return [c.summary, c.at ? " " + (c.since || "Last heard") + " " + ago(c.at) + "." : ""];
}

// partState is a part's state word, in its tone.
function partState(c) {
  const [word, tone] = CHECK_STATE[c.state] || [humanize(c.state), ""];
  return h("span", { class: "state " + tone }, word);
}

// copyable is code to copy and run, with a button that copies it.
function copyable(code) {
  const button = h("button", { class: "btn small", type: "button", onclick: async () => {
    try {
      await navigator.clipboard.writeText(code);
      button.textContent = "Copied";
    } catch {
      button.textContent = "Not copied. Select it.";
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
    out);
}

// modelSaved says the model was just saved, so Setup says so once.
let modelSaved = false;

// modelStep sets the model the install left unset. What the install set is
// shown locked: a change here would be undone by the next upgrade.
function modelStep(v) {
  const saved = modelSaved;
  modelSaved = false;
  const byInstall = (name) => v.byInstall.includes(name);
  const locked = "Set in Helm. Read-only here.";
  const url = h("input", { type: "text", value: v.baseURL, placeholder: "https://api.openai.com/v1", disabled: byInstall("model.baseURL") });
  const name = h("input", { type: "text", value: v.name, placeholder: "gpt-4.1 or qwen3:8b", disabled: byInstall("model.name") });
  const key = h("input", { type: "password", autocomplete: "off", disabled: byInstall("model.apiKey"),
    placeholder: v.apiKeySet ? "Set. Leave empty to keep it." : "Optional" });
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
    return [h("p", {}, "Set in Helm: ", h("code", {}, v.name), " at ", h("code", {}, v.baseURL), ".")];
  }
  return [
    saved ? h("p", { class: "flash", role: "status" }, "Model saved. Used from the next investigation. Done when the model answers.") : null,
    h("p", { class: "quiet" }, "Any OpenAI-compatible endpoint: OpenAI, OpenRouter, Ollama, vLLM. Used from the next investigation."),
    h("form", { class: "form", onsubmit: save },
      field("Address", url, byInstall("model.baseURL") ? locked : null),
      field("Model", name, byInstall("model.name") ? locked : null),
      field("API key", key, byInstall("model.apiKey") ? locked : "Stored encrypted. Never shown again."),
      h("label", { class: "check" }, strict,
        h("span", {}, "Strict format: the model must follow Themis's reply format exactly. For OpenAI, clear it, or install with ",
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
  const signIn = check("Sign-in");
  const acting = checks.filter(needsAction).length;
  const steps = [
    { title: "Sign-in", status: done("Sign-in") ? "Done" : "Not done", body: done("Sign-in") ? [h("p", {}, signIn.summary)] : [
      h("p", {}, partFinding(signIn)),
      h("ul", { class: "remedies" },
        h("li", {}, h("a", { href: "#/accounts" }, "Create accounts")),
        h("li", {}, h("a", { href: OIDC_DOCS, target: "_blank", rel: "noopener" }, "Configure the identity provider (Helm: oidc values)")))] },
    investigatorOff
      ? { title: "The model", status: "Not used", body: [h("p", {}, check("Investigator").summary)] }
      : model ? { title: "The model", status: done("Model") ? "Done" : model.complete ? "Set" : "Not done", body: modelStep(model) } : null,
    { title: "Alerts", status: done("Alerts") ? "Done" : "Not done", body: [
      h("p", {}, partFinding(check("Alerts"))),
      done("Alerts") ? null : [h("p", { class: "quiet" }, "Done when Themis receives its first alert."), alertmanagerSetup()]] },
    { title: "Check", status: acting ? "Action required" : "Done", body: [digest(checks), h("p", { class: "sub" }, h("a", { href: "#/status" }, "Full status"))] },
  ].filter(Boolean);
  const tone = { Done: "good", "Action required": "wait" };
  render(
    h("h1", {}, "Setup"),
    h("p", { class: "lede" }, "Values set in Helm are read-only here."),
    steps.map((s, i) => h("section", { class: "plate" },
      h("div", { class: "step-head" },
        h("h2", {}, h("span", { class: "num" }, String(i + 1)), s.title),
        s.status ? h("span", { class: "state " + (tone[s.status] || "done") }, s.status) : null),
      s.body)));
}

// OIDC_DOCS is where the chart's identity provider values are described.
const OIDC_DOCS = "https://github.com/hzerrad/themis/blob/main/docs/configuration.md#apioidc";

// digest is every part's state in a few lines, what requires action first,
// each with its finding and remedy.
function digest(checks) {
  const parts = [...checks].sort((a, b) => (URGENCY[a.state] ?? 2) - (URGENCY[b.state] ?? 2));
  return h("ul", { class: "parts" }, parts.map((c) => h("li", {},
    h("b", {}, c.name), partState(c),
    needsAction(c) ? h("div", { class: "says" }, partFinding(c), c.fix ? h("div", { class: "fix" }, c.fix) : null) : null)));
}

// status is each part of the install: its state, its finding, and the remedy once.
async function status() {
  const { checks } = await api("/v1/status");
  const acting = checks.filter(needsAction).length;
  render(
    h("h1", {}, "Status"),
    h("p", { class: "lede" }, (acting ? plural(acting, "part requires", "parts require") + " action." : "No part requires action.")
      + " Checked at " + CLOCK.format(new Date()) + "."),
    h("div", { class: "plate" }, h("table", { class: "list" },
      h("thead", {}, h("tr", {}, h("th", {}, "Part"), h("th", {}, "State"), h("th", {}, "Finding"))),
      h("tbody", {}, checks.map((c) => h("tr", {},
        h("td", {}, h("b", {}, c.name)),
        h("td", {}, partState(c)),
        h("td", {}, partFinding(c),
          c.fix ? h("div", { class: "fix" }, c.fix) : null,
          c.name === "Alerts" && c.state !== "ok" && can("admin") ? h("button", { class: "btn small sub", type: "button", onclick: (e) => {
            e.target.replaceWith(alertmanagerSetup());
          } }, "Connect Alertmanager") : null)))))),
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

// GITOPS is the rule by which Themis only recommends a fix: Argo CD or Flux
// manages the workload. A fix denied or set aside passes through the same
// state, but isn't recommended.
const GITOPS = "gitops-owns-the-target";

// listHow tells apart, from an incident's summary, a fix undone and one only
// recommended. A restore that didn't happen says "Not restored".
function listHow(i) {
  const s = i.summary || {};
  return {
    restored: i.state === "escalated" && s.from === "restoring" && !String(s.reason || "").startsWith("Not restored"),
    recommended: s.rule === GITOPS,
  };
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
    ? ["No incidents. One opens when Alertmanager sends an alert about a workload Themis watches. ",
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
    h("p", { class: "quiet small sub" }, "Values that look like secrets are hidden. Commands and arguments show only that they change."),
  ];
}

// SUSPECT_KIND names each kind of evidence flagged text can be in, and what
// a check that reads that kind reads.
const SUSPECT_KIND = {
  container_logs_tail: { where: "the container logs", reads: "log text" },
  target_events: { where: "the events", reads: "events" },
  pod_states: { where: "the pod states", reads: "pod states" },
  referenced_config: { where: "the referenced config", reads: "the referenced config" },
  rollout_history: { where: "the rollout history", reads: "the rollout history" },
};
// SUSPECT_CHECK flags such text: it reads every kind, to flag it.
const SUSPECT_CHECK = "evidence_has_instructions";

const lowerFirst = (s) => (/^.[A-Z]/.test(s) ? s : s.charAt(0).toLowerCase() + s.slice(1));
const and = (xs) => (xs.length > 1 ? xs.slice(0, -1).join(", ") + " and " + xs[xs.length - 1] : xs.join(""));

// flaggedTexts are the texts evidence holds that look written for Themis,
// each once: rules that flag the same line say it once, the first rule's
// reason leading, with the longest excerpt. Viewers get no excerpts, so
// their texts are told apart by where they are.
function flaggedTexts(d) {
  const out = [];
  for (const e of d.Evidence || []) {
    for (const s of e.Suspicious || []) {
      const text = String(s.excerpt || "").replace(/^…|…$/g, "");
      const same = out.find((x) => x.e === e && x.s.at === s.at && (!text || !x.text || x.text.includes(text) || text.includes(x.text)));
      if (!same) out.push({ e, s, text, excerpt: s.excerpt });
      else if (text.length > same.text.length) Object.assign(same, { text, excerpt: s.excerpt });
    }
  }
  return out;
}

// suspect is one flagged text: where it is, the text itself for those who
// may read it, then what it is, whether the checks read it, and what to do.
function suspect(d, x, action, noun) {
  const t = x.e.Template;
  const kind = SUSPECT_KIND[t] || { where: "the evidence", reads: "this evidence" };
  const checks = d.Checks || {};
  const readers = [...new Set((d.Predicates || []).map((p) => p.Name))]
    .filter((n) => n !== SUSPECT_CHECK && ((checks[n] || {}).reads || []).includes(t));
  const subject = t === "container_logs_tail" ? "This line"
    : t === "target_events" && String(x.s.at).endsWith("/message") ? "This event message" : "This text";
  const what = subject + " " + (x.s.what || "looks written for Themis rather than by the workload") + ".";
  let effect = "Themis's checks don't read " + kind.reads + ", so it had no effect on them.";
  if (readers.length) {
    const topics = readers.slice(0, 3).map((n) => lowerFirst((checks[n] || {}).topic || (checks[n] || {}).true || humanize(n)));
    if (readers.length > 3) topics.push(plural(readers.length - 3, "other", "others"));
    effect = "Checks that read " + kind.reads + " may reflect it: " + and(topics) + ".";
  }
  const state = d.Incident.state;
  let todo = "Find out how it got into " + kind.where + ".";
  if (action && action.State === "awaiting_approval" && can("approver")) {
    todo = readers.length ? "Approve only after confirming their results in the cluster."
      : "Approve only if the checks below justify the " + noun + " on their own.";
  } else if (!action && SUGGESTABLE.includes(state) && can("approver")) {
    todo = readers.length ? "Confirm their results in the cluster before suggesting a fix."
      : "Suggest a fix only if the checks below justify it on their own.";
  } else if (REFERRED.includes(state) && can("responder")) {
    todo = readers.length ? "Confirm their results in the cluster before acting." : "Base any fix on the checks below and on the cluster.";
  }
  return h("div", { class: "alert suspect", role: "note" },
    h("span", { class: "sign", "aria-hidden": "true" }, "!"),
    h("div", {},
      h("b", {}, "Suspicious text in " + kind.where),
      x.excerpt ? h("pre", { class: "quote" }, x.excerpt) : h("p", { class: "quiet small" }, "Responders can read the text."),
      h("div", { class: "src" }, x.s.origin ? cap(x.s.origin) : [label(EVIDENCE_LABEL, t), " at ", h("code", {}, x.s.at || "/")]),
      h("p", {}, what + " " + effect + " " + todo)));
}

// suspicion warns of each text evidence holds that looks written for Themis,
// such as instructions or forged tool output. Viewers learn where, not what
// it says. Past three, the rest fold away.
function suspicion(d, action, noun) {
  const blocks = flaggedTexts(d).map((x) => suspect(d, x, action, noun));
  if (!blocks.length) return null;
  return [blocks.slice(0, 3), blocks.length > 3 ? h("details", { class: "fold sub", "data-key": "suspects" },
    h("summary", {}, "More suspicious text", h("span", { class: "n" }, blocks.length - 3)), blocks.slice(3)) : null];
}

// observed is what verifying a change found, such as 2 of 3 pods ready.
function observed(a) {
  const o = a && a.Verification && a.Verification.Observation;
  return o ? (o.Facts || (o.ReadyReplicas + " of " + o.DesiredReplicas + " pods ready.")) : null;
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
    const facts = observed(a);
    add("Outcome", a.Verification.Outcome ? label(OUTCOME_LABEL, a.Verification.Outcome) : "Verifying",
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
  const gitOps = a.PolicyID === GITOPS;
  const replicas = a.BlastRadius && a.BlastRadius.replicas;
  const facts = [
    replicas !== undefined ? ["Affects ", h("b", {}, plural(Number(replicas), "pod", "pods")), ". "] : null,
    a.Risk ? ["Risk: ", a.Risk, ". "] : null,
    awaiting && a.ExpiresAt ? ["Approve before ", h("b", {}, when(a.ExpiresAt)), ". "] : null,
    awaiting ? "Applies exactly this change, then verifies it. Void if " + t.name + " changes before it is applied." : null,
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
    side = h("div", { class: "act" }, approve, field("Reason for denying (optional)", reason), deny, error);
  } else if (awaiting) {
    side = h("div", { class: "act" }, h("p", { class: "quiet" }, "Approving or denying requires the approver role."));
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
      latest ? suspicion(d, a, fx.noun) : null),
    side);
}

// statement is what a check found, as a fact: what it says when true, its
// negation when false, its topic when unknown. A pack that gives no negation
// reads as its statement followed by ": no", never a bare "No".
function statement(d, p) {
  const w = (d.Checks || {})[p.Name] || {};
  const said = w.true || humanize(p.Name);
  if (p.Value === "true") return said;
  if (p.Value === "false") return w.false || said + ": no";
  return (w.topic || said) + ": unknown";
}

// sameFindings joins checks, often of different packs, that found the same
// fact: the same answer, on the same grounds, from the same evidence, with
// the same bearing on the fix. The cited one leads.
function sameFindings(all, cited) {
  const out = [];
  const byKey = new Map();
  for (const p of all) {
    const ids = [...(p.EvidenceIDs || [])].sort().join(",");
    const key = p.Value !== "unknown" && p.Reason && ids
      ? [p.Value, p.Reason, ids, AGAINST_WHEN_TRUE.includes(p.Name)].join("|") : null;
    const same = key ? byKey.get(key) : null;
    if (!same) {
      const g = { p, also: [] };
      out.push(g);
      if (key) byKey.set(key, g);
    } else if (cited && cited.includes(p.Name) && !cited.includes(same.p.Name)) {
      same.also.push(same.p);
      same.p = p;
    } else {
      same.also.push(p);
    }
  }
  return out;
}

// finding is one fact Themis found, marked with what it means for the fix,
// and on what grounds.
function finding(g, d, side) {
  const p = g.p;
  const unknown = p.Value === "unknown";
  return h("li", {},
    h("span", { class: "k " + (side || "none"), "aria-hidden": "true" }, side ? MARK[side] : "·"),
    h("span", {}, side ? h("span", { class: "vh" }, MARK_WORD[side] + ": ") : null,
      statement(d, p) + (unknown && p.Reason ? " — " + p.Reason : ""),
      h("span", { class: "src" }, [unknown ? null : p.Reason, sources(d, p.EvidenceIDs),
        g.also.length ? "Also: " + g.also.map((x) => statement(d, x)).join("; ") : null].filter(Boolean).join(" · "))));
}

// findings weighs Themis's checks: for and against the fix when there is
// one, each fact once.
function findings(d, cited, noun) {
  const all = d.Predicates || [];
  if (!all.length) return null;
  const forFix = [], against = [], unknown = [], other = [];
  for (const g of sameFindings(all, cited)) {
    const p = g.p;
    const flips = AGAINST_WHEN_TRUE.includes(p.Name);
    const supports = p.Value === (flips ? "false" : "true");
    const isCited = cited && [p, ...g.also].some((x) => cited.includes(x.Name));
    if (p.Value === "unknown") unknown.push(finding(g, d, "unknown"));
    else if (isCited) (supports ? forFix : against).push(finding(g, d, supports ? "for" : "against"));
    else if (!supports && flips) against.push(finding(g, d, "against"));
    else if (!cited && p.Value === "true") forFix.push(finding(g, d, "found"));
    else other.push(finding(g, d, null));
  }
  const pan = (cls, title, items, empty) => h("div", { class: "pan " + cls },
    h("h3", {}, title), items.length ? h("ul", {}, items) : h("p", { class: "empty" }, empty));
  const blind = d.Missing && d.Missing.length ? h("div", { class: "blind" },
    h("b", {}, "Missing evidence"), h("ul", {}, d.Missing.map((m) => h("li", {}, m)))) : null;
  const weigh = [plural(forFix.length, "check supports", "checks support") + " the " + noun + ".",
    against.length ? plural(against.length, "is", "are") + " against it." : "None is against it.",
    unknown.length ? plural(unknown.length, "is", "are") + " unknown." : ""].filter(Boolean).join(" ");
  return h("section", { class: "findings", "aria-label": "Findings" },
    h("div", { class: "pans" },
      cited ? pan("for", "Supports the " + noun, forFix, "No check supports it.") : pan("for", "Found", forFix, "No check came out true."),
      cited ? pan("against", "Against, or unknown", [...against, ...unknown], "No check is against it.")
        : pan("against", "Unknown, or against", [...against, ...unknown], "Every check has an answer.")),
    cited ? h("p", { class: "weigh" }, weigh) : null,
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
    row("Identity", "Verified. It ran this incident's investigation.");
  } else if (src === "person:initial-admin") {
    row("From", "A person");
    row("Identity", "The initial admin password");
  } else if (src === "person:setup") {
    // Records from versions of Themis that let port-forward in as admin.
    row("From", "A person");
    row("Identity", "kubectl port-forward, before sign-in existed");
  } else if (src === "admin-token") {
    row("From", "A person");
    row("Identity", "The break-glass token");
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
    return found ? statement(d, found) : statement(d, { Name: c, Value: "true" }) + ": not checked";
  });
  return [
    p.Rationale ? [h("h3", { class: "sub" }, "Rationale"), h("p", {}, p.Rationale)] : null,
    p.ExpectedEffect ? [h("h3", {}, "Expected effect"), h("p", {}, p.ExpectedEffect)] : null,
    p.Message ? [h("h3", {}, "Message"), h("p", {}, p.Message)] : null,
    cited.length ? [h("h3", {}, "Checks it cites"), h("ul", {}, cited.map((c) => h("li", {}, c)))] : null,
    p.Notes ? h("details", { class: "minor", "data-key": "notes-" + p.ID }, h("summary", {}, "What it says it looked at"), h("pre", {}, p.Notes)) : null,
  ];
}

function investigatorNote(inv, p, d) {
  if (!inv || !(inv.Summary || (inv.Hypotheses || []).length || inv.Failure || p)) return null;
  const causes = inv.Hypotheses || [];
  return h("details", { class: "fold", "data-key": "note" },
    h("summary", {}, "The investigator's note", h("span", { class: "claim" }, "Written by the investigator's model")),
    inv.Failure ? h("p", { class: "fix" }, "Investigation failed. " + inv.Failure) : null,
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
    items.map((e) => h("details", { class: "item", "data-key": "evidence-" + e.ID },
      h("summary", {}, h("b", {}, label(EVIDENCE_LABEL, e.Template)), " ",
        h("span", { class: "quiet small" }, [when(e.CollectedAt),
          e.Classification === "sensitive" ? "may hold sensitive data" : "",
          e.Truncated ? "shortened" : "", e.Redactions ? plural(e.Redactions, "secret", "secrets") + " hidden" : "",
          e.Suspicious ? "suspicious text" : ""].filter(Boolean).join(" · "))),
      e.Suspicious ? h("ul", {}, e.Suspicious.map((s) => h("li", {},
        "Suspicious text at ", h("code", {}, s.at || "/"), s.origin ? " (" + s.origin + ")" : "",
        ". The text " + (s.what || "looks written for Themis") + "."))) : null,
      e.Data === undefined
        ? h("p", { class: "quiet" }, "Readable by responders only.")
        : h("pre", {}, JSON.stringify(e.Data, null, 2)))));
}

// timelineFold lists every change of state, each reason as recorded. A step
// taken for the same reason as the one before it reads as the state it
// reached, not the reason again.
function timelineFold(d) {
  const ts = d.Transitions || [];
  if (!ts.length) return null;
  return h("details", { class: "fold", "data-key": "timeline" },
    h("summary", {}, "Timeline", h("span", { class: "n" }, plural(ts.length, "event", "events"))),
    h("ol", { class: "timeline" }, ts.map((t, i) => {
      const again = i > 0 && t.Reason && t.Reason === ts[i - 1].Reason;
      return h("li", {},
        h("time", { datetime: t.At }, when(t.At)),
        h("span", {}, (!again && t.Reason) || stateOf(t.To)[0] + ".", " ", h("span", { class: "by" }, label(ACTOR_LABEL, t.Actor))));
    })));
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
  const why = h("textarea", { rows: "3", placeholder: "Why this fixes it. Shown to the approver." });
  const effect = h("input", { type: "text", placeholder: "For example: pods stop crash-looping" });
  const error = h("div", { class: "error", role: "alert" });
  const submit = async (e) => {
    e.preventDefault();
    error.textContent = "";
    if (!why.value.trim()) {
      error.textContent = "A reason is required.";
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
        error.textContent = REJECTION_LABEL[r.rejectionReason] || r.message || "Refused.";
        return;
      }
      flash = "Rollback suggested. Checking it against the cluster and the policy.";
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
      field("Reason", why),
      field("Expected effect", effect),
      h("div", {}, h("button", { class: "btn primary", type: "submit" }, "Suggest the rollback")),
      h("p", { class: "quiet small" }, "Cites the evidence on this page."),
      error));
}

// opening splits a reason Themis wrote into what happened, its first
// sentence, and the grounds after it.
// handedOver closes a referred incident's explanation: the state already says
// a person takes over, so the reason drops that sentence and ends with what
// Themis does next.
function handedOver(text) {
  const rest = (text || "").replace(/\s*Referred to a person\.\s*$/, "").trim();
  return rest ? rest + " No further action by Themis." : "No further action by Themis.";
}

function opening(reason, otherwise) {
  const cut = reason.indexOf(". ");
  return cut > 0 ? [reason.slice(0, cut + 1), reason.slice(cut + 2)] : [reason || otherwise, ""];
}

// headline is the decision first: the question when a fix waits, or else
// what happened; then one sentence of why.
function headline(d, ctx) {
  const inc = d.Incident;
  const ts = d.Transitions || [];
  const last = ts[ts.length - 1];
  // Reasons are shown as recorded: a person's grounds for denying are theirs.
  const reason = last ? last.Reason : "";
  const { fx, cause, action } = ctx;
  const s = inc.state;
  if (s === "awaiting_approval" && fx) return [fx.question, cause || reason];
  if (ctx.recommended && fx && !["resolved", "self_resolved", "canceled"].includes(s)) {
    return [sentence(fx.short + " in Git"), action ? action.DecisionReason : reason];
  }
  if (s === "received" || s === "investigating") {
    const got = [...new Set((d.Evidence || []).map((e) => label(EVIDENCE_LABEL, e.Template).toLowerCase()))];
    return ["Collecting and weighing evidence.", got.length ? "Collected so far: " + got.join(", ") + "." : "Nothing collected yet."];
  }
  if (s === "diagnosed" || s === "proposed") return ["Checking the suggested fix against the cluster and the policy.", fx ? "Suggested: " + fx.short.toLowerCase() + "." : reason];
  const suggest = can("approver") ? "Suggest a fix below, or send one from a tool such as HolmesGPT." : "An approver, or a tool such as HolmesGPT, can suggest a fix.";
  if (s === "awaiting_suggestion") return ["Waiting for a suggested fix.", "No investigator is installed. " + suggest];
  if (s === "inconclusive") return [d.Headline || "No fix proposed.", suggest];
  if ((s === "authorized" || s === "executing") && fx) {
    const approved = ts.filter((t) => t.To === "authorized").pop();
    return [fx.doing, approved ? approved.Reason : reason];
  }
  if (s === "verifying" && fx) return ["Verifying the " + fx.noun + ".", reason];
  if (s === "restoring" && ctx.undone) {
    const fixed = (d.Actions || []).filter((a) => !(a.Params || {}).restores && a.Verification).pop();
    return ["The " + ctx.undone.noun + " didn't help. Restoring the previous state.", observed(fixed) || ""];
  }
  if (s === "resolved" && fx) return [fx.done, reason];
  if (s === "escalated" && ctx.restored && ctx.undone) return ["The " + ctx.undone.noun + " didn't help and was undone.", handedOver(reason)];
  if (REFERRED.includes(s)) {
    const [title, rest] = opening(reason, "Referred to a person.");
    return [title, handedOver(rest)];
  }
  if (s === "closed_no_action") {
    const leading = cause ? "The investigator's leading cause: " + cause.charAt(0).toLowerCase() + cause.slice(1) : "";
    if (d.Headline) return [d.Headline, leading];
    // A person's grounds for denying follow as they wrote them.
    if (action && action.State === "denied" && action.Decision !== "deny" && fx) return [cap(fx.noun) + " denied.", reason];
    const [title, rest] = opening(reason, "No fix met the checks and policy.");
    return [title, rest || leading];
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
  // The fix a restore undoes, for saying what didn't help.
  const undone = actions.filter((a) => !(a.Params || {}).restores).pop();
  const lead = leadCause(inv);
  const ctx = {
    fx, action,
    undone: undone ? fixWords(undone.Capability, undone.Params, t.name) : fx,
    cause: lead ? causeText(lead.rootCauseClass, (d.Causes || {})[lead.rootCauseClass]) : null,
    restored: actions.some((a) => (a.Params || {}).restores && a.Execution && a.Execution.Status === "succeeded"),
    recommended: actions.some((a) => a.PolicyID === GITOPS),
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

    action ? decision(action, d, ctx, true, reload) : suspicion(d, null, "fix"),
    SUGGESTABLE.includes(inc.state) && can("approver") ? h("div", { class: "more" }, suggestForm(inc, d.Evidence || [], reload)) : null,

    findings(d, source ? source.CitedPredicates || [] : null, fx ? fx.noun : "fix"),

    h("div", { class: "more" },
      actions.length > 1 ? h("details", { class: "fold", "data-key": "earlier" },
        h("summary", {}, "Earlier changes", h("span", { class: "n" }, actions.length - 1)),
        actions.slice(0, -1).reverse().map((a) => decision(a, d, ctx, false, reload))) : null,
      investigatorNote(inv, source && source.LeaseID ? source : null, d),
      fromPerson ? h("details", { class: "fold", "data-key": "suggestion" },
        h("summary", {}, "The suggested fix", h("span", { class: "claim" }, "Written by the suggester")),
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
    h("div", {}, h("b", {}, "Not loaded: "), e.message)));
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
    : me.initialAdmin ? "Signed in with the initial admin password"
    : me.token ? "Signed in with the break-glass token"
    : me.username + (me.role ? " · " + ROLE_LABEL[me.role] : "");
  document.getElementById("logout").hidden = !me;
  document.querySelector(".bar nav").hidden = !me || !me.role || me.temporaryPassword || me.closed || me.initialAdmin;
  if (!me) return login();
  // A temporary password opens nothing but the page that replaces it.
  if (me.temporaryPassword) {
    document.title = "Password · Themis";
    return changePassword();
  }
  // Without an admin account, Themis serves nothing but the making of the
  // first; that is also all the initial admin password ever does.
  if (me.closed) {
    document.title = "Themis";
    return firstAdmin();
  }
  if (me.initialAdmin) {
    document.title = "Sign in · Themis";
    return signInAsAdmin();
  }
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
  else if (hash === "#/setup" && can("admin")) [run, nav] = [setup, "nav-setup"];
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
