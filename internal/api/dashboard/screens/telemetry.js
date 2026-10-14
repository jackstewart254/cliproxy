// Telemetry: one page of stacked panels for the open view. Every panel shares
// the account picker's selection, one time window, one crosshair and one
// range selection; the table under them sums the same buckets per account.
import {
  S, esc, fmt, int, ms, money, clock, day, icon, logo, tokens, cacheReuse, apiCost,
  providerTitle, accountScope, setAccountSelection, warnState, ageParts, stamp,
} from "../core.js";
import { accountColor, readAt } from "./common.js";
import { accountPicker, bindAccountPicker } from "./account-picker.js";
import { pctRate, allowanceRange, resetsUntil } from "./burn.js";
import { PANELS, panelHtml, buildPanel, panelContext, snapGrid, allowanceLines, leftAt, availability, loadSelection } from "./panels.js";
import { usageFigures, perfFigures, historyBefore } from "./series.js";
import { DAY, timeState, forgetTime, bindTime, windowText, selectionLabel, backToNow, zoomHint, endText, dayText } from "./timeaxis.js";
import { V, COLUMN_IDS, findView, defaultId, sameView, viewWindow, hasForecast, loadViews, changedFields } from "./views.js";
import { openDisplay, displayOpen, startRename, renameBox, mountRename, setDefaultView, saveDraft, newViewDialog, viewMenu } from "./viewmenus.js";
import { region, kept } from "../paint.js";

const KEY = "tv";
const copy = (v) => JSON.parse(JSON.stringify(v));
const rerender = () => window.dispatchEvent(new Event("dash:render"));

// ---------- the open view and its unsaved changes ----------

// The saved view, the view as edited (the draft) and whether they differ.
// A draft remembers the saved view it started from (its base). When the
// saved view changes under it, as when a save from this or another tab
// refreshes the views, the draft keeps only the fields the viewer changed
// from its base and takes every other field from the saved view. Saving it
// then sends just those fields, so it never undoes a change made elsewhere.
export function current(id) {
  const saved = findView(V.store, id);
  if (!saved) return null;
  const drafts = (S.ui.drafts ||= {}), bases = (S.ui.draftBases ||= {});
  if (drafts[id]) {
    const fields = changedFields(bases[id] || saved, drafts[id]);
    drafts[id] = { ...copy(saved), ...fields };
    bases[id] = copy(saved);
    if (sameView(drafts[id], saved) && drafts[id].name === saved.name) dropDraft(id);
  }
  const view = drafts[id] || saved;
  return { saved, view, dirty: !!drafts[id] };
}

// Changes the open view without saving it. change gets a copy to edit.
export function edit(id, change) {
  const c = current(id);
  if (!c) return;
  const next = copy(c.view);
  change(next);
  S.ui.drafts[id] = next;
  S.ui.draftBases[id] ||= copy(c.saved);
  if (sameView(next, c.saved) && next.name === c.saved.name) dropDraft(id);
  rerender();
}
export const dropDraft = (id) => {
  if (S.ui.drafts) delete S.ui.drafts[id];
  if (S.ui.draftBases) delete S.ui.draftBases[id];
};

// The shared window and selection for a view. Opening another view starts
// it on its own default window with nothing selected, and applies the
// account selection a view was saved with.
export function viewTime(view) {
  if (S.ui.tvFor !== view.id) {
    forgetTime(KEY);
    S.ui.tvFor = view.id;
    S.ui.tvAccountsFor = "";
  }
  // The saved accounts apply once both the views and the accounts are known:
  // before the first data they would all be filtered out, leaving every
  // account picked, and before the views load a built-in shows unsaved.
  if (S.ui.tvAccountsFor !== view.id && V.loaded && Array.isArray(S.data?.accounts)) {
    S.ui.tvAccountsFor = view.id;
    if (Array.isArray(view.accounts)) setAccountSelection(view.accounts);
  }
  return timeState(KEY, { defaultWindow: (now) => viewWindow(view.window, now), future: hasForecast(view), loads: true });
}

// The windows this page loads, for the router.
export function wants(params) {
  const c = current(params[0]);
  if (!c) return null;
  const w = viewTime(c.view).window;
  return { usage: w, perf: w, percentiles: showsPercentiles(c.view) };
}

