// Routing: how new sessions pick an account, the queue right now, and the
// scorer's ranking and shadow comparison when it is on.
import {
  S, esc, icon, logo, email, pill, warnState, warnIcon, queue, status, limits, signalLimits, left, plan, planDay, planDaysAway,
  resetLong, clock, names, table, menu, api, write, meterCell, providerTitle,
} from "../core.js";

export const STRATEGIES = {
  "soonest-reset": { a: "Soonest reset first", b: "Uses up the allowance that renews first, so less of it goes to waste." },
  "fill-first": { a: "Fill first", b: "Keeps using one account until it runs out, then moves to the next." },
  "round-robin": { a: "Round robin", b: "Takes turns between accounts for each new session." },
  "weighted-round-robin": { a: "Weighted round robin", b: "Takes turns, giving accounts with a higher weight more sessions." },
};

function statusCell(a, isNext) {
  const st = status(a);
  if (isNext) return pill("Next");
  if (st.kind === "ready") return a.priority < 0 ? `<span class="muted">Reserve, used once the others run out</span>` : "";
  if (st.kind === "off") return `<span class="muted">Off</span>`;
  if (st.kind === "usedup") {
    const p = plan(a);
    const reset = st.until || limits(a).week?.reset;
    const endsFirst = p.ends && reset && planDaysAway(p.ends) <= Math.ceil((reset - Date.now()) / 864e5);
    return warnState(endsFirst ? "Used up, plan ends before reset" : "Used up, back " + resetLong(reset));
  }
  if (st.kind === "limited") return warnState(st.text);
  return warnState(st.text + (st.kind === "blocked" ? ", skipped" : ""));
}

function queueRows(provider) {
  const q = queue(provider);
  const list = [...q.order, ...q.rest];
  if (!list.length) return [];
  const rows = [{
    group: true,
    html: `<div class="c" style="width:40px;flex:0 0 40px"></div><div class="c ic" style="width:16px;flex:0 0 16px">${logo(provider)}</div><div class="c g"><span class="ui strong">${providerTitle(provider)}</span><span class="muted">${q.order.length} of ${list.length} available</span></div>`,
  }];
  list.forEach((a) => {
    const rank = q.order.indexOf(a);
    // The meter shows the known reading, router meters first as on Accounts;
    // the reset stays the one the queue orders by.
    const w = limits(a).week;
    const reset = signalLimits(a).week?.reset || w?.reset;
    const st = status(a);
    rows.push({
      href: "#/accounts/" + encodeURIComponent(a.id),
      cells: [
        rank >= 0 ? `<span class="muted">${rank + 1}</span>` : "",
        "",
        email(a.email),
        reset ? esc(resetLong(st.kind === "usedup" ? st.until || reset : reset)) : "",
        st.kind === "off" || st.kind === "blocked" || st.kind === "error" ? "" : w ? meterCell(st.kind === "usedup" ? 0 : left(w), "", w) : `<span class="muted">No reading</span>`,
        statusCell(a, q.next && a.id === q.next.id),
      ],
    });
  });
  return rows;
}

function candidateWhy(c) {
  const problems = (c.problems || []).map((p) => `<span class="warn">${esc(p.charAt(0).toUpperCase() + p.slice(1))}.</span> `).join("");
  return `<span>${problems}${esc(c.summary || "")}</span>`;
}

