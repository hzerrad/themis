// The UI smoke test. It loads the page, as served, in jsdom against what the
// API serves (fixtures the Go views build: make test-ui writes them), visits
// every page as each role, and works the forms that rebuild parts of a page.
// After each step it fails on an exception or unhandled rejection, a
// console.error, a "Not loaded:" box, text a missing value leaves, such as
// "null", "undefined", "NaN" or "[object Object]", and words the page no
// longer says. Where the words come from the change itself, it checks the
// page says them.
//
//   node smoke.mjs <fixtures.json>

import { readFileSync } from "node:fs";
import jsdom from "jsdom";

const { JSDOM, VirtualConsole, requestInterceptor } = jsdom;

const STATIC = new URL("../../internal/server/httpapi/static/", import.meta.url);
const PAGE = readFileSync(new URL("index.html", STATIC), "utf8");
const APP = readFileSync(new URL("app.js", STATIC));
const F = JSON.parse(readFileSync(process.argv[2], "utf8"));

// What a missing value prints. Each text node is read on its own, so a run
// of empty parts, such as "nullnull", is caught too.
const MISSING = /(?<![A-Za-z])(?:null|undefined)(?![a-z])|NaN|\[object Object\]/;
// Evidence is shown as collected: its JSON holds null for a figure no source
// gave.
const AS_COLLECTED = "details.raw > pre, pre.quote";

// OLD are words no page says any more: names of changes that didn't say
// what changes, an unexplained risk label, and the void condition at length.
const OLD = [/memory and CPU (change|settings)/, /probe timing/, /\bRisk: /, /\bAffects \d/, /then verifies it/, /before it is applied/];

// SAYS are, by the incident's number, texts its page shows an admin, worked
// out from its change, and texts it must not show.
const SECRETS = "Values that look like secrets are hidden.";
const SAYS = {
  2: {
    has: ["Roll checkout back to revision 1?", "Approve the rollback", "Deny the rollback", "Supports the rollback",
      "Restarts 3 pods, one at a time.", "Then checks that the pods become ready and stay ready.", "Void if checkout changes first.", SECRETS,
      "Changed from revision 1:", "The image of container app changed from shop/checkout:1.4.1 to shop/checkout:1.5.0.", "The liveness probe of container app changed."],
  },
  3: { has: ["Change checkout's memory request and memory limit?", "Approve the change", "Supports the change"], not: [SECRETS] },
  12: { has: ["Lengthened checkout's liveness probe timeout from 1s to 3s.", "Change checkout's memory request and memory limit"] },
  19: {
    has: ["Raise checkout's CPU limit from 200m to 300m?", "Approve the raise", "Deny the raise", "Supports the raise",
      "2 checks support the raise. None is against it.", "Restarts 4 pods, up to 2 at a time.",
      "Then checks that the pods become ready and stay ready, and are no longer throttled at their CPU limit.",
      "Based on Prometheus's container CPU history over 1h."],
    not: [SECRETS, "Commands and arguments"],
  },
  20: {
    has: ["Lower the CPU request of checkout's app container from 100m to 50m?", "Approve the decrease", "Restarts all 3 pods at once."],
    not: [SECRETS],
  },
  21: {
    has: ["Change checkout's liveness probe?", "Approve the probe change", "Restarts 1 pod.",
      "Then checks that the pods become ready and stay ready, with no restart."],
  },
  22: { has: ["Verifying the raise.", "Raised checkout's memory limit from 256Mi to 512Mi."], not: ["pods ready", "pods are ready"] },
  23: {
    has: ["Follows Themis's fix on an earlier incident, 4m before this incident opened: raised checkout's CPU limit from 200m to 300m.",
      "Raise checkout's CPU limit from 300m to 450m?", "Themis's earlier fix"],
  },
  24: { has: ["Not fixed.", "The change stays, as it left the workload no worse."], not: ["Fixed. The workload recovered"] },
  25: { has: ["Recovered before a fix."], not: ["Recovered before Themis acted."] },
  26: { has: ["Closed by alice.", "Reason: handled by the vendor."], not: ["The evidence doesn't point to one cause.", "Some evidence was missing."] },
  5: { has: ["Reason for closing (optional)", "Themis doesn't close it on its own while its checks find the failure."] },
  30: {
    has: ["catalog runs out of memory.", "No investigator is installed. Suggest a fix below, or send one from an AI SRE agent.",
      "The memory of app peaked at 57Mi of its 60Mi limit over 1h.", "No pod was evicted for its node's memory.",
      "1 check found the failure.", "Evidence 10", "2 pods, 1 ready, 3 restarts. Last ended OOMKilled, exit 137",
      "6 events. BackOff ×4, Started ×2.", "Peak 57Mi of 60Mi (95%) over 1h, from Prometheus.", "1 revision. Created",
      "No earlier revision.", "1 value that looks like a secret is hidden.", "Memory pressure", "Raw data"],
  },
  31: { has: ["Raise catalog's memory limit from 60Mi to 72Mi?", "3 checks support the raise. None is against it.", "Restarts 2 pods, one at a time."] },
  27: {
    has: ["Recovered after a change outside Themis, by kubectl-edit.",
      "Since the change, the checks found that no container is throttled at its CPU limit (app was throttled in 2% of its CPU periods over 7m"],
    not: ["Recovered before Themis acted."],
  },
  28: {
    has: ["The workload was changed outside Themis, by kubectl-edit, and still fails.",
      "Since the change, the checks found that a container is throttled at its CPU limit (app was throttled in 57% of its CPU periods over 7m",
      "A newer incident covers the workload as it is now"],
  },
  29: {
    has: ["Unknown whether it recovered.", "throttling at the CPU limit is unknown (no throttling figures: no metrics source is set up)",
      "A newer incident covers the workload as it is now"],
    not: ["still fails", "still failing"],
  },
};
// LISTED are lines the incidents list shows, worked out from each change.
const LISTED = ["Raise checkout's CPU limit from 200m to 300m?", "Lower the CPU request of checkout's app container from 100m to 50m?",
  "Change checkout's liveness probe?", "Roll checkout back to revision 1?",
  "Changed outside Themis and still failing.", "Changed outside Themis. Unknown whether it recovered."];

