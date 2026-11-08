// Themis UI. Every value from the API is untrusted, evidence most of all:
// elements are built with textContent only, never innerHTML.
//
// The words follow docs/writing.md. This file holds no less-than sign: the
// test that keeps internal words off the page reads markup-like spans.
"use strict";

const TOKEN_KEY = "themis-token";
const POLL_MS = 3000;
// Status checks every part of the install, so it refreshes less often.
const STATUS_POLL_MS = 15000;
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

// setChildren replaces el's children, skipping the empty ones h skips: the
// DOM would print a null as "null".
function setChildren(el, ...children) {
  el.replaceChildren(...children.flat(Infinity).filter((c) => c !== null && c !== undefined && c !== false && c !== ""));
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
// span says how long s seconds are, as the timeline writes durations: 45s,
// 4m, 2h.
function span(s) {
  if (90 > s) return Math.round(s) + "s";
  if (90 * 60 > s) return Math.round(s / 60) + "m";
  return Math.round(s / 3600) + "h";
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

// A fix, in the words of each thing a page says about it, as the fix words
// the change: an action's, a suggestion's, or an incident's in brief. The
// button "Approve the raise" makes "Raise approved.", and so on for each.
const ROLLBACK = "deployment.rollback-to-revision";
const PARTIAL_ROLLBACK = "deployment.roll-back-changes";
function fixWords(x) {
  const w = (x && (x.Words || x.words)) || {};
  const act = w.act || "Apply a change";
  return { noun: w.noun || "change", short: act, question: act + "?", doing: w.doing || "Applying the change.",
    done: w.done || "Changed.", verifies: w.verifies || "", note: w.note || "" };
}

// REFERRAL ends what Themis says when it hands an incident to a person: one
// with no fix applied then waits for the fix they suggest.
const REFERRAL = /\s*Referred to a person\.\s*$/;

// changedOutside says, in the list, why a newer incident covers one the
// workload's change outside Themis replaced, from the reason Themis wrote:
// it still fails, or Themis can't tell whether it recovered.
function changedOutside(reason) {
  if (/, and still fails\./.test(reason)) return "Changed outside Themis and still failing. ";
  if (/ Unknown whether it recovered\./.test(reason)) return "Changed outside Themis. Unknown whether it recovered. ";
  return "Changed outside Themis. ";
}

// States. One name per state, everywhere: docs/writing.md lists them.
const TERMINAL = ["resolved", "contained_escalated", "escalated", "closed_no_action", "self_resolved", "rejected", "canceled"];
const REFERRED = ["escalated", "contained_escalated"];
const WORKING = ["received", "investigating", "diagnosed", "proposed"];
// SUGGESTABLE are the states in which a person may suggest a fix.
const SUGGESTABLE = ["investigating", "awaiting_suggestion", "inconclusive"];
// REFUSING are the states a refused fix leaves for awaiting_suggestion: a
// fix suggested while a person works on the incident waits for another.
const REFUSING = ["diagnosed", "proposed"];

// stateOf names an incident's state, with its tone. how says what the state
// alone can't: whether the fix was undone.
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
  if (state === "recommended") return ["Recommended", "wait"];
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
// AGAINST says which answer of a check is a fact against a fix: against any
// fix ("*"), or against the fix named. Any other check weighs for a fix
// when true. No healthy earlier revision rules out a rollback, and so does a
// rollout that changed nothing the pods run. Failures or throttling the
// latest rollout brought rule out raising a limit, since the new version is
// what fails; failures or throttling it didn't bring rule out the rollback.
// A node short of memory rules out raising a memory limit, which gives it
// none.
const AGAINST = {
  gitops_managed: { "*": "true" },
  config_changed_before_failure: { "*": "true" },
  evidence_has_instructions: { "*": "true" },
  earlier_revision_healthy: { [ROLLBACK]: "false" },
  rollout_changed_the_app: { [ROLLBACK]: "false" },
  failure_onset_after_rollout: { [ROLLBACK]: "false" },
  failures_came_with_rollout: { [ROLLBACK]: "false", "deployment.set-resources": "true" },
  throttling_came_with_rollout: { [ROLLBACK]: "false", "deployment.set-resources": "true" },
  evicted_for_node_memory: { "deployment.set-resources": "true" },
};

// against says whether p's answer is a fact against one of fixes.
function against(p, fixes) {
  const a = AGAINST[p.Name] || {};
  return p.Value !== "unknown" && ["*", ...fixes].some((f) => a[f] === p.Value);
}

// supports says whether p's answer weighs for fix: the answer that isn't
// against it, or true for a check with no bearing of its own on it.
function supports(p, fix) {
  const a = AGAINST[p.Name] || {};
  const no = a["*"] || a[fix];
  return no ? p.Value !== no && p.Value !== "unknown" : p.Value === "true";
}
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
// the first. Its creator chooses its password, and creating it signs this
// browser in as it, in place of the initial admin password or the
// break-glass token.
function firstAdmin() {
  if (!can("admin")) {
    render(h("div", { class: "plate centre" }, h("h1", {}, "Themis has not started"), h("p", {}, CLOSED)));
    return;
  }
  const username = h("input", { type: "text", autocomplete: "username", autocapitalize: "none", required: true });
  const password = h("input", { type: "password", autocomplete: "new-password", minlength: "12", required: true });
  const again = h("input", { type: "password", autocomplete: "new-password", minlength: "12", required: true });
  const error = h("div", { class: "error", role: "alert" });
  const submit = async (e) => {
    e.preventDefault();
    error.textContent = "";
    if (password.value !== again.value) {
      error.textContent = "The passwords don't match.";
      again.value = "";
      again.focus();
      return;
    }
    try {
      await api("/v1/accounts", { method: "POST", body: JSON.stringify({ username: username.value.trim(), role: "admin", password: password.value }) });
    } catch (err) {
      error.textContent = err.message;
      return;
    }
    try { sessionStorage.removeItem(TOKEN_KEY); } catch {}
    location.hash = "#/setup";
    me = null;
    route();
  };
  render(h("div", { class: "plate centre" },
    h("h1", {}, "Create the first admin account"),
    h("p", {}, CLOSED),
    h("form", { class: "form", onsubmit: submit },
      field("Username", username),
      field("Password", password, "12 characters or more."),
      field("Password again", again),
      h("div", {}, h("button", { class: "btn primary", type: "submit" }, "Create and sign in")),
      error)));
  username.focus();
}

// signInAsAdmin follows the first admin account's making when this browser
// still holds the initial admin password's session.
function signInAsAdmin() {
  const pw = passwordForm(true);
  render(h("div", { class: "plate centre" }, h("h1", {}, "Sign in with the admin account"), pw.form));
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
      ? "Temporary password. Choose your own to continue."
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
        a.locked ? h("div", { class: "quiet small" }, "Locked. Too many wrong passwords.") : null,
        a.temporaryPassword ? h("div", { class: "quiet small" }, "Temporary password. A new one is due at the next sign-in.") : null),
      h("td", {}, r),
      h("td", { class: "quiet small" }, "By " + a.createdBy + ", " + when(a.createdAt)),
      h("td", {}, h("div", { class: "inline" }, pw, reset, remove)));
  });
  const said = flash;
  flash = null;
  render(
    h("h1", {}, "Accounts"),
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
      h("p", { class: "quiet small" }, "Newest first."),
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

// partChoice says how to turn on a part that is off by choice: a choice to
// make, not a remedy.
function partChoice(c) {
  return c.turnOn ? h("div", { class: "turn-on" }, c.turnOn) : null;
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
      setChildren(out, ...steps.map((s, i) => h("div", { class: "sub" },
        h("h3", {}, (i + 1) + ". " + s.title), h("p", { class: "quiet" }, s.text), s.code ? copyable(s.code) : null)));
    } catch (e) {
      setChildren(out, h("div", { class: "error" }, e.message));
    }
  };
  ns.addEventListener("change", show);
  show();
  return h("div", { class: "sub" },
    h("div", { class: "inline" }, field("Alertmanager's namespace", ns)),
    out);
}

// alertsSaid is what the alerts step just changed, so Setup says so once.
let alertsSaid = null;

// SWITCH names, for each mode, the other mode and the button that switches to it.
const SWITCH = { poll: ["push", "Switch to push mode"], push: ["poll", "Switch to poll mode"] };

// ADDRESS_FROM says where the Alertmanager address in use comes from.
const ADDRESS_FROM = {
  install: "Set in Helm. Read-only here.",
  found: "Found in the cluster at install.",
  setup: "Set on this page.",
};

// testFinding says what Alertmanager listed when tested.
function testFinding(r) {
  const firing = r.firing.map((f) => f.name + " (" + f.count + ")").join(", ");
  return [
    "Alertmanager" + (r.version ? " " + r.version : "") + " answered. ",
    r.active === 1 ? "1 alert is active. " : r.active + " alerts are active. ",
    firing ? "Of the alerts Themis answers, these fire now: " + firing + "." : "None of the alerts Themis answers fires now.",
  ];
}

// alertRules says which of the alerts Themis answers have rules in
// Prometheus, and offers the rules Themis ships for those missing.
function alertRules() {
  const out = h("div", { class: "sub" });
  (async () => {
    let v;
    try {
      v = await api("/v1/setup/alert-rules");
    } catch (e) {
      setChildren(out, h("div", { class: "error" }, e.message));
      return;
    }
    const have = v.exist.length === 1 ? " has a rule" : " have rules";
    const finding = v.checked
      ? (v.missing.length
        ? v.exist.length + " of " + v.answered.length + " alerts" + have + " in Prometheus. Missing: " + v.missing.join(", ") + "."
        : v.exist.length === 1 ? v.exist[0] + " has a rule in Prometheus." : "All " + v.exist.length + " alerts have rules in Prometheus.")
      : (v.error ? v.error + " " : "Not checked: no Prometheus address. ") + "Check by hand that Prometheus has rules for " + v.answered.join(", ") + ".";
    const values = "alerting:\n  rules:\n    install: true" + (v.standard ? "\n    standard: true" : "");
    setChildren(out, 
      h("h3", {}, "Alert rules"),
      h("p", {}, finding),
      v.unshipped.length ? h("p", { class: "quiet" }, "Themis ships no rule for " + v.unshipped.join(", ") + ". Add one for each.") : null,
      v.ruleFile ? h("details", { class: "fold sub", "data-key": "alert-rules" },
        h("summary", {}, v.checked ? "The rules missing" : "The rules Themis ships for them"),
        h("h3", {}, "With the Prometheus Operator"),
        h("p", { class: "quiet" }, "Install them with the chart, labelled as the first Prometheus it finds selects rules. For another Prometheus, set alerting.rules.labels too."),
        copyable(values),
        h("p", { class: "quiet" }, "Or apply them yourself, with those labels."),
        copyable(v.prometheusRule),
        h("h3", {}, "Any other Prometheus"),
        h("p", { class: "quiet" }, "Add this file to Prometheus's rule files."),
        copyable(v.ruleFile)) : null);
  })();
  return out;
}

