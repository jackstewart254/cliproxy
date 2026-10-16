// Shared state, data model, formatting and components for the dashboard screens.

export const S = {
  data: null,        // last /dashboard/data payload
  readAt: 0,         // when it arrived
  error: "",
  key: "",
  dataRange: "",     // window the loaded payload covers
  dataViewport: "",  // usage window the loaded payload covers, "start,end"
  dataPerf: "",      // performance window asked for, "start,end", or "" for the fixed range
  dataScope: "",     // accounts merged into performance.scopes.selection, comma-joined
  dataUsagePad: "",  // the padded usage span asked for with dataViewport, "start,end"
  dataPerfPad: "",   // the padded performance span asked for with dataPerf, "start,end"
  dataQuery: "",     // the query the loaded payload answered
  ui: {},            // per-screen choices that survive a refresh
  hold: 0,           // >0 while a menu, drawer or hover would be lost by a re-render
  gestureAt: 0,      // when a time plot last moved under a pan or zoom
};

const KEY_STORE = "cliproxy-dashboard-key";
const THEME_STORE = "cliproxy-dashboard-theme";
const PREFS_STORE = "cliproxy-dashboard-prefs";
try { S.key = localStorage.getItem(KEY_STORE) || ""; } catch (e) { /* storage blocked */ }

// ---------- time ranges ----------

// The fixed ranges the backend serves, shortest first.
const FIXED_RANGES = [["24h", 864e5], ["7d", 7 * 864e5], ["30d", 30 * 864e5], ["180d", 180 * 864e5], ["all", Infinity]];

// The ranges the backend says it can serve performance for. A backend without
// the list knows only the original two.
export function offeredRanges() {
  const list = S.data?.summary?.performance?.ranges;
  const keys = Array.isArray(list) ? list.filter((k) => FIXED_RANGES.some(([r]) => r === k)) : [];
  return keys.length ? keys : ["24h", "7d"];
}

// The shortest offered fixed range that reaches back to start: what a backend
// without custom windows serves instead of one.
export function fallbackRange(start, now = Date.now()) {
  const offered = offeredRanges();
  const fit = FIXED_RANGES.filter(([k]) => offered.includes(k)).find(([, len]) => now - start <= len + 60e3);
  return fit ? fit[0] : offered[offered.length - 1];
}

// The windows the current screen draws, set by the router: {usage, perf}
// windows of {start, end}, either missing. Other screens load the last 24 hours.
let screenWants = () => null;
export function setScreenWants(fn) { screenWants = fn; }

const FIVE_MIN = 5 * 60e3;
// What to load for a window: whole five minutes, ending by now, an hour to
// 366 days long, so the request stays the same while now moves a little.
export function loadSpan(v, now = Date.now()) {
  if (!v) return null;
  const end = Math.ceil(Math.min(v.end, now) / FIVE_MIN) * FIVE_MIN;
  let start = Math.floor(v.start / FIVE_MIN) * FIVE_MIN;
  start = Math.min(start, end - 3600e3);
  start = Math.max(start, end - 366 * 864e5);
  return { start, end };
}
const iso = (t) => new Date(t).toISOString().replace(/\.\d{3}Z$/, "Z");
const spanParam = (s) => (s ? iso(s.start) + "," + iso(s.end) : "");

// The performance window to ask for, for a window on screen or a selection
// alike: the stretch itself in whole five minutes, so totals and percentiles
// are its own. The end is not moved back to now, as the proxy stops a window
// at now itself; moving it would stretch the start back before the stretch.
// The proxy needs an hour to 366 days: a shorter stretch gets the hour from
// its start, which contains it, and a longer one its last 366 days.
export function perfSpan(v) {
  if (!v) return null;
  let start = Math.floor(v.start / FIVE_MIN) * FIVE_MIN;
  let end = Math.ceil(v.end / FIVE_MIN) * FIVE_MIN;
  if (end - start < 3600e3) end = start + 3600e3;
  start = Math.max(start, end - 366 * 864e5);
  return { start, end };
}

export function wantRange(now = Date.now()) {
  const p = perfSpan(screenWants()?.perf);
  return p ? fallbackRange(p.start, Math.min(p.end, now)) : "24h";
}
export const wantUsageViewport = () => spanParam(loadSpan(screenWants()?.usage));
export const wantPerfWindow = () => spanParam(perfSpan(screenWants()?.perf));

// The margin loaded around a window: half a window on each side, so a pan
// or a small zoom stays inside data already loaded. Usage stops at now, as
// it has nothing later; performance may run past now, where the proxy stops
// its series. The proxy keeps the window's own bucket size and trims the
// margin to one window each side and to the hours it keeps.
export function padSpan(span, { toNow = false, now = Date.now() } = {}) {
  if (!span) return null;
  const half = (span.end - span.start) / 2;
  const start = Math.floor((span.start - half) / FIVE_MIN) * FIVE_MIN;
  let end = Math.ceil((span.end + half) / FIVE_MIN) * FIVE_MIN;
  if (toNow) end = Math.max(span.end, Math.ceil(Math.min(end, now) / FIVE_MIN) * FIVE_MIN);
  return { start, end };
}
export const wantUsagePad = (now = Date.now()) => spanParam(padSpan(loadSpan(screenWants()?.usage, now), { toNow: true, now }));
export const wantPerfPad = () => spanParam(padSpan(perfSpan(screenWants()?.perf)));
// Whether the screen shows percentiles for its window, which hold only for
// exactly the window they were read for.
const wantsPercentiles = () => !!screenWants()?.percentiles;

// Whether the loaded performance is for the window on screen: the reply is
// for a custom window (a backend without them answers with the fixed
// fallback range) and that window is the one the screen wants now. A window
// that runs up to now moves on every five minutes; until the next reading
// arrives, the last one counts while it has the same length and is at most
// five minutes behind. Whether its buckets cover exactly the window is a
// second question, which series.coversExactly answers.
export function perfCurrent(want = wantPerfWindow(), loaded = S.dataPerf, now = Date.now()) {
  if (!loaded || !want || S.data?.summary?.performance?.range !== "custom") return false;
  if (loaded === want) return true;
  const [a0, a1] = loaded.split(",").map(Date.parse), [b0, b1] = want.split(",").map(Date.parse);
  const live = b1 >= now - FIVE_MIN;
  return live && a1 - a0 === b1 - b0 && b1 > a1 && b1 - a1 <= FIVE_MIN;
}

// ---------- account selection ----------

// One account selection shared by every screen: null for all accounts, or the
// picked ids. It lives in S.ui.accounts and in this viewer's preferences, so it
// carries across screens and survives a reload.
const LEGACY_PROV_KEYS = ["ovProv", "pfProv", "acProv", "seProv"];

// The pick made with the per-screen controls this selection replaced: the
// Usage picker, or a provider tab with its account chips.
function legacySelection() {
  if (Array.isArray(S.ui.usAccounts)) return S.ui.usAccounts;
  for (const key of LEGACY_PROV_KEYS) {
    const prov = S.ui[key];
    if (prov !== "claude" && prov !== "codex") continue;
    const ids = accounts().filter((a) => a.provider === prov).map((a) => a.id);
    const picked = (S.ui[key + "Pick"]?.[prov] || []).filter((id) => ids.includes(id));
    return picked.length ? picked : ids;
  }
  return null;
}

// Read once the first data arrives: the stored selection, else the old
// per-screen pick. The old keys are dropped either way.
let migrated = false;
export function accountSelection() {
  if (!migrated && S.data) {
    migrated = true;
    if (S.ui.accounts === undefined) {
      const stored = prefs().accounts;
      setAccountSelection(Array.isArray(stored) ? stored.filter((id) => typeof id === "string") : legacySelection());
    }
    delete S.ui.usAccounts;
    for (const key of LEGACY_PROV_KEYS) { delete S.ui[key]; delete S.ui[key + "Pick"]; }
  }
  return Array.isArray(S.ui.accounts) ? S.ui.accounts : null;
}

// Keeps the ids that still exist. Every account, or none, means all accounts.
export function setAccountSelection(ids) {
  const all = accounts().map((a) => a.id);
  const keep = Array.isArray(ids) ? all.filter((id) => ids.includes(id)) : [];
  S.ui.accounts = keep.length && keep.length < all.length ? keep : null;
  setPref("accounts", S.ui.accounts || undefined);
}

// Drops picked ids that the latest data no longer lists, so a removed account
// cannot come back picked. Called whenever fresh data arrives.
export function reconcileSelection() {
  const sel = accountSelection();
  if (!sel) return;
  const ids = new Set(accounts().map((a) => a.id));
  if (sel.some((id) => !ids.has(id))) setAccountSelection(sel);
}

// The accounts every screen shows. prov is the one provider they all belong
// to, or "all"; some is true when they leave out an account of that provider.
// No pick means every provider, even when only one has accounts right now,
// so history and sessions of removed accounts still count.
export function accountScope() {
  const all = accounts();
  const sel = accountSelection();
  const picked = sel ? all.filter((a) => sel.includes(a.id)) : [];
  if (!picked.length) return { all, shown: all, ids: all.map((a) => a.id), prov: "all", some: false };
  const providers = [...new Set(picked.map((a) => a.provider))];
  const prov = providers.length === 1 ? providers[0] : "all";
  const some = picked.length < all.filter((a) => prov === "all" || a.provider === prov).length;
  return { all, shown: picked, ids: picked.map((a) => a.id), prov, some };
}

