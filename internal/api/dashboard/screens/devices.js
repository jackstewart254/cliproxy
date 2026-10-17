// Devices: this machine and the other machines in the proxy's config, each
// with one plain sentence on whether it is free for work. #/devices/<id>
// opens one: that sentence, four tiles, what is running grouped by what it
// belongs to, its disks, and the technical details folded away at the end.
// Readings refresh every few seconds while this tab is open and the page is
// in view.
import { esc, int, icon, table, meterCell, clock } from "../core.js";
import { region } from "../paint.js";

const POLL_MS = 3000;
const D = { list: null, error: "", at: 0, timer: 0, busy: false, sort: "cpu", open: new Set(), tech: false };

const onDevices = () => /^#\/devices(\/|$)/.test(location.hash);
const redraw = () => window.dispatchEvent(new Event("dash:patch"));
const rerender = () => window.dispatchEvent(new Event("dash:render"));

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

if (typeof document !== "undefined") {
  document.addEventListener("visibilitychange", () => { if (!document.hidden) poll(); });
  window.addEventListener("hashchange", () => { if (onDevices()) poll(); });
}

// ---------- formatting ----------

const GiB = 2 ** 30;
function bytes(n) {
  n = Number(n) || 0;
  if (n >= 100 * GiB) return Math.round(n / GiB) + " GB";
  if (n >= 1000 * 2 ** 20) return (n / GiB).toFixed(1) + " GB";
  return Math.round(n / 2 ** 20) + " MB";
}
// Gigabytes for a sentence: "9 GB", "1.5 GB".
const gb = (n) => { const v = (Number(n) || 0) / GiB; return (v >= 10 ? Math.round(v) : Math.round(v * 10) / 10) + " GB"; };
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
const clamp = (p) => Math.max(0, Math.min(100, Number(p) || 0));

// ---------- what a machine is good for ----------

// The busiest card, with how full its memory is.
function busiestGPU(st) {
  const gpus = st.gpus || [];
  if (!gpus.length) return null;
  const g = gpus.reduce((a, b) => ((Number(b.percent) || 0) > (Number(a.percent) || 0) ? b : a));
  return { ...g, memPct: g.memory_total ? (g.memory_used / g.memory_total) * 100 : 0 };
}

// One plain sentence on whether the machine can take more work. The first
// rule that holds wins: memory nearly gone (under 10% or 1 GB free), the
// graphics card busy (half its load or three quarters of its memory), the
// processor busy (80%) or partly busy (50%), memory getting short (under
// 25% free), else free for heavy work.
export function verdict(st) {
  const cpu = Number(st.cpu?.percent) || 0;
  const m = st.memory || {}, free = Number(m.available) || 0;
  const freePct = m.total ? (free / m.total) * 100 : 100;
  const groups = st.groups || [];
  const busiest = [...groups].sort((a, b) => b.cpu_percent - a.cpu_percent)[0]?.name || st.top_cpu?.[0]?.name || "";
  const mostly = busiest ? `, mostly by ${busiest}` : "";
  const g = busiestGPU(st);
  if (freePct < 10 || free < GiB) return { cls: "bad", short: "Low on memory", text: "Low on memory: close something before starting a big job." };
  if (g && (g.percent >= 50 || g.memPct >= 75)) {
    const who = groups.filter((x) => x.gpu).sort((a, b) => (b.gpu_memory || 0) - (a.gpu_memory || 0) || b.cpu_percent - a.cpu_percent)[0]?.name;
    return { cls: "warn", short: "Busy", text: who ? `Busy: ${who} is using the graphics card.` : "Busy: the graphics card is in use." };
  }
  if (cpu >= 80) return { cls: "warn", short: "Busy", text: `Busy: ${pct(cpu)} of the processor is in use${mostly}.` };
  if (cpu >= 50) return { cls: "", short: "Partly busy", text: `Partly busy: ${pct(cpu)} of the processor is in use${mostly}. Small jobs are fine.` };
  if (freePct < 25) return { cls: "", short: "Light work only", text: `Light work only: ${gb(free)} of memory is free.` };
  return { cls: "ok", short: "Free for heavy work", text: `Free for heavy work: ${pct(100 - cpu)} of the processor and ${gb(free)} of memory are spare.` };
}