// Problems are what went wrong since the last check, wherever the page was:
// unhandled rejections reach the process, not the window.
let problems = [];
process.on("unhandledRejection", (e) => problems.push("unhandled rejection: " + (e && e.stack || e)));

// files serves app.js, and an empty style sheet: the page's other files hold
// no code. Nothing reaches the network.
const files = requestInterceptor((request) => {
  const path = new URL(request.url).pathname;
  if (path === "/app.js") return new Response(APP, { headers: { "Content-Type": "text/javascript" } });
  if (path === "/style.css") return new Response("", { headers: { "Content-Type": "text/css" } });
  return new Response("", { status: 404 });
});

// api answers the page's requests as Themis does, from the fixtures. who is
// who /v1/me says is signed in, or null. Signing in with an account, or
// creating the first admin account, signs in as admin; the break-glass token
// and the initial admin password sign in while no admin account exists.
function api(o) {
  const state = { who: o.who === undefined ? "admin" : o.who };
  const detail = (id) => {
    const d = F.details[id];
    return d && d[F.me[state.who].role === "viewer" ? "plain" : "raw"];
  };
  const routes = [
    ["GET", /^\/v1\/sign-in$/, () => F.signIn[o.signIn || "password"]],
    ["GET", /^\/v1\/me$/, (headers) => (state.who ? F.me[state.who]
      : headers.Authorization ? F.me.closedToken : [401, "Sign in to Themis first."])],
    ["POST", /^\/auth\/password$/, () => { state.who = "admin"; return [200, ""]; }],
    ["POST", /^\/auth\/initial-admin$/, () => { state.who = "closedInitial"; return [200, ""]; }],
    ["POST", /^\/auth\/logout$/, () => { state.who = null; return [200, ""]; }],
    ["GET", /^\/v1\/incidents$/, () => (o.empty ? [] : F.incidents)],
    ["GET", /^\/v1\/incidents\/([\w-]+)$/, (_, id) => detail(id) || [404, "No such incident or fix."]],
    ["POST", /^\/v1\/incidents\/([\w-]+)\/proposals$/, () => F.suggested],
    ["POST", /^\/v1\/incidents\/([\w-]+)\/investigate$/, () => ({ state: "investigating" })],
    ["POST", /^\/v1\/incidents\/([\w-]+)\/close$/, () => ({ state: "closed_no_action" })],
    ["POST", /^\/v1\/actions\/([\w-]+)\/(approve|deny)$/, () => ({})],
    ["GET", /^\/v1\/status$/, () => F.status[o.status || "mixed"]],
    ["GET", /^\/v1\/settings\/model$/, () => (o.model === null ? [404, "404 page not found"] : F.models[o.model || "unset"])],
    ["PUT", /^\/v1\/settings\/model$/, () => F.models[o.model || "unset"]],
    ["POST", /^\/v1\/settings\/model\/test$/, () => F.modelTest],
    ["GET", /^\/v1\/settings\/alertmanager$/, () => F.alerting[o.alerting || "poll"]],
    ["PUT", /^\/v1\/settings\/alertmanager$/, () => F.alerting[o.alerting || "poll"]],
    ["POST", /^\/v1\/setup\/alertmanager\/test$/, () => F.alertmanagerTest],
    ["GET", /^\/v1\/setup\/alertmanager$/, () => F.alertmanagerSetup],
    ["GET", /^\/v1\/setup\/alert-rules$/, () => F.alertRules[o.rules || "missing"]],
    ["GET", /^\/v1\/accounts$/, () => F.accounts],
    ["POST", /^\/v1\/accounts$/, () => { if (F.me[state.who].closed) state.who = "admin"; return [201, ""]; }],
    ["PATCH", /^\/v1\/accounts\/[^/]+$/, () => [204, ""]],
    ["DELETE", /^\/v1\/accounts\/[^/]+$/, () => [204, ""]],
    ["GET", /^\/v1\/audit\/accounts$/, () => F.accountRecord],
    ["POST", /^\/v1\/me\/password$/, () => { state.who = "admin"; return [204, ""]; }],
  ];
  return (method, path, headers) => {
    for (const [m, re, answer] of routes) {
      const match = path.match(re);
      if (m === method && match) {
        const a = answer(headers, ...match.slice(1));
        return Array.isArray(a) && typeof a[0] === "number" ? a : [200, JSON.stringify(a)];
      }
    }
    problems.push("the page asked for " + method + " " + path + ", which Themis doesn't serve");
    return [404, "404 page not found"];
  };
}