export const scopeParam = (ids) => [...ids].sort().join(",");

// The selection the current screen needs merged by the backend, so its
// percentiles stay exact. Only Overview and Telemetry show percentiles for a
// selection, and only a backend that lists its ranges knows the scope parameter.
export function wantScope() {
  if (!Array.isArray(S.data?.summary?.performance?.ranges)) return "";
  const a = location.hash.replace(/^#\/?/, "").split("/")[0];
  if (a && a !== "overview" && a !== "telemetry") return "";
  const sc = accountScope();
  return sc.some ? scopeParam(sc.ids) : "";
}

// ---------- per-viewer preferences (chart formats, the zoom hint) ----------

// Stored preferences are trusted only as plain objects.
export const plainObject = (v) => (v && typeof v === "object" && !Array.isArray(v) ? v : null);

let prefsCache = null;
export function prefs() {
  if (prefsCache) return prefsCache;
  try { prefsCache = plainObject(JSON.parse(localStorage.getItem(PREFS_STORE) || "{}")); } catch (e) { prefsCache = null; }
  if (!prefsCache) prefsCache = {};
  return prefsCache;
}
export function setPref(key, value) {
  const p = prefs();
  if (value === undefined) delete p[key];
  else p[key] = value;
  try { localStorage.setItem(PREFS_STORE, JSON.stringify(p)); } catch (e) { /* storage blocked */ }
}
// Every time chart is a line unless this viewer switched it to bars.
export const chartFormat = (id) => (plainObject(prefs().charts)?.[id] === "bars" ? "bars" : "line");
export function setChartFormat(id, f) {
  const charts = { ...(plainObject(prefs().charts) || {}) };
  if (f === "bars") charts[id] = "bars";
  else delete charts[id];
  setPref("charts", charts);
}

// ---------- loading data ----------

// The query for /dashboard/data: the fixed range, the picked accounts when
// they need merging, the usage and performance windows the screen draws, and
// the margin loaded around each.
export function dataQuery(want = wantRange(), scope = wantScope(), viewport = wantUsageViewport(), perfWin = wantPerfWindow(), usagePad = "", perfPad = "") {
  const q = new URLSearchParams({ range: want });
  if (scope) q.set("scope", scope);
  if (viewport) { const [a, b] = viewport.split(","); q.set("usage_start", a); q.set("usage_end", b); }
  if (viewport && usagePad) { const [a, b] = usagePad.split(","); q.set("usage_pad_start", a); q.set("usage_pad_end", b); }
  if (perfWin) { const [a, b] = perfWin.split(","); q.set("perf_start", a); q.set("perf_end", b); }
  if (perfWin && perfPad) { const [a, b] = perfPad.split(","); q.set("perf_pad_start", a); q.set("perf_pad_end", b); }
  return q.toString();
}

// What the current screen wants loaded, with its query.
export function wanted(now = Date.now()) {
  const w = { range: wantRange(now), scope: wantScope(), viewport: wantUsageViewport(), perfWin: wantPerfWindow(), usagePad: wantUsagePad(now), perfPad: wantPerfPad(), percentiles: wantsPercentiles() };
  w.query = dataQuery(w.range, w.scope, w.viewport, w.perfWin, w.usagePad, w.perfPad);
  return w;
}

const DAY_MS = 864e5;
const spanOf = (p) => { if (!p) return null; const [a, b] = p.split(",").map(Date.parse); return { start: a, end: b }; };

// The proxy's bucket rules, so a window can tell whether loaded buckets are
// the ones it would get itself. Usage is daily for a window over 7 days or
// one reaching before the hourly buckets kept (14 days); performance takes
// the finest step giving at most 200 buckets, whole days before 35 days back.
const usageDaily = (v, now) => v.end - v.start > 7 * DAY_MS || v.start < now - 14 * DAY_MS;
export function perfHours(v, now) {
  for (const h of [1, 3, 6, 12, 24, 48, 168]) {
    if (h < 24 && v.start < now - 35 * DAY_MS) continue;
    if (Math.ceil((v.end - v.start) / (h * 3600e3)) <= 200) return h;
  }
  return 168;
}

// The stretch loaded buckets cover: usage from its first start to its last
// end, performance over its series (which stops at now) or, with no buckets,
// the span asked for.
function usageExtent(ur) {
  const st = ur?.starts;
  if (!Array.isArray(st) || !st.length) return null;
  const ends = Array.isArray(ur.ends) && ur.ends.length === st.length ? ur.ends : null;
  const end = ends ? Date.parse(ends[ends.length - 1]) : Date.parse(st[st.length - 1]) + (Number(ur.bucket_seconds) || 3600) * 1000;
  return { start: Date.parse(st[0]), end };
}
function perfExtent(perf, asked) {
  for (const sc of Object.values(perf?.scopes || {})) {
    const s = (sc?.series || []).filter((b) => b && b.start);
    if (s.length) return { start: Date.parse(s[0].start), end: Date.parse(s[s.length - 1].start) + (Number(perf.bucket_seconds) || 3600) * 1000 };
  }
  return spanOf(asked);
}
// Whether ext holds the window v up to now, give or take a minute.
const holds = (ext, v, now) => !!ext && v.start >= ext.start - 60e3 && Math.min(v.end, now) <= ext.end + 60e3;

// The reply on screen and what it was asked for.
const loadedNow = () => ({ data: S.data, range: S.dataRange, scope: S.dataScope, viewport: S.dataViewport, perfWin: S.dataPerf, perfPad: S.dataPerfPad, query: S.dataQuery });
const loadedFor = (w, data) => ({ data, range: w.range, scope: w.scope, viewport: w.viewport, perfWin: w.perfWin, perfPad: w.perfPad, query: w.query });

// Whether a loaded reply (the one on screen by default) already draws what
// the screen wants: the same accounts, both windows inside the buckets loaded
// and in the buckets each would get alone, and, where the screen shows
// percentiles, the very window they were read for. A pan or a small zoom
// inside the margin is then drawn from it, with no request.
export function covers(w, now = Date.now(), L = loadedNow()) {
  const sum = L.data?.summary;
  if (!sum || w.scope !== L.scope) return false;
  const perf = sum.performance;
  const custom = perf?.range === "custom";
  if (!custom && w.range !== L.range) return false;
  if (!!w.viewport !== !!L.viewport || !!w.perfWin !== !!L.perfWin) return false;
  if (w.viewport) {
    const v = spanOf(w.viewport), ur = sum.usage_range;
    if (!holds(usageExtent(ur), v, now)) return false;
    if (usageDaily(v, now) !== (Number(ur.bucket_seconds) >= 86400)) return false;
  }
  if (w.perfWin) {
    const v = spanOf(w.perfWin);
    if (!custom || !holds(perfExtent(perf, L.perfPad || L.perfWin), v, now)) return false;
    if (perfHours(v, now) * 3600 !== Number(perf.bucket_seconds)) return false;
    if (w.percentiles && !(w.perfWin === L.perfWin || perfCurrent(w.perfWin, L.perfWin, now))) return false;
  }
  return true;
}

// Whether the loaded data is what the current screen wants, or draws it.
export function dataCurrent(now = Date.now()) {
  if (!S.data) return false;
  const w = wanted(now);
  return w.query === S.dataQuery || covers(w, now);
}

// The last few replies by query, so going back to an earlier zoom draws at
// once. A reply is reused for a minute; the regular refresh keeps the one on
// screen fresh.
const CACHE_SIZE = 8, CACHE_FRESH = 60e3;
const replies = new Map();
function remember(query, data, at) {
  replies.delete(query);
  replies.set(query, { data, at });
  while (replies.size > CACHE_SIZE) replies.delete(replies.keys().next().value);
}
function cached(query, now = Date.now()) {
  const r = replies.get(query);
  return r && now - r.at <= CACHE_FRESH ? r : null;
}
export const forgetReplies = () => replies.clear();

// Requests sent, for tests and the frame check.
export const loads = { requests: 0 };

// One request at a time: a request for another query aborts the one in
// flight, as its reply would be dropped anyway; the same query shares it.
// An aborted request resolves to null.
let inflight = null;
function request(query) {
  if (inflight && inflight.query === query) return inflight.promise;
  inflight?.ctrl?.abort();
  const ctrl = typeof AbortController === "function" ? new AbortController() : null;
  const me = { query, ctrl };
  loads.requests++;
  me.promise = (async () => {
    try {
      const res = await fetch("/dashboard/data?" + query, { cache: "no-store", signal: ctrl?.signal });
      if (!res.ok) throw new Error("Could not read the proxy (" + res.status + ")");
      const data = await res.json();
      remember(query, data, Date.now());
      return data;
    } catch (e) {
      if (e?.name === "AbortError" || ctrl?.signal?.aborted) return null;
      throw e;
    } finally {
      if (inflight === me) inflight = null;
    }
  })();
  inflight = me;
  return me.promise;
}

function install(w, data, at) {
  S.data = data;
  reconcileSelection();
  S.dataRange = w.range;
  S.dataScope = w.scope;
  S.dataViewport = w.viewport;
  S.dataPerf = w.perfWin;
  S.dataUsagePad = w.usagePad;
  S.dataPerfPad = w.perfPad;
  S.dataQuery = w.query;
  S.readAt = at;
}

let started = 0, applied = 0;

// Whether a reply for w should go on screen now: it is for what the screen
// wants, or it draws that.
const stillWanted = (w, data) => { const now = wanted(); return now.query === w.query || covers(now, Date.now(), loadedFor(w, data)); };

// The one way a reply goes on screen, from the network or from the cache
// alike. my is the number its load took when it started; at is when the
// reply was read. Checked at the moment of applying, as the window may have
// moved since the load started or since its frame was asked for: a reply
// older than one already shown, a remembered one gone stale, or one that
// does not draw what the screen now wants is dropped. True when applied.
function applyReply(my, w, data, at) {
  if (data == null || my < applied) return false;
  if (Date.now() - at > CACHE_FRESH) return false;
  if (!stillWanted(w, data)) return false;
  applied = my;
  install(w, data, at);
  return true;
}

// Loads /dashboard/data for the current route, with the margin around its
// windows. A reply that arrives after a newer one, or after the route moved
// to a window or selection it does not draw, is dropped.
export async function fetchData() {
  const w = wanted();
  const my = ++started;
  const data = await request(w.query);
  applyReply(my, w, data, Date.now());
}

// The next frame while the page shows, else a timer, so a hidden tab still
// settles.
export function nextFrame(fn) {
  const shown = typeof document !== "undefined" && document.visibilityState === "visible" && typeof requestAnimationFrame === "function";
  return shown ? requestAnimationFrame(fn) : setTimeout(fn, 16);
}

// A window that loads data moved. Once it has been still for SETTLE_MS, the
// screen gets what it now wants: nothing when the reply on screen draws it,
// a remembered reply for the same query, else one request. The old data
// stays on screen until the new arrives, and the swap is drawn in one frame.
const SETTLE_MS = 150;
let settleTimer = null;
const patchEvent = () => globalThis.window?.dispatchEvent(new Event("dash:patch"));
export function windowMoved(redraw = patchEvent) {
  clearTimeout(settleTimer);
  settleTimer = setTimeout(() => { settleTimer = null; settle(typeof redraw === "function" ? redraw : patchEvent); }, SETTLE_MS);
}
export async function settle(redraw = patchEvent) {
  if (!S.data || dataCurrent()) return;
  const w = wanted();
  const my = ++started;
  const hit = cached(w.query);
  let data = hit?.data, at = hit?.at;
  if (!hit) {
    try { data = await request(w.query); } catch (e) { return; }
    at = Date.now();
    if (data == null || my < applied) return;
  }
  nextFrame(() => { if (applyReply(my, w, data, at)) redraw(); });
}

export function setKey(k) {
  S.key = k || "";
  try { k ? localStorage.setItem(KEY_STORE, k) : localStorage.removeItem(KEY_STORE); } catch (e) { /* storage blocked */ }
}

export function theme() {
  try { return localStorage.getItem(THEME_STORE) || "system"; } catch (e) { return "system"; }
}
export function setTheme(t) {
  try { t === "system" ? localStorage.removeItem(THEME_STORE) : localStorage.setItem(THEME_STORE, t); } catch (e) { /* storage blocked */ }
  applyTheme();
}
export function applyTheme() {
  const t = theme();
  if (t === "light" || t === "dark") document.documentElement.dataset.theme = t;
  else delete document.documentElement.dataset.theme;
}

// ---------- formatting ----------

export const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

export function fmt(n) {
  n = Number(n) || 0;
  const a = Math.abs(n);
  if (a >= 1e9) return (n / 1e9).toFixed(a >= 1e10 ? 1 : 2) + "B";
  if (a >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (a >= 1e3) return (n / 1e3).toFixed(1) + "k";
  return String(Math.round(n));
}

// toLocaleString builds a formatter on every call; this one is made once.
const GB = new Intl.NumberFormat("en-GB");
export function int(n) {
  return GB.format(Number(n) || 0);
}

// US dollars: "$0.42", "$12.30", "$1,234", "$12.3k", "$1.23M". Each unit is
// picked after rounding, so $9,999.50 reads "$10.0k" and $999,950 "$1.00M".
export function money(n) {
  n = Number(n) || 0;
  const sign = n < 0 ? "-" : "";
  const a = Math.abs(n);
  const cents = Math.round(a * 100) / 100, dollars = Math.round(a), k = Math.round(a / 100) / 10;
  if (k >= 1e3) return `${sign}$${(Math.round(a / 1e4) / 100).toFixed(2)}M`;
  if (k >= 10) return `${sign}$${k.toFixed(1)}k`;
  if (cents >= 1e3) return sign + "$" + GB.format(dollars);
  return `${sign}$${cents.toFixed(2)}`;
}
// Dollars on a chart's y axis, short enough for its column. Below a cent it
// keeps two significant figures, so a sub-cent chart does not read "$0".
export const moneyAxis = (v) => "$" + (v >= 1e3 ? fmt(v) : v >= 10 ? Math.round(v) : v >= 0.01 || !v ? Number(v.toFixed(2)) : Number(v.toPrecision(2)));

export function ms(v) {
  v = Number(v) || 0;
  if (!v) return "–";
  if (v < 1000) return Math.round(v) + "ms";
  if (v < 60000) return (v / 1000).toFixed(v < 10000 ? 1 : 0).replace(/\.0$/, "") + "s";
  const m = Math.floor(v / 60000), s = Math.round((v % 60000) / 1000);
  return m + "m" + (s ? " " + s + "s" : "");
}

export const pctText = (p) => (p == null || !Number.isFinite(p) ? "–" : Math.round(p) + "%");
export const rate = (num, den) => (den ? (num / den) * 100 : null);
export function rateText(num, den) {
  const r = rate(num, den);
  if (r == null) return "–";
  if (r > 0 && r < 10) return r.toFixed(1).replace(/\.0$/, "") + "%";
  return Math.round(r) + "%";
}

// Go encodes an unset time as year 1; anything before 2001 counts as missing.
export const validTime = (iso) => (iso && Date.parse(iso) > Date.UTC(2001, 0, 1) ? iso : "");
const toMs = (t) => (typeof t === "number" ? t : Date.parse(t));

function tz() { return S.data?.summary?.timezone || undefined; }
// One formatter per zone and format, made once: building one costs far more
// than using it, and a long forecast formats thousands of times. Each keeps
// the texts it made, as a pan or zoom formats the same instants every frame.
const CLOCK = { hour: "2-digit", minute: "2-digit", hour12: false };
const DAY_NAME = { weekday: "short", day: "numeric", month: "short" };
const WEEKDAY = { weekday: "short" };
const YMD = { year: "numeric", month: "2-digit", day: "2-digit" };
const formats = new Map();
function partsOf(t, opts) {
  const zone = tz();
  let byZone = formats.get(zone);
  if (!byZone) formats.set(zone, (byZone = new Map()));
  let e = byZone.get(opts);
  if (!e) {
    let f;
    try { f = new Intl.DateTimeFormat("en-GB", { timeZone: zone, ...opts }); }
    catch (err) { f = new Intl.DateTimeFormat("en-GB", opts); }
    byZone.set(opts, (e = { f, texts: new Map() }));
  }
  const ms = toMs(t);
  let s = e.texts.get(ms);
  if (s === undefined) {
    if (e.texts.size > 20000) e.texts.clear();
    s = e.f.format(new Date(ms));
    e.texts.set(ms, s);
  }
  return s;
}
export const clock = (t) => partsOf(t, CLOCK);
export const day = (t) => partsOf(t, DAY_NAME).replace(",", "");
export const weekdayTime = (t) => partsOf(t, WEEKDAY) + " " + clock(t);
export function dayKey(t) {
  const p = partsOf(t, YMD); // dd/mm/yyyy
  const [d, m, y] = p.split("/");
  return `${y}-${m}-${d}`;
}
export const isToday = (t) => dayKey(t) === dayKey(Date.now());

// "Sun 09:00" inside the coming week, else "Sun 11 Oct".
export function resetShort(t) {
  if (!t) return "";
  const d = toMs(t) - Date.now();
  return d < 7 * 864e5 ? weekdayTime(t) : day(t);
}
// "Today, 20:00" or "Sun 11 Oct, 09:00".
export function resetLong(t) {
  if (!t) return "";
  return (isToday(t) ? "Today" : day(t)) + ", " + clock(t);
}

// A plan date is a calendar day ("2026-10-18"), shown without a time zone shift.
export function planDay(s) {
  if (!s) return "";
  const [y, m, d] = s.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1, d)).toLocaleDateString("en-GB", { timeZone: "UTC", weekday: "short", day: "numeric", month: "short" }).replace(",", "");
}
export function planDaysAway(s) {
  if (!s) return null;
  const [y, m, d] = s.split("-").map(Number);
  const [ty, tm, td] = dayKey(Date.now()).split("-").map(Number);
  return Math.round((Date.UTC(y, m - 1, d) - Date.UTC(ty, tm - 1, td)) / 864e5);
}
export function inDays(n) {
  if (n == null) return "";
  if (n === 0) return "Today";
  if (n === 1) return "Tomorrow";
  if (n < 0) return n === -1 ? "Yesterday" : -n + " days ago";
  return "In " + n + " days";
}