// A temperature in plain words. NVIDIA cards are warm from 80 °C and hot
// from 88 °C; processors and Apple chips, which run near 90 under load,
// from 85 °C and 95 °C.
export function heat(t, st) {
  const card = (st.gpus || []).some((g) => g.name === t.label);
  const [warm, hot] = card ? [80, 88] : [85, 95];
  const c = Number(t.celsius) || 0;
  const [word, cls] = c >= hot ? ["hot", "bad"] : c >= warm ? ["warm", "warn"] : ["normal", ""];
  const part = card ? "graphics card" : t.label === "CPU" ? "processor" : t.label === "GPU" ? "graphics" : t.label;
  return { c, word, cls, part, over: c - hot };
}
// The sensor nearest its own hot mark.
const hottest = (st) => (st.temps || []).map((t) => heat(t, st)).reduce((a, b) => (!a || b.over > a.over ? b : a), null);

const deg = (h) => `<span class="devtemp ${h.cls}">${Math.round(h.c)} °C</span>`;
const say = (v) => `<span class="room ${v.cls}" title="${esc(v.text)}"><i></i>${esc(v.short)}</span>`;

// macOS lets a user read only their own processes' threads and usage, so
// the thread count, the groups and the busiest lists leave other users'
// processes out.
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
  // Graphics and Temp show only when some device reports them.
  const gpu = D.list.some((d) => d.stats?.gpus?.length), temp = D.list.some((d) => d.stats?.temps?.length);
  const cols = [
    { label: "", w: 16, min: 16, cls: "ic" },
    { label: "Device" },
    { label: "Right now", w: 152 },
    { label: "Processor", w: 120 },
    { label: "Memory", w: 120 },
    gpu && { label: "Graphics", w: 120 },
    temp && { label: "Temp", w: 64, r: true },
  ].filter(Boolean);
  const rows = D.list.map((d) => {
    const st = d.stats || {};
    const k = "dv|" + d.id + "|";
    const sub = d.online
      ? [d.local ? "This machine" : "", platform(st), "up " + uptime(st.uptime_seconds)].filter(Boolean).join(" · ")
      : "Offline · " + sentence(d.error || "no reading");
    const h = d.online && hottest(st);
    return {
      href: "#/devices/" + encodeURIComponent(d.id),
      cells: [
        live(k + "dot", dot(d.online)),
        `<span class="devname"><b class="clamp">${esc(d.name || d.id)}</b>${live(k + "sub", `<span class="clamp ${d.online ? "muted" : "warn"}" title="${esc(sub)}">${esc(sub)}</span>`)}</span>`,
        live(k + "now", d.online ? say(verdict(st)) : ""),
        live(k + "cpu", d.online ? meterCell(st.cpu?.percent) : ""),
        live(k + "mem", d.online ? meterCell(st.memory?.percent) : ""),
        gpu && live(k + "gpu", d.online && st.gpus?.length ? meterCell(busiestGPU(st).percent) : ""),
        temp && live(k + "temp", h ? `<span title="${esc(`Hottest part: ${h.part}, ${h.word}`)}">${deg(h)}</span>` : ""),
      ].filter((c) => c !== false),
    };
  });
  return shell(title, readNote(), table(cols, rows, { fit: true, cls: "devtbl" }));
}

// ---------- one device: the plain view ----------

function tile(label, value, sub, meter) {
  const bar = meter == null ? "" : `<span class="meter"><i style="width:${clamp(meter)}%"></i></span>`;
  return `<div class="stat"><span class="muted">${esc(label)}</span><span class="v">${value}</span>${bar}<span class="s">${sub}</span></div>`;
}

function tiles(st) {
  const cpu = Number(st.cpu?.percent) || 0, m = st.memory || {}, g = busiestGPU(st), h = hottest(st);
  return [
    tile("Processor", `${pct(cpu)}<span class="u">busy</span>`, esc(`${pct(100 - cpu)} spare for new work`), cpu),
    tile("Memory", `${esc(gb(m.used))}<span class="u">of ${esc(gb(m.total))}</span>`, esc(`${gb(m.available)} free for new work`), m.percent),
    g ? tile("Graphics card", `${pct(g.percent)}<span class="u">busy</span>`, esc(`${gb(g.memory_used).slice(0, -3)} of ${gb(g.memory_total)} memory used`), g.percent) : "",
    h ? tile("Temperature", `${Math.round(h.c)} °C<span class="u devtemp ${h.cls}">${h.word}</span>`, esc(`Hottest part: ${h.part}`)) : "",
  ].join("");
}

