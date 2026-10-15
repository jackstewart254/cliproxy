// Devices: this machine and the other machines in the proxy's config, with
// how busy each is and how much more work it can take. #/devices/<id> opens
// one: cores, memory, disks and its busiest processes. Readings refresh every
// few seconds while this tab is open and the page is in view.
import { esc, int, icon, table, meterCell, clock } from "../core.js";
import { region } from "../paint.js";

const POLL_MS = 3000;
const D = { list: null, error: "", at: 0, timer: 0, busy: false, sort: "cpu" };

const onDevices = () => /^#\/devices(\/|$)/.test(location.hash);
const redraw = () => window.dispatchEvent(new Event("dash:patch"));

function schedule() {
  if (D.timer || !onDevices() || document.hidden) return;
  D.timer = setTimeout(poll, POLL_MS);
}

// One request at a time; the next is asked for once this one has answered.
async function poll() {
  clearTimeout(D.timer);
  D.timer = 0;
  if (D.busy || !onDevices() || document.hidden) return;
  D.busy = true;
  try {
    const res = await fetch("/dashboard/devices", { cache: "no-store" });
    if (!res.ok) throw new Error("Could not read the devices (" + res.status + ")");
    D.list = (await res.json()).devices || [];
    D.error = "";
    D.at = Date.now();
  } catch (e) {
    D.error = (e.message || String(e)) + (D.list ? ". Showing the last reading." : "");
  } finally {
    D.busy = false;
  }
  // Scheduled first: the redraw mounts the screen, which polls when nothing is due.
  schedule();
  if (onDevices()) redraw();
}

document.addEventListener("visibilitychange", () => { if (!document.hidden) poll(); });
window.addEventListener("hashchange", () => { if (onDevices()) poll(); });

// ---------- formatting ----------

const GiB = 2 ** 30;
function bytes(n) {
  n = Number(n) || 0;
  if (n >= 100 * GiB) return Math.round(n / GiB) + " GB";
  if (n >= 1000 * 2 ** 20) return (n / GiB).toFixed(1) + " GB";
  return Math.round(n / 2 ** 20) + " MB";
}
const pct = (p) => Math.round(Number(p) || 0) + "%";
function uptime(sec) {
  const m = Math.floor((Number(sec) || 0) / 60), h = Math.floor(m / 60), d = Math.floor(h / 24);
  return d ? `${d}d ${h % 24}h` : h ? `${h}h ${m % 60}m` : `${m}m`;
}
const sentence = (s) => (s ? s.charAt(0).toUpperCase() + s.slice(1) : "");
function platform(st) {
  const name = st.platform === "darwin" || st.os === "darwin" ? "macOS" : st.platform || st.os || "";
  return [name, st.platform_version].filter(Boolean).join(" ");
}

// How much more work a machine can take: the tighter of idle CPU and free memory.
function room(st) {
  const h = st.headroom || {};
  const free = Math.min(Number(h.cpu_idle_percent) || 0, Number(h.memory_available_percent) || 0);
  const [text, cls] = free >= 50 ? ["Plenty of room", "ok"] : free >= 25 ? ["Some room", ""] : free >= 10 ? ["Tight", "warn"] : ["Full", "bad"];
  const title = `${(Number(h.idle_cores) || 0).toFixed(1)} of ${st.cpu?.cores || 0} cores idle, ${bytes(h.memory_available)} of ${bytes(st.memory?.total)} memory free`;
  return `<span class="room ${cls}" title="${esc(title)}"><i></i>${text}</span>`;
}
// macOS lets a user read only their own processes' threads and usage, so
// the thread count and the busiest lists leave other users' processes out.
const partial = (st) => st.os === "darwin" && st.unreadable > 0;
const dot = (on) => `<i class="devdot ${on ? "on" : ""}" role="img" aria-label="${on ? "Online" : "Offline"}"></i>`;

// A live piece of the screen: replaced on each reading without touching
// the links and buttons around it, so focus and scroll stay.
const live = (key, html) => region(key, html, "span", `style="display:contents"`);

const readNote = () => (D.at ? esc(`Read ${clock(D.at)}`) : "");