// open loads the page at hash against the API o describes, and waits until
// it has settled.
async function open(o) {
  const answer = api(o);
  const virtualConsole = new VirtualConsole();
  virtualConsole.on("jsdomError", (e) => problems.push(e.type + ": " + (e.cause && e.cause.stack || e.stack || e.message) + (e.url ? " " + e.url : "")));
  virtualConsole.on("error", (...args) => problems.push("console.error: " + args.join(" ")));
  const dom = new JSDOM(PAGE, {
    url: "http://themis.test/" + (o.hash || ""),
    runScripts: "dangerously",
    resources: { interceptors: [files] },
    virtualConsole,
    beforeParse(window) {
      window.fetch = async (input, init = {}) => {
        const url = new URL(String(input), window.location.href);
        const [status, text] = answer((init.method || "GET").toUpperCase(), url.pathname, init.headers || {});
        return {
          ok: status >= 200 && status < 300, status, statusText: String(status),
          text: async () => text, json: async () => window.JSON.parse(text),
        };
      };
    },
  });
  const page = { dom, window: dom.window, document: dom.window.document, changes: 0 };
  new dom.window.MutationObserver(() => page.changes++).observe(page.document, { subtree: true, childList: true, characterData: true, attributes: true });
  await new Promise((done, fail) => {
    dom.window.addEventListener("load", done);
    setTimeout(() => fail(new Error("the page didn't load in 10 s")), 10000).unref();
  });
  await settle(page);
  return page;
}

// settle waits until the page has stopped changing: its requests answered,
// and what it draws from them drawn.
async function settle(page) {
  for (let quiet = 0, i = 0; quiet < 3; i++) {
    if (i === 200) throw new Error("the page kept changing");
    const before = page.changes;
    await new Promise((done) => setTimeout(done, 0));
    quiet = page.changes === before ? quiet + 1 : 0;
  }
}

