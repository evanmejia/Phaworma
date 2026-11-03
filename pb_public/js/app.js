// ---------------------------------------------------------------------------
// app.js — my Stage 3 frontend for forms-saas.
//
// I wrote this as a plain ES module with no framework and no build step: the
// entire frontend is inspectable in one file, and my only runtime dependency
// is the official PocketBase JS SDK — vendored locally in js/vendor/, never
// from a CDN (privacy-first is the whole point of this product).
//
// View map (pathname routes — no hash URLs anywhere):
//   /                    — guest hero OR my signed-in form list (adapts)
//   /f/<code>            — the respondent view (served by a REAL Go route)
//   /forms/new           — the builder for a new form (guests may build;
//                          the draft rides in localStorage)
//   /forms/<formId>      — the builder in edit mode (owner or co-creator)
//   /forms/<formId>/results — my results panel for that form
//   /forms/<formId>/share  — owner-only share screen (link + co-creators)
// ---------------------------------------------------------------------------

import PocketBase from "/js/vendor/pocketbase.mjs";

// My one PocketBase client. I use it for TWO things only:
//   1. creator auth (the `users` collection — regular accounts, NEVER
//      _superusers: platform admins live on the built-in dashboard), and
//   2. reading back my own token to attach to my /api/v1 creator calls.
// My forms/submissions traffic goes through fetch() + my custom /api/v1
// endpoints — respondents NEVER touch the native PocketBase API.
const pb = new PocketBase();

// The localStorage key for my in-progress guest draft. Guests build freely
// and the draft follows them: sign in, come back, the form is still here.
const DRAFT_KEY = "formsDraft";

/* ------------------------------ shared state --------------------------- */

const state = {
  view: "home",          // the view I'm showing right now
  route: {},             // my parsed route {view, code?, formId?}
  shortCode: null,       // the /f/<code> I'm responding to
  currentForm: null,     // {title, shortCode, questions} once a public form loaded
  accessPassword: "",    // the password that unlocked the current form (resent on submit)
  forms: [],             // my cached creator form list
  currentFormId: null,   // the formId my builder/results/share views are on
  isOwner: false,        // does the signed-in user OWN the current form?
};

// booted flips true after my first renderRoute — it keeps the auth-store
// listener from re-applying routes while the app is still starting up.
let booted = false;

// pendingAfterAuth holds a ONE-SHOT callback the auth dialog runs after a
// successful sign-in OR account creation. The guest publish flow uses it:
// "create the account, THEN publish my draft."
let pendingAfterAuth = null;

// My in-progress builder state. In edit mode the questions are FROZEN for
// EVERYONE (owner and co-creator alike) — resyncing columns could destroy
// existing responses, so a live form's schema is immutable by design; only
// title/lifespan/password are editable.
const builderDraft = {
  title: "",
  lifespanDays: 7,
  accessPassword: "",
  questions: [], // {id, type, label, required, options[]}
  editing: false,
  formId: null,
};

/* --------------------------------- router ------------------------------ */

// Pathname-based routing: my Go side answers /f/<code> with the SPA shell
// for live forms (and real 404/410 for dead ones), and the static index
// fallback covers /forms/... deep links on refresh. The path IS the state.
function parseRoute() {
  const path = window.location.pathname.replace(/\/+$/, "") || "/";
  let m;
  if (path === "/") {
    // Home adapts to auth: guests see my hero, signed-in users see their
    // form list. The same URL serves both — no separate "dashboard" path.
    return { view: isSignedIn() ? "dashboard" : "home" };
  }
  if ((m = path.match(/^\/f\/([a-zA-Z0-9]+)$/))) {
    return { view: "form", code: m[1].toLowerCase() };
  }
  if (path === "/forms/new") {
    return { view: "builder" };
  }
  if ((m = path.match(/^\/forms\/([a-z0-9]{15})$/))) {
    return { view: "builder", formId: m[1] };
  }
  if ((m = path.match(/^\/forms\/([a-z0-9]{15})\/results$/))) {
    return { view: "results", formId: m[1] };
  }
  if ((m = path.match(/^\/forms\/([a-z0-9]{15})\/share$/))) {
    return { view: "share", formId: m[1] };
  }
  return { view: "notfound" };
}

// I navigate with history.pushState — clean URLs in the address bar, no
// hash. If I'm already at the target path I re-render in place so
// programmatic updates (e.g. after sign-in) still take effect.
function navigate(path) {
  if (window.location.pathname === path) {
    renderRoute(parseRoute());
    return;
  }
  window.history.pushState({}, "", path);
  renderRoute(parseRoute());
}

function renderRoute(route) {
  state.view = route.view;
  state.route = route;
  state.shortCode = route.code || null;

  // I show only the matching pane. The [hidden] attribute is the single
  // source of truth for visibility — the CSS just enforces it.
  document.querySelectorAll("[data-view]").forEach((section) => {
    section.hidden = section.dataset.view !== route.view;
  });

  // Nav sync: "New form" is current on any builder view; "My forms" is
  // current on home/dashboard (and only exists when I'm signed in).
  const navKey = route.view === "builder" ? "builder" : ["home", "dashboard"].includes(route.view) ? "home" : null;
  document.querySelectorAll("[data-nav]").forEach((link) => {
    if (navKey && link.dataset.nav === navKey) {
      link.setAttribute("aria-current", "page");
    } else {
      link.removeAttribute("aria-current");
    }
  });

  controllers[route.view]();
  booted = true;
  window.scrollTo(0, 0);
}

window.addEventListener("popstate", () => renderRoute(parseRoute()));

/* ------------------------------ creator auth --------------------------- */

// Stage 3 auth model: regular users are the creators. I sign in against the
// `users` collection — NOT _superusers. A superuser token would be rejected
// by my Go API anyway (403, "use the built-in admin dashboard").
function isSignedIn() {
  return pb.authStore.isValid;
}

function updateAuthUI() {
  const signedIn = isSignedIn();
  const userEl = document.getElementById("auth-user");
  userEl.hidden = !signedIn;
  if (signedIn) {
    // My users records carry a display name — I prefer it, with the email
    // as fallback (the name is required on my create form, but a future
    // account could predate that).
    userEl.textContent = pb.authStore.record?.name || pb.authStore.record?.email || "creator";
  }
  document.getElementById("btn-signin").hidden = signedIn;
  document.getElementById("btn-signout").hidden = !signedIn;
  // The "My forms" nav link is a signed-in surface.
  const myFormsNav = document.querySelector('[data-nav="home"]');
  if (myFormsNav) myFormsNav.hidden = !signedIn;
}

// openAuthDialog shows my tabbed auth dialog. tab lets a caller choose the
// default tab — the guest publish flow opens on "create" because a guest,
// by definition, probably has no account yet.
function openAuthDialog(tab = "signin") {
  setAuthTab(tab);
  document.getElementById("signin-error").hidden = true;
  document.getElementById("create-error").hidden = true;
  document.getElementById("signin-password").value = "";
  document.getElementById("create-password").value = "";
  document.getElementById("create-password-confirm").value = "";
  document.getElementById("auth-dialog").showModal();
  (tab === "signin" ? document.getElementById("signin-email") : document.getElementById("create-name")).focus();
}