function shell(crumbs, end, body) {
  return {
    html: `<div class="bar">${crumbs}<div class="end">${live("dv|read", `<span class="muted nowrap">${end}</span>`)}</div></div>
    <div class="body">${D.error ? `<div class="banner">${esc(D.error)}</div>` : ""}${body}</div>`,
    mount() { if (!D.timer && !D.busy) poll(); },
  };
}

// ---------- list ----------

function listView() {
  const title = `<div class="crumbs"><span class="here">Devices</span></div>`;
  if (!D.list) return shell(title, "", `<div class="empty">${D.error ? "" : "Reading the devices…"}</div>`);
  const cols = [
    { label: "", w: 16, min: 16, cls: "ic" },
    { label: "Device" },
    { label: "CPU", w: 128 },
    { label: "Memory", w: 128 },
    { label: "Room for work", w: 128 },
  ];
  const rows = D.list.map((d) => {
    const st = d.stats || {};
    const k = "dv|" + d.id + "|";
    const sub = d.online
      ? [d.local ? "This machine" : "", platform(st), "up " + uptime(st.uptime_seconds)].filter(Boolean).join(" · ")
      : "Offline · " + sentence(d.error || "no reading");
    return {
      href: "#/devices/" + encodeURIComponent(d.id),
      cells: [
        live(k + "dot", dot(d.online)),
        `<span class="devname"><b class="clamp">${esc(d.name || d.id)}</b>${live(k + "sub", `<span class="clamp ${d.online ? "muted" : "warn"}" title="${esc(sub)}">${esc(sub)}</span>`)}</span>`,
        live(k + "cpu", d.online ? meterCell(st.cpu?.percent) : ""),
        live(k + "mem", d.online ? meterCell(st.memory?.percent) : ""),
        live(k + "room", d.online ? room(st) : ""),
      ],
    };
  });
  return shell(title, readNote(), table(cols, rows, { fit: true, cls: "devtbl" }));
}

// ---------- one device ----------

function stat(label, value, sub, opts = {}) {
  const bar = opts.meter == null ? "" : `<span class="meter"><i style="width:${Math.max(0, Math.min(100, opts.meter))}%"></i></span>`;
  return `<div class="stat"${opts.title ? ` title="${esc(opts.title)}"` : ""}><span class="muted">${esc(label)}</span><span class="v">${value}</span>${bar}<span class="s">${sub}</span></div>`;
}

function statsStrip(st) {
  const cpu = st.cpu || {}, m = st.memory || {}, sw = st.swap || {}, h = st.headroom || {}, l = st.load;
  return [
    stat("CPU", pct(cpu.percent), esc([`${cpu.cores || 0} cores`, cpu.model].filter(Boolean).join(" · ")), { meter: cpu.percent }),
    l ? stat("Load", (Number(l.load1) || 0).toFixed(2), esc(`5 min ${(Number(l.load5) || 0).toFixed(2)} · 15 min ${(Number(l.load15) || 0).toFixed(2)}`)) : "",
    stat("Memory", pct(m.percent), esc(`${bytes(m.used)} of ${bytes(m.total)}`), { meter: m.percent }),
    stat("Swap", sw.total ? pct(sw.percent) : "–", esc(sw.total ? `${bytes(sw.used)} of ${bytes(sw.total)}` : "None"), { meter: sw.total ? sw.percent : 0 }),
    stat("Processes", int(st.processes), esc(int(st.threads) + (partial(st) ? " threads in your processes" : " threads")), { title: partial(st) ? `${int(st.unreadable)} processes belong to other users and cannot be read without root, so their threads are not counted and they are not among the busiest.` : "" }),
    stat("Room for work", room(st), esc(`${(Number(h.idle_cores) || 0).toFixed(1)} cores idle · ${bytes(h.memory_available)} free`)),
  ].join("");
}

function cores(st) {
  return (st.cpu?.per_core || []).map((p, i) => `<div class="devcore"><span class="muted">${i + 1}</span>${meterCell(p)}</div>`).join("");
}