// check fails on whatever went wrong since the last check, and on what the
// page shows: a "Not loaded:" box, nothing at all, or a missing value.
function check(page, where) {
  const { document: doc, window } = page;
  const main = doc.getElementById("main");
  if (!main.textContent.trim()) problems.push("the page shows nothing");
  if (main.textContent.includes("Not loaded:")) problems.push("it shows " + JSON.stringify(main.querySelector(".alert").textContent));
  const walker = doc.createTreeWalker(doc.body, window.NodeFilter.SHOW_TEXT);
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    if (MISSING.test(n.data) && !n.parentElement.closest(AS_COLLECTED)) {
      problems.push("it shows " + JSON.stringify(n.parentElement.textContent.trim().slice(-160)));
    }
  }
  const shown = [doc.title];
  for (const el of doc.querySelectorAll("input, textarea")) shown.push(el.value);
  for (const el of doc.querySelectorAll("[placeholder], [title], [aria-label]")) {
    shown.push(el.getAttribute("placeholder"), el.getAttribute("title"), el.getAttribute("aria-label"));
  }
  for (const s of shown) if (s && MISSING.test(s)) problems.push("it shows " + JSON.stringify(s));
  for (const old of OLD) {
    const at = main.textContent.match(old);
    if (at) problems.push("it still says " + JSON.stringify(at[0]));
  }
  flush(where);
  steps++;
}

// flush fails where on the problems found since the last check.
function flush(where) {
  const found = problems;
  problems = [];
  if (found.length) failures.push({ where, found: [...new Set(found)] });
}

// the is the element selector matches, which the step needs.
function the(page, selector, text) {
  const all = [...page.document.querySelectorAll(selector)];
  const el = !text ? all[0] : all.find((e) => e.textContent.trim() === text) || all.find((e) => e.textContent.includes(text));
  if (!el) throw new Error("no " + selector + (text ? " with " + JSON.stringify(text) : "") + " on the page");
  return el;
}

// The interactions, each followed by a check.
const act = {
  async click(page, selector, text) {
    the(page, selector, text).click();
    await settle(page);
  },
  async choose(page, select, value) {
    const el = typeof select === "string" ? the(page, select) : select;
    const i = [...el.options].findIndex((o) => o.value === value || o.textContent === value);
    if (i < 0) throw new Error("no option " + JSON.stringify(value) + " in " + el.outerHTML.slice(0, 80));
    el.selectedIndex = i;
    el.dispatchEvent(new page.window.Event("change", { bubbles: true }));
    await settle(page);
  },
  async type(page, input, value) {
    const el = typeof input === "string" ? the(page, input) : input;
    el.value = value;
    el.dispatchEvent(new page.window.Event("change", { bubbles: true }));
    await settle(page);
  },
  async submit(page, form) {
    const el = typeof form === "string" ? the(page, form) : form;
    const invalid = el.querySelector(":invalid");
    if (invalid) throw new Error("the form isn't valid: " + invalid.outerHTML.slice(0, 120));
    el.requestSubmit();
    await settle(page);
  },
  // refresh opens every fold, then has the page redraw itself in place, as
  // its timers do.
  async refresh(page) {
    for (const d of page.document.querySelectorAll("details")) d.open = true;
    page.window.dispatchEvent(new page.window.HashChangeEvent("hashchange"));
    await settle(page);
  },
};

// says checks the page shows each text of has and none of not.
function says(page, { has = [], not = [] }) {
  const text = page.document.getElementById("main").textContent.replace(/\s+/g, " ");
  for (const t of has) if (!text.includes(t)) problems.push("it doesn't say " + JSON.stringify(t));
  for (const t of not) if (text.includes(t)) problems.push("it says " + JSON.stringify(t));
}

// rested checks that a check that says only whether figures are there is
// never for or against the fix: it shows apart, as what the others rest on.
function rested(page, detail) {
  const pans = [...page.document.querySelectorAll(".pans li")].map((li) => li.textContent).join("\n");
  for (const [name, c] of Object.entries(detail.Checks || {})) {
    if (c.availability && [c.true, c.false].some((t) => t && pans.includes(t))) problems.push("it weighs " + name + " for or against the fix");
  }
}

// field is the input, select or textarea a field labelled text holds.
function field(page, text, root = page.document) {
  const label = [...root.querySelectorAll("label")].find((l) => (l.firstChild && l.firstChild.textContent.trim() === text)
    || l.textContent.trim() === text);
  if (!label) throw new Error("no field " + JSON.stringify(text) + " on the page");
  return label.querySelector("input, select, textarea");
}

const failures = [];
let visits = 0;
let steps = 0;