// "just now", "4m ago", "2h ago" or "3d ago".
export function ago(t) {
  const m = Math.floor((Date.now() - toMs(t)) / 60e3);
  if (m < 1) return "just now";
  if (m < 60) return m + "m ago";
  return m < 48 * 60 ? Math.floor(m / 60) + "h ago" : Math.floor(m / 1440) + "d ago";
}

// "Now", "15:38", or "Sun 4 Oct" for older.
export function seen(t) {
  if (!t) return "";
  const age = Date.now() - toMs(t);
  if (age < 2 * 60e3) return "Now";
  return isToday(t) ? clock(t) : day(t);
}

// ---------- icons (heroicons outline, as in the Paper file) ----------

const P = {
  share: "M7.217 10.907a2.25 2.25 0 1 0 0 2.186m0-2.186c.18.324.283.696.283 1.093s-.103.77-.283 1.093m0-2.186 9.566-5.314m-9.566 7.5 9.566 5.314m0 0a2.25 2.25 0 1 0 3.935 2.186 2.25 2.25 0 0 0-3.935-2.186Zm0-12.814a2.25 2.25 0 1 0 3.933-2.185 2.25 2.25 0 0 0-3.933 2.185Z",
  overview: "M2.25 7.125C2.25 6.504 2.754 6 3.375 6h6c.621 0 1.125.504 1.125 1.125v3.75c0 .621-.504 1.125-1.125 1.125h-6a1.125 1.125 0 0 1-1.125-1.125v-3.75ZM14.25 8.625c0-.621.504-1.125 1.125-1.125h5.25c.621 0 1.125.504 1.125 1.125v8.25c0 .621-.504 1.125-1.125 1.125h-5.25a1.125 1.125 0 0 1-1.125-1.125v-8.25ZM3.75 16.125c0-.621.504-1.125 1.125-1.125h5.25c.621 0 1.125.504 1.125 1.125v2.25c0 .621-.504 1.125-1.125 1.125h-5.25a1.125 1.125 0 0 1-1.125-1.125v-2.25Z",
  accounts: "M15 19.128a9.38 9.38 0 0 0 2.625.372 9.337 9.337 0 0 0 4.121-.952 4.125 4.125 0 0 0-7.533-2.493M15 19.128v-.003c0-1.113-.285-2.16-.786-3.07M15 19.128v.106A12.318 12.318 0 0 1 8.624 21c-2.331 0-4.512-.645-6.374-1.766l-.001-.109a6.375 6.375 0 0 1 11.964-3.07M12 6.375a3.375 3.375 0 1 1-6.75 0 3.375 3.375 0 0 1 6.75 0Zm8.25 2.25a2.625 2.625 0 1 1-5.25 0 2.625 2.625 0 0 1 5.25 0Z",
  usage: "M3 13.125C3 12.504 3.504 12 4.125 12h2.25c.621 0 1.125.504 1.125 1.125v6.75C7.5 20.496 6.996 21 6.375 21h-2.25A1.125 1.125 0 0 1 3 19.875v-6.75ZM9.75 8.625c0-.621.504-1.125 1.125-1.125h2.25c.621 0 1.125.504 1.125 1.125v11.25c0 .621-.504 1.125-1.125 1.125h-2.25a1.125 1.125 0 0 1-1.125-1.125V8.625ZM16.5 4.125c0-.621.504-1.125 1.125-1.125h2.25C20.496 3 21 3.504 21 4.125v15.75c0 .621-.504 1.125-1.125 1.125h-2.25a1.125 1.125 0 0 1-1.125-1.125V4.125Z",
  performance: "m3.75 13.5 10.5-11.25L12 10.5h8.25L9.75 21.75 12 13.5H3.75Z",
  battery: "M21 10.5h.375c.621 0 1.125.504 1.125 1.125v2.25c0 .621-.504 1.125-1.125 1.125H21M4.5 10.5H12V15H4.5v-4.5ZM3.75 18h15A2.25 2.25 0 0 0 21 15.75v-6.5A2.25 2.25 0 0 0 18.75 7h-15A2.25 2.25 0 0 0 1.5 9.25v6.5A2.25 2.25 0 0 0 3.75 18Z",
  sessions: "M20.25 8.511c.884.284 1.5 1.128 1.5 2.097v4.286c0 1.136-.847 2.1-1.98 2.193-.34.027-.68.052-1.02.072v3.091l-3-3c-1.354 0-2.694-.055-4.02-.163a2.115 2.115 0 0 1-.825-.242m9.345-8.334a2.126 2.126 0 0 0-.476-.095 48.64 48.64 0 0 0-8.048 0c-1.131.094-1.976 1.057-1.976 2.192v4.286c0 .837.46 1.58 1.155 1.951m9.345-8.334V6.637c0-1.621-1.152-3.026-2.76-3.235A48.455 48.455 0 0 0 11.25 3c-2.115 0-4.198.137-6.24.402-1.608.209-2.76 1.614-2.76 3.235v6.226c0 1.621 1.152 3.026 2.76 3.235.577.075 1.157.14 1.74.194V21l4.155-4.155",
  routing: "M7.5 21 3 16.5m0 0L7.5 12M3 16.5h13.5m0-13.5L21 7.5m0 0L16.5 12M21 7.5H7.5",
  settings: "M9.594 3.94c.09-.542.56-.94 1.11-.94h2.593c.55 0 1.02.398 1.11.94l.213 1.281c.063.374.313.686.645.87.074.04.147.083.22.127.325.196.72.257 1.075.124l1.217-.456a1.125 1.125 0 0 1 1.37.49l1.296 2.247a1.125 1.125 0 0 1-.26 1.431l-1.003.827c-.293.241-.438.613-.43.992a7.723 7.723 0 0 1 0 .255c-.008.378.137.75.43.991l1.004.827c.424.35.534.955.26 1.43l-1.298 2.247a1.125 1.125 0 0 1-1.369.491l-1.217-.456c-.355-.133-.75-.072-1.076.124a6.47 6.47 0 0 1-.22.128c-.331.183-.581.495-.644.869l-.213 1.281c-.09.543-.56.94-1.11.94h-2.594c-.55 0-1.019-.398-1.11-.94l-.213-1.281c-.062-.374-.312-.686-.644-.87a6.52 6.52 0 0 1-.22-.127c-.325-.196-.72-.257-1.076-.124l-1.217.456a1.125 1.125 0 0 1-1.369-.49l-1.297-2.247a1.125 1.125 0 0 1 .26-1.431l1.004-.827c.292-.24.437-.613.43-.991a6.932 6.932 0 0 1 0-.255c.007-.38-.138-.751-.43-.992l-1.004-.827a1.125 1.125 0 0 1-.26-1.43l1.297-2.247a1.125 1.125 0 0 1 1.37-.491l1.216.456c.356.133.751.072 1.076-.124.072-.044.146-.086.22-.128.332-.183.582-.495.644-.869l.214-1.28ZM15 12a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z",
  server: "M21.75 17.25v-.228a4.5 4.5 0 0 0-.12-1.03l-2.268-9.64a3.375 3.375 0 0 0-3.285-2.602H7.923a3.375 3.375 0 0 0-3.285 2.602l-2.268 9.64a4.5 4.5 0 0 0-.12 1.03v.228m19.5 0a3 3 0 0 1-3 3H5.25a3 3 0 0 1-3-3m19.5 0a3 3 0 0 0-3-3H5.25a3 3 0 0 0-3 3m16.5 0h.008v.008h-.008v-.008Zm-3 0h.008v.008h-.008v-.008Z",
  reset: "M16.023 9.348h4.992v-.001M2.985 19.644v-4.992m0 0h4.992m-4.993 0 3.181 3.183a8.25 8.25 0 0 0 13.803-3.7M4.031 9.865a8.25 8.25 0 0 1 13.803-3.7l3.181 3.182m0-4.991v4.99",
  // Heroicons has no skull: Lucide's "skull" (ISC licence), its four shapes in one path.
  skull: "M15 22a1 1 0 0 0 1-1v-1a2 2 0 0 0 1.56-3.25 8 8 0 1 0-11.12 0A2 2 0 0 0 8 20v1a1 1 0 0 0 1 1zM12.5 17l-.5-1-.5 1h1zM14 12a1 1 0 1 0 2 0 1 1 0 1 0-2 0M8 12a1 1 0 1 0 2 0 1 1 0 1 0-2 0",
  chevronRight: "m8.25 4.5 7.5 7.5-7.5 7.5",
  chevronLeft: "M15.75 19.5 8.25 12l7.5-7.5",
  chevronDown: "m19.5 8.25-7.5 7.5-7.5-7.5",
  chevronUpDown: "M8.25 15 12 18.75 15.75 15m-7.5-6L12 5.25 15.75 9",
  plus: "M12 4.5v15m7.5-7.5h-15",
  star: "M11.48 3.499a.562.562 0 0 1 1.04 0l2.125 5.111a.563.563 0 0 0 .475.345l5.518.442c.499.04.701.663.321.988l-4.204 3.602a.563.563 0 0 0-.182.557l1.285 5.385a.562.562 0 0 1-.84.61l-4.725-2.885a.562.562 0 0 0-.586 0L6.982 20.54a.562.562 0 0 1-.84-.61l1.285-5.386a.562.562 0 0 0-.182-.557l-4.204-3.602a.562.562 0 0 1 .321-.988l5.518-.442a.563.563 0 0 0 .475-.345L11.48 3.5Z",
  pencil: "m16.862 4.487 1.687-1.688a1.875 1.875 0 1 1 2.652 2.652L10.582 16.07a4.5 4.5 0 0 1-1.897 1.13L6 18l.8-2.685a4.5 4.5 0 0 1 1.13-1.897l8.932-8.931Zm0 0L19.5 7.125M18 14v4.75A2.25 2.25 0 0 1 15.75 21H5.25A2.25 2.25 0 0 1 3 18.75V8.25A2.25 2.25 0 0 1 5.25 6H10",
  home: "m2.25 12 8.954-8.955c.44-.439 1.152-.439 1.591 0L21.75 12M4.5 9.75v10.125c0 .621.504 1.125 1.125 1.125H9.75v-4.875c0-.621.504-1.125 1.125-1.125h2.25c.621 0 1.125.504 1.125 1.125V21h4.125c.621 0 1.125-.504 1.125-1.125V9.75M8.25 21h8.25",
  trash: "m14.74 9-.346 9m-4.788 0L9.26 9m9.968-3.21c.342.052.682.107 1.022.166m-1.022-.165L18.16 19.673a2.25 2.25 0 0 1-2.244 2.077H8.084a2.25 2.25 0 0 1-2.244-2.077L4.772 5.79m14.456 0a48.108 48.108 0 0 0-3.478-.397m-12 .562c.34-.059.68-.114 1.022-.165m0 0a48.11 48.11 0 0 1 3.478-.397m7.5 0v-.916c0-1.18-.91-2.164-2.09-2.201a51.964 51.964 0 0 0-3.32 0c-1.18.037-2.09 1.022-2.09 2.201v.916m7.5 0a48.667 48.667 0 0 0-7.5 0",
  stack: "M6 6.878V6a2.25 2.25 0 0 1 2.25-2.25h7.5A2.25 2.25 0 0 1 18 6v.878m-12 0c.235-.083.487-.128.75-.128h10.5c.263 0 .515.045.75.128m-12 0A2.25 2.25 0 0 0 4.5 9v.878m13.5-3A2.25 2.25 0 0 1 19.5 9v.878m0 0a2.246 2.246 0 0 0-.75-.128H5.25c-.263 0-.515.045-.75.128m15 0A2.25 2.25 0 0 1 21 12v6a2.25 2.25 0 0 1-2.25 2.25H5.25A2.25 2.25 0 0 1 3 18v-6c0-.98.626-1.813 1.5-2.122",
  more: "M6.75 12a.75.75 0 1 1-1.5 0 .75.75 0 0 1 1.5 0ZM12.75 12a.75.75 0 1 1-1.5 0 .75.75 0 0 1 1.5 0ZM18.75 12a.75.75 0 1 1-1.5 0 .75.75 0 0 1 1.5 0Z",
  check: "m4.5 12.75 6 6 9-13.5",
  calendar: "M6.75 3v2.25M17.25 3v2.25M3 18.75V7.5a2.25 2.25 0 0 1 2.25-2.25h13.5A2.25 2.25 0 0 1 21 7.5v11.25m-18 0A2.25 2.25 0 0 0 5.25 21h13.5A2.25 2.25 0 0 0 21 18.75m-18 0v-7.5A2.25 2.25 0 0 1 5.25 9h13.5A2.25 2.25 0 0 1 21 11.25v7.5",
  search: "m21 21-5.197-5.197m0 0A7.5 7.5 0 1 0 5.196 5.196a7.5 7.5 0 0 0 10.607 10.607Z",
  adjust: "M10.5 6h9.75M10.5 6a1.5 1.5 0 1 1-3 0m3 0a1.5 1.5 0 1 0-3 0M3.75 6H7.5m3 12h9.75m-9.75 0a1.5 1.5 0 0 1-3 0m3 0a1.5 1.5 0 0 0-3 0m-3.75 0H7.5m9-6h3.75m-3.75 0a1.5 1.5 0 0 1-3 0m3 0a1.5 1.5 0 0 0-3 0m-9.75 0h9.75",
  copy: "M15.75 17.25v3.375c0 .621-.504 1.125-1.125 1.125h-9.75a1.125 1.125 0 0 1-1.125-1.125V7.875c0-.621.504-1.125 1.125-1.125H6.75a9.06 9.06 0 0 1 1.5.124m7.5 10.376h3.375c.621 0 1.125-.504 1.125-1.125V11.25c0-4.46-3.243-8.161-7.5-8.876a9.06 9.06 0 0 0-1.5-.124H9.375c-.621 0-1.125.504-1.125 1.125v3.5m7.5 10.375H9.375a1.125 1.125 0 0 1-1.125-1.125v-9.25m12 6.625v-1.875a3.375 3.375 0 0 0-3.375-3.375h-1.5a1.125 1.125 0 0 1-1.125-1.125v-1.5a3.375 3.375 0 0 0-3.375-3.375H9.75",
  desktop: "M9 17.25v1.007a3 3 0 0 1-.879 2.122L7.5 21h9l-.621-.621A3 3 0 0 1 15 18.257V17.25m6-12V15a2.25 2.25 0 0 1-2.25 2.25H5.25A2.25 2.25 0 0 1 3 15V5.25m18 0A2.25 2.25 0 0 0 18.75 3H5.25A2.25 2.25 0 0 0 3 5.25m18 0V12a2.25 2.25 0 0 1-2.25 2.25H5.25A2.25 2.25 0 0 1 3 12V5.25",
  close: "M6 18 18 6M6 6l12 12",
  external: "M13.5 6H5.25A2.25 2.25 0 0 0 3 8.25v10.5A2.25 2.25 0 0 0 5.25 21h10.5A2.25 2.25 0 0 0 18 18.75V10.5m-10.5 6L21 3m0 0h-5.25M21 3v5.25",
  key: "M15.75 5.25a3 3 0 0 1 3 3m3 0a6 6 0 0 1-7.029 5.912c-.563-.097-1.159.026-1.563.43L10.5 17.25H8.25v2.25H6v2.25H2.25v-2.818c0-.597.237-1.17.659-1.591l6.499-6.499c.404-.404.527-1 .43-1.563A6 6 0 1 1 21.75 8.25Z",
  line: "M3 17.25 8.25 12l3.75 3.75 9-9",
  table: "M6 4.5h12a2.25 2.25 0 0 1 2.25 2.25v10.5A2.25 2.25 0 0 1 18 19.5H6a2.25 2.25 0 0 1-2.25-2.25V6.75A2.25 2.25 0 0 1 6 4.5ZM3.75 9.75h16.5M3.75 14.625h16.5M9.75 9.75v9.75",
  card: "M2.25 8.25h19.5M2.25 9h19.5m-16.5 5.25h6m-6 2.25h3m-3.75 3h15a2.25 2.25 0 0 0 2.25-2.25V6.75A2.25 2.25 0 0 0 19.5 4.5h-15a2.25 2.25 0 0 0-2.25 2.25v10.5A2.25 2.25 0 0 0 4.5 19.5Z",
};

