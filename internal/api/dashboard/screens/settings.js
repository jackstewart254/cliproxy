// Settings: connection details, display, and the management key on this device.
import { S, esc, icon, seg, theme, setTheme, askKey, setKey, toast } from "../core.js";

export function view() {
  const server = S.data?.server || {};
  const address = server.address || location.host;
  const tz = S.data?.summary?.timezone || "";
  const city = tz ? tz.split("/").pop().replace(/_/g, " ") : "This device";
  const t = theme();

  const html = `
    <div class="bar titled"><span class="title">Settings</span></div>
    <div class="body">
      <div class="setgrp">
        <h3>Connection</h3>
        <div class="setrow"><div class="k"><b>Address</b><span>Point Claude Code and Codex here instead of their own servers.</span></div><div class="v"><span class="mono">${esc(address)}</span><button class="iconbtn" data-copy aria-label="Copy address">${icon("copy")}</button></div></div>
        <div class="setrow"><div class="k"><b>Host</b><span>Every request leaves from this machine's home connection.</span></div><div class="v">${esc(server.host || "Unknown")}</div></div>
        <div class="setrow"><div class="k"><b>Reachable from</b><span>Other machines cannot open this page.</span></div><div class="v">This machine only</div></div>
        ${server.version ? `<div class="setrow"><div class="k"><b>Build</b><span>The commit this proxy was built from.</span></div><div class="v mono">${esc(server.version)}</div></div>` : ""}
      </div>
      <div class="setgrp">
        <h3>Display</h3>
        <div class="setrow"><div class="k"><b>Theme</b><span>Follow system matches this device, and changes when it does.</span></div><div class="v">${seg([{ id: "light", label: "Light" }, { id: "dark", label: "Dark" }, { id: "system", label: "Follow system" }], t, "data-theme-pick")}</div></div>
        <div class="setrow"><div class="k"><b>Time zone</b><span>Where a day starts for usage, and how reset times are shown. Set on the proxy.</span></div><div class="v">${esc(city)}</div></div>
      </div>
      <div class="setgrp">
        <h3>Management key</h3>
        <div class="setrow"><div class="k"><b>Changes on this device</b><span>Needed to change routing, account modes and sign-ins. Viewing needs no key.</span></div>
          <div class="v">${S.key ? `<span class="muted">Saved on this device</span><button class="btn" data-forget>Forget</button>` : `<button class="btn" data-key>${icon("key", 14, "var(--fg)")}Enter key</button>`}</div></div>
      </div>
    </div>`;
  return {
    html,
    mount(root) {
      const rerender = () => window.dispatchEvent(new Event("dash:render"));
      root.querySelector("[data-copy]").onclick = async () => {
        try { await navigator.clipboard.writeText(address); toast("Address copied"); } catch (e) { toast(address); }
      };
      root.querySelectorAll("[data-theme-pick]").forEach((b) => { b.onclick = () => { setTheme(b.dataset.themePick); rerender(); }; });
      const k = root.querySelector("[data-key]");
      if (k) k.onclick = async () => { if (await askKey()) toast("Key saved on this device"); rerender(); };
      const f = root.querySelector("[data-forget]");
      if (f) f.onclick = () => { setKey(""); toast("Key removed from this device"); rerender(); };
    },
  };
}
