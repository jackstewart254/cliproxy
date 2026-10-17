# Hub routing

Every Mac sends its Claude traffic through the PC proxy (the hub). The Mac's own
logins are the fallback when the hub is down or hung.

## Topology

```
Claude Code on a Mac  (Foundry env in ~/.claude/settings.json, unchanged)
  -> Mac proxy 127.0.0.1:8317
       1. pc-hub: claude API key, priority 100, base-url http://jacks-desktop.taila5e4cd.ts.net
            -> Tailscale Serve on the PC -> PC proxy 127.0.0.1:8317 -> PC logins
       2. the Mac's own Claude logins (rank 1, 2, ...), used when the hub is out
```

- The hub wins while it is up: the rank picker gives logins priorities from
  `count - 1` down, and never touches config API keys, so 100 stays first.
- Each Mac has its own client key on the PC. Revoking it cuts off that Mac only.
- The Mac copies the caller's `X-Claude-Code-Session-Id` to the hub, so the hub
  keeps each Mac session on one PC account. The body's `metadata.user_id` goes
  through as well.
- Health check: `GET /healthz` every 12 s, 3 s timeout. Two failures block the hub
  for all models within 27 s, cancel its in flight requests (they fail over in the
  same request). While blocked, each request of a session bound to the hub is
  served by a local login (fill first keeps it on one) and the binding stays on
  the hub, so the session returns to the hub, and the PC ranking, as soon as
  the block lifts. The block is a cooldown that ends by itself about 27 s after
  the last failed probe, so a stopped checker never strands the hub.
- Unreachable hub: a connect failure matches the `status: 0` rule and cools the
  hub for that model for 60 s; the health check then blocks it for all models.
- Never add a Mac as an upstream on the PC. That makes a loop.

## Mac config snippet

`scripts/hub-enable.sh` keeps this group under `api-keys.claude`; the source is
`scripts/hub-entry.yaml`. Placeholders: `<HUB_URL>`, `<DEVICE_KEY>`.

```yaml
api-keys:
  claude:
    - name: pc-hub
      base-url: "<HUB_URL>"
      priority: 100
      headers:
        X-Claude-Code-Session-Id: "$X-Claude-Code-Session-Id"
      request-scoped-errors:
        - status: 0
          match: ["dial tcp", "connection reset", "EOF"]
          action: "continue-and-cooldown"
      keys:
        - api-key: "<DEVICE_KEY>"
          health-check: true
```

`health-check` sits on the key: an older binary ignores it there, but rejects it
at group level. An omitted `status` matches nothing; the explicit 0 is required.

## Deploy order

PC binary (optional), then PC key, then Mac binary, then Mac config. Do one Mac
at a time.

0. **PC binary (optional).** Only device naming in the PC dashboard needs the PC
   proxy built from this change; routing, keys and failover work with the PC's
   current build. Build it with `GOOS=windows GOARCH=amd64 go build -o
   cliproxy-dash.exe ./cmd/server`, copy it to the PC, keep the running exe as
   `cliproxy-dash.prev.exe`, swap, restart the `CLIProxy` task (stop the exe by
   PID; `Stop-ScheduledTask` alone leaves it running) and expect `/healthz` 200
   through Serve.

1. **PC key.** Copy the script over (`scp scripts/pc/hub-add-device.ps1
   jacks-desktop:cliproxy/`), then on the PC (owner run, or `ssh -t jacks-desktop`):

   ```powershell
   powershell -NoProfile -ExecutionPolicy Bypass -File C:\Users\User\cliproxy\hub-add-device.ps1 -Device macbook-air          # dry run
   powershell -NoProfile -ExecutionPolicy Bypass -File C:\Users\User\cliproxy\hub-add-device.ps1 -Device macbook-air -Apply
   ```

   Contract: `hub-add-device.ps1 -Device <d> [-Apply] [-OutFile <p>] [-Revoke]`.
   Dry run unless `-Apply`. It backs up `C:\Users\User\cliproxy\config.yaml`
   (`.bak-hubdev-<stamp>`), adds the key to `access.api-keys` and writes it,
   readable by this user only, to `-OutFile` when given (that file only; it must
   not exist yet, and its folder must), else to
   `C:\Users\User\cliproxy\hub-keys\<d>.key`. A setup that Taildrops the file
   deletes it afterwards. `-Revoke -Apply` removes the key from the config and
   deletes `hub-keys\<d>.key`. `-Config` and `-KeyDir` override the defaults.
   It refuses any path inside `%USERPROFILE%\.cli-proxy-api`, which holds the logins.
   The PC proxy reloads client keys by itself (watcher, then
   `access.ApplyAccessProviders`); no restart. Device names: `macbook-pro-3`, `macbook-air`.