function setAuthTab(tab) {
  const signin = tab === "signin";
  document.getElementById("auth-tab-signin").setAttribute("aria-selected", String(signin));
  document.getElementById("auth-tab-create").setAttribute("aria-selected", String(!signin));
  document.getElementById("signin-form").hidden = !signin;
  document.getElementById("create-form").hidden = signin;
}

function closeAuthDialog() {
  if (document.getElementById("auth-dialog").open) {
    document.getElementById("auth-dialog").close();
  }
  // A cancelled dialog cancels the pending action too — I don't want a
  // publish firing on a later, unrelated sign-in.
  pendingAfterAuth = null;
}

// runPendingAfterAuth runs the one-shot post-auth callback (if any) and
// clears it. Called from both auth success paths.
function runPendingAfterAuth() {
  const fn = pendingAfterAuth;
  pendingAfterAuth = null;
  if (fn) fn();
}

/* ------------------------------- API layer ----------------------------- */

// My fetch wrapper. I keep it tiny and boring: JSON in, JSON out, and a
// single decision — a 401 on a CREATOR call means "open the auth dialog";
// a 401 on a PUBLIC call means "wrong access password" and must NEVER open
// the dialog.
class ApiError extends Error {
  constructor(message, status, data) {
    super(message);
    this.status = status;
    this.data = data;
  }
}

// isPublicCall mirrors my Go-side split: public endpoints and respondent
// submissions are the two flows that are allowed to be unauthenticated.
function isPublicCall(method, path) {
  return (
    path.startsWith("/api/v1/public") ||
    (method === "POST" && path.endsWith("/submissions"))
  );
}

