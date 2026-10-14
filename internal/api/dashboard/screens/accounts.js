// Accounts: every signed-in account with its allowance, plan and mode.
// #/accounts/add opens the Add account drawer over it.
import {
  S, esc, int, icon, logo, email, pill, warnState, accounts, status, limits, left, queue, plan, planDay, day,
  meterCell, table, modeMenu, planMenu, modeTitle, MODES, api, setMode, toast, fetchData, accountScope, routingOrder, rankTitle,
} from "../core.js";
import { accountPicker, bindAccountPicker } from "./account-picker.js";

function nextIds() {
  return new Set(["claude", "codex"].map((p) => queue(p).next?.id).filter(Boolean));
}

export function accountBadge(a, next) {
  const st = status(a);
  if (st.kind === "blocked" || st.kind === "error") return warnState(st.text);
  if (next.has(a.id)) return pill("Next");
  return "";
}

export function planCell(a) {
  const p = plan(a);
  if (p.ends) return warnState("Ends " + planDay(p.ends), "sm");
  if (p.renews) return esc(planDay(p.renews));
  return `<span class="muted">–</span>`;
}

function weekCells(a) {
  const st = status(a);
  const lim = limits(a);
  const w = lim.week, s = lim.short;
  if (st.kind === "off" || st.kind === "blocked" || st.kind === "error") return ["", "", ""];
  const resetAt = st.kind === "usedup" ? st.until || w?.reset : w?.reset;
  const weekLeft = st.kind === "usedup" ? meterCell(0) : w ? (w.notStarted ? `<span class="muted">New week</span>` : meterCell(left(w))) : `<span class="muted">No reading</span>`;
  const shortLeft = s && st.kind !== "usedup" ? meterCell(left(s)) : "";
  return [weekLeft, resetAt ? esc(day(resetAt)) : "", shortLeft];
}

export function view(ctx) {
  const keep = new Set(accountScope().ids);
  const all = accounts();
  const ordered = ["claude", "codex"].flatMap((p) => routingOrder(all.filter((a) => a.provider === p)));
  const list = ordered.filter((a) => keep.has(a.id));
  const next = nextIds();
  const cols = [
    { label: "Priority", w: 56, min: 52 },
    { label: "", w: 16, min: 16, cls: "ic" },
    { label: "Account" },
    { label: "Week left", w: 120 },
    { label: "Week resets", w: 104 },
    { label: "Plan renews", w: 128, cls: "plancol" },
    { label: "5 hours left", w: 120 },
    { label: "Requests today", w: 96, r: true },
    { label: "Mode", w: 104, cls: "modecol" },
    { label: "", w: 20, min: 20 },
  ];
  const rows = list.map((a) => {
    const [wl, wr, sl] = weekCells(a);
    const today = S.data?.summary?.accounts?.[a.id]?.today?.requests || 0;
    return {
      cls: "click",
      attrs: `data-href="#/accounts/${encodeURIComponent(a.id)}"`,
      cells: [
        `<span class="${a.rank ? "" : "muted"}" title="${esc(rankTitle(a))}">${a.rank || "—"}</span>`,
        logo(a.provider),
        `${email(a.email)}${accountBadge(a, next)}`,
        wl, wr, planCell(a), sl,
        int(today),
        `<button class="select" data-mode="${esc(a.id)}">${esc(modeTitle(a.mode))}${icon("chevronUpDown", 12)}</button>`,
        `<button class="iconbtn" data-more="${esc(a.id)}" aria-label="More" style="width:20px;height:20px">${icon("more")}</button>`,
      ],
    };
  });
  const html = `
    <div class="bar"><div class="end"><a class="btn" href="#/accounts/add">${icon("plus", 14, "var(--fg)")}Add account</a>${accountPicker()}</div></div>
    <div class="body">
      ${table(cols, rows, { empty: "No accounts signed in yet.", fit: true, cls: "acctbl" })}
      <div class="tfoot">${list.length} of ${all.length}</div>
    </div>
    ${ctx.add ? drawer() : ""}`;
  return {
    html,
    mount(root) {
      bindAccountPicker(root);
      root.querySelectorAll(".modecol").forEach((c) => { c.style.paddingLeft = "16px"; });
      root.querySelectorAll("[data-href]").forEach((r) => {
        r.onclick = (e) => { if (!e.target.closest("button")) location.hash = r.dataset.href; };
      });
      root.querySelectorAll("[data-mode]").forEach((b) => { b.onclick = () => modeMenu(b, all.find((a) => a.id === b.dataset.mode)); });
      root.querySelectorAll("[data-more]").forEach((b) => {
        const a = all.find((x) => x.id === b.dataset.more);
        b.onclick = () => planMenu(b, a, [{ a: "Open details", run: () => { location.hash = "#/accounts/" + encodeURIComponent(a.id); } }]);
      });
      if (ctx.add) mountDrawer(root);
    },
  };
}