// Whether a view shows percentiles for its window, which hold only for the
// exact window they were read for, so a pan asks for them once it settles.
const PCT_PANELS = ["ttft", "latency", "throughput"], PCT_COLS = ["ttft50", "ttft90", "throughput"];
const showsPercentiles = (v) => v.panels.some((p) => PCT_PANELS.includes(p.type)) || (v.columns || []).some((c) => PCT_COLS.includes(c));

// ---------- the table ----------

const resetName = (t, now) => (t - now < 6 * DAY ? `${day(t).split(" ")[0]} ${clock(t)}` : `${dayText(t, now)} ${clock(t)}`);
const ALLOWANCE_COLS = ["weekLeft", "perDay", "heading", "nextReset"];
const USAGE_COLS = ["tokens", "requests", "input", "cacheWrite", "cacheRead", "output", "cost", "cacheReuse"];
const PERF_COLS = ["requests", "ttft50", "ttft90", "throughput", "failures", "failovers"];

// Column labels and widths from the Paper boards. r: right-aligned.
export const COLUMNS = {
  weekLeft: { label: "Week left", w: 120 },
  perDay: { label: "Use per day", w: 140 },
  heading: { label: "Heading for", w: 270 },
  nextReset: { label: "Next reset", w: 240 },
  tokens: { label: "Tokens", w: 100, r: true },
  requests: { label: "Requests", w: 90, r: true },
  input: { label: "New input", w: 104, r: true },
  cacheWrite: { label: "Written to cache", w: 126, r: true },
  cacheRead: { label: "Read from cache", w: 126, r: true },
  output: { label: "Output", w: 92, r: true },
  cost: { label: "API cost", w: 114, r: true, cls: "pr16" },
  cacheReuse: { label: "Cache reuse", w: 106 },
  ttft50: { label: "First token p50", w: 130, r: true },
  ttft90: { label: "First token p90", w: 130, r: true },
  throughput: { label: "Throughput", w: 140, r: true },
  failures: { label: "Failures", w: 110, r: true },
  failovers: { label: "Failovers", w: 118, r: true },
};
const SEL_COLS = { leftAt: { w: 120 }, usedIn: { label: "Used in range", w: 140 }, resetsIn: { label: "Resets in range", w: 510 } };

const cell = (w, html, r = false, cls = "") => `<div class="c ${r ? "r" : ""} ${cls}" style="width:${w}px">${html}</div>`;
// item, a stamped reading, adds its age (ageParts), muted once stale.
const meter = (pct, color, cls = "", item = null) => {
  const age = ageParts(item);
  return `<span class="pctcell${item?.stale ? " stale" : ""}"${age.title}><span class="meter ${cls}"><i style="width:${Math.max(0, Math.min(100, pct))}%;background:${color}"></i></span><span class="${pct ? "" : "muted"}">${Math.round(pct)}%</span>${age.note ? `<span>${age.note}</span>` : ""}</span>`;
};
const reuseCell = (x) => { const r = cacheReuse(x); return r == null ? `<span class="muted">–</span>` : `<span class="pctcell"><span class="meter reuse"><i style="width:${Math.round(r)}%"></i></span><span class="muted">${Math.round(r)}%</span></span>`; };
const perSec = (x) => (x ? (x < 10 ? x.toFixed(1) : String(Math.round(x))) + " tokens/s" : "–");

const fullTime = (t) => `${dayText(t)}, ${clock(t)}`;

// Where an account's week is heading, and under it what follows: after a
// used-up week, how the next one goes at the same rate.
function headingFor(l, now) {
  const tr = l.tr;
  if (tr.rate == null && !tr.usedUp) return `<span class="muted">Too few readings yet</span>`;
  let main, sub = "";
  if (tr.usedUp) main = warnState("Used up");
  else if (tr.runsOut) main = warnState("Runs out " + fullTime(tr.runsOut));
  else main = "Lasts to reset";
  if (tr.usedUp && tr.reset > now && tr.rate > 0 && l.period) {
    const out = tr.reset + (100 / tr.rate) * 3600e3;
    sub = out < tr.reset + l.period ? "Runs out " + fullTime(out) : `${Math.max(0, Math.round(100 - tr.rate * (l.period / 3600e3)))}% left at next reset`;
  } else if (!tr.usedUp && !tr.runsOut && tr.leftAtReset != null) sub = `About ${Math.round(tr.leftAtReset)}% left at reset`;
  return `<span class="two"><span>${main}</span>${sub ? `<span class="muted">${esc(sub)}</span>` : ""}</span>`;
}