// alertsStep is how alerts reach Themis. In poll mode, Themis reads
// Alertmanager: the address in use, where it comes from, and a test. In push
// mode, Alertmanager sends them: the steps for this install. An admin may
// switch modes, unless Helm set the mode.
function alertsStep(v, check) {
  const said = alertsSaid;
  alertsSaid = null;
  const [other, label] = SWITCH[v.mode] || SWITCH.poll;
  const switchError = h("div", { class: "error", role: "alert" });
  const switchMode = v.modeByInstall ? h("p", { class: "quiet sub" }, "Mode set in Helm.") : h("div", { class: "sub" },
    h("button", { class: "btn", type: "button", onclick: async () => {
      switchError.textContent = "";
      try {
        await api("/v1/settings/alertmanager", { method: "PUT", body: JSON.stringify({ mode: other }) });
        alertsSaid = "Switched to " + other + " mode.";
        route();
      } catch (err) {
        switchError.textContent = err.message;
      }
    } }, label),
    switchError);
  const head = [said ? h("p", { class: "flash", role: "status" }, said) : null, h("p", {}, partFinding(check))];
  if (v.mode === "push") return [head, alertmanagerSetup(), switchMode, alertRules()];

  const locked = v.from === "install";
  const address = h("input", { type: "text", value: v.address, placeholder: "http://alertmanager-operated.monitoring:9093", autocapitalize: "none", disabled: locked });
  const from = v.from === "setup" && v.found ? ADDRESS_FROM.setup + " Empty it to go back to " + v.found + "."
    : ADDRESS_FROM[v.from] || "Not found in the cluster at install.";
  const error = h("div", { class: "error", role: "alert" });
  const result = h("p", { role: "status" });
  const save = async (e) => {
    e.preventDefault();
    error.textContent = "";
    try {
      await api("/v1/settings/alertmanager", { method: "PUT", body: JSON.stringify({ address: address.value.trim() }) });
      alertsSaid = "Address saved. Used from the next read.";
      route();
    } catch (err) {
      error.textContent = err.message;
    }
  };
  const test = async () => {
    error.textContent = "";
    setChildren(result, "Testing.");
    try {
      const r = await api("/v1/setup/alertmanager/test", { method: "POST", body: JSON.stringify({ address: locked ? "" : address.value.trim() }) });
      setChildren(result, ...testFinding(r));
    } catch (err) {
      result.replaceChildren();
      error.textContent = err.message;
    }
  };
  return [
    head,
    h("form", { class: "form", onsubmit: save },
      field("Alertmanager's address", address, from),
      h("div", { class: "inline" },
        locked ? null : h("button", { class: "btn primary", type: "submit" }, "Save the address"),
        h("button", { class: "btn", type: "button", onclick: test }, "Test")),
      error, result),
    switchMode,
    alertRules()];
}

// modelSaved says the model was just saved, so Setup says so once.
let modelSaved = false;

// PROVIDERS name the APIs the investigator speaks; PROVIDER_CHOICES offer them.
const PROVIDERS = { openai: "OpenAI-compatible", anthropic: "Anthropic" };
const PROVIDER_CHOICES = { openai: "OpenAI-compatible: OpenAI, OpenRouter, Ollama, vLLM", anthropic: "Anthropic" };

// REPLY_FORMATS are how an OpenAI-compatible API is asked for its reply: the
// name on the page, strict output, and the form the schema is sent in.
const REPLY_FORMATS = {
  strict: ["Strict", true, "as-is"],
  "openai-strict": ["OpenAI's strict form", true, "openai-strict"],
  loose: ["Not strict", false, "as-is"],
};
const replyFormat = (v) => (!v.strictOutput ? "loose" : v.schemaDialect === "openai-strict" ? "openai-strict" : "strict");

// choice is a select of options, {value: label}, with value chosen.
function choice(options, value) {
  return h("select", {}, Object.entries(options).map(([k, text]) => h("option", { value: k, selected: k === value }, text)));
}

// modelStep sets the model the install left unset, and tests its connection.
// What the install set is shown, not offered: a change here would be undone by
// the next upgrade. Only the settings the chosen provider uses are asked for.
function modelStep(v) {
  const saved = modelSaved;
  modelSaved = false;
  const byInstall = (name) => v.byInstall.includes(name);
  // The reply format is two settings: the install may have made either.
  const formatByInstall = byInstall("model.strictOutput") || byInstall("model.schemaDialect");
  const provider = choice(PROVIDER_CHOICES, v.provider);
  const chosen = () => (byInstall("model.provider") ? v.provider : provider.value);
  const url = h("input", { type: "text", value: v.baseURL, placeholder: "https://api.openai.com/v1", autocapitalize: "none" });
  const name = h("input", { type: "text", value: v.name, autocapitalize: "none" });
  const key = h("input", { type: "password", autocomplete: "off" });
  const format = choice(Object.fromEntries(Object.entries(REPLY_FORMATS).map(([k, [text]]) => [k, text])), replyFormat(v));
  const error = h("div", { class: "error", role: "alert" });
  const result = h("p", { class: "test-result", role: "status" });

  // change is what the form sets: what the install left, and the provider uses.
  const change = () => {
    const c = {};
    const openai = chosen() === "openai";
    if (!byInstall("model.provider")) c.provider = provider.value;
    if (openai && !byInstall("model.baseURL")) c.baseURL = url.value.trim();
    if (!byInstall("model.name")) c.name = name.value.trim();
    if (!byInstall("model.apiKey") && key.value) c.apiKey = key.value;
    if (openai && !formatByInstall) [, c.strictOutput, c.schemaDialect] = REPLY_FORMATS[format.value];
    return c;
  };
  const save = async (e) => {
    e.preventDefault();
    error.textContent = "";
    try {
      await api("/v1/settings/model", { method: "PUT", body: JSON.stringify(change()) });
      modelSaved = true;
      route();
    } catch (err) {
      error.textContent = err.message;
    }
  };
  // The test sends what the form holds, saved or not; nothing is saved.
  const test = h("button", { class: "btn", type: "button", onclick: async () => {
    test.disabled = true;
    error.textContent = "";
    result.className = "test-result";
    setChildren(result, "Testing. The model has 20 s to answer.");
    try {
      const r = await api("/v1/settings/model/test", { method: "POST", body: JSON.stringify(change()) });
      result.className = "test-result " + (r.ok ? "good" : "bad");
      setChildren(result, r.text, ...(r.outcome === "no_investigator" ? [" ", h("a", { href: "#/status" }, "Open Status")] : []));
    } catch (err) {
      result.className = "test-result bad";
      setChildren(result, err.message);
    } finally {
      test.disabled = false;
    }
  } }, "Test the connection");

  const form = h("form", { class: "form", onsubmit: save });
  const fill = () => {
    const anthropic = chosen() === "anthropic";
    name.placeholder = anthropic ? "claude-sonnet-5-5" : "gpt-5 or qwen3:8b";
    key.placeholder = v.apiKeySet ? "Set. Leave empty to keep it." : anthropic ? "Required" : "Optional";
    const asked = [
      byInstall("model.provider") ? null : field("Provider", provider),
      anthropic || byInstall("model.baseURL") ? null : field("Address", url),
      byInstall("model.name") ? null : field("Model", name),
      byInstall("model.apiKey") ? null : field("API key", key, "Stored encrypted. Never shown again."),
      anthropic || formatByInstall ? null : field("Reply format", format, "For OpenAI, choose OpenAI's strict form."),
    ].filter(Boolean);
    setChildren(form, ...asked,
      h("div", { class: "inline" }, asked.length ? h("button", { class: "btn primary", type: "submit" }, "Save the model") : null, test),
      result, error);
  };
  provider.addEventListener("change", fill);
  fill();

  // What Helm set, once, as facts.
  const fixed = [
    byInstall("model.provider") ? ["Provider", PROVIDERS[v.provider] || v.provider] : null,
    byInstall("model.baseURL") && v.baseURL ? ["Address", h("code", {}, v.baseURL)] : null,
    byInstall("model.name") ? ["Model", h("code", {}, v.name)] : null,
    byInstall("model.apiKey") ? ["API key", "Set"] : null,
    formatByInstall && v.provider === "openai" ? ["Reply format", REPLY_FORMATS[replyFormat(v)][0]] : null,
  ].filter(Boolean);
  if (fixed.length) form.classList.add("sub");
  return [
    saved ? h("p", { class: "flash", role: "status" }, "Model saved. Used from the next investigation.") : null,
    fixed.length ? [h("p", { class: "quiet" }, "Set in Helm."),
      h("dl", { class: "tech" }, fixed.map(([k, value]) => [h("dt", {}, k), h("dd", {}, value)]))] : null,
    form];
}