// ---------- Add account ----------

const PROVIDERS = [{ id: "claude", label: "Claude" }, { id: "codex", label: "Codex" }];

function drawer() {
  const prov = S.ui.addProv || "claude";
  const mode = S.ui.addMode || "rotation";
  const st = S.ui.addState || null;
  const host = S.data?.server?.host || "the proxy";
  let body;
  if (st?.phase === "waiting") {
    const privacy = st.prov === "codex" ? "https://chatgpt.com/#settings/DataControls" : "https://claude.ai/settings/data-privacy-controls";
    body = `<div class="note"><b>Finish signing in in the new tab.</b><span>Sign in as the account you want to add and click Authorize. On a Mac with the sign-in helper, the tab comes back to the dashboard by itself, and this page updates when the account is saved.</span><span>Then switch off model training on its <a class="strong" href="${privacy}" target="_blank" rel="noopener">privacy page</a>.</span></div>
      <div><label class="k">If the tab shows an error page after Authorize instead, copy its address and paste it here</label>
      <div class="row gap8"><input class="input grow" data-paste value="${esc(S.ui.addPaste || "")}" placeholder="http://localhost:.../callback?code=..."><button class="btn outline" data-submit>Send</button></div></div>`;
  } else if (st?.phase === "error") {
    body = `<div class="note"><b class="warn">${esc(st.title || "Sign-in did not finish.")}</b><span>${esc(st.msg)}</span></div>`;
  } else {
    body = `<div class="muted">Sign in from home with no VPN on, as with cliproxy-login. The sign-in opens in a new tab.</div>`;
  }
  return `<div class="scrim" data-close></div>
  <div class="drawer" role="dialog" aria-label="Add account">
    <div class="dh"><span>Add account</span><button class="iconbtn" data-close aria-label="Close">${icon("close")}</button></div>
    <div class="db">
      <div><label class="k">Provider</label><div class="provseg">${PROVIDERS.map((p) => `<button class="${p.id === prov ? "on" : ""}" data-prov-pick="${p.id}" ${st?.phase === "waiting" ? "disabled" : ""}>${logo(p.id)}${p.label}</button>`).join("")}</div></div>
      <div><label class="k">Mode</label><div class="radios">${MODES.map((m) => `<button class="${m.id === mode ? "on" : ""}" data-mode-pick="${m.id}"><span class="dot"></span><span class="tx"><div class="a">${m.a}</div><div class="b">${m.b}</div></span></button>`).join("")}</div></div>
      ${body}
    </div>
    <div class="df"><button class="btn" data-close>Cancel</button><button class="btn primary" data-start ${st?.phase === "waiting" ? "disabled" : ""}>Sign in on ${esc(host)}${icon("external", 14)}</button></div>
  </div>`;
}

let poll = null;
let flow = 0; // bumped on every start and stop, so replies for an older flow are dropped

function stopFlow() {
  flow++;
  if (poll) clearTimeout(poll);
  poll = null;
  S.ui.addState = null;
  S.ui.addPaste = "";
}

// Leaving the drawer by any route (Back, a nav link, Close) ends the flow,
// including a sign-in link request that has not answered yet.
window.addEventListener("hashchange", () => {
  if (location.hash !== "#/accounts/add") stopFlow();
});