// The sections of What's running, in order. Unknown kinds count as work;
// the system's section is named for the machine's system.
const KINDS = [["work", "Your work"], ["app", "Apps"], ["remote", "Remote access"], ["removable", "Removable background apps"], ["driver", "Drivers"], ["system", ""]];
const KNOWN = new Set(KINDS.map(([k]) => k));
const SMALL = 16 * 2 ** 20;

function running(d, st) {
  const groups = st.groups;
  if (!groups) return `<div class="empty">This device's proxy is too old to group what is running. Update it to see this.</div>`;
  const cores = Number(st.cpu?.cores) || 1;
  // Columns: chevron, name, processor, memory and, with a card, its memory.
  const cols = ["c ic", "c g", "c r n", "c r n", st.gpus?.length && "c r n gm only-wide"].filter(Boolean);
  const row = (cells, cls, tag = "div", attrs = "", after = "") => `<${tag} class="tr ${cls}" ${attrs}>${cols.map((c, i) => `<span class="${c}">${cells[i] ?? ""}</span>`).join("")}${after}</${tag}>`;
  const head = row(["", "", "Processor", "Memory", "Graphics memory"], "head");

  const body = KINDS.map(([kind, label]) => {
    const list = groups.filter((g) => (KNOWN.has(g.kind) ? g.kind : "work") === kind);
    if (!list.length) return "";
    const k = "dv|run|" + kind + "|";
    const mem = list.reduce((a, g) => a + g.rss, 0);
    const note = kind === "removable" ? `${gb(mem)} could be freed by removing these${st.os === "windows" ? "; the PC's cleanup script does this" : ""}.` : "";
    const name = label || (st.os === "windows" ? "Windows and security" : "System");
    const cat = row(["", `<b>${esc(name)}</b>`, "", live(k + "mem", esc(bytes(mem)))], "devcat", "div", "", note ? live(k + "note", `<span class="devcatnote muted">${esc(note)}</span>`) : "");
    // Groups too small to matter fold into one line when there are a few.
    const keyOf = (g) => d.id + ":" + g.name;
    let small = list.filter((g) => g.rss < SMALL && g.cpu_percent < 0.5 && !D.open.has(keyOf(g)));
    if (small.length < 2) small = [];
    const rest = small.length ? row(["", `<span class="muted">and ${int(small.length)} more using ${esc(bytes(small.reduce((a, g) => a + g.rss, 0)))} in all</span>`], "devmore") : "";
    return cat + list.filter((g) => !small.includes(g)).map((g) => {
      const key = keyOf(g), open = D.open.has(key);
      const id = "devg-" + encodeURIComponent(d.id + "-" + g.name).replace(/[^\w-]/g, "_");
      const gk = "dv|g|" + g.name + "|";
      const btn = row([
        `<span class="chev">${icon("chevronRight", 14)}</span>`,
        `<span class="devgname"><b class="clamp">${esc(g.name)}</b>${g.about ? `<span class="clamp muted">${esc(g.about)}</span>` : ""}</span>`,
        live(gk + "cpu", pct(g.cpu_percent)),
        live(gk + "mem", esc(bytes(g.rss))),
        live(gk + "gpu", g.gpu_memory ? esc(bytes(g.gpu_memory)) : g.gpu ? "In use" : ""),
      ], "click devgrp", "button", `type="button" aria-expanded="${open}" aria-controls="${id}" data-dev-group="${esc(key)}" data-focus-key="${id}"`);
      if (!open) return btn;
      const procs = (g.processes || []).map((p) => row(["", `<span class="clamp">${esc(p.name)}</span><span class="muted" title="Process ID">${p.pid}</span>`, pct(p.cpu_percent / cores), esc(bytes(p.rss))], "devproc")).join("");
      const more = g.count > (g.processes || []).length ? row(["", `<span class="muted">and ${int(g.count - g.processes.length)} smaller</span>`], "devproc") : "";
      return btn + `<div class="devprocs" id="${id}" role="group" aria-label="${esc(g.name)} processes">${region(gk + "procs", procs + more)}</div>`;
    }).join("") + rest;
  }).join("");
  return `<div class="tblwrap devrun"><div class="tbl">${head}${body}</div></div>`;
}

