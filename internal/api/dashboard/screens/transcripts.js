// Transcripts: upload a voice memo and this proxy's wrapper turns it into a
// speaker-labelled transcript, one job at a time. #/transcripts lists the
// jobs; #/transcripts/<id> shows one. Jobs are read every 2 s while one is
// waiting or running, else every 15 s, and only while this tab is open and
// in view.
import { esc, icon, table, resetLong, toast } from "../core.js";
import { region } from "../paint.js";

const API = "/dashboard/transcriptions";
const EXTS = ["m4a", "mp3", "wav", "ogg", "opus", "webm", "mp4", "aac", "flac"];
const SPEAKER_COLORS = ["var(--chart-1)", "var(--ok-text)", "var(--warn-text)", "#9B6BF2", "var(--chart-5)", "var(--error-text)"];
const STATUS = { queued: "Queued", running: "Transcribing", done: "Done", failed: "Failed" };
const T = { jobs: null, off: false, error: "", timer: 0, busy: false, again: false, job: null, missing: "", upload: null };

const onTab = () => /^#\/transcripts(\/|$)/.test(location.hash);
const openId = () => decodeURIComponent((location.hash.match(/^#\/transcripts\/([^/]+)/) || [])[1] || "");
const redraw = () => window.dispatchEvent(new Event("dash:patch"));
const active = (j) => j?.status === "queued" || j?.status === "running";

function schedule() {
  if (T.timer || !onTab() || document.hidden) return;
  T.timer = setTimeout(poll, (T.jobs || []).some(active) || active(T.job) ? 2000 : 15000);
}

async function getJSON(url) {
  const res = await fetch(url, { cache: "no-store" });
  return { res, body: await res.json().catch(() => ({})) };
}

// One request at a time; one asked for meanwhile follows at once. A job's
// transcript is fetched again only while it is waiting or running, or when
// another job is opened.
async function poll() {
  clearTimeout(T.timer);
  T.timer = 0;
  if (!onTab() || document.hidden) return;
  if (T.busy) { T.again = true; return; }
  T.busy = true;
  try {
    const { res, body } = await getJSON(API);
    T.off = res.status === 404;
    if (!res.ok && !T.off) throw new Error(body.error || `Could not read the transcripts (${res.status})`);
    T.jobs = body.jobs || [];
    const id = openId();
    if (id && !T.off && (T.job?.id !== id || active(T.job))) {
      const one = await getJSON(`${API}/${encodeURIComponent(id)}`);
      T.job = one.res.ok ? one.body : null;
      T.missing = one.res.ok ? "" : id;
    }
    T.error = "";
  } catch (e) {
    T.error = (e.message || String(e)) + (T.jobs ? ". Showing the last reading." : "");
  } finally {
    T.busy = false;
  }
  if (T.again) { T.again = false; return poll(); }
  schedule();
  if (onTab()) redraw();
}

document.addEventListener("visibilitychange", () => { if (!document.hidden) poll(); });
window.addEventListener("hashchange", () => { if (onTab()) poll(); });

// ---------- formatting ----------

function size(n) {
  n = Number(n) || 0;
  return n >= 2 ** 20 ? (n / 2 ** 20).toFixed(1) + " MB" : Math.max(1, Math.round(n / 1024)) + " KB";
}
function span(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  return h ? `${h}h ${String(m).padStart(2, "0")}m` : m ? `${m}m ${String(s).padStart(2, "0")}s` : `${s}s`;
}
// How long a job ran, or has been running; waiting jobs have no time yet.
function elapsed(j) {
  if (!j.started) return "–";
  return span(((j.finished ? Date.parse(j.finished) : Date.now()) - Date.parse(j.started)) / 1000);
}
function stamp(sec) {
  sec = Math.max(0, Math.floor(Number(sec) || 0));
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = String(sec % 60).padStart(2, "0");
  return h ? `${h}:${String(m).padStart(2, "0")}:${s}` : `${m}:${s}`;
}
const dot = (status) => `<i class="txdot ${esc(status)}" role="img" aria-label="${esc(STATUS[status] || status)}"></i>`;
function speakerName(raw) {
  const m = /^SPEAKER_(\d+)$/.exec(raw || "");
  return m ? "Speaker " + (Number(m[1]) + 1) : raw || "Unknown speaker";
}

// Consecutive segments by one speaker read as one turn.
function turns(transcript) {
  const order = new Map();
  const out = [];
  for (const seg of transcript?.segments || []) {
    const text = String(seg.text || "").trim();
    if (!text) continue;
    const who = seg.speaker || "";
    if (!order.has(who)) order.set(who, order.size);
    const last = out[out.length - 1];
    if (last && last.who === who) last.text += " " + text;
    else out.push({ who, start: seg.start, text });
  }
  return { list: out, order };
}

// ---------- list ----------

function uploadForm() {
  const busy = T.upload ? `Uploading ${esc(T.upload.name)}: ${T.upload.pct}%` : "";
  return `<form class="txup" data-tx-form>
    <div class="txdrop">
      <b class="ui strong">Upload a voice memo</b>
      <span class="muted">Drop an audio file here or choose one: ${EXTS.join(", ")}.</span>
      <input type="file" name="file" accept="${EXTS.map((e) => "." + e).join(",")},audio/*" aria-label="Audio file">
    </div>
    <label class="txfield"><span>Speakers</span><input class="input" type="number" name="speakers" min="1" max="20" step="1" placeholder="Auto" inputmode="numeric"></label>
    <label class="txfield"><span>Language</span><input class="input" type="text" name="language" maxlength="3" placeholder="Auto" autocomplete="off" spellcheck="false" title="A two- or three-letter code such as en"></label>
    <button class="btn primary" type="submit" data-tx-go>${icon("plus", 14)}Transcribe</button>
    ${region("tx|up", busy, "span", `class="muted txnote" role="status" aria-live="polite"`)}
  </form>`;
}

function listView() {
  const banner = region("tx|banner", T.error ? `<div class="banner">${esc(T.error)}</div>` : "");
  const head = `<div class="bar titled"><span class="title">Transcripts</span></div>`;
  if (T.off) {
    return { html: `${head}<div class="body">${banner}<div class="empty">Transcription is off on this proxy. Set transcription.enabled and its wrapper command in the proxy's config.</div></div>`, mount };
  }
  const cols = [
    { label: "", w: 16, min: 16, cls: "ic" },
    { label: "Memo" },
    { label: "Status", w: 112 },
    { label: "Took", w: 88, r: true },
    { label: "Size", w: 80, r: true },
    { label: "Added", w: 136, r: true },
  ];
  const rows = (T.jobs || []).map((j) => ({
    href: "#/transcripts/" + encodeURIComponent(j.id),
    cells: [dot(j.status), `<span class="clamp" title="${esc(j.name)}">${esc(j.name)}</span>`, `<span class="${j.status === "failed" ? "warn" : j.status === "done" ? "" : "muted"}">${esc(STATUS[j.status] || j.status)}</span>`, elapsed(j), size(j.size), esc(resetLong(j.created))],
  }));
  const list = T.jobs ? table(cols, rows, { fit: true, empty: "No transcripts yet. Upload a voice memo above." }) : `<div class="empty">Reading the transcripts…</div>`;
  return { html: `${head}<div class="body">${banner}${uploadForm()}${region("tx|list", list)}</div>`, mount };
}

function send(form) {
  const file = form.file.files[0];
  if (!file) return toast("Choose an audio file first", true);
  if (!EXTS.includes(file.name.split(".").pop().toLowerCase())) return toast(`Only ${EXTS.join(", ")} files can be transcribed`, true);
  if (T.upload) return toast("An upload is already running", true);
  const xhr = new XMLHttpRequest();
  T.upload = { name: file.name, pct: 0 };
  redraw();
  xhr.upload.onprogress = (e) => { if (e.lengthComputable) { T.upload.pct = Math.round((e.loaded / e.total) * 100); redraw(); } };
  xhr.onload = () => {
    let body = {};
    try { body = JSON.parse(xhr.responseText); } catch (e) { /* not JSON */ }
    T.upload = null;
    if (xhr.status === 202) {
      form.reset();
      T.jobs = [{ id: body.id, name: file.name, size: file.size, status: "queued", created: new Date().toISOString() }, ...(T.jobs || [])];
      toast(`${file.name} is queued`);
      poll();
    } else {
      toast(body.error || `Upload failed (${xhr.status})`, true);
    }
    redraw();
  };
  xhr.onerror = () => { T.upload = null; toast("Upload failed: the proxy could not be reached", true); redraw(); };
  xhr.open("POST", API);
  xhr.send(new FormData(form));
}

// Runs after every draw, so handlers are assigned, never added.
function mount(root) {
  if (!T.timer && !T.busy) poll();
  const form = root.querySelector("[data-tx-form]");
  if (form) {
    form.onsubmit = (e) => { e.preventDefault(); send(form); };
    form.ondragover = (e) => { e.preventDefault(); form.classList.add("over"); };
    form.ondragleave = (e) => { if (!form.contains(e.relatedTarget)) form.classList.remove("over"); };
    form.ondrop = (e) => {
      e.preventDefault();
      form.classList.remove("over");
      if (e.dataTransfer?.files?.length) form.file.files = e.dataTransfer.files;
    };
  }
  const copy = root.querySelector("[data-tx-copy]");
  if (copy) copy.onclick = async () => {
    try {
      const res = await fetch(`${API}/${encodeURIComponent(copy.dataset.txCopy)}/txt`, { cache: "no-store" });
      if (!res.ok) throw new Error(String(res.status));
      await navigator.clipboard.writeText(await res.text());
      toast("Transcript copied");
    } catch (e) {
      toast("Could not copy the transcript", true);
    }
  };
  const del = root.querySelector("[data-tx-delete]");
  if (del) del.onclick = async () => {
    const id = del.dataset.txDelete;
    if (!confirm(`Delete ${T.job?.name || "this memo"} and its transcripts?`)) return;
    const res = await fetch(`${API}/${encodeURIComponent(id)}`, { method: "DELETE" }).catch(() => null);
    const body = res ? await res.json().catch(() => ({})) : {};
    if (!res?.ok) return toast(body.error || "Could not delete it", true);
    T.job = null;
    T.jobs = (T.jobs || []).filter((j) => j.id !== id);
    toast("Deleted");
    location.hash = "#/transcripts";
  };
}

// ---------- one job ----------

function stat(label, value, sub = "") {
  return `<div class="stat"><span class="muted">${esc(label)}</span><span class="v">${value}</span>${sub ? `<span class="s">${sub}</span>` : ""}</div>`;
}

function detailView(id) {
  const j = T.job?.id === id ? T.job : (T.jobs || []).find((x) => x.id === id);
  const crumbs = `<div class="crumbs"><a href="#/transcripts">Transcripts</a>${icon("chevronRight", 14)}<span class="here">${j ? dot(j.status) : ""}<span class="clamp">${esc(j?.name || id)}</span></span></div>`;
  const banner = region("tx|banner", T.error ? `<div class="banner">${esc(T.error)}</div>` : "");
  const shell = (end, body) => ({ html: `<div class="bar">${crumbs}<div class="end">${end}</div></div><div class="body">${banner}${body}</div>`, mount });
  if (T.off) return shell("", `<div class="empty">Transcription is off on this proxy.</div>`);
  if (!j) return shell("", T.missing === id ? `<div class="empty">No such transcript. It may have been deleted.</div>` : `<div class="empty">Reading the transcript…</div>`);

  const done = j.status === "done" && T.job?.id === id;
  const fmt = (f) => `<a class="btn outline" href="${API}/${encodeURIComponent(id)}/${f}" download>${f.toUpperCase()}</a>`;
  const actions = region("tx|acts", `${done ? `<button class="btn outline" type="button" data-tx-copy="${esc(id)}">${icon("copy", 14, "var(--fg)")}Copy</button>${["txt", "srt", "json"].map(fmt).join("")}` : ""}${j.status !== "running" ? `<button class="iconbtn" type="button" data-tx-delete="${esc(id)}" aria-label="Delete this transcript" title="Delete">${icon("trash")}</button>` : ""}`, "span", `class="row gap8"`);

  const t = done ? turns(T.job.transcript) : null;
  const ahead = (T.jobs || []).filter((x) => active(x) && x.id !== id && Date.parse(x.created) <= Date.parse(j.created)).length;
  const stats = [
    stat("Status", esc(STATUS[j.status] || j.status), j.status === "queued" ? esc(ahead ? `${ahead} ahead of it` : "Next up") : ""),
    stat("Took", elapsed(j), j.started ? esc("Started " + resetLong(j.started)) : ""),
    stat("Audio", size(j.size), esc(resetLong(j.created))),
    stat("Speakers", t ? String(t.order.size) : esc(j.speakers ? String(j.speakers) : "Auto"), t && j.speakers && j.speakers !== t.order.size ? esc(`${j.speakers} asked for`) : ""),
    stat("Language", esc((t && T.job.transcript?.language) || j.language || "Auto")),
  ].join("");

  let main;
  if (j.status === "failed") main = `<div class="banner">${esc(j.error || "The wrapper failed.")}</div>`;
  else if (j.status === "running") main = `<div class="empty">Transcribing… <span class="mono">${esc(T.job?.progress || "")}</span></div>`;
  else if (j.status === "queued") main = `<div class="empty">Waiting for the GPU. One memo is transcribed at a time.</div>`;
  else if (!t) main = `<div class="empty">Reading the transcript…</div>`;
  else if (!t.list.length) main = `<div class="empty">No speech was found in this memo.</div>`;
  else {
    main = `<div class="txturns">${t.list.map((x) => `<div class="txturn" style="--spk:${SPEAKER_COLORS[t.order.get(x.who) % SPEAKER_COLORS.length]}"><div class="who"><b>${esc(speakerName(x.who))}</b><span class="muted mono">${stamp(x.start)}</span></div><p>${esc(x.text)}</p></div>`).join("")}</div>`;
  }
  return shell(actions, `${region("tx|stats", `<div class="stats">${stats}</div>`)}${region("tx|main", main)}`);
}

export function view(ctx) {
  return ctx.params[0] ? detailView(ctx.params[0]) : listView();
}