function acctCell(a, w) {
  const e = String(a.email || a.id), at = e.indexOf("@");
  const name = at > 0 ? `<b>${esc(e.slice(0, at))}</b><span>${esc(e.slice(at))}</span>` : `<b>${esc(e)}</b>`;
  return `<div class="c acct" style="min-width:${w}px"><span class="lead"><i class="sq" style="background:${accountColor(a.id)}"></i>${logo(a.provider)}</span><span class="email clamp">${name}</span></div>`;
}

// The table for the view's columns: one row per picked account, with an All
// accounts row under usage columns and the local logs before per-account data.
export function tableHtml(view, ctx) {
  // Columns in their fixed order, so each group's columns sit together.
  const cols = COLUMN_IDS.filter((c) => view.columns.includes(c) && COLUMNS[c]);
  if (!cols.length) return "";
  const now = ctx.now, r = ctx.range;
  const from = r ? r.start : ctx.window.start;
  const sc = ctx.sc;
  const hasAllow = cols.some((c) => ALLOWANCE_COLS.includes(c));
  // Requests count from performance next to other performance columns, else from usage.
  const perfMode = cols.some((c) => PERF_COLS.includes(c) && c !== "requests");
  const perfCols = perfMode ? cols.filter((c) => PERF_COLS.includes(c)) : [];
  const usageCols = cols.filter((c) => USAGE_COLS.includes(c) && !perfCols.includes(c));
  const acctW = hasAllow ? 380 : perfCols.length ? 412 : 292;
  // Allowance columns become the selection's columns while a range is set.
  const shownCols = [];
  for (const c of cols) {
    if (ALLOWANCE_COLS.includes(c) && r) {
      if (!shownCols.some((x) => x.sel)) shownCols.push({ id: "leftAt", sel: true, ...SEL_COLS.leftAt, label: `Left at ${endText(r.end, now)}` }, { id: "usedIn", sel: true, ...SEL_COLS.usedIn }, { id: "resetsIn", sel: true, ...SEL_COLS.resetsIn });
    } else shownCols.push({ id: c, ...COLUMNS[c] });
  }
  const head = `<div class="tr head">${`<div class="c acct" style="min-width:${acctW}px">${r ? selectionLabel(r) : esc("Accounts, " + windowText(ctx.window, now))}</div>`}${shownCols.map((c) => cell(c.w, esc(c.label), c.r, c.cls)).join("")}</div>`;

  // Every figure reads the same span data as the panels above.
  const sd = ctx.span;
  const sums = usageFigures(sd, sc.ids, now);
  const lines = new Map(allowanceLines(sc.ids, true, now).map((l) => [l.id, l]));
  const deadNow = availability(sc.ids, { start: now - 1, end: now + 30 * DAY }, now).filter((p) => p.at(now) === 0)
    .map((p) => { const back = p.segs.find((s) => s.from > now && s.n > 0)?.from; return [p.provider, `no ${providerTitle(p.provider)} allowance${back ? " until " + resetName(back, now) : ""}`]; });
  const deadText = new Map(deadNow);
  const dash = `<span class="muted">–</span>`;
  const groupOf = (c) => (c.sel || ALLOWANCE_COLS.includes(c.id) ? "allow" : perfCols.includes(c.id) ? "perf" : "usage");

  // A row's cells, one per column in column order. msgs maps a group to a
  // message that replaces its values ("No requests"): it fills the group's
  // first run of adjacent columns, and its other columns show a dash.
  const cellsFor = (msgs, valueOf) => {
    let out = "";
    const used = new Set();
    for (let i = 0; i < shownCols.length;) {
      const c = shownCols[i], g = groupOf(c), msg = msgs[g];
      if (msg == null) { out += valueOf(c, g); i++; continue; }
      if (used.has(g)) { out += cell(c.w, dash, c.r, c.cls); i++; continue; }
      let w = 0, j = i;
      while (j < shownCols.length && groupOf(shownCols[j]) === g) w += shownCols[j++].w;
      out += `<div class="c" style="width:${w}px"><span class="span2">${msg}</span></div>`;
      used.add(g);
      i = j;
    }
    return out;
  };

  const allowCell = (l, c) => {
    if (c.id === "weekLeft") return cell(c.w, meter(l.tr.usedUp ? 0 : l.tr.leftNow, l.color, "", stamp({}, l.ser.last_at)));
    if (c.id === "perDay") return cell(c.w, l.tr.rate == null ? dash : esc(pctRate(l.tr.rate * 24)));
    if (c.id === "heading") return cell(c.w, headingFor(l, now));
    if (c.id === "nextReset") return cell(c.w, l.tr.reset > now ? esc(fullTime(l.tr.reset)) : dash);
    if (c.id === "leftAt") { const v = leftAt(l, r.end, now); return cell(c.w, v == null ? dash : meter(v, l.color)); }
    if (c.id === "usedIn") { const u = allowanceRange(l.ser, l.tr, now, r.start, r.end); return cell(c.w, u?.used == null ? dash : Math.round(u.used) + "%"); }
    const list = resetsUntil(l.tr, now, r.end, l.period).filter((t) => t >= r.start);
    // The first few by name, then a count: a long selection holds thousands
    // of five-hour resets, too many to format or read in one cell.
    const named = list.slice(0, 3).map((t) => resetName(t, now)).join(", ") + (list.length > 3 ? `, ${list.length - 3} more` : "");
    return cell(c.w, list.length ? esc(named) : `<span class="muted">None</span>`);
  };
  const usageCell = (c, x) => {
    if (!x) return cell(c.w, dash, c.r, c.cls);
    if (c.id === "cacheReuse") return cell(c.w, reuseCell(x));
    const v = { tokens: fmt(tokens(x)), requests: int(x.requests), input: fmt(x.input_tokens), cacheWrite: fmt(x.cache_write_tokens), cacheRead: fmt(x.cache_read_tokens), output: fmt(x.output_tokens), cost: money(apiCost(x)) }[c.id];
    return cell(c.w, esc(v), c.r, c.cls);
  };
  // Performance values for one account's scope, or the picked accounts'.
  // null while the span's data loads.
  const perfVals = (sel) => {
    const f = perfFigures(sd, sel);
    if (!f) return null;
    if (!f.requests) return { none: true };
    const q = f.q;
    return {
      requests: int(f.requests),
      ttft50: q?.ttft_ms?.p50 ? ms(q.ttft_ms.p50) : "–",
      ttft90: q?.ttft_ms?.p90 ? ms(q.ttft_ms.p90) : "–",
      throughput: perSec(q?.throughput?.p50),
      failures: int(f.failed),
      failovers: f.failovers == null ? "–" : int(f.failovers),
    };
  };
  const perfCell = (c, vals) => cell(c.w, vals ? esc(vals[c.id]) : dash, c.r, c.cls);

  const rows = sc.shown.map((a) => {
    const l = hasAllow ? lines.get(a.id) : null;
    const pv = perfCols.length ? perfVals({ some: false, prov: a.id, ids: [a.id] }) : null;
    const msgs = {};
    if (hasAllow && !l) msgs.allow = "No allowance reading yet";
    if (pv?.none) { const extra = deadText.get(a.provider); msgs.perf = `No requests${extra ? ", " + esc(extra) : ""}`; }
    const html = cellsFor(msgs, (c, g) => (g === "allow" ? allowCell(l, c) : g === "perf" ? perfCell(c, pv) : usageCell(c, sums?.per[a.id])));
    return `<div class="tr row" data-row-acct="${esc(a.id)}">${acctCell(a, acctW)}${html}</div>`;
  });
  // An All accounts row under usage and performance columns, never under allowance.
  if (!hasAllow && sc.shown.length > 1) {
    const pv = perfCols.length ? perfVals(sc) : null;
    const cells = cellsFor(pv?.none ? { perf: "No requests" } : {}, (c, g) => (g === "perf" ? perfCell(c, pv) : usageCell(c, sums?.sum)));
    rows.push(`<div class="tr total"><div class="c acct" style="min-width:${acctW}px">All accounts</div>${cells}</div>`);
  }
  // Earlier history only makes sense for the usage columns.
  const onlyUsage = usageCols.length && !hasAllow && !perfCols.length;
  if (onlyUsage && sums?.any) {
    // Per-account counts start later than the provider history from local logs.
    if (sums.first > from + 60e3 && !sc.some) {
      const provs = sc.prov === "all" ? null : [sc.prov];
      const early = historyBefore(sums.first, from, provs, sd.to);
      if (early) rows.push(`<div class="tr earlier"><div class="c acct" style="min-width:${acctW}px">Earlier, from local logs</div>${shownCols.map((c) => usageCell(c, early)).join("")}</div>`);
    }
  }
  if (!sc.shown.length) rows.push(`<div class="empty">No accounts picked.</div>`);
  return `<div class="ttbl" role="table">${head}${rows.join("")}</div>`;
}