function disks(st) {
  const cols = [{ label: "Drive" }, { label: "Used", w: 160 }, { label: "Free", w: 160, r: true }];
  const rows = (st.disks || []).map((d) => {
    const freePct = d.total ? (d.free / d.total) * 100 : 100;
    const low = freePct < 10 ? `<span class="warn">Almost full: less than 10% free</span>` : "";
    return { cells: [`<span class="devname"><span class="mono clamp">${esc(d.mount)}</span>${low}</span>`, meterCell(100 - freePct), esc(`${bytes(d.free)} free of ${bytes(d.total)}`)] };
  });
  return table(cols, rows, { fit: true, cls: "devtbl", empty: "No drives reported." });
}

// ---------- one device: technical details ----------

function stat(label, value, sub, opts = {}) {
  const bar = opts.meter == null ? "" : `<span class="meter"><i style="width:${clamp(opts.meter)}%"></i></span>`;
  return `<div class="stat"${opts.title ? ` title="${esc(opts.title)}"` : ""}><span class="muted">${esc(label)}</span><span class="v">${value}</span>${bar}<span class="s">${sub}</span></div>`;
}

function statsStrip(st) {
  const cpu = st.cpu || {}, m = st.memory || {}, sw = st.swap || {}, l = st.load;
  return [
    stat("CPU", pct(cpu.percent), esc([`${cpu.cores || 0} logical cores`, cpu.model].filter(Boolean).join(" · ")), { meter: cpu.percent }),
    l ? stat("Load", (Number(l.load1) || 0).toFixed(2), esc(`5 min ${(Number(l.load5) || 0).toFixed(2)} · 15 min ${(Number(l.load15) || 0).toFixed(2)}`)) : "",
    stat("Memory", pct(m.percent), esc(`${bytes(m.used)} of ${bytes(m.total)}`), { meter: m.percent }),
    stat("Swap", sw.total ? pct(sw.percent) : "–", esc(sw.total ? `${bytes(sw.used)} of ${bytes(sw.total)}` : "None"), { meter: sw.total ? sw.percent : 0 }),
    stat("Processes", int(st.processes), esc(int(st.threads) + (partial(st) ? " threads in your processes" : " threads")), { title: partial(st) ? `${int(st.unreadable)} processes belong to other users and cannot be read without root, so their threads are not counted and they are not among the busiest.` : "" }),
    stat("Uptime", esc(uptime(st.uptime_seconds)), esc(platform(st))),
  ].join("");
}

function cores(st) {
  return (st.cpu?.per_core || []).map((p, i) => `<div class="devcore"><span class="muted">${i + 1}</span>${meterCell(p)}</div>`).join("");
}

function gpuTable(st) {
  const cols = [{ label: "Card" }, { label: "Load", w: 128 }, { label: "Memory", w: 128 }, { label: "Used", w: 136, r: true }, { label: "Temp", w: 64, r: true }, { label: "Power", w: 64, r: true }];
  const rows = st.gpus.map((g) => ({
    cells: [
      `<span class="clamp">${esc(g.name)}</span>`,
      meterCell(g.percent),
      meterCell(g.memory_total ? (g.memory_used / g.memory_total) * 100 : 0),
      esc(`${bytes(g.memory_used)} of ${bytes(g.memory_total)}`),
      g.celsius ? deg(heat({ label: g.name, celsius: g.celsius }, st)) : "–",
      g.watts ? `${Math.round(g.watts)} W` : "–",
    ],
  }));
  return table(cols, rows, { fit: true, cls: "devtbl" });
}