// visit opens the page o describes, checks it, refreshes it with every fold
// open, checks it again, then runs then's steps, checking after each.
async function visit(name, o, then) {
  visits++;
  let page;
  try {
    page = await open(o);
    check(page, name);
    await act.refresh(page);
    check(page, name + ", every fold open");
    if (then) await then(page, (what) => check(page, name + ", " + what));
  } catch (e) {
    problems.push("the test couldn't go on: " + (e.stack || e));
    flush(name);
  } finally {
    if (page) page.window.close();
  }
}

const ids = Object.keys(F.details);
const id = (n) => ids.find((i) => i.endsWith(String(n).padStart(12, "0")));

// Signing in, and the pages someone signed in sees before any other.
await visit("sign-in with an account", { who: null, signIn: "password" }, async (page, ok) => {
  await act.type(page, field(page, "Username"), "admin");
  await act.type(page, field(page, "Password"), "correct horse battery");
  await act.submit(page, "form");
  ok("signed in");
});
await visit("sign-in through the identity provider or an account", { who: null, signIn: "oidc" });
await visit("first run: the break-glass token", { who: null, signIn: "none" }, async (page, ok) => {
  await act.type(page, field(page, "Break-glass token"), "s3cret");
  await act.submit(page, "form");
  ok("signed in, no admin account yet");
});
await visit("first run: the initial admin password", { who: null, signIn: "firstRun" }, async (page, ok) => {
  await act.type(page, field(page, "Initial admin password"), "initial");
  await act.submit(page, "form");
  ok("signed in, no admin account yet");
  await act.type(page, field(page, "Username"), "admin");
  await act.type(page, field(page, "Password"), "a long passphrase");
  await act.type(page, field(page, "Password again"), "a long passphrase");
  await act.submit(page, "form");
  ok("created the first admin account, on Setup");
});
await visit("first run: the initial admin password, once an admin account exists", { who: "initialAdmin" }, async (page, ok) => {
  await act.type(page, field(page, "Username"), "admin");
  await act.type(page, field(page, "Password"), "a long passphrase");
  await act.submit(page, "form");
  ok("signed in, on Setup");
});
await visit("first run: someone who isn't an admin", { who: "closedViewer" });
await visit("no role", { who: "noRole" });
await visit("temporary password", { who: "temporary" }, async (page, ok) => {
  await act.type(page, field(page, "Temporary password"), "temporary-1234");
  await act.type(page, field(page, "New password"), "my own passphrase");
  await act.submit(page, "form");
  ok("password chosen");
});

// Every page as each role. A viewer asking for an admin's page gets the
// incidents.
for (const who of ["admin", "approver", "viewer"]) {
  await visit(who + ": no incidents", { who, empty: true });
  await visit(who + ": incidents", { who }, async (page, ok) => {
    says(page, { has: LISTED });
    ok("each change named by what it does");
  });
  for (const i of ids) {
    await visit(who + ": incident " + F.details[i].raw.Incident.state + " " + i.slice(-2), { who, hash: "#/incidents/" + i }, async (page, ok) => {
      rested(page, F.details[i].raw);
      const said = SAYS[Number(i.slice(-12))];
      if (said && who === "admin") says(page, said);
      ok("the words of its change");
    });
  }
  await visit(who + ": status", { who, hash: "#/status" }, async (page, ok) => {
    await act.click(page, "button", "Check again");
    ok("checked again");
  });
  if (who !== "viewer") await visit(who + ": password", { who, hash: "#/password" });
}
await visit("viewer: setup", { who: "viewer", hash: "#/setup" });
await visit("viewer: accounts", { who: "viewer", hash: "#/accounts" });

// expect fails the step unless what holds.
function expect(what, holds) {
  if (!holds) problems.push("expected " + what);
}