// ---------- the page ----------

const STAR = (on) => `<svg width="16" height="16" viewBox="0 0 24 24" aria-hidden="true"><path d="${"M11.48 3.499a.562.562 0 0 1 1.04 0l2.125 5.111a.563.563 0 0 0 .475.345l5.518.442c.499.04.701.663.321.988l-4.204 3.602a.563.563 0 0 0-.182.557l1.285 5.385a.562.562 0 0 1-.84.61l-4.725-2.885a.562.562 0 0 0-.586 0L6.982 20.54a.562.562 0 0 1-.84-.61l1.285-5.386a.562.562 0 0 0-.182-.557l-4.204-3.602a.562.562 0 0 1 .321-.988l5.518-.442a.563.563 0 0 0 .475-.345L11.48 3.5Z"}"/></svg>`;

// The view's options (rename, duplicate, delete) in the header, for narrow
// screens where the sidebar's options button is hidden.
const MORE = `<svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">${[3.5, 8, 12.5].map((x) => `<circle cx="${x}" cy="8" r="1.25" fill="var(--icon)"/>`).join("")}</svg>`;

export function view(ctx) {
  const id = ctx.params[0];
  if (!V.loaded) loadViews().then(rerender);
  const c = current(id);
  if (!c) {
    return { html: `<div class="tbar"><div class="l"><span class="vname">${V.loaded ? "View not found" : "Loading"}</span></div></div><div class="body">${V.loaded ? `<div class="empty">This view was deleted. <a class="linkbtn" href="#/telemetry">Open the default view</a></div>` : ""}</div>` };
  }
  const v = c.view;
  const ts = viewTime(v);
  const sc = accountScope();
  const forecast = hasForecast(v);
  const base = panelContext(ts, sc, { forecast });
  const axisIdx = v.panels.map((p, i) => (PANELS[p.type]?.axis === false ? -1 : i)).filter((i) => i >= 0);
  const firstAxis = axisIdx[0], lastAxis = axisIdx[axisIdx.length - 1];
  const built = v.panels.map((p, i) => buildPanel(p.type, p.options, { ...base, labels: i === lastAxis, hint: i === firstAxis ? zoomHint() : "" }));
  const panels = v.panels.map((p, i) => panelHtml(p.type, i, built[i], p.options)).join("");
  const isDefault = defaultId(V.store) === v.id;
  const name = renameBox(v.id) ?? `<span class="vname" data-rename tabindex="0" role="button" title="Rename this view">${esc(v.name)}</span>`;
  const html = `
    <div class="tbar">
      <div class="l">${name}<button class="star ${isDefault ? "on" : ""}" data-star aria-pressed="${isDefault}" aria-label="${isDefault ? "Default view" : "Set as default view"}" title="${isDefault ? "Opens first under Telemetry" : "Set as default view"}">${STAR(isDefault)}</button><button class="vopts" data-vopts data-anchor="vopts-${esc(v.id)}" aria-label="${esc(v.name)} options" aria-haspopup="menu">${MORE}</button></div>
      <div class="r">
        ${region("tv|when", `<span class="wlabel">${esc(windowText(ts.window, base.now))}</span>${ts.moved ? backToNow("data-home") : ""}`, "div", `style="display:contents"`)}
        <button class="dispbtn ${displayOpen() ? "open" : ""}" data-display data-anchor="display" aria-haspopup="dialog" aria-expanded="${displayOpen()}">${icon("adjust", 14)}<span>Display</span></button>
        ${accountPicker()}
      </div>
    </div>
    ${c.dirty ? `<div class="ubar" role="status"><span>You changed this view</span><div class="acts"><button data-u-reset>Reset</button><button data-u-new>Save as new view</button><button class="save" data-u-save>Save</button></div></div>` : ""}
    <div class="body tele" data-tele>
      ${panels || `<div class="empty">This view has no panels. Add some from Display.</div>`}
      ${region("tv|table", kept("tv|table", () => tableHtml(v, base)))}
      ${region("tv|read", esc(readAt()), "div", `class="readrow"`)}
    </div>`;

  return {
    html,
    mount(root) {
      bindAccountPicker(root);
      const tele = root.querySelector("[data-tele]");
      const setWindow = (w) => { ts.setRange(null); ts.setWindow(w); };
      bindTime(tele, {
        key: KEY,
        window: ts.window,
        range: ts.range,
        grid: snapGrid(base),
        future: forecast,
        defaultWindow: ts.defaultWindow,
        setWindow: ts.setWindow,
        setRange: ts.setRange,
        probe(t, el, hovered) {
          const i = v.panels.findIndex((p) => p.type === el.dataset.panel);
          return i >= 0 ? built[i].probe?.(t, el, hovered) || null : null;
        },
      });
      built.forEach((b) => b.mount?.(root, setWindow));
      tele.querySelectorAll("[data-opt]").forEach((b) => {
        b.onclick = () => edit(v.id, (d) => { const p = d.panels[Number(b.dataset.opt)]; if (p) p.options = { ...p.options, [b.dataset.key]: b.dataset.val }; });
      });
      tele.querySelectorAll("[data-clear-range]").forEach((b) => { b.onclick = () => ts.setRange(null); });
      // A redraw that keeps the page binds it again: handlers are set as
      // properties, so they replace the last ones and read this drawing's state.
      tele.onkeydown = (e) => { if (e.key === "Escape" && ts.range && !e.defaultPrevented) ts.setRange(null); };
      // A table row lights its account's lines on every panel.
      const marks = () => tele.querySelectorAll("[data-acct]");
      tele.querySelectorAll("[data-row-acct]").forEach((row) => {
        row.onmouseenter = () => { const id2 = row.dataset.rowAcct; marks().forEach((m) => { m.style.opacity = m.dataset.acct === id2 ? "" : ".25"; }); };
        row.onmouseleave = () => marks().forEach((m) => { m.style.opacity = ""; });
      });
      const home = root.querySelector("[data-home]");
      if (home) home.onclick = () => { ts.setRange(null); ts.setWindow(ts.defaultWindow()); };
      root.querySelector("[data-display]").onclick = (e) => openDisplay(e.currentTarget, v.id);
      root.querySelector("[data-star]").onclick = () => setDefaultView(v.id);
      const opts = root.querySelector("[data-vopts]");
      if (opts) opts.onclick = () => viewMenu(opts, v.id);
      const nm = root.querySelector("[data-rename]");
      if (nm) {
        nm.ondblclick = () => startRename(v.id);
        nm.onkeydown = (e) => { if (e.key === "Enter" || e.key === "F2") { e.preventDefault(); startRename(v.id); } };
      }
      mountRename(root);
      const u = (sel, f) => { const b = root.querySelector(sel); if (b) b.onclick = f; };
      u("[data-u-reset]", () => { dropDraft(v.id); forgetTime(KEY); rerender(); });
      u("[data-u-save]", () => saveDraft(v.id));
      u("[data-u-new]", () => newViewDialog(v));
      // A selection's figures read its own data, loaded for exactly it.
      loadSelection(base);
    },
  };
}