function tempTable(st) {
  const rows = st.temps.map((t) => { const h = heat(t, st); return { cells: [`<span class="clamp">${esc(t.label)}</span>`, `${deg(h)}<span class="muted">${h.word}</span>`] }; });
  return table([{ label: "Sensor" }, { label: "Temperature", w: 140, r: true }], rows, {});
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

function technical(st) {
  return `<div id="devtech">
    ${live("dv|one|stats", `<div class="stats devstats">${statsStrip(st)}</div>`)}
    <div class="sech"><div class="t"><b>Cores</b>${live("dv|one|cores-n", `<span class="muted">${(st.cpu?.per_core || []).length} logical</span>`)}</div></div>
    ${live("dv|one|cores", `<div class="devcores">${cores(st)}</div>`)}
    ${st.gpus?.length ? `<div class="sech"><div class="t"><b>GPU</b></div></div>${live("dv|one|gpus", gpuTable(st))}` : ""}
    ${st.temps?.length ? `<div class="sech"><div class="t"><b>Temperatures</b></div></div>${live("dv|one|temps", tempTable(st))}` : ""}
    <div class="sech"><div class="t"><b>Busiest processes</b><span class="muted">Top ${(st.top_cpu || []).length}${partial(st) ? " of your processes" : ""}</span></div></div>
    ${procHead()}${live("dv|one|procs", procRows(st))}
  </div>`;
}

// ---------- one device ----------

function deviceView(id) {
  const d = (D.list || []).find((x) => x.id === id);
  const crumbs = (name, on) => `<div class="crumbs"><a href="#/devices">Devices</a>${icon("chevronRight", 14)}<span class="here">${on == null ? "" : live("dv|one|dot", dot(on))}<span class="clamp">${esc(name)}</span></span></div>`;
  if (!D.list) return shell(crumbs(id), "", `<div class="empty">${D.error ? "" : "Reading the device…"}</div>`);
  if (!d) return shell(crumbs(id), "", `<div class="empty">No such device. Devices come from management.devices in the proxy's config.</div>`);
  if (!d.online) return shell(crumbs(d.name || d.id, false), readNote(), `<div class="empty">${esc(d.local ? "This machine" : d.name)} is offline. ${esc(sentence(d.error || ""))}</div>`);
  const st = d.stats;
  const v = verdict(st);
  // Phones keep the device name and the reading time in the bar.
  const end = `<span class="only-wide">${esc(`${platform(st)} · up ${uptime(st.uptime_seconds)}`)} · </span>${readNote()}`;
  const counted = (st.groups || []).reduce((a, g) => a + g.count, 0);
  const view = shell(crumbs(d.name || d.id, true), end, `
    <div class="sec">
      ${live("dv|one|verdict", `<p class="devverdict ${v.cls}"><i></i><span>${esc(v.text)}</span></p>`)}
      ${live("dv|one|tiles", `<div class="stats devtiles">${tiles(st)}</div>`)}
    </div>
    <div class="sec">
      <div class="sech"><div class="t"><b>What's running</b>${live("dv|one|count", st.groups ? `<span class="muted">${int(counted)} programs in ${int(st.groups.length)} groups</span>` : "")}</div></div>
      ${partial(st) ? `<p class="devcaveat muted">Only your own programs are counted. macOS keeps other users' programs, including much of the system, hidden from this proxy.</p>` : ""}
      ${running(d, st)}
    </div>
    <div class="sec">
      <div class="sech"><div class="t"><b>Disks</b></div></div>
      ${live("dv|one|disks", disks(st))}
    </div>
    <div class="sec last">
      <button type="button" class="sech devtech" aria-expanded="${D.tech}" aria-controls="devtech" data-dev-tech data-focus-key="dev-tech"><span class="t"><span class="chev">${icon("chevronRight", 14)}</span><b>Technical details</b></span></button>
      ${D.tech ? technical(st) : ""}
    </div>`);
  const first = view.mount;
  view.mount = (root) => {
    first();
    root.querySelectorAll("[data-dev-sort]").forEach((b) => {
      b.onclick = () => { D.sort = b.dataset.devSort; rerender(); };
    });
    root.querySelectorAll("[data-dev-group]").forEach((b) => {
      b.onclick = () => {
        if (!D.open.delete(b.dataset.devGroup)) D.open.add(b.dataset.devGroup);
        rerender();
      };
    });
    const tech = root.querySelector("[data-dev-tech]");
    if (tech) tech.onclick = () => { D.tech = !D.tech; rerender(); };
  };
  return view;
}

export function view(ctx) {
  return ctx.params[0] ? deviceView(ctx.params[0]) : listView();
}
