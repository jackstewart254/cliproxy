// Account details: limits and plan, the last 24 hours, its active sessions.
import {
  S, esc, icon, logo, accountById, status, limits, left, plan, planDay, planDaysAway, inDays, resetLong,
  modeMenu, planMenu, priorityMenu, modeTitle, sessions, warnState, seg, accounts,
} from "../core.js";
import { sessionTable } from "./common.js";
import { allowanceSeries, trajectory, outlook, rateText } from "./burn.js";
import { timeStrip, stripTime } from "./strip.js";
import { buildPanel, panelContext, snapGrid } from "./panels.js";
import { DAY, timeState, bindTime, selectionLabel, backToNow, windowText } from "./timeaxis.js";
import { region } from "../paint.js";

// One account as an account scope. Its performance scope is keyed by its id,
// so prov names the account and its percentiles stay exact.
const oneScope = (a) => ({ all: accounts(), shown: [a], ids: [a.id], prov: a.id, some: false });

const allowanceTime = (id) => timeState("acAllowance:" + id, { defaultWindow: (now) => ({ start: now - 3.5 * DAY, end: now + 3.5 * DAY }), future: true });

// The windows an Account page loads: its strip's.
export function wants(params) {
  const w = stripTime("acStrip:" + params[0]).window;
  // The strip's first-token figure is a median for its window.
  return { usage: w, perf: w, percentiles: true };
}

// "12% a day · Lasts to reset", or the warning when it runs out first.
function burnLine(tr) {
  if (!tr) return "";
  const rate = rateText(tr);
  return `<div class="sub">${rate ? esc(rate) + `<span class="dot">·</span>` : ""}${outlook(tr, { short: true })}</div>`;
}

function limitBlock(title, item, st, tr) {
  if (!item) {
    return `<div class="lim"><span class="muted">${title}</span><div class="bignum"><span class="v muted">–</span></div><div class="bar6"></div><div class="sub">No reading yet</div></div>`;
  }
  if (item.notStarted) {
    return `<div class="lim"><span class="muted">${title}</span><div class="bignum"><span class="v">100%</span><span class="u">left</span></div><div class="bar6"><i style="width:100%"></i></div><div class="sub">Starts at the next request</div></div>`;
  }
  const usedUp = st.kind === "usedup" && title === "Week";
  const p = usedUp ? 0 : left(item);
  const reset = usedUp ? st.until || item.reset : item.reset;
  return `<div class="lim"><span class="muted">${title}</span>
    <div class="bignum"><span class="v">${usedUp ? "Used up" : Math.round(p) + "%"}</span>${usedUp ? "" : `<span class="u">left</span>`}</div>
    <div class="bar6"><i style="width:${Math.round(p)}%"></i></div>
    <div class="sub">${reset ? icon("reset", 14) + esc(resetLong(reset)) : "&nbsp;"}</div>${usedUp ? "" : burnLine(tr)}</div>`;
}

function planBlock(a) {
  const p = plan(a);
  let main, sub;
  if (p.ends) {
    main = `<span class="v">${esc(planDay(p.ends))}</span><span class="u">ends</span>`;
    sub = warnState(inDays(planDaysAway(p.ends)) + ", no renewal", "sm");
  } else if (p.renews) {
    main = `<span class="v">${esc(planDay(p.renews))}</span><span class="u">renews</span>`;
    sub = esc(inDays(planDaysAway(p.renews))) + (p.source === "token" ? " · from sign-in" : "");
  } else {
    main = `<span class="v muted">–</span>`;
    sub = `<button class="linkbtn" data-plan>Set renewal date</button>`;
  }
  return `<div class="lim fixed"><span class="muted">Plan${p.type ? " · " + esc(p.type) : ""}</span><div class="bignum">${main}</div><div class="spacer6"></div><div class="sub">${sub}</div></div>`;
}