function fail(id, msg, title) {
  if (id !== flow) return;
  if (poll) clearTimeout(poll);
  poll = null;
  S.ui.addState = { phase: "error", msg, title };
  window.dispatchEvent(new Event("dash:render"));
}

function mountDrawer(root) {
  const rerender = () => window.dispatchEvent(new Event("dash:render"));
  const close = () => { stopFlow(); location.hash = "#/accounts"; };
  root.querySelectorAll("[data-close]").forEach((b) => { b.onclick = close; });
  root.querySelectorAll("[data-prov-pick]").forEach((b) => { b.onclick = () => { S.ui.addProv = b.dataset.provPick; rerender(); }; });
  root.querySelectorAll("[data-mode-pick]").forEach((b) => { b.onclick = () => { S.ui.addMode = b.dataset.modePick; rerender(); }; });
  const start = root.querySelector("[data-start]");
  start.onclick = async () => {
    const prov = S.ui.addProv || "claude";
    stopFlow();
    const id = flow;
    const win = window.open("", "_blank");
    try {
      const before = accounts().map((a) => a.id);
      const res = await api(`/oauth/auth-url?provider=${prov}&is_webui=true`);
      if (id !== flow) { if (win) win.close(); return; }
      if (!res?.url || !res?.state) throw new Error("The proxy did not return a sign-in link.");
      if (win) { win.opener = null; win.location = res.url; } else window.open(res.url, "_blank", "noopener");
      S.ui.addState = { phase: "waiting", state: res.state, prov, before };
      rerender();
      watch(id);
    } catch (e) {
      if (win) win.close();
      if (!e.cancelled) fail(id, e.message);
    }
  };
  const paste = root.querySelector("[data-paste]");
  if (paste) paste.oninput = () => { S.ui.addPaste = paste.value; };
  const submit = root.querySelector("[data-submit]");
  if (submit) {
    submit.onclick = async () => {
      const v = root.querySelector("[data-paste]").value.trim();
      if (!v || !S.ui.addState?.state) return;
      try {
        await api("/oauth/callback", { method: "POST", body: JSON.stringify({ redirect_url: v, state: S.ui.addState.state }) });
        toast("Sent. Waiting for the proxy to finish.");
      } catch (e) { toast(e.message, true); }
    };
  }
}

// One status check at a time, two seconds apart, until the flow ends.
function watch(id) {
  const tick = async () => {
    poll = null;
    const st = S.ui.addState;
    if (id !== flow || st?.phase !== "waiting") return;
    let r;
    try {
      r = await api(`/oauth/status?state=${encodeURIComponent(st.state)}`);
    } catch (e) {
      if (e.auth || e.cancelled) return fail(id, "This page stopped checking because it has no valid management key. Start again to enter it.");
      if (id === flow) poll = setTimeout(tick, 2000); // a network blip should not end the flow
      return;
    }
    if (id !== flow) return;
    if (r?.status === "ok") return finish(st, id);
    if (r?.status === "error") return fail(id, r.error || "The sign-in failed.");
    poll = setTimeout(tick, 2000);
  };
  poll = setTimeout(tick, 2000);
}

async function finish(st, id) {
  const mode = S.ui.addMode || "rotation";
  try {
    await fetchData();
  } catch (e) {
    return fail(id, e.message + ". Close this panel; the account shows up when the page next refreshes.", "Signed in, but this page could not reload.");
  }
  if (id !== flow) return;
  const added = accounts().find((a) => a.provider === st.prov && !st.before.includes(a.id));
  if (added && mode !== "rotation") {
    try { await setMode(added, mode); } catch (e) { toast("Signed in, but the mode did not change: " + e.message, true); }
  }
  toast(added ? added.email + " added" : "Signed in");
  // The account is saved either way; only move the page if the drawer is still open.
  if (id === flow) {
    S.ui.addState = null;
    location.hash = added ? "#/accounts/" + encodeURIComponent(added.id) : "#/accounts";
  }
  window.dispatchEvent(new Event("dash:refresh"));
}