// The scorer: how it would place a new session now, and how its shadow
// picks compared with the live router's.
function scorer() {
  const R = S.data?.router || {};
  if (!R.mode || R.mode === "off") return "";
  const nm = names();
  const shadow = R.shadow || {};
  const choices = shadow.choices || 0, agreed = shadow.agreed || 0;
  let lead = R.mode === "shadow"
    ? (choices
      ? `Runs next to the live router without changing anything. In the last 24 hours it agreed with the live pick on <b class="strong">${agreed} of ${choices}</b> new sessions.`
      : "Runs next to the live router without changing anything. No choices recorded in the last 24 hours: a choice needs two ready accounts.")
    : "Places new sessions. The ranking below is what it would do now.";
  const headroom = Object.entries(R.headroom || {});
  if (headroom.length) lead += ` Keeps ${headroom.map(([a, line]) => `${esc(a)} below <b class="strong">${Math.round(line * 100)}%</b> of its 5-hour window`).join(", ")} while another account has room.`;

  const cols = [{ label: "#", w: 40 }, { label: "", w: 16, cls: "ic" }, { label: "Account", w: 280 }, { label: "Why", cls: "why" }];
  let html = `<div class="sec">
    <div class="sech tall"><div class="t"><b>${R.mode === "shadow" ? "Scorer, shadow test" : "Scorer"}</b></div></div>
    <div class="ui muted" style="padding:0 24px 16px;max-width:820px">${lead}</div>`;
  for (const r of R.rankings || []) {
    const ranked = (r.candidates || []).filter((c) => !c.excluded);
    const excluded = (r.candidates || []).filter((c) => c.excluded);
    const rows = [...ranked, ...excluded].map((c) => ({
      cells: [
        c.excluded ? `<span class="muted">–</span>` : `<span class="muted">${ranked.indexOf(c) + 1}</span>`,
        logo(r.provider),
        `<span class="${c.excluded ? "muted" : ""} clamp">${esc(nm[c.auth_id] || c.account || c.auth_id)}</span>`,
        candidateWhy(c),
      ],
      cls: c.excluded ? "dim" : "",
    }));
    html += `<div class="subhead">If a new ${esc(providerTitle(r.provider))} session started now${r.model ? ` <span class="mono">${esc(r.model)}</span>` : ""}</div>${table(cols, rows)}`;
  }
  const disagreed = shadow.disagreed || [];
  if (R.mode === "shadow" && disagreed.length) {
    const dcols = [{ label: "When", w: 64 }, { label: "Model", w: 180 }, { label: "Live picked", w: 220 }, { label: "Scorer picks", w: 220 }, { label: "Why", cls: "why" }];
    const rows = disagreed.map((d) => {
      const top = (d.ranking || []).find((c) => c.auth_id === d.shadow) || {};
      return { cells: [clock(d.at), `<span class="mono clamp">${esc(d.model || "")}</span>`, `<span class="clamp">${esc(nm[d.live] || d.live)}</span>`, `<span class="clamp">${esc(nm[d.shadow] || d.shadow)}</span>`, candidateWhy(top)] };
    });
    html += `<div class="subhead">Where it would have picked differently</div>${table(dcols, rows)}`;
  }
  return html + `</div>`;
}

export function view() {
  const routing = S.data?.routing || {};
  const strategies = routing.strategies || Object.keys(STRATEGIES);
  const cur = routing.strategy || "round-robin";
  const curInfo = STRATEGIES[cur] || { a: cur, b: "" };
  const affinity = !!routing.session_affinity;
  const prime = (routing.prime_after_reset || []).map(providerTitle);

  const cols = [{ label: "#", w: 40 }, { label: "", w: 16, cls: "ic" }, { label: "Account" }, { label: "Week resets", w: 160 }, { label: "Week left", w: 150 }, { label: "Status", w: 260 }];
  const rows = [...queueRows("claude"), ...queueRows("codex")];

  const html = `
    <div class="bar titled"><span class="title">Routing</span></div>
    <div class="body">
      <div class="setgrp">
        <h3>How new sessions pick an account</h3>
        <div class="setrow"><div class="k"><b>Order</b><span>${esc(curInfo.b)}</span></div><div class="v"><button class="btn outline" data-strategy>${esc(curInfo.a)}${icon("chevronUpDown", 14)}</button></div></div>
        <div class="setrow"><div class="k"><b>Keep a session on one account</b><span>The prompt cache belongs to one account. A session moves only when its account runs out.</span></div><div class="v"><button class="toggle ${affinity ? "on" : ""}" data-affinity role="switch" aria-checked="${affinity}" aria-label="Keep a session on one account"></button></div></div>
        ${prime.length ? `<div class="setrow"><div class="k"><b>Start new weeks early</b><span>Sends a first request after a weekly reset so the new week starts at once.</span></div><div class="v">${esc(prime.join(", "))}</div></div>` : ""}
      </div>
      <div class="sec ${S.data?.router?.mode && S.data.router.mode !== "off" ? "" : "last"}">
        <div class="sech tall"><div class="t"><b>Queue right now</b></div></div>
        ${table(cols, rows, { empty: "No accounts signed in." })}
      </div>
      ${scorer()}
    </div>`;

  return {
    html,
    mount(root) {
      root.querySelector("[data-strategy]").onclick = (e) => menu(e.currentTarget, strategies.map((id) => ({
        a: (STRATEGIES[id] || { a: id }).a, b: (STRATEGIES[id] || {}).b, on: id === cur,
        run: () => { if (id !== cur) write(() => api("/config/routing/strategy", { method: "PUT", body: JSON.stringify(id) }), "Order set to " + (STRATEGIES[id] || { a: id }).a); },
      })), { width: 320 });
      root.querySelector("[data-affinity]").onclick = () => write(() => api("/config/routing/session-affinity", { method: "PUT", body: JSON.stringify(!affinity) }), affinity ? "Sessions can now move between accounts" : "Sessions now stay on one account");
    },
  };
}