export function view(ctx) {
  const id = ctx.params[0];
  const a = accountById(id);
  if (!a) {
    return { html: `<div class="bar"><div class="crumbs"><a href="#/accounts">Accounts</a>${icon("chevronRight", 14)}<span class="here">Not found</span></div></div><div class="body"><div class="empty">This account is not signed in on the proxy.</div></div>` };
  }
  const st = status(a);
  const lim = limits(a);
  const sc = oneScope(a);
  const strip = timeStrip({ key: "acStrip:" + a.id, sc });
  const long = (S.ui.acWindow || "week") === "week";
  const ats = allowanceTime(a.id);
  const actx = panelContext(ats, sc, { forecast: true });
  const allowance = buildPanel("allowance", { window: long ? "week" : "5h" }, { ...actx, labels: true });
  const aend = ats.range ? selectionLabel(ats.range, "data-ac-clear") : ats.moved ? backToNow("data-ac-home") : `<span class="muted">${esc(windowText(ats.window))}</span>`;
  const mine = sessions().filter((s) => (s.auth_ids || []).includes(a.id));
  const active = mine.filter((s) => Date.now() - Date.parse(s.last_seen) < 10 * 60e3);
  const stateLine = st.kind === "blocked" || st.kind === "error" ? `<div class="banner">${esc(st.text)}${st.detail && st.detail !== st.text ? ": " + esc(st.detail) : ""}</div>` : "";

  const html = `
    <div class="bar">
      <div class="crumbs"><a href="#/accounts">Accounts</a>${icon("chevronRight", 14)}<span class="here">${logo(a.provider, "sm")}<span class="clamp">${esc(a.email)}</span></span></div>
      <div class="end">
        <button class="modebtn" data-mode><span class="k">Mode</span><span class="v">${esc(modeTitle(a.mode))}</span>${icon("chevronDown", 14, "var(--muted-fg)")}</button>
        <button class="modebtn" data-priority><span class="k">Priority</span><span class="v">${esc(a.priority)}</span></button>
        <button class="iconbtn" data-more aria-label="More">${icon("more")}</button>
      </div>
    </div>
    <div class="body">
      ${stateLine}
      ${region("ac|limits", `${limitBlock("Week", lim.week, st, trajectory(allowanceSeries(a.id, true)))}${limitBlock("5 hours", lim.short, st, trajectory(allowanceSeries(a.id, false)))}${planBlock(a)}`, "div", `class="limits"`)}
      <div class="activity">
        <div class="head"><div class="t"><b>Allowance left</b>${region("ac|end", aend, "div", `style="display:contents"`)}</div>${seg([{ id: "week", label: "Week" }, { id: "5h", label: "5 hours" }], long ? "week" : "5h", "data-ac-window", "bare")}</div>
        ${allowance.html}
      </div>
      ${strip.html}
      ${region("ac|sessions", `
        <div class="sech">
          <div class="t"><b>Active sessions</b><span class="muted">Seen in the last 10 minutes</span></div>
          <a class="linkbtn" href="#/sessions">All sessions ${icon("chevronRight", 14)}</a>
        </div>
        ${sessionTable(active, { withAccount: false, empty: "No sessions on this account in the last 10 minutes." })}`, "div", `class="sec last"`)}
    </div>`;
  return {
    html,
    mount(root) {
      strip.mount(root);
      bindTime(root.querySelector(".activity"), {
        key: ats.key, window: ats.window, range: ats.range, grid: snapGrid(actx), future: true,
        defaultWindow: ats.defaultWindow, setWindow: ats.setWindow, setRange: ats.setRange,
        probe: (t, el, hovered) => allowance.probe(t, el, hovered),
      });
      const clear = root.querySelector("[data-ac-clear]");
      if (clear) clear.onclick = () => ats.setRange(null);
      const home = root.querySelector("[data-ac-home]");
      if (home) home.onclick = () => ats.setWindow(ats.defaultWindow());
      root.querySelectorAll("[data-ac-window]").forEach((b) => { b.onclick = () => { S.ui.acWindow = b.dataset.acWindow; window.dispatchEvent(new Event("dash:render")); }; });
      root.querySelector("[data-mode]").onclick = (e) => modeMenu(e.currentTarget, a);
      root.querySelector("[data-more]").onclick = (e) => planMenu(e.currentTarget, a);
      root.querySelector("[data-priority]").onclick = (e) => priorityMenu(e.currentTarget, a);
      const setPlan = root.querySelector("[data-plan]");
      if (setPlan) setPlan.onclick = () => planMenu(root.querySelector("[data-more]"), a);
    },
  };
}