// The facts cite the evidence, which opens where it is and lights up for a
// moment; each kind of evidence opens to a view of its own, labelled where
// its figures are.
await visit("admin: the facts and every kind of evidence", { hash: "#/incidents/" + id(30) }, async (page, ok) => {
  const doc = page.document;
  const ev = (n) => doc.getElementById("ev-" + n);
  expect("the title to say what fails", doc.querySelector("h1").textContent === "catalog runs out of memory.");
  for (const d of doc.querySelectorAll("details")) d.open = false;
  const ref = the(page, ".facts a.ref", "5");
  expect("the citation to name the evidence", ref.getAttribute("aria-label") === "Evidence 5, Container memory");
  await act.click(page, ".facts a.ref", "5");
  expect("evidence 5 open and lit", ev(5).open && ev(5).classList.contains("lit"));
  expect("the focus on evidence 5", doc.activeElement === ev(5).querySelector("summary"));
  ok("opened the evidence a fact cites");

  expect("a table of pods", ev(2).querySelectorAll("table.data tbody tr").length === 2);
  expect("the warning event marked", the(page, "#ev-3 td.warn").textContent === "Warning");
  const gauge = (sel) => the(page, "#ev-5 figure.gauge " + sel).textContent;
  expect("the gauge labelled", gauge(".above") === "peak 57Mi" && gauge(".below:not(.end)") === "request 32Mi" && gauge(".below.end") === "limit 60Mi");
  expect("a throttling figure of its own", ev(6).querySelectorAll("figure.gauge").length === 2);
  expect("the revision", the(page, "#ev-1 .rev").textContent.includes("No earlier revision."));
  expect("the referenced config", ev(7).querySelectorAll("table.data tbody tr").length === 2);
  expect("the revision's health", ev(8).querySelectorAll("ul.lines li").length === 1);
  expect("the fields of an unknown kind", ev(9).querySelectorAll("dl.tech dt").length === 5);
  expect("the later item's time", ev(10).querySelector(".when") !== null && ev(4).querySelector(".when") === null);

  const logs = () => [...ev(4).querySelectorAll(".log")];
  expect("the previous run shown", !logs()[0].hidden && logs()[1].hidden && logs()[0].querySelector(".last") !== null);
  await act.click(page, "#ev-4 .tabs button", "Current run");
  expect("the current run shown", logs()[0].hidden && !logs()[1].hidden
    && the(page, "#ev-4 .tabs button", "Current run").getAttribute("aria-pressed") === "true");
  ok("switched to the current run");
  await act.refresh(page);
  expect("the current run still shown after a refresh", logs()[0].hidden && !logs()[1].hidden);
  ok("refreshed");

  ev(2).open = false;
  doc.querySelector("details.checks").open = true;
  await act.click(page, ".pans .src a", "2");
  expect("a finding to open its evidence", ev(2).open && ev(2).classList.contains("lit"));
  ok("opened the evidence a finding cites");
});
await visit("admin: Themis's earlier fix", { hash: "#/incidents/" + id(23) }, async (page, ok) => {
  expect("a link to the earlier incident", the(page, "#ev-1 a", "an earlier incident").getAttribute("href") === "#/incidents/" + id(24));
  ok("the fix it follows");
});
await visit("viewer: the facts, and no raw evidence", { who: "viewer", hash: "#/incidents/" + id(30) }, async (page, ok) => {
  const doc = page.document;
  expect("the facts", doc.querySelectorAll(".facts p").length === 4);
  expect("no line of what each holds", [...doc.querySelectorAll(".ev .gist")].every((s) => !s.textContent.trim()));
  expect("every piece readable by responders only", [...doc.querySelectorAll(".ev .body")].every((b) => b.textContent.includes("Readable by responders only.")));
  expect("no raw data", !doc.querySelector("details.raw"));
  ok("what a viewer reads");
});