export function icon(name, size = 16, color = "var(--icon)") {
  return `<svg width="${size}" height="${size}" viewBox="0 0 24 24" aria-hidden="true"><path d="${P[name]}" fill="none" stroke="${color}" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>`;
}

export function warnIcon(size = 14) {
  return `<svg width="${size}" height="${size}" viewBox="0 0 16 16" aria-hidden="true"><path fill-rule="evenodd" clip-rule="evenodd" d="M6.701 2.25c.577-1 2.02-1 2.598 0l5.196 9a1.5 1.5 0 0 1-1.299 2.25H2.804a1.5 1.5 0 0 1-1.3-2.25l5.197-9ZM8 4a.75.75 0 0 1 .75.75v3a.75.75 0 1 1-1.5 0v-3A.75.75 0 0 1 8 4Zm0 8a1 1 0 1 0 0-2 1 1 0 0 0 0 2Z" fill="var(--warn)"/></svg>`;
}

// Provider marks (Claude, OpenAI) from the Paper file, filled with the text colour.
const LOGO = {
  claude: "m4.7144 15.9555 4.7174-2.6471.079-.2307-.079-.1275h-.2307l-.7893-.0486-2.6956-.0729-2.3375-.0971-2.2646-.1214-.5707-.1215-.5343-.7042.0546-.3522.4797-.3218.686.0608 1.5179.1032 2.2767.1578 1.6514.0972 2.4468.255h.3886l.0546-.1579-.1336-.0971-.1032-.0972L6.973 9.8356l-2.55-1.6879-1.3356-.9714-.7225-.4918-.3643-.4614-.1578-1.0078.6557-.7225.8803.0607.2246.0607.8925.686 1.9064 1.4754 2.4893 1.8336.3643.3035.1457-.1032.0182-.0728-.164-.2733-1.3539-2.4467-1.445-2.4893-.6435-1.032-.17-.6194c-.0607-.255-.1032-.4674-.1032-.7285L6.287.1335 6.6997 0l.9957.1336.419.3642.6192 1.4147 1.0018 2.2282 1.5543 3.0296.4553.8985.2429.8318.091.255h.1579v-.1457l.1275-1.706.2368-2.0947.2307-2.6957.0789-.7589.3764-.9107.7468-.4918.5828.2793.4797.686-.0668.4433-.2853 1.8517-.5586 2.9021-.3643 1.9429h.2125l.2429-.2429.9835-1.3053 1.6514-2.0643.7286-.8196.85-.9046.5464-.4311h1.0321l.759 1.1293-.34 1.1657-1.0625 1.3478-.8804 1.1414-1.2628 1.7-.7893 1.36.0729.1093.1882-.0183 2.8535-.607 1.5421-.2794 1.8396-.3157.8318.3886.091.3946-.3278.8075-1.967.4857-2.3072.4614-3.4364.8136-.0425.0304.0486.0607 1.5482.1457.6618.0364h1.621l3.0175.2247.7892.522.4736.6376-.079.4857-1.2142.6193-1.6393-.3886-3.825-.9107-1.3113-.3279h-.1822v.1093l1.0929 1.0686 2.0035 1.8092 2.5075 2.3314.1275.5768-.3218.4554-.34-.0486-2.2039-1.6575-.85-.7468-1.9246-1.621h-.1275v.17l.4432.6496 2.3436 3.5214.1214 1.0807-.17.3521-.6071.2125-.6679-.1214-1.3721-1.9246L14.38 17.959l-1.1414-1.9428-.1397.079-.674 7.2552-.3156.3703-.7286.2793-.6071-.4614-.3218-.7468.3218-1.4753.3886-1.9246.3157-1.53.2853-1.9004.17-.6314-.0121-.0425-.1397.0182-1.4328 1.9672-2.1796 2.9446-1.7243 1.8456-.4128.164-.7164-.3704.0667-.6618.4008-.5889 2.386-3.0357 1.4389-1.882.929-1.0868-.0062-.1579h-.0546l-6.3385 4.1164-1.1293.1457-.4857-.4554.0608-.7467.2307-.2429 1.9064-1.3114Z",
  codex: "M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z",
};

