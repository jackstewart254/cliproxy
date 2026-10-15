# Devices

The dashboard's Devices tab (`/dashboard#/devices`) shows this machine and any
other machine running this proxy: CPU (total and per core), memory, swap,
disks, process and thread counts, the busiest processes, and how much room is
left for more work. It refreshes every 3 seconds while the tab is open and the
page is in view.

Each proxy reports its own host at `GET /v8/management/server/system` (management
key required). The dashboard asks its own server at `/dashboard/devices`
(loopback only, no key), which reads this host and fetches each configured
device itself, so another machine's management key never reaches the browser.
A device that does not answer within 3 seconds shows as offline with the
reason. A device that answers 401 (wrong key) is not asked again until this proxy's
config reloads, so a wrong key cannot get this host banned there. A 403
(banned, management off) shows as offline and is asked again on the next poll.

On macOS, processes owned by other users (root, system daemons) cannot be read
without root, so they are missing from the busiest-process lists and the
thread count. CPU, memory and room for work still cover the whole machine.

GPUs and temperatures come from tools the machine already has, read every
5 seconds within 200 ms; a missing or slow tool leaves its part out:
- NVIDIA cards (load, memory, temperature, power) from `nvidia-smi`.
- macOS CPU and GPU temperatures from `smctemp` (Homebrew
  `narugit/tap/smctemp`, as the /sysmon skill uses; no root needed).
- Windows CPU temperature is not shown. Windows exposes no reliable Ryzen
  temperature without a driver such as LibreHardwareMonitor, which is not
  installed.

## Adding the Windows PC

The PC's proxy stays loopback-only (127.0.0.1:8317). The Mac reaches it through
an SSH local forward kept alive by launchd.

While the tunnel is up, any process on the Mac can use the PC's keyless,
loopback-only dashboard routes at 127.0.0.1:18317: reading `/dashboard/data`
and `/dashboard/devices`, and writing session titles (`/dashboard/sessions`)
and saved views (`/dashboard/views`). The PC also sees the Mac's management
requests as coming from its own loopback address, so the Mac's failed key
attempts and the PC's own local ones count toward the same ban (5 failures,
30 minutes).

1. On the PC, run a build of this proxy that has the Devices feature, with a
   management `secret-key` set. `allow-remote` can stay `false`: the tunnel
   arrives on the PC's loopback address.
2. On the Mac, check the tunnel works by hand (key-based SSH, no prompt):

   ```sh
   ssh -N -o ServerAliveInterval=30 -o ExitOnForwardFailure=yes -L 18317:127.0.0.1:8317 jacks-desktop
   # in another terminal, with the PC's management key:
   curl -H "X-Management-Key: <PC key>" http://127.0.0.1:18317/v8/management/server/system
   ```

3. Install the launchd agent from this repo:

   ```sh
   cp deploy/launchd/com.jackstewart.cliproxy-pc-tunnel.plist ~/Library/LaunchAgents/
   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.jackstewart.cliproxy-pc-tunnel.plist
   ```

   Logs go to `~/Library/Logs/cliproxy-pc-tunnel.log`. To remove it:
   `launchctl bootout gui/$(id -u)/com.jackstewart.cliproxy-pc-tunnel`.

4. Add the PC to the Mac proxy's config under `management` (the legacy layout
   spells it `remote-management`); the proxy picks it up on reload:

   ```yaml
   management:
     devices:
       - name: "jacks-desktop"
         url: "http://127.0.0.1:18317"
         management-key: "<the PC's management secret-key, in plain text>"
   ```