// setup walks through what an install needs, in order, each step marked
// done when the status page says so.
async function setup() {
  const { checks } = await api("/v1/status");
  const check = (name) => checks.find((c) => c.name === name) || { state: "waiting" };
  const done = (name) => check(name).state === "ok";
  let model = null;
  try { model = await api("/v1/settings/model"); } catch {}
  let alertmanager = { mode: "push", modeByInstall: true };
  try { alertmanager = await api("/v1/settings/alertmanager"); } catch {}
  const signIn = check("Sign-in");
  const acting = checks.filter(needsAction).length;
  const steps = [
    { title: "Sign-in", status: done("Sign-in") ? "Done" : "Not done", body: done("Sign-in") ? [h("p", {}, "Signed in as ", h("b", {}, me.username), ".")] : [
      h("p", {}, partFinding(signIn)),
      h("ul", { class: "remedies" },
        h("li", {}, h("a", { href: "#/accounts" }, "Create accounts")),
        h("li", {}, h("a", { href: OIDC_DOCS, target: "_blank", rel: "noopener" }, "Configure the identity provider (Helm: oidc values)")))] },
    // Without the investigator, no model is used: Check says how to turn it on.
    check("Investigator").state !== "off" && model
      ? { title: "The model", status: model.complete ? "Done" : "Not done", body: modelStep(model) } : null,
    { title: "Alerts", status: done("Alerts") ? "Done" : "Not done", body: alertsStep(alertmanager, check("Alerts")) },
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
// each with its finding and remedy, and each part off by choice with how to
// turn it on.
function digest(checks) {
  const parts = [...checks].sort((a, b) => (URGENCY[a.state] ?? 2) - (URGENCY[b.state] ?? 2));
  return h("ul", { class: "parts" }, parts.map((c) => h("li", {},
    h("b", {}, c.name), partState(c),
    needsAction(c) || c.state === "off"
      ? h("div", { class: "says" }, partFinding(c), c.fix ? h("div", { class: "fix" }, c.fix) : null, partChoice(c)) : null)));
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
          partChoice(c),
          c.name === "Alerts" && c.state !== "ok" && can("admin") ? h("p", { class: "sub" }, h("a", { href: "#/setup" }, "Connect Alertmanager on the Setup page")) : null)))))),
    h("p", { class: "sub" }, h("button", { class: "btn", type: "button", onclick: route }, "Check again")));
}

// The queue ---------------------------------------------------------------

// queueGroup places an incident: what waits for the reader and they can act
// on, what is still open, and what is closed.
function queueGroup(i) {
  if (i.supersededBy) return "closed";
  if (i.state === "awaiting_approval") return can("approver") ? "needs" : "open";
  if (i.state === "awaiting_suggestion" || i.state === "inconclusive") return can("approver") ? "needs" : "open";
  if (i.state === "recommended") return can("responder") ? "needs" : "open";
  if (REFERRED.includes(i.state)) return can("responder") ? "needs" : "open";
  return TERMINAL.includes(i.state) ? "closed" : "open";
}

// GITOPS and EXECUTOR are the rules by which Themis only recommends a fix
// policy allows, for a person to make the change: Argo CD or Flux manages
// the workload, or the executor may not apply the fix there.
const GITOPS = "gitops-owns-the-target";
const EXECUTOR = "executor-may-not-apply";
const RECOMMENDING = [GITOPS, EXECUTOR];

// nextStep is how a person makes the change Themis recommends: in Git
// under GitOps, or else themselves.
function nextStep(rule) {
  return rule === GITOPS ? "Apply it in Git." : "Apply it yourself.";
}

// listHow tells, from an incident's summary, a fix undone. A restore that
// didn't happen says "Not restored".
function listHow(i) {
  const s = i.summary || {};
  return {
    restored: i.state === "escalated" && s.from === "restoring" && !String(s.reason || "").startsWith("Not restored"),
  };
}

// rowWhat is one plain sentence of what happened and what Themis proposes.
function rowWhat(i) {
  const s = i.summary || {};
  const cause = s.cause ? causeText(s.cause.name, s.cause.description) : null;
  const fx = s.fix ? fixWords(s.fix) : null;
  const em = (text) => (text ? h("em", {}, text) : null);
  if (i.supersededBy) return [changedOutside(s.reason || ""), em("A newer incident covers it.")];
  if (i.state === "awaiting_approval" && fx) return [cause ? cause + " " : null, em(fx.short + "?")];
  if (i.state === "recommended" && fx) return [cause ? cause + " " : null, em(sentence(fx.short) + " " + nextStep(s.rule))];
  if (WORKING.includes(i.state)) return "Collecting and weighing evidence.";
  if (i.state === "awaiting_suggestion" && REFERRAL.test(s.reason || "")) return [cause ? cause + " " : null, em("Referred to a person.")];
  if (i.state === "awaiting_suggestion" && s.from === "awaiting_approval" && fx) return [cause ? cause + " " : null, em(cap(fx.noun) + " denied. Waiting for another fix.")];
  if (i.state === "awaiting_suggestion" && REFUSING.includes(s.from)) return [cause ? cause + " " : null, em("Suggested fix refused. Waiting for another fix.")];
  if (i.state === "awaiting_suggestion" || i.state === "inconclusive") return [cause ? cause + " " : null, em("No fix proposed yet.")];
  if (i.state === "closed_no_action" && cause) return [cause + " ", em(s.reason)];
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
  return e ? e.Kind : "evidence " + String(id).slice(-8);
};
const sources = (d, ids) => [...new Set((ids || []).map((id) => evidenceName(d, id)))].join(", ");

// clock is a moment as a page of one incident says it: the time today, the
// time yesterday, or the day and time.
function clock(t) {
  const d = new Date(t);
  const day = new Date();
  if (sameDay(d, day)) return CLOCK.format(d);
  day.setDate(day.getDate() - 1);
  if (sameDay(d, day)) return CLOCK.format(d) + " yesterday";
  return when(t);
}

// said is a line the API wrote in pieces: words, literal cluster values in
// monospace, and moments in the reader's time zone.
function said(p) {
  return (p || []).map((x) => (x.value ? h("span", { class: "val" }, x.value)
    : x.time ? h("time", { datetime: x.time, title: when(x.time) }, clock(x.time)) : x.text));
}

// openEvidence opens the piece of evidence numbered n, brings it into view
// and marks it for a moment.
function openEvidence(n) {
  const ev = document.getElementById("ev-" + n);
  if (!ev) return;
  ev.open = true;
  const still = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  if (ev.scrollIntoView) ev.scrollIntoView({ behavior: still ? "auto" : "smooth", block: "start" });
  ev.classList.remove("lit");
  void ev.offsetWidth;
  ev.classList.add("lit");
  const s = ev.querySelector("summary");
  if (s) s.focus({ preventScroll: true });
}

// evidenceLink points at a piece of evidence by its number, as text says it.
function evidenceLink(n, text, aria) {
  return h("a", { href: "#ev-" + n, "aria-label": aria, onclick: (e) => { e.preventDefault(); openEvidence(n); } }, text);
}

// refs cite evidence by number, as superscripts after a fact.
function refs(cites) {
  return (cites || []).map((c, i) => [i ? h("span", { class: "ref-sep", "aria-hidden": "true" }, ",") : null,
    h("a", { class: "ref", href: "#ev-" + c.n, "aria-label": "Evidence " + c.n + ", " + c.kind,
      onclick: (e) => { e.preventDefault(); openEvidence(c.n); } }, String(c.n))]);
}

// citing links the evidence of ids, as a finding cites it: "evidence 1, 4".
function citing(d, ids) {
  const items = [...new Set(ids || [])].map((id) => (d.Evidence || []).find((x) => x.ID === id)).filter(Boolean)
    .sort((a, b) => a.Number - b.Number);
  if (!items.length) return null;
  return ["evidence ", items.map((e, i) => [i ? ", " : "", evidenceLink(e.Number, String(e.Number), "Evidence " + e.Number + ", " + e.Kind)])];
}

// factsBlock is what the checks found that matters most, each fact citing
// its evidence.
function factsBlock(d) {
  const facts = d.Facts || [];
  if (!facts.length) return null;
  return h("section", { class: "facts", "aria-label": "The facts" }, facts.map((f) => h("p", {}, said(f.words), refs(f.cites))));
}

// withheld says why some of a change's values aren't shown, when some aren't:
// they look like secrets, or they show only that they change.
function withheld(changes) {
  const summed = changes.filter((c) => c.withheld === "summary").map((c) => c.field);
  return [
    changes.some((c) => c.withheld === "secret") ? "Values that look like secrets are hidden." : "",
    !summed.length ? "" : summed.every((f) => f === "command" || f === "args") ? "Commands and arguments show only that they change."
      : "Fields marked changed show only that they change.",
  ].filter(Boolean).join(" ");
}

// digestOf shows an image's digest below its tag, which can move to other
// code: shortened, with the whole digest on hover, or that it isn't known,
// as when no pod runs that image any more.
function digestOf(d) {
  if (!d) return h("div", { class: "sha" }, "Image digest unknown");
  return h("div", { class: "sha", title: d }, d.length > 19 ? d.slice(0, 19) + "…" : d);
}