// Suggesting each fix the incident offers, as an approver and an admin.
for (const who of ["approver", "admin"]) {
  await visit(who + ": suggest every fix", { who, hash: "#/incidents/" + id(4) }, async (page, ok) => {
    const kinds = [...the(page, "details[data-key=suggest] select").options].map((o) => o.textContent);
    const offered = ["Rollback", "Memory or CPU change", "Probe change", "Scale change"];
    if (kinds.join() !== offered.join()) throw new Error("the form offers " + kinds.join(", ") + ", not " + offered.join(", "));
    for (const kind of kinds) {
      const form = () => the(page, "details[data-key=suggest] form");
      await act.choose(page, field(page, "Fix", form()), kind);
      ok("chose " + kind);
      if (kind === "Memory or CPU change") {
        await act.choose(page, field(page, "Container", form()), "proxy");
        ok("memory or CPU of proxy");
        await act.choose(page, field(page, "Container", form()), "app");
        await act.type(page, field(page, "Memory limit", form()), "1Gi");
      } else if (kind === "Probe change") {
        await act.choose(page, field(page, "Probe", form()), "readiness");
        ok("readiness probe");
        await act.choose(page, field(page, "Container", form()), "proxy");
        ok("probes of proxy, which has none");
        await act.choose(page, field(page, "Container", form()), "app");
        await act.type(page, field(page, "Timeout, seconds", form()), "4");
      } else if (kind === "Scale change") {
        await act.type(page, field(page, "Replicas", form()), "4");
        await act.choose(page, field(page, "Strategy", form()), "recreate");
        field(page, "From", form()).value = "22:00";
        field(page, "Labels", form()).value = "[\"night\"]";
        field(page, "Pause. Hold the rollout.", form()).checked = true;
      }
      await act.type(page, field(page, "Reason", form()), "The checks support it.");
      await act.submit(page, form());
      ok("suggested " + kind);
    }
    await act.click(page, "button", "Investigate again");
    ok("investigating again");
  });
  await visit(who + ": approve the rollback", { who, hash: "#/incidents/" + id(2) }, async (page, ok) => {
    await act.click(page, "button", "Approve the rollback");
    ok("approved");
  });
}
await visit("approver: close an incident that waits for a fix", { who: "approver", hash: "#/incidents/" + id(5) }, async (page, ok) => {
  await act.type(page, field(page, "Reason for closing (optional)"), "Handled by the vendor.");
  await act.click(page, "button", "Close");
  ok("closed");
});
await visit("approver: deny the fix alice suggested", { who: "approver", hash: "#/incidents/" + id(3) }, async (page, ok) => {
  await act.type(page, field(page, "Reason for denying (optional)"), "Raise it in Git instead.");
  await act.click(page, "button", "Deny and close");
  ok("denied and closed");
});

// Setup, for the installs that differ: what Helm set for the model, how
// alerts arrive, and which alert rules Prometheus has.
const setups = [
  { model: "unset", alerting: "poll", rules: "missing", status: "mixed" },
  { model: "anthropic", alerting: "push", rules: "all", status: "working" },
  { model: "openai", alerting: "pollHelm", rules: "unchecked", status: "mixed" },
  { model: null, alerting: "push", rules: "failing", status: "working" },
];
for (const s of setups) {
  const name = "setup: model " + s.model + ", " + s.alerting + " mode, rules " + s.rules + ", parts " + s.status;
  await visit(name, { ...s, hash: "#/setup" }, async (page, ok) => {
    if (s.model === "unset") {
      await act.choose(page, field(page, "Provider"), "anthropic");
      ok("provider Anthropic");
      await act.choose(page, field(page, "Provider"), "openai");
      await act.choose(page, field(page, "Reply format"), "openai-strict");
      ok("provider OpenAI-compatible");
      await act.click(page, "button", "Test the connection");
      ok("tested the model");
      await act.type(page, field(page, "Model"), "gpt-5");
      await act.click(page, "button", "Save the model");
      ok("saved the model");
    }
    if (s.alerting === "poll") {
      await act.click(page, "button", "Test");
      ok("tested Alertmanager");
      await act.type(page, field(page, "Alertmanager's address"), "http://alertmanager.monitoring:9093");
      await act.click(page, "button", "Save the address");
      ok("saved the address");
      await act.click(page, "button", "Switch to push mode");
      ok("switched mode");
    }
    if (s.alerting === "push") {
      await act.type(page, field(page, "Alertmanager's namespace"), "observability");
      ok("another namespace");
    }
  });
}

await visit("accounts", { hash: "#/accounts" }, async (page, ok) => {
  await act.choose(page, "select[aria-label='Role of alice']", "responder");
  ok("changed a role");
  const pw = the(page, "input[aria-label='Temporary password for bob']");
  pw.value = "temporary-5678";
  pw.closest("tr").querySelector("button").click();
  await settle(page);
  ok("set a temporary password");
  await act.type(page, field(page, "Username"), "carol");
  await act.type(page, field(page, "Temporary password, 12 characters or more"), "temporary-9012");
  await act.submit(page, "form.inline");
  ok("created an account");
});
await visit("signing out", {}, async (page, ok) => {
  await act.click(page, "#logout");
  ok("signed out");
});

flush("after the last page");
for (const f of failures) console.error("FAIL " + f.where + "\n  " + f.found.join("\n  ").replaceAll("\n", "\n  "));
console.log((failures.length ? "FAIL" : "ok") + "  " + visits + " pages, " + steps + " checks, " + failures.length + " failed");
process.exitCode = failures.length ? 1 : 0;