function diskTable(st) {
  const cols = [{ label: "Volume" }, { label: "Used", w: 160 }, { label: "Free", w: 88, r: true }, { label: "Size", w: 88, r: true }];
  const rows = (st.disks || []).map((d) => ({
    cells: [`<span class="mono clamp">${esc(d.mount)}</span>`, meterCell(d.total ? ((d.total - d.free) / d.total) * 100 : 0), bytes(d.free), bytes(d.total)],
  }));
  return table(cols, rows, { empty: "No volumes reported." });
}

const PROC_COLS = [{ label: "Process" }, { label: "PID", w: 72, r: true }, { id: "cpu", label: "CPU", w: 88, r: true, title: "Share of one core" }, { id: "mem", label: "Memory", w: 96, r: true }, { label: "Threads", w: 72, r: true }];

function procHead() {
  const cols = PROC_COLS.map((c) => {
    if (!c.id) return c;
    const on = D.sort === c.id;
    return { ...c, head: `<button class="sortbtn ${on ? "on" : ""}" data-dev-sort="${c.id}" data-focus-key="dev-sort-${c.id}" aria-pressed="${on}"${c.title ? ` title="${esc(c.title)}"` : ""}>${esc(c.label)}${on ? icon("chevronDown", 12, "var(--fg)") : ""}</button>` };
  });
  return table(cols, [], {});
}

function procRows(st) {
  const list = (D.sort === "mem" ? st.top_memory : st.top_cpu) || [];
  const rows = list.map((p) => ({ cells: [`<span class="clamp">${esc(p.name)}</span>`, `<span class="muted">${p.pid}</span>`, pct(p.cpu_percent), bytes(p.rss), int(p.threads)] }));
  return table(PROC_COLS, rows, { noHead: true, empty: "No processes could be read." });
}

function deviceView(id) {
  const d = (D.list || []).find((x) => x.id === id);
  const crumbs = (name, on) => `<div class="crumbs"><a href="#/devices">Devices</a>${icon("chevronRight", 14)}<span class="here">${on == null ? "" : live("dv|one|dot", dot(on))}<span class="clamp">${esc(name)}</span></span></div>`;
  if (!D.list) return shell(crumbs(id), "", `<div class="empty">${D.error ? "" : "Reading the device…"}</div>`);
  if (!d) return shell(crumbs(id), "", `<div class="empty">No such device. Devices come from management.devices in the proxy's config.</div>`);
  if (!d.online) return shell(crumbs(d.name || d.id, false), readNote(), `<div class="empty">${esc(d.local ? "This machine" : d.name)} is offline. ${esc(sentence(d.error || ""))}</div>`);
  const st = d.stats;
  // Phones keep the device name and the reading time in the bar.
  const end = `<span class="only-wide">${esc(`${platform(st)} · up ${uptime(st.uptime_seconds)}`)} · </span>${readNote()}`;
  const view = shell(crumbs(d.name || d.id, true), end, `
    ${live("dv|one|stats", `<div class="stats devstats">${statsStrip(st)}</div>`)}
    <div class="sec">
      <div class="sech"><div class="t"><b>Cores</b>${live("dv|one|cores-n", `<span class="muted">${(st.cpu?.per_core || []).length} logical</span>`)}</div></div>
      ${live("dv|one|cores", `<div class="devcores">${cores(st)}</div>`)}
    </div>
    <div class="sec">
      <div class="sech"><div class="t"><b>Busiest processes</b><span class="muted">Top ${(st.top_cpu || []).length}${partial(st) ? " of your processes" : ""}</span></div></div>
      ${procHead()}${live("dv|one|procs", procRows(st))}
    </div>
    <div class="sec last">
      <div class="sech"><div class="t"><b>Disks</b></div></div>
      ${live("dv|one|disks", diskTable(st))}
    </div>`);
  const first = view.mount;
  view.mount = (root) => {
    first();
    root.querySelectorAll("[data-dev-sort]").forEach((b) => {
      b.onclick = () => { D.sort = b.dataset.devSort; window.dispatchEvent(new Event("dash:render")); };
    });
  };
  return view;
}

export function view(ctx) {
  return ctx.params[0] ? deviceView(ctx.params[0]) : listView();
}