// changesTable lists what a change does to the pod template. An image shows
// the digest its pods run too, from radius.images, so nobody takes back new
// code without seeing which.
function changesTable(changes, radius) {
  if (!changes || changes.length === 0) return h("p", { class: "quiet" }, "No field of the pod template changes.");
  const note = withheld(changes);
  const images = (radius && radius.images) || {};
  const value = (c, side) => (c.field === "image" ? [c[side], digestOf((images[c.container] || {})[side])] : c[side]);
  return [
    h("table", { class: "diff" },
      h("thead", {}, h("tr", {}, h("th", {}, "Container"), h("th", {}, "Field"), h("th", {}, "Now"), h("th", {}, "After"))),
      h("tbody", {}, changes.map((c) => h("tr", {},
        h("td", {}, h("code", {}, c.container)), h("td", {}, h("code", {}, c.field)),
        h("td", { class: "val from" }, value(c, "before")), h("td", { class: "val to" }, value(c, "after")))))),
    note ? h("p", { class: "quiet small sub" }, note) : null,
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
      h("div", { class: "src" }, x.s.origin ? cap(x.s.origin) : [x.e.Kind, " at ", h("code", {}, x.s.at || "/")],
        " · ", evidenceLink(x.e.Number, "evidence " + x.e.Number, "Evidence " + x.e.Number + ", " + x.e.Kind)),
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

// observed is what verifying a change found, such as "2 of 3 pods are
// ready.", or null when it recorded nothing to say.
function observed(a) {
  const o = (a && a.Verification && a.Verification.Observation) || {};
  if (o.Facts) return o.Facts;
  const n = (v) => typeof v === "number";
  return n(o.ReadyReplicas) && n(o.DesiredReplicas) ? o.ReadyReplicas + " of " + o.DesiredReplicas + " pods are ready." : null;
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

// restarts says how many pods a change restarts, and how many at a time,
// from its blast radius: replicas, and atOnce as its rollout strategy has it.
function restarts(radius) {
  const r = radius || {};
  if (typeof r.replicas !== "number") return null;
  const n = r.replicas;
  const pods = h("b", {}, plural(n, "pod", "pods"));
  if (n === 0) return "No pod runs, so none restarts. ";
  if (n === 1 || typeof r.atOnce !== "number") return ["Restarts ", pods, ". "];
  if (r.atOnce >= n) return ["Restarts all ", pods, " at once. "];
  return ["Restarts ", pods, r.atOnce === 1 ? ", one at a time. " : ", up to " + r.atOnce + " at a time. "];
}

// decision is a change: exactly what it does, and the decision on it, or
// what became of it.
function decision(a, d, ctx, latest, reload) {
  const t = workload(d.Incident.target);
  const fx = fixWords(a);
  const awaiting = a.State === "awaiting_approval";
  const gitOps = a.PolicyID === GITOPS;
  const recommending = RECOMMENDING.includes(a.PolicyID);
  const facts = [
    awaiting || recommending ? restarts(a.BlastRadius) : null,
    awaiting && a.ExpiresAt ? ["Approve before ", h("b", {}, when(a.ExpiresAt)), ". "] : null,
    awaiting ? "Applies exactly this change. " : null,
    awaiting && fx.verifies ? "Then checks that " + fx.verifies + ". " : null,
    awaiting ? "Void if " + t.name + " changes first." : null,
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
      onclick: () => decide("deny", { reason: reason.value }, deny, cap(fx.noun) + " denied. Waiting for another fix.") }, "Deny the " + fx.noun);
    const close = h("button", { class: "btn deny", type: "button",
      onclick: () => decide("deny", { reason: reason.value, close: true }, close, cap(fx.noun) + " denied. Closed, no change.") }, "Deny and close");
    // With another approver required, whoever suggested the fix can't approve it.
    const suggested = (d.Proposals || []).find((p) => p.ID === a.ProposalID);
    const own = d.AnotherApprover && me && suggested && !suggested.LeaseID
      && (suggested.Provenance || {}).source === "person:" + me.username;
    side = h("div", { class: "act" },
      own ? h("p", { class: "quiet" }, "Another approver must approve this fix.") : approve,
      field("Reason for denying (optional)", reason), deny, close, error);
  } else if (awaiting) {
    side = h("div", { class: "act" }, h("p", { class: "quiet" }, "Approving or denying requires the approver role."));
  } else if (recommending && latest && d.Incident.state === "recommended") {
    const commands = a.Commands || [];
    const how = gitOps ? h("p", {}, "Apply this change in Git.")
      : commands.length ? [h("p", {}, commands.length > 1 ? "Run these commands:" : "Run this command:"), copyable(commands.join("\n"))]
        : h("p", {}, "Make this change with kubectl, or where " + t.name + "'s manifest lives.");
    side = h("div", { class: "act" }, h("h3", {}, "Next step"), how,
      h("p", { class: "fine" }, "Themis checks the change once it is made."));
  } else if (recommending) {
    side = h("div", {}, h("h3", {}, "Decision"), h("p", {}, "Recommended. " + a.DecisionReason));
  } else {
    side = h("div", {}, h("h3", {}, "Decision"), result(a));
  }
  // Something else deploys the workload, such as Helm: its next deploy writes
  // the template it has over this change. Under GitOps, it is made in Git.
  const deployer = (awaiting || recommending) && !gitOps && !(a.Params || {}).restores && (a.BlastRadius || {}).deployedBy;
  return h("section", { class: "decide", "aria-label": latest ? "The change" : "An earlier change" },
    h("div", {},
      h("h3", {}, latest ? (recommending ? "The recommended change" : "The exact change") : cap(fx.short)),
      changesTable(a.Changes, a.BlastRadius),
      fx.note ? h("p", { class: "terms" }, fx.note) : null,
      facts.some(Boolean) ? h("p", { class: "terms" }, facts) : null,
      deployer ? h("p", { class: "terms", title: "Deployed by " + deployer }, "Apply the same change where " + t.name + "'s manifest lives, or the next deploy undoes it.") : null,
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
// the same bearing on the fix. The cited one leads, and the fact shows once.
function sameFindings(all, cited) {
  const out = [];
  const byKey = new Map();
  for (const p of all) {
    const ids = [...(p.EvidenceIDs || [])].sort().join(",");
    const key = p.Value !== "unknown" && p.Reason && ids
      ? [p.Value, p.Reason, ids, JSON.stringify(AGAINST[p.Name] || {})].join("|") : null;
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
      h("span", { class: "src" }, unknown ? null : p.Reason, !unknown && p.Reason && citing(d, p.EvidenceIDs) ? " · " : null,
        citing(d, p.EvidenceIDs))));
}

// basis says once what the checks rest on, from the checks that say only
// whether the figures they need are there: "Based on Prometheus's container
// CPU history over 1h.", or what such a check found otherwise.
function basis(d) {
  const checks = d.Checks || {};
  const lines = (d.Predicates || []).filter((p) => (checks[p.Name] || {}).availability).map((p) => {
    const on = p.Value === "true" && (d.Basis || {})[p.Name];
    return on ? "Based on " + on + "." : sentence(statement(d, p));
  });
  return [...new Set(lines)].join(" ");
}

// findings weighs Themis's checks: for and against fix, the fix weighed,
// when there is one, each fact once. Without one, a fact against any fix
// the incident offers, such as no earlier revision to roll back to, is
// against, and what the checks found true is found. A check that says only
// whether figures are there is neither: it shows once, as what the others
// rest on.
function findings(d, cited, noun, fix) {
  const checks = d.Checks || {};
  const all = (d.Predicates || []).filter((p) => !(checks[p.Name] || {}).availability);
  const based = basis(d);
  if (!all.length && !based) return null;
  const fixes = cited ? [fix] : (d.Fixes || []).map((f) => f.name);
  const forFix = [], againstFix = [], unknown = [], other = [];
  for (const g of sameFindings(all, cited)) {
    const p = g.p;
    const isCited = cited && [p, ...g.also].some((x) => cited.includes(x.Name));
    if (p.Value === "unknown") unknown.push(finding(g, d, "unknown"));
    else if (against(p, cited || p.Value === "false" ? fixes : [])) againstFix.push(finding(g, d, "against"));
    else if (isCited) (supports(p, fix) ? forFix : againstFix).push(finding(g, d, supports(p, fix) ? "for" : "against"));
    else if (!cited && p.Value === "true") forFix.push(finding(g, d, "found"));
    else other.push(finding(g, d, null));
  }
  const pan = (cls, title, items, empty) => h("div", { class: "pan " + cls },
    h("h3", {}, title), items.length ? h("ul", {}, items) : h("p", { class: "empty" }, empty));
  const blind = d.Missing && d.Missing.length ? h("div", { class: "blind" },
    h("b", {}, "Missing evidence"), h("ul", {}, d.Missing.map((m) => h("li", {}, m)))) : null;
  const rest = [againstFix.length ? plural(againstFix.length, "is", "are") + " against " + (cited ? "it." : "a fix.") : cited ? "None is against it." : "",
    unknown.length ? plural(unknown.length, "is", "are") + " unknown." : ""];
  // Without a fix weighed, the line counts the checks that found the failure.
  const failing = all.filter((p) => p.Value === "true" && (d.Symptoms || []).includes(p.Name)).length;
  const weigh = [cited ? plural(forFix.length, "check supports", "checks support") + " the " + noun + "."
    : failing ? plural(failing, "check found", "checks found") + " the failure." : "No check found the failure.", ...rest].filter(Boolean).join(" ");
  const mark = againstFix.length ? "against" : (cited ? forFix.length : failing) ? "for" : "unknown";
  return [h("details", { class: "checks", "data-key": "checks" },
    h("summary", {}, h("span", { class: "k " + mark, "aria-hidden": "true" }, MARK[mark]), " ", weigh),
    h("section", { class: "findings", "aria-label": "Findings" },
      h("div", { class: "pans" },
        cited ? pan("for", "Supports the " + noun, forFix, "No check supports it.") : pan("for", "Found", forFix, "No check came out true."),
        cited ? pan("against", "Against, or unknown", [...againstFix, ...unknown], "No check is against it.")
          : pan("against", "Unknown, or against", [...againstFix, ...unknown], "Every check has an answer.")),
      based ? h("p", { class: "quiet small" }, based) : null,
      other.length ? h("details", { class: "fold sub", "data-key": "other-checks" },
        h("summary", {}, "Other checks", h("span", { class: "n" }, other.length)),
        h("div", { class: "pan against" }, h("ul", {}, other))) : null)), blind];
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
    h("b", {}, fixWords(p).short), " ",
    h("span", { class: "state " + (p.Status === "rejected" ? "bad" : "done") }, label(SUGGESTION_LABEL, p.Status)),
    p.RejectionReason ? h("p", {}, label(REJECTION_LABEL, p.RejectionReason)) : null,
    h("dl", { class: "tech sub" }, sourceRows(p)),
    suggestionText(p, d));
}

// The evidence. Each piece is a row: its number, its kind and what it holds
// in one line; opening it lays it out by its kind, with the raw data folded
// under it. The API works out both; the page only draws them.

// table is a table of rows, its columns each a heading and how to show a
// row's cell, with the classes of a column's cells.
function table(rows, columns) {
  return h("table", { class: "data" },
    h("thead", {}, h("tr", {}, columns.map(([title, , cls]) => h("th", { class: cls || null }, title)))),
    h("tbody", {}, rows.map((r) => h("tr", {}, columns.map(([, cell, cls]) => {
      const v = cell(r);
      return h("td", { class: [cls, v && v.cls].filter(Boolean).join(" ") || null }, v && v.cls !== undefined ? v.node : v);
    })))));
}

// placed sets where an element sits along a gauge's track, in percent. The
// page's policy allows no inline style in its markup.
function placed(el, side, at) {
  el.style[side] = at + "%";
  return el;
}

// gaugeFigure is a figure against its scale that labels itself: the figure
// above the fill's end, a request under its tick, the scale's end under the
// track's end.
function gaugeFigure(g) {
  const fill = placed(h("span", { class: "fill" }), "width", g.fill);
  const track = h("span", { class: "track" }, fill, g.tick ? placed(h("span", { class: "tick" }), "left", g.tick.at) : null);
  const label = (text) => {
    const i = text.lastIndexOf(" ");
    return i > 0 ? [text.slice(0, i) + " ", h("b", {}, text.slice(i + 1))] : h("b", {}, text);
  };
  const above = h("span", { class: "above" + (g.fill > 50 ? " left" : "") }, label(g.fillText));
  placed(above, g.fill > 50 ? "right" : "left", g.fill > 50 ? 100 - g.fill : g.fill);
  return h("figure", { class: "gauge", "aria-label": g.aria },
    h("figcaption", {}, g.label),
    h("div", { class: "g", "aria-hidden": "true" }, above, track,
      g.tick ? placed(h("span", { class: "below" }, label(g.tick.text)), "left", g.tick.at) : null,
      h("span", { class: "below end" }, label(g.end))));
}

// runShown remembers which run of logs a reader switched to, by item, so a
// refresh keeps it.
const runShown = new Map();

// logView is a container's logs, each line with its time when it has one,
// how the run ended marked, and buttons to switch between its runs.
function logView(e, v) {
  const runs = v.runs || [];
  const marked = (text) => {
    const parts = String(text).split("[REDACTED]");
    return parts.map((p, i) => [i ? h("span", { class: "redacted" }, "[REDACTED]") : null, p]);
  };
  const shownRun = runShown.has(e.ID) ? runShown.get(e.ID) : 0;
  const stamp = (t) => h("time", { datetime: t, title: when(t) }, CLOCK.format(new Date(t)) + ":" + String(new Date(t).getSeconds()).padStart(2, "0"));
  const blocks = runs.map((r, i) => h("div", { class: "log", hidden: i !== shownRun, tabindex: "0", "aria-label": cap(r.previous ? "previous run" : "current run") },
    r.lines.length ? r.lines.map((l) => h("div", { class: l.flagged ? "flag" : null },
      l.at ? stamp(l.at) : h("span"), h("span", {}, marked(l.text)))) : h("div", {}, h("span"), h("span", { class: "quiet" }, r.note || "No lines.")),
    r.ended ? h("div", { class: "last" }, r.endedAt ? stamp(r.endedAt) : h("span"), h("span", { class: "quiet" }, "Ended " + r.ended + ".")) : null));
  const buttons = runs.map((r, i) => h("button", { type: "button", "aria-pressed": String(i === shownRun),
    onclick: (ev) => {
      runShown.set(e.ID, i);
      for (const [j, b] of buttons.entries()) b.setAttribute("aria-pressed", String(j === i));
      for (const [j, el] of blocks.entries()) el.hidden = j !== i;
      ev.currentTarget.focus();
    } }, r.previous ? "Previous run" : "Current run"));
  return [
    runs.length > 1 ? h("div", { class: "tabs", role: "group", "aria-label": "Run" }, buttons)
      : h("p", { class: "quiet small runname" }, runs[0] && runs[0].previous ? "Previous run" : "Current run"),
    blocks,
  ];
}

// evidenceView lays a piece of evidence out by its kind.
function evidenceView(d, e) {
  const v = e.View || {};
  const yes = (b) => (b ? "Yes" : "No");
  const out = [];
  if (v.pods) {
    out.push(table(v.pods.rows, [
      ["Pod", (r) => r.name, "mono"], ["Revision", (r) => r.revision || "—"],
      ["Ready", (r) => yes(r.ready) + (r.waiting ? ", " + r.waiting : "")], ["Restarts", (r) => String(r.restarts), "num"],
      ["Last ended", (r) => (r.ended ? { cls: r.failed ? "warn" : "", node: [r.ended, r.endedAt ? ", " + clock(r.endedAt) : ""] } : "—")],
    ]));
  }
  if (v.events) {
    out.push(table(v.events.rows, [
      ["Last seen", (r) => (r.lastSeen ? clock(r.lastSeen) : "—")], ["Type", (r) => ({ cls: r.warning ? "warn" : "", node: r.type })],
      ["Reason", (r) => r.reason, "mono"], ["Object", (r) => r.object, "mono"], ["Message", (r) => r.message || "—"],
      ["Count", (r) => String(r.count), "num"],
    ]));
  }
  if (v.logs) out.push(logView(e, v.logs));
  if (v.gauges) out.push(v.gauges.map(gaugeFigure));
  if (v.revisions) {
    out.push(h("div", { class: "revs" }, v.revisions.map((r) => h("div", { class: "rev" },
      h("div", {}, h("b", {}, "Revision " + r.revision), r.current ? h("div", { class: "quiet" }, "current") : null),
      h("div", {}, r.createdAt ? ["Created ", clock(r.createdAt), ". "] : null, r.runs.map((x) => [said(x), " "]),
        (r.changed || []).length ? [h("div", { class: "quiet small" }, "Changed from revision " + r.from + ":"),
          h("ul", { class: "changed" }, r.changed.map((c) => h("li", {}, said(c))))]
          : r.quiet ? h("div", { class: "quiet small" }, r.quiet)
            : r.first ? h("div", { class: "quiet small" }, "No earlier revision.") : null)))));
  }
  if (v.config) {
    out.push(v.config.length ? table(v.config, [
      ["Kind", (r) => r.kind], ["Name", (r) => r.name, "mono"],
      ["Changed", (r) => (r.missing ? { cls: "warn", node: "Missing" } : r.changedAt ? clock(r.changedAt) : r.kind === "Secret" ? "Not read" : "—")],
      ["Optional", (r) => yes(r.optional)],
    ]) : null);
  }
  if (v.fix) {
    out.push(h("p", {}, v.fix.done || "A fix.", " Applied at ", clock(v.fix.appliedAt), " for ",
      h("a", { href: "#/incidents/" + v.fix.incident }, "an earlier incident"),
      v.fix.outcome ? ". " + label(OUTCOME_LABEL, v.fix.outcome) + "." : "."),
    (v.fix.found || []).length ? [h("h3", {}, "What that incident's checks found"), h("ul", {}, v.fix.found.map((f) => h("li", {}, sentence(f))))] : null);
  }
  if (v.health) out.push(h("ul", { class: "lines" }, v.health.map((l) => h("li", {}, said(l)))));
  if (v.fields) out.push(h("dl", { class: "tech" }, v.fields.map((f) => [h("dt", {}, f.name), h("dd", {}, f.value)])));
  for (const n of v.notes || []) out.push(h("p", { class: "logmeta" }, said(n)));
  return out;
}

// evidenceRow is one piece of evidence: what it holds in one line, then, open,
// laid out by its kind with its raw data. Viewers learn only what it is.
function evidenceRow(d, e) {
  const flagged = (e.Suspicious || []).map((s) => h("li", {}, "Suspicious text at ", h("code", {}, s.at || "/"),
    s.origin ? " (" + s.origin + ")" : "", ". The text " + (s.what || "looks written for Themis") + "."));
  return h("details", { class: "ev", id: "ev-" + e.Number, "data-key": "evidence-" + e.ID },
    h("summary", {}, h("span", { class: "no" }, String(e.Number)), h("span", { class: "kind" }, e.Kind),
      h("span", { class: "gist" }, e.Digest ? said(e.Digest) : null),
      e.Later ? h("span", { class: "when" }, clock(e.CollectedAt)) : null),
    h("div", { class: "body" },
      e.Data === undefined ? h("p", { class: "quiet" }, "Readable by responders only.") : evidenceView(d, e),
      flagged.length ? h("ul", { class: "flagged" }, flagged) : null,
      e.Data === undefined ? null : h("details", { class: "raw", "data-key": "raw-" + e.ID },
        h("summary", {}, "Raw data"), h("pre", {}, JSON.stringify(e.Data, null, 2)))));
}

// collected is the piece of evidence a person just had collected, which the
// page lights when it next draws, and says so once.
let collected = null;

// collectMore lets an approver have Themis read more evidence, of the kinds
// the incident's profiles allow, through the same templates it reads with.
// Opening it stops the page refreshing, so nothing chosen is lost.
function collectMore(d, hold, reload) {
  const kinds = d.Collectable || [];
  if (!kinds.length || !can("approver")) return null;
  const status = h("span", { class: "status", role: "status" });
  const args = h("div", { class: "args" });
  let fields = [];
  const what = h("select", { onchange: () => show() }, kinds.map((k, i) => h("option", { value: String(i) }, k.kind)));
  const show = () => {
    const k = kinds[Number(what.value)];
    fields = (k.params || []).map((p) => ({ p, input: (p.choices || []).length
      ? h("select", {}, p.choices.map((c) => h("option", { value: c.value }, c.label)))
      : h("input", { type: "text", autocapitalize: "none", spellcheck: "false" }) }));
    setChildren(args, fields.map((f) => field(f.p.label, f.input)));
  };
  show();
  const button = h("button", { class: "btn small", type: "button", onclick: async () => {
    const k = kinds[Number(what.value)];
    const params = {};
    for (const f of fields) if (f.input.value.trim()) params[f.p.name] = f.input.value.trim();
    button.disabled = true;
    status.textContent = "Collecting.";
    try {
      const r = await api("/v1/incidents/" + encodeURIComponent(d.Incident.id) + "/evidence", { method: "POST", body: JSON.stringify({ template: k.template, params }) });
      collected = r.number;
      reload();
    } catch (e) {
      status.textContent = e.message;
      button.disabled = false;
    }
  } }, "Collect");
  const form = h("div", { class: "collect", hidden: true }, field("Collect more", what), args, button, status);
  const open = h("button", { class: "btn small more-ev", type: "button", onclick: () => {
    open.hidden = true;
    form.hidden = false;
    hold();
    what.focus();
  } }, "Collect more evidence");
  return [open, form];
}

// evidenceSection lists the evidence, numbered as facts and findings cite it.
function evidenceSection(d, hold, reload) {
  const items = d.Evidence || [];
  if (!items.length) return null;
  const lit = collected;
  collected = null;
  return h("section", { class: "evidence", "aria-label": "Evidence" },
    h("h2", {}, "Evidence ", h("span", { class: "n" }, String(items.length)),
      h("span", { class: "quiet small coll" }, "Collected at ", clock(items[0].CollectedAt))),
    h("div", { class: "evs" }, items.map((e) => {
      const row = evidenceRow(d, e);
      if (e.Number === lit) row.classList.add("lit");
      return row;
    })),
    lit ? h("p", { class: "logmeta", role: "status" }, "Collected. Added as evidence " + lit + ".") : null,
    collectMore(d, hold, reload));
}

// timelineFold lists every change of state, each reason as recorded, and
// the evidence people asked for, in order. A step taken for the same reason
// as the one before it reads as the state it reached, not the reason again.
function timelineFold(d) {
  const ts = d.Transitions || [];
  const asked = (d.Evidence || []).filter((e) => e.RequestedBy)
    .map((e) => ({ At: e.CollectedAt, Reason: e.Kind + " collected by " + e.RequestedBy + ".", Asked: true }));
  const all = [...ts, ...asked].sort((a, b) => new Date(a.At) - new Date(b.At));
  if (!all.length) return null;
  return h("details", { class: "fold", "data-key": "timeline" },
    h("summary", {}, "Timeline", h("span", { class: "n" }, plural(all.length, "event", "events"))),
    h("ol", { class: "timeline" }, all.map((t, i) => {
      const again = i > 0 && t.Reason && t.Reason === all[i - 1].Reason && !t.Asked;
      return h("li", {},
        h("time", { datetime: t.At }, when(t.At)),
        h("span", {}, (!again && t.Reason) || stateOf(t.To)[0] + ".", " ",
          t.Asked ? null : h("span", { class: "by" }, label(ACTOR_LABEL, t.Actor))));
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

// Suggesting a fix. The form offers every fix the incident's profiles offer:
// the shipped ones with forms of their own, any other from the JSON Schema
// of its values. Each form's params() returns the fix's values, or throws
// what to correct.

// fixNoun names a kind of fix the incident's profiles offer, in the fix's own
// words: "a probe change" reads "probe change".
function fixNoun(f) {
  return String(f.noun || f.name).replace(/^an? /, "");
}

const NO_CHANGE = "Change at least one value.";

// latestHistory is the rollout history read last: evidence read later, by a
// person or for an approval asked again, says how the workload is now.
function latestHistory(evidence) {
  return evidence.filter((e) => e.Template === "rollout_history" && e.Data).pop();
}

// currentContainers are the workload's containers as its current revision
// has them, with their resources and probes, from the rollout history.
function currentContainers(evidence) {
  const history = latestHistory(evidence);
  const now = history ? (history.Data.revisions || []).find((r) => r.current) : null;
  return now ? now.containers || [] : [];
}

// containerPicker chooses one of the workload's containers, or takes a name
// when the evidence doesn't list them.
function containerPicker(containers, onchange) {
  if (!containers.length) return h("input", { type: "text", required: true, autocapitalize: "none", spellcheck: "false" });
  return h("select", { onchange }, containers.map((c) => h("option", { value: c.name }, c.name)));
}

// rollbackFields choose an earlier revision from the rollout history.
function rollbackFields(f, evidence) {
  const history = latestHistory(evidence);
  const earlier = history ? (history.Data.revisions || []).filter((r) => !r.current)
    .sort((a, b) => Number(b.revision) - Number(a.revision)) : [];
  const revision = earlier.length
    ? h("select", {}, earlier.map((r) => h("option", { value: r.revision },
      "Revision " + r.revision + " · " + (r.containers || []).map((c) => c.image).join(", ") + (r.createdAt ? " · " + when(r.createdAt) : ""))))
    : h("input", { type: "number", min: "1", step: "1", required: true });
  return { first: field("Roll back to", revision), nodes: [], params: () => ({ revision: Number(revision.value) }) };
}

// partialFields choose which of the latest rollout's changes to roll back,
// as the rollout history says them: no value is typed, each goes back as the
// revision before had it.
function partialFields(f, evidence) {
  const history = latestHistory(evidence);
  const change = history ? history.Data.change : null;
  const app = (change && change.app) || [];
  if (!app.length) {
    const why = change ? "The latest rollout changed nothing the pods run." : "The rollout history doesn't say what the latest rollout changed.";
    return { nodes: [h("p", { class: "quiet small" }, why)], params: () => { throw new Error(why); } };
  }
  const boxes = app.map((c) => ({ said: c, box: h("input", { type: "checkbox", value: c }) }));
  const all = h("p", { class: "quiet small" });
  const ticked = () => boxes.filter((b) => b.box.checked).map((b) => b.said);
  const count = () => {
    all.textContent = ticked().length === app.length
      ? "Every change is ticked, the same as a rollback to revision " + change.from + "." : "";
  };
  const list = h("fieldset", { class: "group changes-pick", onchange: count },
    h("legend", {}, "Changes revision " + change.to + " made, to roll back to revision " + change.from),
    boxes.map((b) => h("label", { class: "check" }, b.box, h("span", {}, cap(b.said) + "."))));
  return {
    nodes: [list, all],
    params: () => {
      const changes = ticked();
      if (!changes.length) throw new Error("Tick at least one change.");
      return { revision: Number(change.from), changes };
    },
  };
}

// SUFFIX is what each Kubernetes quantity suffix multiplies by.
const SUFFIX = {
  "": 1, n: 1e-9, u: 1e-6, m: 1e-3, k: 1e3, M: 1e6, G: 1e9, T: 1e12, P: 1e15, E: 1e18,
  Ki: 2 ** 10, Mi: 2 ** 20, Gi: 2 ** 30, Ti: 2 ** 40, Pi: 2 ** 50, Ei: 2 ** 60,
};
// amount is a Kubernetes quantity, such as 512Mi or 250m, as a number; null
// when it isn't one.
function amount(q) {
  const m = String(q).match(/^\+?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|[numkMGTPE])?$/);
  return m ? Number(m[1] + (m[2] || "")) * SUFFIX[m[3] || ""] : null;
}
// sameAmount says two quantities are the same amount, however written, such
// as 0.1 and 100m.
function sameAmount(a, b) {
  const x = amount(a);
  const y = amount(b);
  return x !== null && y !== null && !(Math.abs(x - y) > Math.abs(y) * 1e-9);
}

// RESOURCES are the values a memory and CPU change sets, in the order shown.
const RESOURCES = [
  { kind: "limits", res: "memory", label: "Memory limit", example: "1Gi" },
  { kind: "requests", res: "memory", label: "Memory request", example: "512Mi" },
  { kind: "limits", res: "cpu", label: "CPU limit", example: "500m" },
  { kind: "requests", res: "cpu", label: "CPU request", example: "250m" },
];

// usedBy says how much memory a container used at its peak, and CPU now,
// when a metrics source could tell: "Peak 57Mi.", by each limit's field.
function usedBy(evidence, name) {
  const of = (template) => {
    const e = evidence.filter((x) => x.Template === template && x.Data).pop();
    return e ? (e.Data.containers || []).find((c) => c.name === name) || {} : {};
  };
  const memory = of("container_memory").peakBytes;
  const cpu = of("container_cpu").usageCores;
  return {
    "limits.memory": typeof memory === "number" ? "Peak " + Math.ceil(memory / 2 ** 20) + "Mi." : "",
    "limits.cpu": typeof cpu === "number" ? "Now " + Math.ceil(cpu * 1000) + "m." : "",
  };
}

// resourceFields set a container's memory and CPU limits and requests,
// starting from the values it has now, each in its own field: the browser
// never fills them. An empty field keeps its value.
function resourceFields(f, evidence) {
  const containers = currentContainers(evidence);
  const values = RESOURCES.map((r) => ({ ...r,
    input: h("input", { type: "text", class: "mono", autocomplete: "off", autocapitalize: "none", spellcheck: "false", placeholder: r.example }),
    hint: h("span", { class: "hint" }) }));
  const now = () => containers.find((c) => c.name === picker.value) || {};
  const was = (c, r) => (c[r.kind] || {})[r.res] || "";
  const fill = () => {
    const c = now();
    const used = usedBy(evidence, picker.value);
    for (const r of values) {
      r.input.value = was(c, r);
      const currently = !containers.length ? "" : was(c, r) ? "Currently " + was(c, r) + "." : "Currently not set.";
      r.hint.textContent = [currently, used[r.kind + "." + r.res] || ""].filter(Boolean).join(" ");
    }
  };
  const picker = containerPicker(containers, fill);
  fill();
  const params = () => {
    const c = now();
    const set = {};
    const after = {};
    for (const r of values) {
      const v = r.input.value.trim();
      after[r.kind + "." + r.res] = v || was(c, r);
      if (!v) continue;
      if (amount(v) === null) throw new Error(r.label + ": " + v + " isn't a Kubernetes quantity, such as " + r.example + ".");
      if (sameAmount(v, was(c, r))) continue;
      (set[r.kind] = set[r.kind] || {})[r.res] = v;
    }
    if (!Object.keys(set).length) throw new Error(NO_CHANGE);
    for (const [res, name] of [["memory", "memory"], ["cpu", "CPU"]]) {
      const request = after["requests." + res];
      const limit = after["limits." + res];
      if (request && limit && amount(request) > amount(limit)) {
        throw new Error(cap(name) + " request " + request + " is above the " + name + " limit of " + limit + ".");
      }
    }
    return { containers: { [picker.value.trim()]: set } };
  };
  const box = (r) => h("label", { class: "field" }, r.label, r.input, r.hint);
  return {
    first: field("Container", picker),
    nodes: [h("div", { class: "row2" }, box(values[0]), box(values[1])), h("div", { class: "row2" }, box(values[2]), box(values[3]))],
    params,
  };
}

// PROBES are a container's probes, and TIMING the fields of each, in the
// order shown. Only a readiness probe takes a success threshold.
const PROBES = ["liveness", "readiness", "startup"];
const TIMING = [
  { name: "initialDelaySeconds", label: "Initial delay, seconds", min: 0 },
  { name: "periodSeconds", label: "Period, seconds", min: 1 },
  { name: "timeoutSeconds", label: "Timeout, seconds", min: 1 },
  { name: "failureThreshold", label: "Failure threshold", min: 1 },
  { name: "successThreshold", label: "Success threshold", min: 1, readiness: true },
];

// probeFields change the timing of one of a container's probes, starting
// from the values it has now. An empty field keeps its value.
function probeFields(f, evidence) {
  const containers = currentContainers(evidence);
  const kind = h("select", { onchange: () => fill() });
  const kindField = field("Probe", kind);
  const none = h("p", { class: "quiet small" });
  const box = h("div", { class: "inline" });
  let inputs = [];
  // probes are the container's, or null when the evidence doesn't say.
  const probes = () => {
    const c = containers.find((x) => x.name === picker.value);
    return c ? c.probes || {} : null;
  };
  const fill = () => {
    const p = (probes() || {})[kind.value] || null;
    inputs = !kind.value ? [] : TIMING.filter((t) => !t.readiness || kind.value === "readiness").map((t) => {
      const was = p ? p[t.name] : undefined;
      return { ...t, was, input: h("input", { type: "number", min: t.min, step: "1", value: was }) };
    });
    setChildren(box, ...inputs.map((t) => field(t.label, t.input, t.was === undefined ? null : "Currently " + t.was + ".")));
  };
  const fillKinds = () => {
    const p = probes();
    const kinds = p ? PROBES.filter((k) => p[k]) : PROBES;
    setChildren(kind, ...kinds.map((k) => h("option", { value: k }, cap(k))));
    kindField.hidden = !kinds.length;
    none.textContent = kinds.length ? "" : "Container " + picker.value + " has no probes to change.";
    fill();
  };
  const picker = containerPicker(containers, fillKinds);
  fillKinds();
  const params = () => {
    if (!kind.value) throw new Error(none.textContent);
    const set = {};
    for (const t of inputs) {
      const v = t.input.value.trim();
      if (v !== "" && Number(v) !== t.was) set[t.name] = Number(v);
    }
    if (!Object.keys(set).length) throw new Error(NO_CHANGE);
    return { container: picker.value.trim(), [kind.value]: set };
  };
  return { first: field("Container", picker), nodes: [kindField, none, box], params };
}

// words makes a value's name a label: initialDelaySeconds reads "Initial
// delay seconds".
const words = (name) => cap(String(name).replace(/([a-z0-9])([A-Z])/g, "$1 $2").replaceAll("_", " ").toLowerCase());

// schemaInput is a field for one value of a fix this page has no form for,
// from its JSON Schema: text, a number, a choice, yes or no, or an object of
// those. Anything else is written as JSON. needed says the value is
// required; an empty value is left out otherwise.
function schemaInput(name, schema, needed) {
  const s = schema || {};
  const type = Array.isArray(s.type) ? s.type.find((t) => t !== "null") : s.type;
  const title = words(name);
  if (type === "object" && s.properties) {
    const required = s.required || [];
    const parts = Object.entries(s.properties).map(([k, v]) => [k, schemaInput(k, v, needed && required.includes(k))]);
    return {
      nodes: h("fieldset", { class: "group" }, h("legend", {}, title),
        s.description ? h("p", { class: "quiet small" }, s.description) : null, parts.map(([, p]) => p.nodes)),
      value: () => {
        const out = {};
        for (const [k, p] of parts) {
          const v = p.value();
          if (v !== undefined) out[k] = v;
        }
        return Object.keys(out).length || needed ? out : undefined;
      },
    };
  }
  if (type === "boolean") {
    const box = h("input", { type: "checkbox" });
    return {
      nodes: h("label", { class: "check" }, box, h("span", {}, title, s.description ? ". " + s.description : "")),
      value: () => (box.checked || needed ? box.checked : undefined),
    };
  }
  let input;
  let read;
  if (Array.isArray(s.enum)) {
    input = h("select", { required: needed }, needed ? null : h("option", { value: "" }, "—"),
      s.enum.map((v, i) => h("option", { value: String(i) }, String(v))));
    read = (v) => s.enum[Number(v)];
  } else if (type === "integer" || type === "number") {
    input = h("input", { type: "number", required: needed, min: s.minimum, max: s.maximum, step: type === "integer" ? "1" : "any" });
    read = Number;
  } else if (type === "string") {
    input = h("input", { type: "text", required: needed, autocapitalize: "none", spellcheck: "false" });
    read = (v) => v;
  } else {
    input = h("textarea", { rows: "3", required: needed, spellcheck: "false", placeholder: "JSON" });
    read = (v) => {
      try { return JSON.parse(v); } catch { throw new Error(title + ": not valid JSON."); }
    };
  }
  return {
    nodes: field(title, input, s.description),
    value: () => (input.value.trim() === "" ? undefined : read(input.value.trim())),
  };
}

// schemaFields are the fields of a fix this page has no form for, from the
// JSON Schema of its values, after what the fix does.
function schemaFields(f) {
  const s = f.schema || {};
  const about = f.description ? h("p", { class: "quiet small" }, f.description) : null;
  if (s.type !== "object" || !s.properties) {
    const all = schemaInput("values", {}, true);
    return { nodes: [about, all.nodes], params: () => all.value() };
  }
  const required = s.required || [];
  const parts = Object.entries(s.properties).map(([k, v]) => [k, schemaInput(k, v, required.includes(k))]);
  return {
    nodes: [about, parts.map(([, p]) => p.nodes)],
    params: () => {
      const out = {};
      for (const [k, p] of parts) {
        const v = p.value();
        if (v !== undefined) out[k] = v;
      }
      return out;
    },
  };
}

// FIX_FIELDS are the forms of the fixes Themis ships with.
const FIX_FIELDS = {
  [ROLLBACK]: rollbackFields,
  [PARTIAL_ROLLBACK]: partialFields,
  "deployment.set-resources": resourceFields,
  "deployment.set-probes": probeFields,
};

// suggestForm lets a person suggest any fix the incident's profiles offer.
// Themis checks it exactly as it checks the investigator's: against the
// cluster, then the policy. It cites all the evidence collected.
function suggestForm(d, reload) {
  const inc = d.Incident;
  const t = workload(inc.target);
  const evidence = d.Evidence || [];
  const fixes = (d.Fixes || []).map((f) => ({ ...f, noun: fixNoun(f), fields: (FIX_FIELDS[f.name] || schemaFields)(f, evidence) }));
  if (!fixes.length) return null;
  let chosen = fixes[0];
  const top = h("div", { class: "row2" });
  const fields = h("div", { class: "fields" });
  const button = h("button", { class: "btn primary", type: "submit" });
  const choice = fixes.length > 1
    ? h("select", { onchange: (e) => { chosen = fixes[e.target.selectedIndex]; show(); } },
      fixes.map((f) => h("option", { value: f.name }, cap(f.noun))))
    : null;
  const show = () => {
    setChildren(top, choice ? field("Fix", choice) : null, chosen.fields.first || null);
    setChildren(fields, ...[chosen.fields.nodes].flat(Infinity).filter(Boolean));
    button.textContent = "Suggest the " + chosen.noun;
  };
  show();
  const why = h("textarea", { rows: "2", placeholder: "Why this fixes it. Shown to the approver." });
  const effect = h("input", { type: "text", placeholder: "For example: pods stop crash-looping" });
  const error = h("div", { class: "error", role: "alert" });
  const body = (params) => ({
    clientProposalId: "ui-" + Date.now(),
    target: { group: t.group || "apps", kind: t.kind, namespace: t.namespace, name: t.name, uid: t.uid },
    capabilityName: chosen.name,
    params,
    rationale: why.value.trim(),
    expectedEffect: effect.value.trim(),
    citedEvidenceIds: evidence.map((x) => x.ID),
  });
  const submit = async (e) => {
    e.preventDefault();
    error.textContent = "";
    if (!why.value.trim()) {
      error.textContent = "A reason is required.";
      return;
    }
    let params;
    try {
      params = chosen.fields.params();
    } catch (err) {
      error.textContent = err.message;
      return;
    }
    try {
      const r = await api("/v1/incidents/" + encodeURIComponent(inc.id) + "/proposals", { method: "POST", body: JSON.stringify(body(params)) });
      // A refusal ends its status name with _REJECTED.
      if (r && String(r.status).endsWith("_REJECTED")) {
        error.textContent = REJECTION_LABEL[r.rejectionReason] || r.message || "Refused.";
        return;
      }
      flash = cap(chosen.noun) + " suggested. Checking it against the cluster and the policy.";
      reload();
    } catch (err) {
      error.textContent = err.message;
    }
  };

  // The preview: what Themis would do with the fix as it stands, judged the
  // way a suggestion is, a moment after each edit.
  const preview = h("div", { class: "preview", "aria-live": "polite" });
  const checksOf = (items, side) => items.map((c) => h("li", {},
    h("span", { class: "k " + side, "aria-hidden": "true" }, MARK[side]),
    h("span", {}, h("span", { class: "vh" }, MARK_WORD[side] + ": "), c.says, c.reason ? h("span", { class: "src" }, c.reason) : null)));
  const showPreview = (r, note) => {
    preview.className = "preview" + (r ? (r.accepted ? " ok" : " no") : "");
    setChildren(preview, r ? [
      h("div", { class: "verdict" }, r.verdict),
      r.note ? h("p", { class: "small" }, r.note) : null,
      r.reason ? h("p", { class: "small" }, r.reason) : null,
      (r.for || []).length || (r.against || []).length ? h("ul", { class: "checklist" }, checksOf(r.for || [], "for"), checksOf(r.against || [], "against")) : null,
      r.then ? h("p", { class: "small then" }, r.then) : null,
    ] : h("p", { class: "quiet small" }, note));
  };
  let wait = null;
  let asked = 0;
  const judge = async () => {
    let params;
    try {
      params = chosen.fields.params();
    } catch (err) {
      showPreview(null, err.message);
      return;
    }
    const mine = ++asked;
    try {
      const r = await api("/v1/incidents/" + encodeURIComponent(inc.id) + "/proposals/preview", { method: "POST", body: JSON.stringify(body(params)) });
      if (mine === asked) showPreview(r);
    } catch (err) {
      if (mine === asked) showPreview(null, err.message);
    }
  };
  const later = () => {
    clearTimeout(wait);
    wait = setTimeout(judge, 400);
  };
  if (d.Previewed) {
    showPreview(null, "Change a value to see what Themis would do with it.");
    later();
  }
  // Every value comes from the incident: a value the browser kept or filled
  // could land in another field.
  const form = h("form", { class: "fields", autocomplete: "off", onsubmit: submit, oninput: d.Previewed ? later : null, onchange: d.Previewed ? later : null },
    top, fields, field("Reason", why), field("Expected effect", effect), h("div", {}, button), error);
  const grid = h("div", { class: d.Previewed ? "grid" : null }, form, d.Previewed ? preview : null);
  if (inc.state === "investigating") {
    return h("details", { class: "fold suggest", "data-key": "suggest" }, h("summary", {}, "Suggest a fix while Themis investigates"), grid);
  }
  return h("section", { class: "plate suggest", "data-key": "suggest", "aria-label": "Suggest a fix" }, h("h2", {}, "Suggest a fix"), grid);
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
  if (s === "recommended" && fx && action) {
    return [sentence(fx.short + (action.PolicyID === GITOPS ? " in Git" : "")), action.DecisionReason];
  }
  if (s === "received" || s === "investigating") {
    const got = [...new Set((d.Evidence || []).map((e) => (String(e.Kind).startsWith("Themis") ? e.Kind : String(e.Kind).toLowerCase())))];
    return ["Collecting and weighing evidence.", got.length ? "Collected so far: " + got.join(", ") + "." : "Nothing collected yet."];
  }
  if (s === "diagnosed" || s === "proposed") return ["Checking the suggested fix against the cluster and the policy.", fx ? "Suggested: " + lowerFirst(fx.short) + "." : reason];
  const suggest = !(d.Fixes || []).length ? "Themis has no fix for this kind of incident."
    : can("approver") ? "Suggest a fix below, or send one from an AI SRE agent." : "An approver, or an AI SRE agent, can suggest a fix.";
  if (s === "awaiting_suggestion") return waitingFor(d, last, fx, suggest);
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
  // The reason's first sentence says why it closed: "Recovered before a
  // fix.", "All of its alerts stopped firing.", "Recovered after a change
  // outside Themis, by …".
  if (s === "self_resolved") return opening(reason, "Recovered before Themis acted.");
  return [reason || stateOf(s)[0] + ".", ""];
}

// waitingFor says why an incident waits for a suggested fix: Themis referred
// it to a person, a person denied its fix, a fix suggested was refused,
// investigating it again found none, or no investigator is installed.
function waitingFor(d, last, fx, suggest) {
  const from = last ? last.From : "";
  const referred = REFERRAL.test(last ? last.Reason : "");
  // A referral says, after why Themis stopped, which fix the checks support
  // or what they found: the person suggests the fix.
  const reason = last ? last.Reason.replace(REFERRAL, "") : "";
  const next = referred ? " Referred to a person." : " Waiting for another fix.";
  // A person's reason for denying follows as they wrote it.
  if (from === "awaiting_approval" && fx && !referred) return [cap(fx.noun) + " denied." + next, reason];
  if (REFUSING.includes(from)) {
    // The fix refused: a suggestion kept with the refusal, or the change
    // policy refused.
    const p = (d.Proposals || []).filter((x) => x.Message && reason.startsWith(x.Message)).pop()
      || (d.Actions || []).filter((a) => a.DecisionReason && reason.startsWith(a.DecisionReason)).pop();
    const noun = p ? fixWords(p).noun : "suggested fix";
    return [cap(noun) + " refused." + next, [reason.replace(/^Refused\. /, ""), referred ? suggest : ""].filter(Boolean).join(" ")];
  }
  if (referred) {
    const [title, rest] = opening(reason, "");
    return [title + next, [rest, suggest].filter(Boolean).join(" ")];
  }
  if (from === "investigating") return [reason + next, suggest];
  // No investigator: the title is what fails, as the symptoms found it.
  return [(d.Situation || []).length ? said(d.Situation) : "Waiting for a suggested fix.", "No investigator is installed. " + suggest, true];
}

// againBlock offers to ask again for approval of the fix whose approval
// expired, while Themis says that can work, and to have the incident
// investigated again, or says why it can't be. The investigator then reads
// the fixes people denied, and why.
function againBlock(d, reload) {
  const offer = d.InvestigateAgain;
  const actions = d.Actions || [];
  const last = actions[actions.length - 1];
  const expired = last && last.Renewable ? last : null;
  if ((!offer && !expired) || !can("approver")) return null;
  const error = h("div", { class: "error", role: "alert" });
  // post asks for path, and says said once it is done.
  const post = (path, said) => async (e) => {
    const button = e.currentTarget;
    button.disabled = true;
    error.textContent = "";
    try {
      await api(path, { method: "POST" });
      flash = said;
      reload();
    } catch (err) {
      error.textContent = err.message;
      button.disabled = false;
    }
  };
  const fine = (note) => (note ? h("p", { class: "fine" }, note) : null);
  const renew = expired ? [
    h("button", { class: "btn", type: "button", onclick: post("/v1/actions/" + encodeURIComponent(expired.ID) + "/renew", "Approval asked again.") },
      "Ask for approval again"),
    fine("Asks for approval of the same " + fixWords(expired).noun + " again, if the checks still support it on evidence read now."),
  ] : null;
  let again = null;
  if (offer) {
    const denied = actions.some((a) => a.State === "denied" && a.Decision !== "deny");
    again = [
      h("button", { class: "btn", type: "button", disabled: Boolean(offer.refused),
        onclick: post("/v1/incidents/" + encodeURIComponent(d.Incident.id) + "/investigate", "Investigating again.") }, "Investigate again"),
      fine(offer.refused ? "Not available. " + offer.refused : denied ? "The investigator reads which fixes were denied, and why." : null),
    ];
  }
  return h("div", { class: "act" }, renew, again, error);
}

// closeBlock lets an approver close an incident that waits for a suggested
// fix, or for a person to make the change Themis recommends, with no change:
// Themis keeps one whose checks still find its failure open until it
// recovers or a person closes it.
function closeBlock(d, reload) {
  const state = d.Incident.state;
  if (!(SUGGESTABLE.includes(state) || state === "recommended") || state === "investigating" || !can("approver")) return null;
  const error = h("div", { class: "error", role: "alert" });
  const reason = h("textarea", { rows: "2" });
  const button = h("button", { class: "btn deny", type: "button",
    onclick: async () => {
      button.disabled = true;
      error.textContent = "";
      try {
        await api("/v1/incidents/" + encodeURIComponent(d.Incident.id) + "/close", { method: "POST", body: JSON.stringify({ reason: reason.value }) });
        flash = "Closed, no change.";
        reload();
      } catch (e) {
        error.textContent = e.message;
        button.disabled = false;
      }
    } }, "Close");
  return h("div", { class: "act" }, field("Reason for closing (optional)", reason), button,
    h("p", { class: "fine" }, state === "recommended"
      ? "Closes the incident with no change. Otherwise Themis closes it once the change is made and the workload recovers, or its alerts stop."
      : "Closes the incident with no change. Themis doesn't close it on its own while its checks find the failure."), error);
}

// followsNote says which fix of Themis's the workload ran when the incident
// opened, and links the incident whose fix it was.
function followsNote(f) {
  if (!f) return null;
  const before = f.before >= 0 ? ", " + span(f.before) + " before this incident opened" : "";
  const done = f.done ? ": " + f.done.charAt(0).toLowerCase() + f.done.slice(1) : ".";
  return h("p", { class: "note" }, "Follows Themis's fix on ", h("a", { href: "#/incidents/" + f.incident }, "an earlier incident"), before + done);
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
  const fx = fixed ? fixWords(fixed) : null;
  // The fix a restore undoes, for saying what didn't help.
  const undone = actions.filter((a) => !(a.Params || {}).restores).pop();
  const lead = leadCause(inv);
  const ctx = {
    fx, action,
    undone: undone ? fixWords(undone) : fx,
    cause: lead ? causeText(lead.rootCauseClass, (d.Causes || {})[lead.rootCauseClass]) : null,
    restored: actions.some((a) => (a.Params || {}).restores && a.Execution && a.Execution.Status === "succeeded"),
  };
  const [word, tone] = stateOf(inc.state, ctx);
  const [title, because, plain] = headline(d, ctx);
  // The fix the findings weigh: the one the latest change came from.
  const source = action ? proposals.find((p) => p.ID === action.ProposalID) : accepted;
  const mine = new Set([...actions.map((a) => a.ProposalID), accepted && accepted.ID].filter(Boolean));
  const others = proposals.filter((p) => !mine.has(p.ID));
  const fromPerson = source && !source.LeaseID ? source : null;
  const suggesting = SUGGESTABLE.includes(inc.state) && can("approver") ? suggestForm(d, reload) : null;
  const again = againBlock(d, reload);
  const closing = closeBlock(d, reload);
  const told = flash;
  flash = null;

  render(
    h("a", { class: "back", href: "#" }, "Incidents"),
    h("div", { class: "head" },
      h("span", { class: "wl2" }, h("b", {}, t.name), " in " + t.namespace + " · " + t.kind),
      h("span", { class: "tag state " + tone }, word)),
    h("h1", { class: "question" }, title),
    because ? h("p", { class: plain ? "because plain" : "because" }, because) : null,
    told ? h("p", { class: "flash", role: "status" }, told) : null,
    inc.supersededBy ? h("p", { class: "note" }, "A newer incident covers the workload as it is now: ",
      h("a", { href: "#/incidents/" + inc.supersededBy }, "open the newer incident"), ".") : null,
    followsNote(d.Follows),
    factsBlock(d),

    action ? decision(action, d, ctx, true, reload) : suspicion(d, null, "fix"),
    again || suggesting || closing ? h("div", { class: "more" }, again, suggesting, closing) : null,

    findings(d, source ? source.CitedPredicates || [] : null, fx ? fx.noun : "fix", fixed ? fixed.Capability : ""),
    evidenceSection(d, () => {
      clearTimeout(timer);
      timer = setTimeout(() => watchChanges(id, d, reload), POLL_MS * 2);
    }, reload),

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
      timelineFold(d),
      technical(d, action)));

  // Refreshing would discard what an approver or suggester is typing.
  const typing = actions.some((a) => a.State === "awaiting_approval") || Boolean(suggesting) || Boolean(closing);
  if (!typing) timer = setTimeout(reload, POLL_MS * 2);
  else timer = setTimeout(() => watchChanges(id, d, reload), POLL_MS * 2);
}

// watchChanges keeps checking an incident a person is typing on, and says
// when it changed, so nobody decides from a stale page and nothing typed is
// lost.
function watchChanges(id, seen, reload) {
  const here = location.hash;
  const stamp = (d) => JSON.stringify([d.Incident.state, (d.Transitions || []).length,
    (d.Actions || []).map((a) => a.State), (d.Proposals || []).map((p) => p.Status)]);
  const again = () => { if (location.hash === here) timer = setTimeout(() => watchChanges(id, seen, reload), POLL_MS * 2); };
  api("/v1/incidents/" + encodeURIComponent(id)).then((d) => {
    if (location.hash !== here) return;
    if (stamp(d) === stamp(seen)) return again();
    if (!main.querySelector(".changed")) {
      main.prepend(h("p", { class: "flash changed", role: "status" }, "This incident changed. ",
        h("a", { href: here, onclick: (e) => { e.preventDefault(); reload(); } }, "Reload"), "."));
    }
  }).catch(again);
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
  else if (hash === "#/status") [run, nav] = [async () => { await status(); timer = setTimeout(route, STATUS_POLL_MS); }, "nav-status"];
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