2. **Key to the Mac**, over Tailscale, never printed (or Taildrop the `-OutFile`):

   ```sh
   scp 'jacks-desktop:cliproxy/hub-keys/macbook-air.key' ~/.cli-proxy-api/hub.key
   chmod 600 ~/.cli-proxy-api/hub.key
   printf 'Authorization: Bearer %s\n' "$(cat ~/.cli-proxy-api/hub.key)" \
     | curl -s -o /dev/null -w '%{http_code}\n' -H @- http://jacks-desktop.taila5e4cd.ts.net/v1/models   # expect 200
   ```

3. **Mac binary** built from `fleet` with this change: build, smoke start it on a
   spare port against a mode 600 copy of the config with an empty auth dir, copy
   the live binary to `.prev`, rename the new one over it, `launchctl kickstart -k
   gui/$(id -u)/com.jackstewart.cliproxy-dash`, expect `/v1/models` 200, else
   restore `.prev`. The kickstart drops in flight requests; run it between turns.
   The old binary ignores `health-check` and `status: 0`: a hung hub would never
   leave rotation. That is why the binary goes first.

4. **Mac config:**

   ```sh
   scripts/hub-enable.sh --key-file ~/.cli-proxy-api/hub.key            # dry run, shows the group with the key masked
   scripts/hub-enable.sh --key-file ~/.cli-proxy-api/hub.key --apply    # backup config.yaml.bak-hub-<stamp>, then write
   scripts/hub-enable.sh --key-file ~/.cli-proxy-api/hub.key --check    # exit 0: entry current and /healthz 200
   grep -E 'claude-api-key count|config successfully reloaded' ~/Library/Logs/cliproxy-dash.log | tail -2
   ```

   The proxy reloads the config by itself. A new chat then logs `new binding`
   with `auth=claude:apikey:...`. Chats open before the change stay on their login.

## Rollback

- **Mac config** (hot reload, keeps the new binary):
  `cp -p "$(ls -t ~/.cli-proxy-api/config.yaml.bak-hub-* | head -1)" ~/.cli-proxy-api/config.yaml`
- **Mac binary:** copy the `.prev` binary over the live one (copy, then rename)
  and kickstart. The hub group is safe on the old binary, so either order works.
- **PC binary:** copy `cliproxy-dash.prev.exe` back over `cliproxy-dash.exe` and
  restart the `CLIProxy` task the same way. Hub sessions then show the PC's host
  name again; keys and routing are unaffected.
- **One Mac's access** (hot reload on the PC):
  `hub-add-device.ps1 -Device macbook-air -Revoke -Apply`, which also deletes
  `hub-keys\macbook-air.key`. That Mac gets 401 from the hub, cools it for
  30 min per model and serves from its own logins.
- Backups hold the same secrets as the live configs. Keep them mode 600, or
  delete them once settled.

## Which device sent a request

On the PC dashboard, open **Sessions**. The machine column shows the device the
client key was issued to: keys read `dev.<device>.<secret>`, and the proxy keeps
only the name. Sessions started on the PC itself show the PC's host name.

- Everything through Tailscale Serve arrives from loopback, and both Macs log in
  as the same Tailscale user, so neither the address nor the Serve identity
  headers can tell the Macs apart. The key can.
- Naming needs the PC proxy built with this change. An older PC binary accepts
  the keys but labels every hub session with the PC's host name.
- The Devices tab (`management.devices`) shows host load of other proxies, not
  who sent a request.
- On a Mac dashboard, hub traffic shows as the `claude:apikey:` credential.

## Limits

- Latency: each request takes one more hop over the tailnet.
- The health check sees the PC proxy and Serve, not the PC's own upstream. A PC
  whose `/healthz` answers while Anthropic stalls is not caught.
- A stream that already sent bytes cannot move. A health block ends it with one
  `event: error`; a stream that got a chunk in the last 10 s is spared until the
  next round.
- All Macs draw on the PC's accounts while the hub is up; local ranking only
  matters in fallback.