async function api(method, path, { body, headers = {}, isPublic = isPublicCall(method, path) } = {}) {
  const res = await fetch(path, {
    method,
    headers: {
      ...(body ? { "Content-Type": "application/json" } : {}),
      ...headers,
      // my creator token goes on creator calls only
      ...(!isPublic && pb.authStore.token ? { Authorization: `Bearer ${pb.authStore.token}` } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });

  let data = null;
  try {
    data = await res.json();
  } catch {
    // 204 NoContent and a few other shapes have no body — that's fine.
  }

  if (!res.ok) {
    const message = (data && data.message) || `Request failed (${res.status})`;
    if (res.status === 401 && !isPublic) {
      openAuthDialog("signin");
    }
    throw new ApiError(message, res.status, data);
  }
  return data;
}

/* -------------------------------- toasts ------------------------------- */

// Tiny toast stack. app.css animates the in/out; I just schedule removal.
function toast(message, kind = "info") {
  const el = document.createElement("div");
  el.className = `toast${kind === "info" ? "" : ` toast-${kind}`}`;
  el.textContent = message;
  document.getElementById("toast-wrap").appendChild(el);
  setTimeout(() => el.classList.add("toast-out"), 3400);
  setTimeout(() => el.remove(), 3900);
}

/* ---------------------------- view controllers ------------------------- */

const controllers = {
  home() {
    initHome();
  },
  dashboard() {
    void initDashboard();
  },
  form() {
    initRespondent();
  },
  results() {
    void initResults();
  },
  builder() {
    initBuilder();
  },
  share() {
    void initShare();
  },
  notfound() {
    // Nothing to fetch — the pane is static.
  },
};

/* --------------------------------- home -------------------------------- */

// The guest landing page. I keep it deliberately small: one pitch, one
// button, and the ephemeral promise in plain words.
function initHome() {
  // If I'm actually signed in, home means "my forms" — the router already
  // mapped / to dashboard, so this controller only runs for guests. This
  // guard covers the edge where a token restored from localStorage flips
  // auth state after the first render.
  if (isSignedIn()) {
    navigate("/");
    return;
  }
}

/* ----------------------------- dashboard ------------------------------- */

async function initDashboard() {
  const prompt = document.getElementById("dashboard-signin-prompt");
  const empty = document.getElementById("dashboard-empty");
  const list = document.getElementById("form-list");

  if (!isSignedIn()) {
    prompt.hidden = false;
    empty.hidden = true;
    list.hidden = true;
    return;
  }
  prompt.hidden = true;

  let forms;
  try {
    forms = await api("GET", "/api/v1/forms");
  } catch (err) {
    toast(err.message, "error");
    return;
  }
  state.forms = forms;

  if (!forms.length) {
    empty.hidden = false;
    list.hidden = true;
    return;
  }
  empty.hidden = true;
  list.hidden = false;
  list.innerHTML = forms.map(formCardHtml).join("");
}

// One card per form. Every string that came from my forms (or could be
// edited by a future co-creator) goes through escapeHtml/escapeAttr before
// it touches innerHTML.
function formCardHtml(f) {
  const badges =
    (f.status === "active"
      ? '<span class="badge badge-active">Active</span>'
      : '<span class="badge badge-expired">Expired</span>') +
    (f.accessPasswordSet ? ' <span class="badge badge-lock">Locked</span>' : "");
  // Ownership badge: "Shared with you" marks a co-creator seat, and shows
  // WHO owns the form so I always know who to ask.
  const owner = f.isOwner
    ? ""
    : ` <span class="badge badge-shared" title="Owner: ${escapeAttr(f.ownerName || f.ownerEmail || "unknown")}">Shared with you</span>`;
  const meta = `${f.questionCount} question${f.questionCount === 1 ? "" : "s"} · ` +
    `${f.submissionCount} response${f.submissionCount === 1 ? "" : "s"} · ` +
    expiryText(f);
  const editBtn =
    f.status === "active"
      ? `<button type="button" class="btn btn-ghost btn-sm" data-action="edit" data-id="${escapeAttr(f.id)}">Edit</button>`
      : "";
  // Owner-only actions: Share (invite/manage co-creators) and Delete.
  // Co-creators see Open/Results/Edit — never Delete, never Share.
  const shareBtn = f.isOwner
    ? `<button type="button" class="btn btn-ghost btn-sm" data-action="share" data-id="${escapeAttr(f.id)}">Share</button>`
    : "";
  const deleteBtn = f.isOwner
    ? `<button type="button" class="btn btn-danger btn-sm" data-action="delete" data-id="${escapeAttr(f.id)}" data-title="${escapeAttr(f.title)}">Delete</button>`
    : "";
  const publicUrl = `${window.location.origin}/f/${f.shortCode}`;
  return `<li class="card form-card">
    <div class="form-card-main">
      <div class="form-card-title-row"><h2>${escapeHtml(f.title)}</h2>${badges}${owner}</div>
      <p class="form-card-meta">${escapeHtml(meta)}</p>
      <code class="short-link" data-action="copy" data-code="${escapeAttr(publicUrl)}" title="Copy public link">/f/${escapeAttr(f.shortCode)}</code>
    </div>
    <div class="form-card-actions">
      <button type="button" class="btn btn-ghost btn-sm" data-action="open" data-code="${escapeAttr(f.shortCode)}">Open</button>
      <button type="button" class="btn btn-ghost btn-sm" data-action="results" data-id="${escapeAttr(f.id)}">Results</button>
      ${shareBtn}
      ${editBtn}
      ${deleteBtn}
    </div>
  </li>`;
}

// I use ONE delegated click handler for the whole form list instead of a
// listener per button — the list re-renders on every update, and this way
// the wiring never goes stale.
function initDashboardEvents() {
  document.getElementById("btn-new-form").addEventListener("click", () => {
    navigate("/forms/new");
  });

  document.getElementById("form-list").addEventListener("click", (e) => {
    const el = e.target.closest("[data-action]");
    if (!el) return;
    const { action, id, code, title } = el.dataset;
    if (action === "open") {
      navigate(`/f/${code}`);
    } else if (action === "results") {
      navigate(`/forms/${id}/results`);
    } else if (action === "edit") {
      navigate(`/forms/${id}`);
    } else if (action === "share") {
      navigate(`/forms/${id}/share`);
    } else if (action === "copy") {
      navigator.clipboard
        .writeText(code)
        .then(() => toast("Public link copied", "success"))
        .catch(() => toast("Couldn't copy — it's right there", "error"));
    } else if (action === "delete") {
      if (!confirm(`Delete "${title}" and ALL of its responses? This can't be undone.`)) return;
      void (async () => {
        try {
          await api("DELETE", `/api/v1/forms/${id}`);
          toast("Form deleted", "success");
          await initDashboard();
        } catch (err) {
          toast(err.message, "error");
        }
      })();
    }
  });
}

/* --------------------------- respondent view --------------------------- */

function initRespondent() {
  state.currentForm = null;
  state.accessPassword = "";
  document.getElementById("view-form-title").textContent = "Respond";
  document.getElementById("password-error").hidden = true;
  document.getElementById("password-input").value = "";

  if (!state.shortCode) {
    showOnly("form-empty");
    return;
  }
  void loadPublicForm();
}

// Hides every respondent state except the one I name. The five states are
// the exact set in index.html — this is my switch statement.
function showOnly(id) {
  for (const el of ["form-empty", "form-expired", "form-password", "form-mount", "form-thanks"]) {
    document.getElementById(el).hidden = el !== id;
  }
}

// Fetches the public schema. password === null means "no password sent"
// (first load); a non-null password means the user hit Unlock, and a 401
// in that case gets the "wrong password" error line.
async function loadPublicForm(password = null) {
  const headers = password != null ? { "X-Access-Password": password } : {};
  try {
    const data = await api(
      "GET",
      `/api/v1/public/forms/${encodeURIComponent(state.shortCode)}`,
      { headers, isPublic: true },
    );
    state.currentForm = data;
    if (password != null) state.accessPassword = password;
    document.getElementById("password-error").hidden = true;
    renderRespondentForm();
  } catch (err) {
    if (err.status === 410) {
      // My Go side answers 410 Gone for expired forms — their table was
      // already dropped by the cron. (Direct loads of /f/<code> for dead
      // forms get the 410 page straight from Go; this state covers the
      // password-gate race.)
      showOnly("form-expired");
      return;
    }
    if (err.status === 404) {
      showOnly("form-empty");
      return;
    }
    if (err.status === 401) {
      showOnly("form-password");
      // My API's 401 body carries no extra data (the router mangles
      // arbitrary maps), so the gate stays generic — the form's title
      // appears once the password unlocks the schema.
      if (password != null) {
        document.getElementById("password-error").hidden = false;
      }
      document.getElementById("password-input").focus();
      return;
    }
    showOnly("form-empty");
    toast(err.message, "error");
  }
}

function renderRespondentForm() {
  const f = state.currentForm;
  showOnly("form-mount");
  document.getElementById("view-form-title").textContent = f.title;
  document.getElementById("form-mount-body").innerHTML = f.questions.map(questionHtml).join("");
}

// My question renderer. I map my three schema types to native controls:
//   text     -> <input type=text>
//   choice   -> radio group (one <input type=radio> per option)
//   dropdown -> <select> with a blank placeholder option
// SECURITY: every label and option value passes escapeHtml/escapeAttr —
// a sneaky form creator must not be able to XSS a respondent (or myself,
// when I read results back).
function questionHtml(q) {
  const inputId = `q_${q.id}`;
  const req = q.required ? '<span class="req" aria-hidden="true">*</span>' : "";
  if (q.type === "text") {
    return `<div class="question" data-qid="${escapeAttr(q.id)}">
      <label class="question-label" for="${inputId}">${escapeHtml(q.label)}${req}</label>
      <input id="${inputId}" class="field-text" type="text" maxlength="2000" />
      <p class="question-error" data-qerr="${escapeAttr(q.id)}" hidden></p>
    </div>`;
  }
  if (q.type === "choice") {
    const opts = q.options
      .map((opt) => `<label><input type="radio" name="${inputId}" value="${escapeAttr(opt)}" /> ${escapeHtml(opt)}</label>`)
      .join("");
    return `<div class="question" data-qid="${escapeAttr(q.id)}">
      <span class="question-label" id="label_${q.id}">${escapeHtml(q.label)}${req}</span>
      <div class="field-choice" role="radiogroup" aria-labelledby="label_${q.id}">${opts}</div>
      <p class="question-error" data-qerr="${escapeAttr(q.id)}" hidden></p>
    </div>`;
  }
  // dropdown
  const opts =
    '<option value="">—</option>' +
    q.options.map((opt) => `<option value="${escapeAttr(opt)}">${escapeHtml(opt)}</option>`).join("");
  return `<div class="question" data-qid="${escapeAttr(q.id)}">
    <label class="question-label" for="${inputId}">${escapeHtml(q.label)}${req}</label>
    <select id="${inputId}" class="field-dropdown">${opts}</select>
    <p class="question-error" data-qerr="${escapeAttr(q.id)}" hidden></p>
  </div>`;
}

function showQuestionError(qid, msg) {
  const el = document.querySelector(`[data-qerr="${CSS.escape(qid)}"]`);
  if (el) {
    el.textContent = msg;
    el.hidden = false;
  }
}

function clearQuestionError(qid) {
  const el = document.querySelector(`[data-qerr="${CSS.escape(qid)}"]`);
  if (el) {
    el.textContent = "";
    el.hidden = true;
  }
}

async function handleRespondentSubmit(e) {
  e.preventDefault();
  const f = state.currentForm;
  if (!f) return;

  // I collect answers by schema id — the same keys my Go side expects,
  // and the same keys the dynamic collection's columns use.
  const answers = {};
  let firstBad = null;
  for (const q of f.questions) {
    clearQuestionError(q.id);
    let value = "";
    if (q.type === "text") {
      value = document.getElementById(`q_${q.id}`).value.trim();
    } else if (q.type === "choice") {
      const sel = document.querySelector(`input[name="q_${q.id}"]:checked`);
      value = sel ? sel.value : "";
    } else {
      value = document.getElementById(`q_${q.id}`).value;
    }
    answers[q.id] = value;
    if (q.required && !value) {
      showQuestionError(q.id, q.type === "text" ? "Please answer this question." : "Please pick an option.");
      firstBad ??= q.id;
    }
  }
  if (firstBad) {
    const bad = document.querySelector(`[data-qid="${CSS.escape(firstBad)}"]`);
    bad?.querySelector("input, select")?.focus();
    return;
  }

  const btn = document.getElementById("submit-btn");
  btn.disabled = true;
  try {
    await api(
      "POST",
      `/api/v1/forms/${encodeURIComponent(f.shortCode)}/submissions`,
      {
        body: { answers },
        isPublic: true,
        // I resend the password that unlocked the form — it travels in a
        // header, never in the URL, so it can't leak into access logs.
        headers: state.accessPassword ? { "X-Access-Password": state.accessPassword } : {},
      },
    );
    showOnly("form-thanks");
  } catch (err) {
    if (err.status === 400) {
      // My Go side translates PocketBase field-validation failures into a
      // 400 whose data is {questionId: [messages]} — I surface each one
      // under its question.
      const data = err.data?.data;
      if (data && typeof data === "object") {
        for (const [qid, item] of Object.entries(data)) {
          const msg = validationMessage(item);
          if (msg) showQuestionError(qid, msg);
        }
      }
      toast(err.message, "error");
    } else if (err.status === 401) {
      // The password stopped being accepted (changed or added) — back to
      // the gate.
      state.accessPassword = "";
      showOnly("form-password");
      document.getElementById("password-error").hidden = false;
    } else {
      toast(err.message, "error");
    }
  } finally {
    btn.disabled = false;
  }
}

/* ------------------------------ results panel -------------------------- */

async function initResults() {
  const prompt = document.getElementById("results-signin-prompt");
  const grid = document.getElementById("results-grid");

  if (!isSignedIn()) {
    prompt.hidden = false;
    grid.hidden = true;
    return;
  }
  prompt.hidden = true;

  const formId = state.route.formId;
  if (!formId) {
    // /forms/<id>/results always carries the id — a bare results URL is
    // not a route I support anymore; home is where form picking happens.
    navigate("/");
    return;
  }
  state.currentFormId = formId;
  await loadResults(formId);
}

async function loadResults(formId) {
  const grid = document.getElementById("results-grid");
  grid.hidden = false;
  document.getElementById("results-form-title").textContent = "Loading…";
  document.getElementById("results-form-sub").textContent = "";
  document.getElementById("results-charts").innerHTML = '<p class="question-hint">Loading…</p>';
  document.getElementById("results-table-body").innerHTML = "";

  let data, subs;
  try {
    [data, subs] = await Promise.all([
      api("GET", `/api/v1/forms/${formId}/results`),
      api("GET", `/api/v1/forms/${formId}/submissions`),
    ]);
  } catch (err) {
    if (err.status === 404) {
      toast("This form doesn't exist (or I don't have access to it).", "error");
      navigate("/");
      return;
    }
    toast(err.message, "error");
    grid.hidden = true;
    return;
  }

  // textContent, so no escaping needed — my browser handles it.
  document.getElementById("results-form-title").textContent = data.form.title;
  document.getElementById("results-form-sub").textContent =
    `${data.form.submissionCount} responses · ${expiryText(data.form)}` +
    (data.form.isOwner ? "" : ` · shared with you by ${data.form.ownerName || data.form.ownerEmail || "the owner"}`);

  renderCharts(data.results);
  renderDatasheet(data.results, subs);
}

// One figure per question. Choice/dropdown questions get my inline SVG bar
// charts (no charting library — zero third-party code in my product);
// free-text questions get a plain answer-count stat.
function renderCharts(results) {
  const stack = document.getElementById("results-charts");
  if (!results.length) {
    stack.innerHTML = '<p class="question-hint">This form has no questions.</p>';
    return;
  }
  stack.innerHTML = results.map(chartFigure).join("");
}

function chartFigure(r) {
  const typeLabel =
    r.type === "choice" ? "Multiple choice" : r.type === "dropdown" ? "Dropdown" : "Free text";
  const total = r.answered + r.unanswered;
  let body;
  if (r.type === "text") {
    body = `<div class="text-stat"><strong>${r.answered}</strong> answers from ${total} responses</div>`;
  } else if (total === 0) {
    body = '<div class="chart-empty">No responses yet — the chart appears here as they arrive.</div>';
  } else {
    body = barChartSVG(r);
  }
  return `<figure class="chart-figure">
    <figcaption>${escapeHtml(r.label)} <span class="chart-qtype">${typeLabel}</span></figcaption>
    ${body}
  </figure>`;
}

// My bar chart is a hand-built SVG: a label, a track, a proportional bar,
// and a count per option row. I scale against the MAX count so the biggest
// option always reads full-width.
function barChartSVG(r) {
  const rows = r.options.map((opt) => [opt, r.counts?.[opt] || 0]);
  const max = Math.max(1, ...rows.map(([, n]) => n));
  const rowH = 26;
  const labelW = 170;
  const barW = 220;
  const countW = 40;
  const pad = 4;
  const width = labelW + barW + countW;
  const height = rows.length * rowH + pad * 2;

  let inner = "";
  rows.forEach(([opt, n], i) => {
    const y = pad + i * rowH;
    const w = Math.round((barW * n) / max);
    inner += `<text class="chart-label" x="0" y="${y + 16}">${escapeHtml(truncate(opt, 24))}</text>`;
    inner += `<rect class="chart-bar-track" x="${labelW}" y="${y + 7}" width="${barW}" height="12" rx="3"></rect>`;
    if (n > 0) {
      inner += `<rect class="chart-bar" x="${labelW}" y="${y + 7}" width="${Math.max(w, 4)}" height="12" rx="3"></rect>`;
    }
    inner += `<text class="chart-count" x="${labelW + barW + 8}" y="${y + 16}">${n}</text>`;
  });

  return `<svg class="bar-chart" viewBox="0 0 ${width} ${height}" role="img" aria-label="Bar chart for ${escapeAttr(r.label)}">${inner}</svg>`;
}

// The raw datasheet: one row per submission, one column per question,
// newest first (my Go side sorts that way). I escape every cell —
// respondent answers are the most likely XSS payload in this product.
function renderDatasheet(results, rows) {
  const head = document.getElementById("results-table-head");
  const body = document.getElementById("results-table-body");
  const cols = results.map((r) => r.label || r.id);
  head.innerHTML =
    "<th>Submitted</th>" + cols.map((c) => `<th>${escapeHtml(c)}</th>`).join("");
  if (!rows.length) {
    body.innerHTML = `<tr><td class="empty-row" colspan="${cols.length + 1}">No submissions yet.</td></tr>`;
    return;
  }
  body.innerHTML = rows
    .map((row) => {
      const tds = results
        .map((r) => `<td title="${escapeAttr(row[r.id] || "")}">${escapeHtml(row[r.id] || "—")}</td>`)
        .join("");
      return `<tr><td>${escapeHtml(fmtDate(row.created))}</td>${tds}</tr>`;
    })
    .join("");
}

function initResultsEvents() {
  document.getElementById("btn-results-back").addEventListener("click", () => {
    navigate("/");
  });
}

/* -------------------------------- builder ------------------------------ */

function initBuilder() {
  const prompt = document.getElementById("builder-signin-prompt");
  const body = document.getElementById("builder-body");

  // GUESTS MAY BUILD. The builder is open to everyone — the account only
  // becomes necessary at publish time (my guest draft rides in
  // localStorage until then). So there's no sign-in gate in this view
  // anymore; the prompt card stays only as a fallback for a dead token.
  if (!isSignedIn() && state.route.formId) {
    // Editing a SPECIFIC form requires a session (I need a form id I'm
    // allowed to touch) — without one, the API answers 401 and the dialog
    // opens; I still show the prompt so the pane isn't empty.
    prompt.hidden = false;
    body.hidden = true;
    return;
  }
  prompt.hidden = true;
  body.hidden = false;

  // ?form=<id> is GONE — the path carries it: /forms/<id> is edit mode,
  // /forms/new is a fresh draft.
  const formId = state.route.formId;
  if (formId) {
    state.currentFormId = formId;
    void loadBuilderForEdit(formId);
  } else {
    startNewBuilder();
  }
}

// Guest draft persistence. I write on every change and read on open — the
// draft is the guest's form until publish claims it.
function saveDraft() {
  try {
    localStorage.setItem(
      DRAFT_KEY,
      JSON.stringify({
        title: builderDraft.title,
        lifespanDays: builderDraft.lifespanDays,
        accessPassword: builderDraft.accessPassword,
        questions: builderDraft.questions,
      }),
    );
  } catch {
    // localStorage can throw (private mode, quota) — a lost draft is
    // annoying but not fatal, so I swallow it.
  }
}

function loadDraft() {
  try {
    const raw = localStorage.getItem(DRAFT_KEY);
    if (!raw) return null;
    const d = JSON.parse(raw);
    if (!d || !Array.isArray(d.questions)) return null;
    return d;
  } catch {
    return null;
  }
}

function clearDraft() {
  try {
    localStorage.removeItem(DRAFT_KEY);
  } catch {
    // same reasoning as saveDraft
  }
}

function startNewBuilder() {
  state.currentFormId = null;
  state.isOwner = true; // a fresh draft is mine by definition

  // Restore my in-progress draft if I have one — guests expect their
  // work to survive a navigation, and signed-in users get the same.
  const draft = loadDraft();
  builderDraft.title = draft?.title || "";
  builderDraft.lifespanDays = draft?.lifespanDays || 7;
  builderDraft.accessPassword = draft?.accessPassword || "";
  builderDraft.questions = draft?.questions?.length
    ? draft.questions
    : [{ id: "q1", type: "text", label: "", required: false, options: [] }];
  builderDraft.editing = false;
  builderDraft.formId = null;

  const restored = Boolean(draft?.title || (draft?.questions || []).some((q) => q.label));
  document.getElementById("builder-heading").textContent = restored ? "Form Builder — restored draft" : "Form Builder";
  document.getElementById("builder-draft-note").hidden = !restored;
  document.getElementById("builder-form-title").value = builderDraft.title;
  document.getElementById("builder-lifespan").value = builderDraft.lifespanDays;
  document.getElementById("builder-password").value = "";
  document.getElementById("builder-password").placeholder = "none";
  document.getElementById("builder-password-label").textContent = "Access password (optional)";
  document.getElementById("builder-remove-password-row").hidden = true;
  document.getElementById("builder-remove-password").checked = false;
  document.getElementById("builder-readonly-note").hidden = true;
  document.getElementById("btn-add-question").hidden = false;
  document.getElementById("btn-builder-share").hidden = true;
  document.getElementById("btn-builder-save").textContent = isSignedIn() ? "Publish form" : "Publish form";
  document.getElementById("builder-error").hidden = true;
  renderBuilderQuestions();
}

async function loadBuilderForEdit(formId) {
  document.getElementById("builder-questions").innerHTML = '<p class="question-hint">Loading…</p>';
  let data;
  try {
    data = await api("GET", `/api/v1/forms/${formId}`);
  } catch (err) {
    toast(err.message, "error");
    if (err.status === 404) {
      // The form vanished or I don't have a seat on it — my API answers
      // 404 for BOTH on purpose (no existence leaks), so I send the user
      // home instead of stranding them in a half-loaded editor.
      navigate("/");
    }
    return;
  }

  builderDraft.title = data.form.title;
  builderDraft.lifespanDays = data.form.lifespanDays;
  builderDraft.accessPassword = "";
  builderDraft.questions = data.schema.questions.map((q) => ({
    id: q.id,
    type: q.type,
    label: q.label,
    required: q.required,
    options: [...(q.options || [])],
  }));
  builderDraft.editing = true;
  builderDraft.formId = formId;
  state.isOwner = data.form.isOwner;

  // Co-creators see a different heading — the same editor, a different
  // role. The schema is frozen for BOTH (see loadBuilderForEdit's
  // read-only blocks); owners additionally get the Share shortcut.
  document.getElementById("builder-heading").textContent = data.form.isOwner
    ? `Editing — ${data.form.title}`
    : `Editing — ${data.form.title} (co-creator)`;
  document.getElementById("builder-draft-note").hidden = true;
  document.getElementById("builder-form-title").value = data.form.title;
  document.getElementById("builder-lifespan").value = data.form.lifespanDays;
  document.getElementById("builder-password").value = "";
  document.getElementById("builder-password").placeholder = data.form.accessPasswordSet ? "keep current" : "none";
  document.getElementById("builder-password-label").textContent = "New access password (optional)";
  document.getElementById("builder-remove-password-row").hidden = !data.form.accessPasswordSet;
  document.getElementById("builder-remove-password").checked = false;
  document.getElementById("builder-readonly-note").hidden = false;
  document.getElementById("btn-add-question").hidden = true;
  const shareBtn = document.getElementById("btn-builder-share");
  shareBtn.hidden = !data.form.isOwner;
  shareBtn.dataset.formId = formId;
  document.getElementById("btn-builder-save").textContent = "Save changes";
  document.getElementById("builder-error").hidden = true;
  renderBuilderQuestions();
}

// I re-render the WHOLE question list on add/remove, but for per-block
// changes (type switch, option add/remove) I swap only that one block —
// re-rendering everything on a keystroke-adjacent action would cost me
// focus.
function renderBuilderQuestions() {
  const wrap = document.getElementById("builder-questions");
  wrap.innerHTML = "";
  builderDraft.questions.forEach((q, i) => {
    wrap.appendChild(builderDraft.editing ? readonlyQuestionBlock(q, i) : questionEditorBlock(q, i));
  });
}

function replaceQuestionBlock(i) {
  const wrap = document.getElementById("builder-questions");
  const q = builderDraft.questions[i];
  const fresh = builderDraft.editing ? readonlyQuestionBlock(q, i) : questionEditorBlock(q, i);
  wrap.replaceChild(fresh, wrap.children[i]);
}

// My builder keeps ids canonical (q1..qN) in the same order my Go side
// will canonicalize them on save — so what I see is what gets stored.
function renumberDraftQuestions() {
  builderDraft.questions.forEach((q, i) => {
    q.id = `q${i + 1}`;
  });
}

function questionEditorBlock(q, i) {
  const div = document.createElement("div");
  div.className = "builder-question";
  const hasOptions = q.type === "choice" || q.type === "dropdown";
  div.innerHTML = `
    <div class="builder-question-head">
      <span class="builder-q-index">Question ${i + 1}</span>
      <div style="display:flex; gap:0.5rem; align-items:center;">
        <select class="field-dropdown builder-q-type" aria-label="Question type">
          <option value="text"${q.type === "text" ? " selected" : ""}>Text</option>
          <option value="choice"${q.type === "choice" ? " selected" : ""}>Multiple choice</option>
          <option value="dropdown"${q.type === "dropdown" ? " selected" : ""}>Dropdown</option>
        </select>
        <button type="button" class="btn btn-ghost btn-sm builder-q-remove">Remove</button>
      </div>
    </div>
    <div>
      <label class="field-label">Question</label>
      <input class="field-text builder-q-label" placeholder="What do you want to ask?" maxlength="200" value="${escapeAttr(q.label)}" />
    </div>
    <label class="check-label"><input type="checkbox" class="builder-q-required"${q.required ? " checked" : ""} /> Required</label>
    ${hasOptions ? optionsEditorHtml(q) : ""}
  `;

  // I update the draft in place on input (no re-render) so I never lose
  // focus while typing — and I persist the guest draft on every change.
  div.querySelector(".builder-q-label").addEventListener("input", (e) => {
    q.label = e.target.value;
    persistDraftIfNew();
  });
  div.querySelector(".builder-q-required").addEventListener("change", (e) => {
    q.required = e.target.checked;
    persistDraftIfNew();
  });
  div.querySelector(".builder-q-type").addEventListener("change", (e) => {
    q.type = e.target.value;
    replaceQuestionBlock(i);
    persistDraftIfNew();
  });
  div.querySelector(".builder-q-remove").addEventListener("click", () => {
    builderDraft.questions.splice(i, 1);
    renumberDraftQuestions();
    renderBuilderQuestions();
    persistDraftIfNew();
  });
  wireOptionEditors(div, q, i);
  return div;
}

function optionsEditorHtml(q) {
  const rows = q.options
    .map(
      (opt, j) => `<div class="builder-option-row">
        <input class="field-text builder-option-input" placeholder="Option ${j + 1}" value="${escapeAttr(opt)}" data-opt="${j}" />
        <button type="button" class="btn btn-ghost btn-sm builder-option-remove" data-opt="${j}" aria-label="Remove option">×</button>
      </div>`,
    )
    .join("");
  return `<div class="builder-options">${rows}
    <div><button type="button" class="btn btn-ghost btn-sm builder-option-add">Add option</button></div>
  </div>`;
}

function wireOptionEditors(div, q, i) {
  div.querySelectorAll(".builder-option-input").forEach((inp) => {
    inp.addEventListener("input", () => {
      q.options[Number(inp.dataset.opt)] = inp.value;
      persistDraftIfNew();
    });
  });
  div.querySelectorAll(".builder-option-remove").forEach((btn) => {
    btn.addEventListener("click", () => {
      q.options.splice(Number(btn.dataset.opt), 1);
      replaceQuestionBlock(i);
      persistDraftIfNew();
    });
  });
  const add = div.querySelector(".builder-option-add");
  if (add) {
    add.addEventListener("click", () => {
      q.options.push("");
      replaceQuestionBlock(i);
      persistDraftIfNew();
    });
  }
}

// persistDraftIfNew saves the localStorage draft, but ONLY in new-form
// mode — in edit mode the "draft" is the live form, and overwriting the
// guest draft with a loaded form would corrupt what I saved earlier.
function persistDraftIfNew() {
  if (!builderDraft.editing) saveDraft();
}

// Edit mode: the schema is FROZEN. I render a read-only summary so I can
// see what's in the form without being able to break its columns.
function readonlyQuestionBlock(q, i) {
  const div = document.createElement("div");
  div.className = "builder-question-readonly";
  const typeLabel = q.type === "text" ? "Text" : q.type === "choice" ? "Multiple choice" : "Dropdown";
  const chips = (q.options || [])
    .map((o) => `<span class="option-chip">${escapeHtml(o)}</span>`)
    .join(" ");
  div.innerHTML = `
    <div class="qro-meta">
      <span class="builder-q-index">Question ${i + 1}</span>
      <span class="chart-qtype">${typeLabel}</span>
      ${q.required ? '<span class="option-chip">Required</span>' : ""}
    </div>
    <div class="qro-label">${escapeHtml(q.label || "(no label)")}</div>
    ${chips ? `<div class="qro-meta">${chips}</div>` : ""}
  `;
  return div;
}

// buildFormPayload assembles the create request from my current inputs.
// Kept separate from saveBuilder so the guest publish flow (which runs
// AFTER the auth dialog) can reuse the exact same payload.
function buildFormPayload() {
  const title = document.getElementById("builder-form-title").value.trim();
  const lifespan = Number(document.getElementById("builder-lifespan").value);
  const pw = document.getElementById("builder-password").value;
  return {
    title,
    lifespanDays: lifespan,
    accessPassword: pw, // empty string == no password
    schema: {
      questions: builderDraft.questions.map((q, i) => ({
        id: `q${i + 1}`,
        type: q.type,
        label: q.label.trim(),
        required: q.required,
        ...(q.type === "text" ? {} : { options: q.options.map((o) => o.trim()).filter(Boolean) }),
      })),
    },
  };
}

// validateDraft is my client-side validation — a convenience. My Go side
// re-checks everything anyway; I never trust the frontend to be the judge.
function validateDraft() {
  const problems = [];
  const payload = buildFormPayload();
  if (!payload.title) problems.push("Give the form a title.");
  if (!Number.isInteger(payload.lifespanDays) || payload.lifespanDays < 1 || payload.lifespanDays > 30) {
    problems.push("Lifespan must be a whole number of days between 1 and 30.");
  }
  payload.schema.questions.forEach((q, i) => {
    if (!q.label) problems.push(`Question ${i + 1} needs a label.`);
    if (q.type !== "text") {
      if (q.options.length < 2) problems.push(`Question ${i + 1} needs at least 2 options.`);
      else if (new Set(q.options).size !== q.options.length) problems.push(`Question ${i + 1} has duplicate options.`);
    }
  });
  return { problems, payload };
}

// doPublish posts the draft to my API and lands on the new form's results
// page. Called directly when I'm signed in, or via pendingAfterAuth after
// the guest account dialog succeeds.
async function doPublish() {
  const payload = buildFormPayload();
  const created = await api("POST", "/api/v1/forms", { body: payload });
  clearDraft();
  builderDraft.editing = false;
  builderDraft.formId = created.id;
  toast(`Form created — share /f/${created.shortCode}`, "success");
  navigate(`/forms/${created.id}/results`);
}

async function saveBuilder() {
  const errorEl = document.getElementById("builder-error");

  if (!builderDraft.editing) {
    // NEW FORM. Validate first (both modes share the validator).
    const { problems, payload } = validateDraft();
    if (problems.length) {
      errorEl.textContent = problems.join(" ");
      errorEl.hidden = false;
      return;
    }
    errorEl.hidden = true;

    if (!isSignedIn()) {
      // The guest flow: my draft stays put, the auth dialog opens on the
      // "create account" tab, and doPublish runs the moment auth succeeds
      // (sign-in OR new account — either way the form lands in the
      // account that authenticated).
      pendingAfterAuth = () => {
        void doPublish().catch((err) => {
          errorEl.textContent = err.message;
          errorEl.hidden = false;
        });
      };
      openAuthDialog("create");
      return;
    }
    try {
      await doPublish();
    } catch (err) {
      errorEl.textContent = err.message;
      errorEl.hidden = false;
    }
    return;
  }

  // EDIT MODE (owner or co-creator): metadata only — title, lifespan,
  // password. The schema is frozen for everyone, so nothing about the
  // questions is sent.
  const title = document.getElementById("builder-form-title").value.trim();
  const lifespan = Number(document.getElementById("builder-lifespan").value);
  if (!title) {
    errorEl.textContent = "Give the form a title.";
    errorEl.hidden = false;
    return;
  }
  if (!Number.isInteger(lifespan) || lifespan < 1 || lifespan > 30) {
    errorEl.textContent = "Lifespan must be a whole number of days between 1 and 30.";
    errorEl.hidden = false;
    return;
  }
  errorEl.hidden = true;

  const pw = document.getElementById("builder-password").value;
  try {
    const payload = { title, lifespanDays: lifespan };
    if (pw) payload.accessPassword = pw;
    if (document.getElementById("builder-remove-password").checked) {
      payload.removePassword = true;
    }
    await api("PUT", `/api/v1/forms/${builderDraft.formId}`, { body: payload });
    toast("Form updated", "success");
    navigate("/");
  } catch (err) {
    errorEl.textContent = err.message;
    errorEl.hidden = false;
  }
}

function initBuilderEvents() {
  // I sync the draft fields into builderDraft on input so the localStorage
  // copy and the inputs never disagree.
  document.getElementById("builder-form-title").addEventListener("input", (e) => {
    builderDraft.title = e.target.value;
    persistDraftIfNew();
  });
  document.getElementById("builder-lifespan").addEventListener("input", (e) => {
    builderDraft.lifespanDays = Number(e.target.value);
    persistDraftIfNew();
  });
  document.getElementById("builder-password").addEventListener("input", (e) => {
    builderDraft.accessPassword = e.target.value;
    persistDraftIfNew();
  });

  document.getElementById("btn-add-question").addEventListener("click", () => {
    builderDraft.questions.push({
      id: `q${builderDraft.questions.length + 1}`,
      type: "text",
      label: "",
      required: false,
      options: [],
    });
    renumberDraftQuestions();
    renderBuilderQuestions();
    persistDraftIfNew();
  });
  document.getElementById("btn-builder-save").addEventListener("click", () => {
    void saveBuilder();
  });
  document.getElementById("btn-builder-share").addEventListener("click", (e) => {
    const id = e.currentTarget.dataset.formId;
    if (id) navigate(`/forms/${id}/share`);
  });
  document.getElementById("btn-builder-cancel").addEventListener("click", () => {
    navigate("/");
  });
}

/* ------------------------------ share screen --------------------------- */

// The owner's co-creation surface: the public link plus the invite list.
// Co-creators hitting /forms/<id>/share get a clear "owner only" card —
// my Go API enforces the same thing, this is just the polite version.
async function initShare() {
  const denied = document.getElementById("share-denied");
  const body = document.getElementById("share-body");
  const grid = document.getElementById("results-grid"); // not used here, but keep hidden state honest
  if (grid) grid.hidden = true;

  if (!isSignedIn()) {
    denied.hidden = false;
    body.hidden = true;
    // The API would 401 and open the dialog — I show the card instead and
    // let the user sign in from the header, then come back.
    document.getElementById("share-signin-note").hidden = false;
    return;
  }
  document.getElementById("share-signin-note").hidden = true;

  const formId = state.route.formId;
  if (!formId) {
    navigate("/");
    return;
  }

  let data;
  try {
    data = await api("GET", `/api/v1/forms/${formId}`);
  } catch (err) {
    toast(err.message, "error");
    if (err.status === 404) navigate("/");
    return;
  }

  state.isOwner = data.form.isOwner;
  if (!data.form.isOwner) {
    denied.hidden = false;
    body.hidden = true;
    return;
  }
  denied.hidden = true;
  body.hidden = false;

  const publicUrl = `${window.location.origin}/f/${data.form.shortCode}`;
  document.getElementById("share-link").value = publicUrl;
  document.getElementById("share-form-title").textContent = data.form.title;

  await renderCollaboratorList(formId);
}

// One row per seat. Active rows show the account's name; pending rows show
// the invited email with a "waiting for sign-up" badge — that's the state
// where the invite exists but the address hasn't registered yet.
function collaboratorRowHtml(c) {
  const who = c.status === "active" ? c.name || c.email : c.email;
  const badge =
    c.status === "active"
      ? '<span class="badge badge-active">Active</span>'
      : '<span class="badge badge-pending">Waiting for sign-up</span>';
  return `<li class="collab-row">
    <div class="collab-row-main">
      <span class="collab-row-email">${escapeHtml(who)}</span>
      <span class="collab-row-sub">${c.status === "active" ? escapeHtml(c.email) : "Invited — activates the moment this address signs up"}</span>
    </div>
    ${badge}
    <button type="button" class="btn btn-danger btn-sm collab-remove" data-id="${escapeAttr(c.id)}" data-email="${escapeAttr(c.email)}">Remove</button>
  </li>`;
}

async function renderCollaboratorList(formId) {
  const list = document.getElementById("collab-list");
  let rows;
  try {
    rows = await api("GET", `/api/v1/forms/${formId}/collaborators`);
  } catch (err) {
    toast(err.message, "error");
    list.innerHTML = "";
    return;
  }
  if (!rows.length) {
    list.innerHTML = '<p class="question-hint">No co-creators yet — invite the first one above.</p>';
    return;
  }
  list.innerHTML = rows.map(collaboratorRowHtml).join("");
}

function initShareEvents() {
  document.getElementById("btn-copy-link").addEventListener("click", () => {
    const input = document.getElementById("share-link");
    navigator.clipboard
      .writeText(input.value)
      .then(() => toast("Public link copied", "success"))
      .catch(() => {
        input.select();
        toast("Couldn't auto-copy — the link is selected for you", "error");
      });
  });

  document.getElementById("invite-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const formId = state.route.formId;
    const email = document.getElementById("invite-email").value.trim();
    const errEl = document.getElementById("invite-error");
    errEl.hidden = true;
    if (!email) return;

    const btn = document.getElementById("btn-invite");
    btn.disabled = true;
    try {
      await api("POST", `/api/v1/forms/${formId}/collaborators`, { body: { email } });
      document.getElementById("invite-email").value = "";
      toast(`Invite sent to ${email}`, "success");
      await renderCollaboratorList(formId);
    } catch (err) {
      errEl.textContent = err.message;
      errEl.hidden = false;
    } finally {
      btn.disabled = false;
    }
  });

  document.getElementById("collab-list").addEventListener("click", async (e) => {
    const btn = e.target.closest(".collab-remove");
    if (!btn) return;
    const formId = state.route.formId;
    if (!confirm(`Remove ${btn.dataset.email}'s access to this form?`)) return;
    try {
      await api("DELETE", `/api/v1/forms/${formId}/collaborators/${btn.dataset.id}`);
      toast("Co-creator removed", "success");
      await renderCollaboratorList(formId);
    } catch (err) {
      toast(err.message, "error");
    }
  });
}

/* ----------------------------- small utils ----------------------------- */

// escapeHtml — I escape every string before it touches the DOM. This is
// the one habit that's easy to forget under time pressure, so it lives at
// the top of my utils and gets used at EVERY injection site.
function escapeHtml(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

// escapeAttr — same escaping; the alias keeps call sites honest ("this
// goes into an attribute").
const escapeAttr = escapeHtml;

// PocketBase renders each field of a validation-error body as a single
// {code, message} object (older shapes may carry strings or arrays of
// them) — I normalize whatever arrives to the human message, so my
// per-question error lines never show "[object Object]".
function validationMessage(item) {
  if (typeof item === "string") return item;
  if (Array.isArray(item)) {
    return item
      .map((m) => (typeof m === "string" ? m : m?.message || ""))
      .filter(Boolean)
      .join(" ");
  }
  return item?.message || "";
}

function truncate(s, n) {
  s = String(s);
  return s.length > n ? `${s.slice(0, n - 1)}…` : s;
}

function fmtDate(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return String(iso);
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

// "expires in 3 days" / "expired" — my dashboard meta line.
function expiryText(f) {
  if (f.status !== "active") return "expired";
  const days = Math.ceil((new Date(f.expiresAt).getTime() - Date.now()) / 86400000);
  if (days <= 0) return "expiring today";
  if (days === 1) return "expires tomorrow";
  return `expires in ${days} days`;
}

/* --------------------------------- boot -------------------------------- */

function initApp() {
  // Auth UI + auth dialog (sign-in / create tabs).
  updateAuthUI();
  document.getElementById("btn-signin").addEventListener("click", () => openAuthDialog("signin"));
  document.getElementById("btn-signout").addEventListener("click", () => {
    pb.authStore.clear();
    state.forms = [];
    toast("Signed out");
    navigate("/");
  });

  document.getElementById("auth-tab-signin").addEventListener("click", () => setAuthTab("signin"));
  document.getElementById("auth-tab-create").addEventListener("click", () => setAuthTab("create"));
  document.getElementById("btn-auth-cancel").addEventListener("click", closeAuthDialog);

  // My prompt cards (dashboard, results, builder, share) each carry a
  // button that opens the auth dialog directly — less clicking than
  // hunting for the header button.
  document.querySelectorAll("[data-signin-open]").forEach((btn) => {
    btn.addEventListener("click", () => openAuthDialog("signin"));
  });
  // Clicking the dialog backdrop closes it (the event's target IS the
  // <dialog> element itself when I click outside its content).
  const dialog = document.getElementById("auth-dialog");
  dialog.addEventListener("click", (e) => {
    if (e.target === dialog) closeAuthDialog();
  });

  // SIGN IN tab. authWithPassword saves my token to the authStore itself.
  document.getElementById("signin-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const email = document.getElementById("signin-email").value.trim();
    const password = document.getElementById("signin-password").value;
    const errEl = document.getElementById("signin-error");
    errEl.hidden = true;
    try {
      await pb.collection("users").authWithPassword(email, password);
      closeAuthDialog();
      toast(`Welcome back, ${pb.authStore.record?.name || email}`, "success");
      runPendingAfterAuth();
    } catch {
      errEl.textContent = "Wrong email or password.";
      errEl.hidden = false;
    }
  });

  // CREATE tab. I create the account against the users collection (public
  // create rule), then log in through the same authWithPassword call my
  // sign-in tab uses: in PocketBase v0.40.6 the create response carries NO
  // token (just the record), and only the auth* methods save a session to
  // my authStore — so create-then-login is the only way to end up signed
  // in with a real token.
  document.getElementById("create-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const name = document.getElementById("create-name").value.trim();
    const email = document.getElementById("create-email").value.trim();
    const password = document.getElementById("create-password").value;
    const passwordConfirm = document.getElementById("create-password-confirm").value;
    const errEl = document.getElementById("create-error");
    errEl.hidden = true;
    if (password !== passwordConfirm) {
      errEl.textContent = "The passwords don't match.";
      errEl.hidden = false;
      return;
    }
    try {
      await pb.collection("users").create({
        name,
        email,
        password,
        passwordConfirm,
      });
      // authWithPassword saves the real token to my authStore itself —
      // the create response above carries no token in v0.40.6.
      await pb.collection("users").authWithPassword(email, password);
      closeAuthDialog();
      toast(`Account created — welcome, ${name}`, "success");
      runPendingAfterAuth();
    } catch (err) {
      // PocketBase field-validation failures arrive with a data map of
      // {field: {code, message}} — I render the first useful message.
      const fieldMsgs = err.data?.data;
      let msg = "Couldn't create the account.";
      if (fieldMsgs && typeof fieldMsgs === "object") {
        for (const item of Object.values(fieldMsgs)) {
          const text = validationMessage(item);
          if (text) {
            msg = text;
            break;
          }
        }
      }
      errEl.textContent = msg;
      errEl.hidden = false;
    }
  });

  // The auth store fires on sign-in AND sign-out (and on token restore).
  // When I'm sitting on a view that depends on auth (home adapts, creator
  // views re-render), I re-run the current route so the sign-in prompts
  // swap out for the real content.
  pb.authStore.onChange(() => {
    updateAuthUI();
    if (booted && ["home", "dashboard", "results", "builder", "share"].includes(state.view)) {
      renderRoute(parseRoute());
    }
  });

  // View-specific one-time wiring.
  initDashboardEvents();
  initResultsEvents();
  initBuilderEvents();
  initShareEvents();
  document.getElementById("form-mount").addEventListener("submit", handleRespondentSubmit);
  document.getElementById("password-form").addEventListener("submit", (e) => {
    e.preventDefault();
    const pw = document.getElementById("password-input").value;
    document.getElementById("password-error").hidden = true;
    document.getElementById("password-input").value = "";
    void loadPublicForm(pw);
  });
  document.getElementById("btn-home-start").addEventListener("click", () => {
    navigate("/forms/new");
  });

  // First route — the path decides which view opens.
  renderRoute(parseRoute());
}

// Module scripts are deferred, so the DOM is ready by the time this runs.
initApp();