export function logo(provider, cls = "") {
  const d = LOGO[provider];
  if (!d) return `<span class="logo ${cls}"></span>`;
  return `<svg class="logo ${cls}" viewBox="0 0 24 24" aria-label="${provider === "claude" ? "Claude" : "Codex"}"><path d="${d}" fill="currentColor"/></svg>`;
}

export const providerTitle = (p) => (p === "claude" ? "Claude" : p === "codex" ? "Codex" : p);

// ---------- data model ----------

const sig = (signals, name) => {
  if (!signals) return "";
  const want = name.toLowerCase();
  for (const k in signals) if (k.toLowerCase() === want) return String(signals[k]).trim();
  return "";
};
const resetTime = (raw, observedAt, afterSeconds) => {
  if (raw) {
    const n = Number(raw);
    if (Number.isFinite(n) && n > 0) return n * 1000;
    const t = Date.parse(raw);
    if (Number.isFinite(t)) return t;
  }
  if (afterSeconds && observedAt) return Date.parse(observedAt) + Number(afterSeconds) * 1000;
  return 0;
};
const rollForward = (t, windowMs) => {
  if (!t || !windowMs) return t;
  while (t <= Date.now()) t += windowMs;
  return t;
};

// A reading's number, or NaN when it is missing: Number("") and Number(null)
// are 0, which would read as nothing used.
const num = (v) => (v === "" || v == null ? NaN : Number(v));

