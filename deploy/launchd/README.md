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
reason. A device that refuses the key is not asked again until the key in the
config changes, so a wrong key cannot get this host banned there.

On macOS, processes owned by other users (root, system daemons) cannot be read
without root, so they are missing from the busiest-process lists and the
thread count. CPU, memory and room for work still cover the whole machine.

## Adding the Windows PC

The PC's proxy stays loopback-only (127.0.0.1:8317). The Mac reaches it through
an SSH local forward kept alive by launchd.

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