// Idle accounts are probed every 20 minutes, so a reading older than this
// means readings are not arriving.
export const STALE_MS = 30 * 60e3;

// Marks an item with when it was read and whether that is older than STALE_MS.
export function stamp(item, iso) {
  if (!item) return null;
  const at = validTime(iso) ? Date.parse(iso) : 0;
  return Object.assign(item, { observed: at, stale: !!at && Date.now() - at > STALE_MS });
}

// An item without a used share shows as No reading, not as 100% left.
const known = (item) => (item && Number.isFinite(item.used) ? item : null);

// A limits() item's reading age as markup: a title attribute ("Read 2h ago"),
// and once the reading is stale a short note ("· 2h") to show beside it.
export function ageParts(item) {
  const age = item?.observed ? ago(item.observed) : "";
  return { title: age ? ` title="Read ${age}"` : "", note: item?.stale && age ? "· " + age.replace(/ ago$/, "") : "" };
}

export function primeSet() {
  return new Set((S.data?.routing?.prime_after_reset || []).map((p) => String(p).toLowerCase()));
}

// A weekly reading whose reset has passed describes the old week. With priming
// on, the new week has not started yet; otherwise it runs on a fixed cycle.
function weekItem(provider, used, reset, windowMs) {
  if (reset && reset <= Date.now()) {
    return primeSet().has(provider) ? { used: 0, reset: 0, notStarted: true } : { used: 0, reset: rollForward(reset, windowMs) };
  }
  return { used, reset };
}

// Limits from the router's meters (per-model weeks, survive restarts), else
// from the credential's last quota headers. Items carry when they were read.
export function limits(acct) {
  const meters = S.data?.router?.accounts?.[acct.id]?.meters;
  const out = { week: null, short: null, extra: [] };
  if (meters && meters.length) {
    for (const m of meters) {
      const reset = validTime(m.reset_at) ? Date.parse(m.reset_at) : 0;
      const item = stamp({ used: num(m.utilization) * 100, reset: reset > Date.now() ? reset : 0, burn: m.burn_per_hour > 0 ? m.burn_per_hour * 100 : 0, title: m.title }, m.observed_at);
      if (m.long && m.reset_since_reading && primeSet().has(acct.provider)) Object.assign(item, { used: 0, reset: 0, notStarted: true });
      if (!known(item)) continue;
      if ((m.name === "7d" || m.name === "secondary") && !out.week) out.week = item;
      else if ((m.name === "5h" || m.name === "primary") && !out.short) out.short = item;
      else out.extra.push(item);
    }
    if (out.week || out.short) return out;
  }
  const sl = signalLimits(acct, out);
  return { ...sl, week: known(sl.week), short: known(sl.short) };
}

// Limits from the credential's last quota headers. This is what the live
// soonest-reset router reads, so the queue order uses it. Like the router it
// keeps a reset time that came without a used share; that item's used is NaN.
export function signalLimits(acct, out = { week: null, short: null, extra: [] }) {
  const q = acct.quota || {};
  const s = q.signals || {};
  if (acct.provider === "claude") {
    const w = sig(s, "Anthropic-Ratelimit-Unified-7d-Utilization");
    const wr = resetTime(sig(s, "Anthropic-Ratelimit-Unified-7d-Reset"));
    if (w || wr) out.week = stamp(weekItem("claude", num(w) * 100, wr, 7 * 864e5), q.observed_at);
    const f = sig(s, "Anthropic-Ratelimit-Unified-5h-Utilization");
    const fr = resetTime(sig(s, "Anthropic-Ratelimit-Unified-5h-Reset"));
    if (f || fr) out.short = stamp({ used: num(f) * 100, reset: fr > Date.now() ? fr : 0 }, q.observed_at);
  } else if (acct.provider === "codex") {
    for (const p of ["X-Codex-Primary-", "X-Codex-Secondary-"]) {
      const minutes = Number(sig(s, p + "Window-Minutes"));
      if (!minutes) continue;
      const reset = resetTime(sig(s, p + "Reset-At"), q.observed_at, sig(s, p + "Reset-After-Seconds"));
      const used = num(sig(s, p + "Used-Percent"));
      if (minutes >= 6 * 24 * 60) out.week = stamp(weekItem("codex", used, reset, minutes * 6e4), q.observed_at);
      else out.short = stamp({ used, reset: reset > Date.now() ? reset : 0 }, q.observed_at);
    }
  }
  return out;
}

export const left = (item) => (item && Number.isFinite(item.used) ? Math.max(0, Math.min(100, 100 - item.used)) : null);

export function accounts() {
  const list = (S.data?.accounts || []).map((a) => {
    const provider = String(a.provider || a.type || "").toLowerCase();
    const priority = Number.isFinite(Number(a.priority)) ? Number(a.priority) : 0;
    const mode = a.mode || (a.disabled ? "off" : priority < 0 ? "reserve" : "rotation");
    return { ...a, provider, priority, mode, email: a.email || a.label || a.name || a.id };
  }).filter((a) => a.provider === "claude" || a.provider === "codex");
  list.sort((x, y) => (x.provider === y.provider ? 0 : x.provider === "claude" ? -1 : 1));
  return list;
}

export function accountById(id) {
  return accounts().find((a) => a.id === id);
}

export function names() {
  const n = {};
  for (const a of accounts()) n[a.id] = a.email;
  return n;
}

// What an account can do right now.
//   kind: ready | off | usedup | limited | blocked | error
export function status(acct) {
  if (acct.disabled || acct.mode === "off") return { kind: "off", text: "Off" };
  const r = S.data?.router?.accounts?.[acct.id];
  if (r?.refused) return { kind: "error", text: r.refused + ", skipped for new sessions", detail: r.refused };
  const retry = validTime(acct.next_retry_after) ? Date.parse(acct.next_retry_after) : 0;
  const lim = limits(acct);
  if (retry && retry > Date.now()) {
    const weekGone = left(lim.week) === 0 || retry - Date.now() > 6 * 3600e3;
    return weekGone ? { kind: "usedup", text: "Used up", until: retry } : { kind: "limited", text: "Resting until " + clock(retry), until: retry };
  }
  const msg = String(acct.status_message || "");
  if (String(acct.status || "").toLowerCase() === "error") {
    if (/401|403|unauthor|invalid_grant|refresh|sign|login|expired|revoked/i.test(msg)) return { kind: "blocked", text: "Sign-in blocked", detail: msg };
    return { kind: "error", text: msg ? "Error" : "Error", detail: msg };
  }
  if (acct.unavailable) return { kind: "limited", text: "Unavailable" };
  if (left(lim.week) === 0) return { kind: "usedup", text: "Used up" };
  return { kind: "ready", text: "Ready" };
}

// Mirror the live soonest-reset selector: highest priority tier among ready
// accounts, unobserved accounts first, then the earliest weekly reset. It
// reads the quota headers, not the router meters, so this does too.
export function queue(provider) {
  const list = accounts().filter((a) => a.provider === provider);
  const ready = list.filter((a) => status(a).kind === "ready");
  const order = ready.slice().sort((a, b) => {
    if (a.priority !== b.priority) return b.priority - a.priority;
    const ra = signalLimits(a).week?.reset || 0, rb = signalLimits(b).week?.reset || 0;
    if (!ra !== !rb) return ra ? 1 : -1;
    if (ra !== rb) return ra - rb;
    return String(a.id).localeCompare(String(b.id));
  });
  const rest = list.filter((a) => !ready.includes(a));
  return { order, rest, next: order.find((a) => a.priority >= 0) || order[0] || null };
}

export function plan(acct) {
  const p = acct.plan || {};
  const ends = p.ends_on || "";
  const renews = p.renews_on || "";
  return { type: p.type || "", renews, ends, source: p.source || "" };
}

export const tokens = (u) => (u.input_tokens || 0) + (u.output_tokens || 0) + (u.cache_read_tokens || 0) + (u.cache_write_tokens || 0);
export function cacheReuse(u) {
  const read = u.cache_read_tokens || 0;
  const total = read + (u.cache_write_tokens || 0) + (u.input_tokens || 0);
  return total ? (read / total) * 100 : null;
}
// What usage would have cost at the public API list prices, in USD. A backend
// without summary.pricing does not price usage, so cost stays hidden there.
export const costKnown = () => !!plainObject(S.data?.summary?.pricing);
export const apiCost = (u) => Number(u?.api_cost) || 0;

// Milliseconds for a timestamp, or 0 when it is missing or unset.
const ms0 =(iso) => Date.parse(validTime(iso)) || 0;

// True when Go timestamp a is after b, to the nanosecond; Date.parse keeps only milliseconds.
function laterThan(a, b) {
  const sub = (iso) => Number(((/\.(\d+)/.exec(iso) || [])[1] || "").padEnd(9, "0").slice(3, 9));
  const ma = ms0(a), mb = ms0(b);
  if (ma !== mb) return ma > mb;
  return ma > 0 && sub(a) > sub(b);
}

// Adds each account's numeric counters from src into dst.
function addByAuth(dst, src) {
  for (const [id, v] of Object.entries(src || {})) {
    if (!v) continue;
    const d = dst[id] || (dst[id] = {});
    for (const [k, n] of Object.entries(v)) if (typeof n === "number") d[k] = (d[k] || 0) + n;
  }
  return dst;
}

// Sessions with agent threads folded into their parent.
export function sessions() {
  const raw = S.data?.summary?.sessions || [];
  const byId = new Map();
  const parentOf = (s) => s.parent_id || (String(s.id).includes(":agent:") ? String(s.id).split(":agent:")[0] : "");
  for (const s of raw) if (!parentOf(s)) byId.set(s.id, { ...s, threads: [], auth_ids: [...(s.auth_ids || [])] });
  for (const s of raw) {
    const pid = parentOf(s);
    if (!pid) continue;
    let p = byId.get(pid);
    if (!p) {
      p = { id: pid, provider: s.provider, auth_ids: [], first_seen: s.first_seen, last_seen: s.last_seen, requests: 0, failed: 0, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, api_cost: 0, threads: [], title: "", machine: s.machine || "" };
      byId.set(pid, p);
    }
    p.threads.push(s);
  }
  const out = [...byId.values()].map((p) => {
    const t = { ...p, own: { requests: p.requests, failed: p.failed }, by_auth: addByAuth({}, p.by_auth) };
    for (const c of p.threads) {
      for (const k of ["requests", "failed", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "api_cost"]) t[k] = (t[k] || 0) + (c[k] || 0);
      for (const id of c.auth_ids || []) if (!t.auth_ids.includes(id)) t.auth_ids.push(id);
      addByAuth(t.by_auth, c.by_auth);
      if (ms0(c.last_seen) > ms0(t.last_seen)) t.last_seen = c.last_seen;
      // served_at is when the latest successful request started; failures do not move it.
      if (c.serving_auth_id && laterThan(c.served_at, t.served_at)) {
        t.serving_auth_id = c.serving_auth_id;
        t.served_at = c.served_at;
      }
      if (ms0(c.first_seen) && (!ms0(t.first_seen) || ms0(c.first_seen) < ms0(t.first_seen))) t.first_seen = c.first_seen;
      if (!t.machine && c.machine) t.machine = c.machine;
    }
    return t;
  });
  out.sort((a, b) => String(b.last_seen).localeCompare(String(a.last_seen)));
  return out;
}

export const sessionTitle = (s) => s.title || String(s.id).replace(/^[a-z]+:/, "").slice(0, 8);
export const sessionHref = (s) => "#/sessions/" + encodeURIComponent(s.id);

// ---------- components ----------

export function email(addr) {
  const s = String(addr || "");
  const at = s.indexOf("@");
  if (at < 0) return `<span class="email clamp"><b>${esc(s)}</b></span>`;
  return `<span class="email clamp"><b>${esc(s.slice(0, at))}</b><span>${esc(s.slice(at))}</span></span>`;
}

export const pill = (text, cls = "") => `<span class="pill ${cls}">${esc(text)}</span>`;
export const warnState = (text, cls = "") => `<span class="state ${cls}" title="${esc(text)}">${warnIcon(14)}<span class="clamp">${esc(text)}</span></span>`;

// item, a limits() item, adds its reading's age (ageParts), on a muted cell
// once the reading is stale.
export function meterCell(pctLeft, color = "", item = null) {
  if (pctLeft == null) return "";
  const p = Math.max(0, Math.min(100, pctLeft));
  const age = ageParts(item);
  return `<span class="metercell${item?.stale ? " stale" : ""}"${age.title}><span class="meter"><i style="width:${p}%${color ? ";background:" + color : ""}"></i></span><span class="${p <= 0 ? "muted" : ""}">${Math.round(p)}%</span>${age.note ? `<span>${age.note}</span>` : ""}</span>`;
}

export function seg(items, active, attr, cls = "") {
  return `<div class="seg ${cls}">${items.map((t) => `<button class="${t.id === active ? "on" : ""}" ${attr}="${esc(t.id)}">${esc(t.label)}</button>`).join("")}</div>`;
}

// cols: [{label, head (html in place of label), w (px) or grow, r (right aligned), cls}]; rows: [{cells: [html], href, cls, attrs}]
export function table(cols, rows, opts = {}) {
  const cell = (c, html, i) => {
    // opts.fit lets sized columns shrink to c.min (default 70%) instead of scrolling sideways.
    const style = !c.w ? "" : opts.fit
      ? `style="width:${c.w}px;flex:0 1 ${c.w}px;min-width:${c.min ?? Math.round(c.w * 0.7)}px"`
      : `style="width:${c.w}px;flex:0 0 ${c.w}px"`;
    return `<div class="c ${c.w ? "" : "g"} ${c.r ? "r" : ""} ${c.cls || ""}" ${style}>${html ?? ""}</div>`;
  };
  const head = opts.noHead ? "" : `<div class="tr head">${cols.map((c, i) => cell(c, c.head ?? esc(c.label || ""), i)).join("")}</div>`;
  const body = rows.map((r) => {
    if (r.group) return `<div class="tr group ${r.cls || ""}">${r.html}</div>`;
    const tag = r.href ? "a" : "div";
    const href = r.href ? ` href="${esc(r.href)}"` : "";
    return `<${tag} class="tr ${r.cls || ""}"${href} ${r.attrs || ""}>${cols.map((c, i) => cell(c, r.cells[i], i)).join("")}</${tag}>`;
  }).join("");
  const empty = !rows.length && opts.empty ? `<div class="empty">${esc(opts.empty)}</div>` : "";
  return `<div class="tblwrap ${opts.cls || ""}"><div class="tbl">${head}${body}</div></div>${empty}`;
}

export function figure(label, value, unit = "", opts = {}) {
  const tag = opts.metric ? "button" : "div";
  return `<${tag} class="fig ${opts.on ? "on" : ""} ${opts.metric ? "" : "static"}" ${opts.metric ? `data-metric="${esc(opts.metric)}"` : ""}${opts.title ? ` title="${esc(opts.title)}"` : ""}><span class="l">${esc(label)}</span><span class="v">${value}${unit ? `<span class="u">${esc(unit)}</span>` : ""}</span>${opts.sub ? `<span class="muted">${opts.sub}</span>` : ""}</${tag}>`;
}

// A round number at or above v for the top of a y axis.
export function niceMax(v) {
  if (v <= 0) return 1;
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) if (m * exp >= v) return m * exp;
  return 10 * exp;
}

// ---------- popovers, toasts, writes ----------

let openMenu = null;
export function closeMenu() {
  if (!openMenu) return;
  const m = openMenu;
  openMenu = null;
  m.el.remove();
  document.removeEventListener("mousedown", m.away, true);
  document.removeEventListener("keydown", m.esc, true);
  S.hold = Math.max(0, S.hold - 1);
  if (m.onClose) m.onClose();
}

// Opens a menu under `anchor`. items: [{a, b, on, danger, run}] or "sep" or {html, mount}.
// opts: width, alignLeft, cls (extra class), gap (px below the anchor), onClose.
export function menu(anchor, items, opts = {}) {
  closeMenu();
  const el = document.createElement("div");
  el.className = "menu" + (opts.cls ? " " + opts.cls : "");
  el.style.width = (opts.width || 248) + "px";
  el.innerHTML = items.map((it, i) => {
    if (it === "sep") return `<div class="sep"></div>`;
    if (it.html) return `<div data-i="${i}">${it.html}</div>`;
    return `<button class="mi ${it.on ? "on" : ""} ${it.danger ? "danger" : ""}" data-i="${i}"><span class="tx"><span class="a">${esc(it.a)}</span>${it.b ? `<span class="b">${esc(it.b)}</span>` : ""}</span>${it.on ? icon("check", 14, "var(--fg)") : ""}</button>`;
  }).join("");
  document.body.appendChild(el);
  const r = anchor.getBoundingClientRect();
  const w = el.offsetWidth, h = el.offsetHeight;
  let x = opts.alignLeft ? r.left : r.right - w;
  x = Math.max(8, Math.min(x, window.innerWidth - w - 8));
  const gap = opts.gap ?? 4;
  let y = r.bottom + gap;
  if (y + h > window.innerHeight - 8) y = Math.max(8, r.top - h - gap);
  el.style.left = x + "px";
  el.style.top = y + "px";
  el.addEventListener("click", (e) => {
    const b = e.target.closest("button.mi");
    if (!b) return;
    const it = items[Number(b.dataset.i)];
    closeMenu();
    if (it?.run) it.run();
  });
  items.forEach((it, i) => { if (it.mount) it.mount(el.querySelector(`[data-i="${i}"]`)); });
  const away = (e) => { if (!el.contains(e.target) && !anchor.contains(e.target)) closeMenu(); };
  const escKey = (e) => { if (e.key === "Escape") closeMenu(); };
  document.addEventListener("mousedown", away, true);
  document.addEventListener("keydown", escKey, true);
  openMenu = { el, away, esc: escKey, onClose: opts.onClose };
  S.hold++;
  return el;
}

export function toast(msg, err = false) {
  const t = document.createElement("div");
  t.className = "toast" + (err ? " err" : "");
  t.textContent = msg;
  document.body.appendChild(t);
  setTimeout(() => t.remove(), err ? 6000 : 2500);
}

// Asks for the management key once per device; resolves "" when cancelled.
export function askKey() {
  return new Promise((resolve) => {
    const scrim = document.createElement("div");
    scrim.className = "scrim";
    const box = document.createElement("div");
    box.className = "menu";
    box.style.cssText = "width:340px;left:50%;top:30%;transform:translateX(-50%);padding:16px;gap:12px";
    box.innerHTML = `<div class="ui strong">Management key</div><div class="muted">Needed once on this device to change routing, account modes and sign-ins. Viewing needs no key.</div><input class="input" type="password" autocomplete="off" placeholder="Paste the key"><div class="row gap8 right" style="justify-content:flex-end"><button class="btn" data-x>Cancel</button><button class="btn primary" data-ok>Save</button></div>`;
    document.body.append(scrim, box);
    S.hold++;
    const input = box.querySelector("input");
    input.focus();
    const done = (v) => { scrim.remove(); box.remove(); S.hold = Math.max(0, S.hold - 1); resolve(v); };
    box.querySelector("[data-x]").onclick = () => done("");
    scrim.onclick = () => done("");
    box.querySelector("[data-ok]").onclick = () => { const v = input.value.trim(); if (v) setKey(v); done(v); };
    input.onkeydown = (e) => { if (e.key === "Enter") box.querySelector("[data-ok]").click(); if (e.key === "Escape") done(""); };
  });
}

export async function api(path, opts = {}) {
  if (!S.key && !(await askKey())) throw Object.assign(new Error("No key"), { cancelled: true });
  const res = await fetch("/v8/management" + path, { ...opts, headers: { "X-Management-Key": S.key, "Content-Type": "application/json", ...(opts.headers || {}) } });
  if (res.status === 401 || res.status === 403) {
    setKey("");
    throw Object.assign(new Error("The management key was rejected. Enter it again."), { auth: true });
  }
  let body = null;
  try { body = await res.json(); } catch (e) { /* empty body */ }
  if (!res.ok) throw new Error((body && (body.message || body.error)) || path + " returned " + res.status);
  return body;
}

// A custom priority survives a mode change that keeps its sign: Reserve is
// any priority below zero, Rotation zero or above.
export async function setMode(acct, want) {
  const p = acct.priority || 0;
  await api("/credentials/status", { method: "PATCH", body: JSON.stringify({ name: acct.name || acct.id, disabled: want === "off" }) });
  if (want !== "off") await api("/credentials/fields", { method: "PATCH", body: JSON.stringify({ name: acct.name || acct.id, priority: want === "reserve" ? (p < 0 ? p : -1) : Math.max(0, p) }) });
}

// One provider's accounts in the order the scheduler picks them: enabled
// accounts by priority high to low, then auth ID in byte order. Fill-first
// uses one at a time, so each gets its own rank; other strategies rotate
// equal priorities, so those share a rank (1, 1, 3). `after` is the rank
// used just before (0 for the first). Off accounts get rank 0, last.
export function routingOrder(list, strategy = S.data?.routing?.strategy) {
  const off = (a) => a.disabled || a.mode === "off";
  const on = list.filter((a) => !off(a)).sort((a, b) => b.priority - a.priority || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  let rank = 0, after = 0;
  const ranked = on.map((a, i) => {
    if (strategy === "fill-first" || !i || a.priority !== on[i - 1].priority) { after = rank; rank = i + 1; }
    return { ...a, rank, after };
  });
  return [...ranked, ...list.filter(off).map((a) => ({ ...a, rank: 0, after: 0 }))];
}

export const rankTitle = (a) => (!a.rank ? "Off, never used" : a.after ? "Used after #" + a.after : "Used first");

export const MODES = [
  { id: "rotation", a: "Rotation", b: "Takes new sessions" },
  { id: "reserve", a: "Reserve", b: "Only once the others run out" },
  { id: "off", a: "Off", b: "Never used, stays signed in" },
];
export const modeTitle = (m) => (MODES.find((x) => x.id === m) || MODES[0]).a;

// Runs a write, then refreshes. Errors become a toast.
export async function write(fn, done) {
  try {
    await fn();
    if (done) toast(done);
  } catch (e) {
    if (!e.cancelled) toast(e.message || String(e), true);
  }
  window.dispatchEvent(new Event("dash:refresh"));
}

// Mode, plan-date and rank menus shared by Accounts and Account details.
export function modeMenu(anchor, acct) {
  menu(anchor, MODES.map((m) => ({ a: m.a, b: m.b, on: acct.mode === m.id, run: () => { if (m.id !== acct.mode) write(() => setMode(acct, m.id), acct.email + ": " + m.a); } })));
}

// One saved date field of an account's auth file.
function accountField(acct, key, label, value, hint) {
  return {
    html: `<div class="field"><label>${esc(label)}</label><div class="inline"><input class="input" type="date" value="${esc(value)}" data-k="${key}" style="flex:1"><button class="btn outline" data-save="${key}">Save</button></div>${hint ? `<span class="muted">${esc(hint)}</span>` : ""}</div>`,
    mount: (el) => {
      el.querySelector("[data-save]").onclick = () => {
        const input = el.querySelector("input");
        if (input.validity.badInput) return toast(label + " must be a date", true);
        const v = input.value.trim();
        closeMenu();
        write(() => api("/credentials/fields", { method: "PATCH", body: JSON.stringify({ name: acct.name || acct.id, [key]: v || null }) }), v ? label + " set to " + planDay(v) : label + " cleared");
      };
    },
  };
}

// The account's place among its provider's enabled accounts. The proxy moves
// it there and renumbers the priorities behind it, so no raw number shows.
export function rankMenu(anchor, acct) {
  const on = routingOrder(accounts().filter((a) => a.provider === acct.provider)).filter((a) => a.rank);
  const at = on.findIndex((a) => a.id === acct.id) + 1;
  const note = (text) => ({ html: `<div class="field"><label>Rank</label><span class="muted">${esc(text)}</span></div>` });
  if (!at) return menu(anchor, [note("Off accounts are never used. Switch it to Rotation or Reserve to give it a place.")], { width: 280 });
  menu(anchor, [
    note("The others shift to make room. Reserve accounts always come after Rotation ones."),
    ...on.map((_, i) => ({
      a: i + 1 + (!i ? " — used first" : i === on.length - 1 ? " — used last" : ""),
      on: i + 1 === at,
      run: () => { if (i + 1 !== at) write(() => api("/credentials/rank", { method: "PATCH", body: JSON.stringify({ name: acct.name || acct.id, rank: i + 1 }) }), acct.email + " moved to #" + (i + 1)); },
    })),
  ], { width: 280 });
}

export function planMenu(anchor, acct, extra = []) {
  const p = plan(acct);
  menu(anchor, [
    ...extra,
    ...(extra.length ? ["sep"] : []),
    accountField(acct, "plan_renews_on", "Plan renews on", p.source === "manual" ? p.renews : "", p.source === "token" && p.renews ? "From the sign-in token: " + planDay(p.renews) + ". Set a date to override it." : "The day the subscription bills again."),
    accountField(acct, "plan_ends_on", "Plan ends on", p.ends, "Set this when you have cancelled. Leave empty while it renews."),
    "sep",
    { a: "Change rank", run: () => rankMenu(anchor, acct) },
  ], { width: 280 });
}
