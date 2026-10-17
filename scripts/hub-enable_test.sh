#!/usr/bin/env bash
# Assert based checks for hub-enable.sh, run against temp configs with fake
# keys. Never touches ~/.cli-proxy-api. Run: bash scripts/hub-enable_test.sh
set -euo pipefail

script="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/hub-enable.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"; [[ -z ${server_pid:-} ]] || kill "$server_pid" 2>/dev/null || true' EXIT
pass=0 fail=0

ok() { pass=$((pass + 1)); }
bad() { fail=$((fail + 1)); echo "FAIL: $*" >&2; }
check() { if eval "$2"; then ok; else bad "$1"; fi; }

key1="dev.test-mac.$(printf '%064d' 1)"
key2="dev.test-mac.$(printf '%064d' 2)"
printf '%s\r\n' "$key1" >"$tmp/key1" # CRLF, as a file from the PC may be
printf '%s\n' "$key2" >"$tmp/key2"
printf 'has "quote\n' >"$tmp/badkey"
: >"$tmp/emptykey"

# Shaped like a live Mac config: v8, comments, no upstream api-keys yet.
cat >"$tmp/mac.yaml" <<'EOF'
config-version: 8
# Server
server:
  host: 127.0.0.1
  port: 8317
management:
  secret-key: "mgmt-secret-placeholder" # never touched
access:
  api-keys:
    - "client-key-placeholder"
upstream:
  codex:
    enabled: true
routing:
  strategy: fill-first
  session-affinity: true
  session-affinity-rebind-on: quota
# trailing comment
EOF

proxy="$tmp/no-proxy" # no validator unless a case sets one; never the live binary
run() { # config, extra args...; sets out and rc
  local cfg=$1; shift
  rc=0
  out=$("$script" --config "$cfg" --hub-url http://127.0.0.1:9 --proxy-bin "$proxy" "$@" 2>&1) || rc=$?
}
backups() { ls "$1".bak-hub-* 2>/dev/null | wc -l | tr -d ' '; }
hubs() { grep -c 'name: pc-hub' "$1" || true; }

# 1. Dry run changes nothing and never prints the key.
cfg="$tmp/c1.yaml"; cp "$tmp/mac.yaml" "$cfg"; chmod 600 "$cfg"
run "$cfg" --key-file "$tmp/key1"
check "dry run exits 0" '[[ $rc == 0 ]]'
check "dry run leaves the file alone" 'cmp -s "$cfg" "$tmp/mac.yaml"'
check "dry run says append" '[[ $out == *"would append api-keys.claude"* ]]'
check "dry run hides the key" '[[ $out != *"$key1"* && $out == *"<from $tmp/key1>"* ]]'
check "dry run writes no backup" '[[ $(backups "$cfg") == 0 ]]'

# 2. Apply appends the group, keeps every other line, backs up, mode 600.
run "$cfg" --key-file "$tmp/key1" --apply
check "apply exits 0" '[[ $rc == 0 ]]'
check "apply hides the key" '[[ $out != *"$key1"* ]]'
check "apply backs up once" '[[ $(backups "$cfg") == 1 ]]'
check "backup is the original" 'cmp -s "$(ls "$cfg".bak-hub-*)" "$tmp/mac.yaml"'
check "original lines kept in order" '[[ $(head -n "$(wc -l <"$tmp/mac.yaml")" "$cfg") == "$(cat "$tmp/mac.yaml")" ]]'
check "one pc-hub group" '[[ $(hubs "$cfg") == 1 ]]'
check "key written once" '[[ $(grep -c "api-key: \"$key1\"" "$cfg") == 1 ]]'
check "no CR from the key file" '! grep -q $'"'"'\r'"'"' "$cfg"'
check "priority 100" 'grep -q "^      priority: 100$" "$cfg"'
check "health-check on the key" 'grep -q "^          health-check: true$" "$cfg"'
check "explicit status 0 rule" 'grep -q "^        - status: 0$" "$cfg"'
check "session header forwarded" 'grep -q "X-Claude-Code-Session-Id: \"\$X-Claude-Code-Session-Id\"" "$cfg"'
check "hub url" 'grep -q "base-url: \"http://127.0.0.1:9\"" "$cfg"'
check "template comments not copied" '! grep -q "TestHubEntryForwardsSessionID" "$cfg"'
check "config mode 600" '[[ $(stat -f %Lp "$cfg" 2>/dev/null || stat -c %a "$cfg") == 600 ]]'

# 3. A second apply is a no-op.
before=$(cat "$cfg")
run "$cfg" --key-file "$tmp/key1" --apply
check "second apply exits 0" '[[ $rc == 0 ]]'
check "second apply up to date" '[[ $out == *"up to date"* ]]'
check "second apply writes nothing" '[[ $(cat "$cfg") == "$before" && $(backups "$cfg") == 1 ]]'

# 4. A new key replaces the group in place.
run "$cfg" --key-file "$tmp/key2" --apply
check "rotate exits 0" '[[ $rc == 0 && $out == *"update pc-hub at lines"* ]]'
check "rotate keeps one group" '[[ $(hubs "$cfg") == 1 ]]'
check "rotate swaps the key" '! grep -q "$key1" "$cfg" && grep -q "$key2" "$cfg"'
check "rotate keeps the trailing comment" 'grep -q "^# trailing comment$" "$cfg"'
check "rotate backs up again" '[[ $(backups "$cfg") == 2 ]]'

# 5. Existing api-keys with claude and codex groups: add after the claude list.
cfg="$tmp/c5.yaml"
cat >"$cfg" <<'EOF'
config-version: 8
api-keys:
  # upstream keys
  claude:
    - name: other
      keys:
        - api-key: "other-placeholder"
  codex:
    - name: cx
      keys:
        - api-key: "cx-placeholder"
routing:
  strategy: fill-first
EOF
cp "$cfg" "$tmp/c5.orig"
run "$cfg" --key-file "$tmp/key1" --apply
check "existing claude list exits 0" '[[ $rc == 0 && $out == *"add pc-hub to api-keys.claude"* ]]'
check "pc-hub after the other claude group" '[[ $(grep -n "name: other" "$cfg" | cut -d: -f1) -lt $(grep -n "name: pc-hub" "$cfg" | cut -d: -f1) ]]'
check "pc-hub before codex" '[[ $(grep -n "name: pc-hub" "$cfg" | cut -d: -f1) -lt $(grep -n "^  codex:" "$cfg" | cut -d: -f1) ]]'
check "other groups kept" 'grep -q "other-placeholder" "$cfg" && grep -q "cx-placeholder" "$cfg" && grep -q "# upstream keys" "$cfg"'
check "adds exactly the group lines" '[[ $(( $(wc -l <"$cfg") - $(wc -l <"$tmp/c5.orig") )) == $(grep -vc "^#" "$(dirname "$script")/hub-entry.yaml") ]]'

# 6. pc-hub written by a save with name as a later key is found and updated.
cfg="$tmp/c6.yaml"
cat >"$cfg" <<'EOF'
config-version: 8
api-keys:
  claude:
    - base-url: "http://old.invalid"
      name: pc-hub
      priority: 5
    - name: after
      keys:
        - api-key: "after-placeholder"
EOF
run "$cfg" --key-file "$tmp/key1" --apply
check "later name key exits 0" '[[ $rc == 0 && $out == *"update pc-hub at lines 4-6"* ]]'
check "later name key replaced" '[[ $(hubs "$cfg") == 1 ]] && ! grep -q "old.invalid" "$cfg" && grep -q "after-placeholder" "$cfg"'

# 7. api-keys without claude gets a claude list.
cfg="$tmp/c7.yaml"
printf 'config-version: 8\napi-keys:\n  codex:\n    - name: cx\n      keys:\n        - api-key: "cx-placeholder"\nrouting:\n  strategy: fill-first\n' >"$cfg"
run "$cfg" --key-file "$tmp/key1" --apply
check "no claude exits 0" '[[ $rc == 0 && $out == *"add claude with pc-hub under api-keys"* ]]'
check "claude added under api-keys before routing" '[[ $(grep -n "^  claude:" "$cfg" | cut -d: -f1) -lt $(grep -n "^routing:" "$cfg" | cut -d: -f1) ]]'

# 8. Refusals leave the file alone.
cfg="$tmp/c8.yaml"; printf 'api-keys:\n  - "legacy-client-key"\n' >"$cfg"; cp "$cfg" "$tmp/c8.orig"
run "$cfg" --key-file "$tmp/key1" --apply
check "legacy layout refused" '[[ $rc != 0 && $out == *"not a config-version 8"* ]] && cmp -s "$cfg" "$tmp/c8.orig"'
printf 'config-version: 8\napi-keys:\n- "x"\n' >"$cfg"; cp "$cfg" "$tmp/c8.orig"
run "$cfg" --key-file "$tmp/key1" --apply
check "list api-keys refused" '[[ $rc != 0 && $out == *"refused"* ]] && cmp -s "$cfg" "$tmp/c8.orig"'
printf 'config-version: 8\napi-keys:\n  claude: []\n' >"$cfg"; cp "$cfg" "$tmp/c8.orig"
run "$cfg" --key-file "$tmp/key1" --apply
check "flow list refused" '[[ $rc != 0 && $out == *"refused"* ]] && cmp -s "$cfg" "$tmp/c8.orig"'
cp "$tmp/mac.yaml" "$cfg"
run "$cfg" --key-file "$tmp/badkey" --apply
check "bad key refused" '[[ $rc != 0 && $out == *"unexpected characters"* && $out != *quote* ]] && cmp -s "$cfg" "$tmp/mac.yaml"'
run "$cfg" --key-file "$tmp/emptykey" --apply
check "empty key refused" '[[ $rc != 0 && $out == *"is empty"* ]]'
run "$cfg" --apply
check "missing key file refused" '[[ $rc != 0 && $out == *"--key-file is required"* ]]'
run "$cfg" --key-file "$tmp/key1" --hub-url "http://host/path"
check "url with a path refused" '[[ $rc != 0 && $out == *"no path"* ]]'
ln -s "$cfg" "$tmp/link.yaml"
run "$tmp/link.yaml" --key-file "$tmp/key1"
check "symlinked config refused" '[[ $rc != 0 && $out == *"symlink"* ]]'

# 9. --check: entry state and hub health.
cfg="$tmp/c1.yaml" # holds key2 from step 4
run "$cfg" --key-file "$tmp/key2" --check
check "check sees the entry" '[[ $out == *"entry present and current"* ]]'
check "check fails on a dead hub" '[[ $rc == 1 && $out == *"did not answer 200"* ]]'
run "$cfg" --key-file "$tmp/key1" --check
check "check sees a stale key" '[[ $rc == 1 && $out == *"missing or out of date"* ]]'
check "check never prints the key" '[[ $out != *"$key1"* && $out != *"$key2"* ]]'
if command -v python3 >/dev/null; then
  mkdir "$tmp/www"; echo ok >"$tmp/www/healthz"
  port=$(python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
  python3 -I -m http.server "$port" --bind 127.0.0.1 --directory "$tmp/www" >/dev/null 2>&1 &
  server_pid=$!
  for _ in 1 2 3 4 5 6 7 8 9 10; do curl -s -o /dev/null "http://127.0.0.1:$port/healthz" && break; sleep 0.3; done
  rc=0
  out=$("$script" --config "$tmp/c7.yaml" --hub-url "http://127.0.0.1:$port" --proxy-bin "$proxy" --key-file "$tmp/key1" --check 2>&1) || rc=$?
  check "check passes on a live hub" '[[ $rc == 1 && $out == *"answered 200"* ]]' # c7 has 127.0.0.1:9, so the entry is out of date
  "$script" --config "$tmp/c7.yaml" --hub-url "http://127.0.0.1:$port" --proxy-bin "$proxy" --key-file "$tmp/key1" --apply >/dev/null
  rc=0
  out=$("$script" --config "$tmp/c7.yaml" --hub-url "http://127.0.0.1:$port" --proxy-bin "$proxy" --key-file "$tmp/key1" --check 2>&1) || rc=$?
  check "check exits 0 when entry and hub are good" '[[ $rc == 0 ]]'
else
  echo "skip: python3 missing, live hub check not run"
fi

# The proxy's own saves (SaveConfigPreserveComments) indent by 4 spaces. The
# fixtures below were captured from that save path on 2026-10-17 and trimmed;
# internal/config TestHubEnableOnManagementSavedConfig runs the live path.
ones() { # file: top-level api-keys, claude lists, pc-hub groups (all must be 1)
  [[ $(grep -c '^api-keys:' "$1") == 1 && $(grep -cE '^ +claude:' "$1") == 1 && $(hubs "$1") == 1 ]]
}
loads() { [[ ! -x $proxy ]] || { cp "$1" "$tmp/load.yaml" && "$proxy" check-config "$tmp/load.yaml" >/dev/null 2>&1; }; }
repo="$(cd "$(dirname "$script")/.." && pwd)"
if command -v go >/dev/null && (cd "$repo" && go build -o "$tmp/proxy" ./cmd/server) >/dev/null 2>&1; then
  proxy="$tmp/proxy"
else
  echo "skip: go build failed or go missing, check-config cases use the layout check only"
fi

cat >"$tmp/saved.yaml" <<'EOF'
config-version: 8
upstream:
    codex: {}
routing:
    strategy: fill-first
    session-affinity: true
    session-affinity-rebind-on: quota
    cooldown:
        disable-cooling: false
# trailing comment

access:
    api-keys:
        - "client-key-placeholder"
# Server
server:
    host: 127.0.0.1
    port: 8317
    discovery:
        subtypes:
            - _messages
oauth:
    providers:
        aistudio:
            ws-auth: true
EOF
saved_hub_block=$(cat <<'EOF'
api-keys:
    claude:
        - name: pc-hub
          base-url: "http://127.0.0.1:9"
          priority: 100
          headers:
            X-Claude-Code-Session-Id: "$X-Claude-Code-Session-Id"
          request-scoped-errors:
            - status: 0
              match:
                - "dial tcp"
                - "connection reset"
                - "EOF"
              action: "continue-and-cooldown"
          keys:
            - api-key: "__KEY1__"
              health-check: true
EOF
)

# 10. A saved 4 space config without upstream keys: a 4 space block, then current.
cfg="$tmp/c10.yaml"; cp "$tmp/saved.yaml" "$cfg"
run "$cfg" --key-file "$tmp/key1" --apply
check "saved: apply exits 0" '[[ $rc == 0 && $out == *"append api-keys.claude"* ]]'
check "saved: block at 4 spaces" 'grep -q "^    claude:$" "$cfg" && grep -q "^        - name: pc-hub$" "$cfg" && grep -q "^              health-check: true$" "$cfg"'
run "$cfg" --key-file "$tmp/key1" --apply
check "saved: rerun up to date" '[[ $rc == 0 && $out == *"up to date"* && $(backups "$cfg") == 1 ]]'
check "saved: one of each, loads" 'ones "$cfg" && loads "$cfg"'

# 11. The group as a save rewrites it (block match list, 4 spaces) is current.
cfg="$tmp/c11.yaml"
{ head -n 14 "$tmp/saved.yaml"; printf '%s\n' "${saved_hub_block//__KEY1__/$key1}"; tail -n +15 "$tmp/saved.yaml"; } >"$cfg"
cp "$cfg" "$tmp/c11.orig"
run "$cfg" --key-file "$tmp/key1" --apply
check "saved hub: same key up to date" '[[ $rc == 0 && $out == *"up to date"* ]] && cmp -s "$cfg" "$tmp/c11.orig" && [[ $(backups "$cfg") == 0 ]]'
run "$cfg" --key-file "$tmp/key1" --check
check "saved hub: check sees it current" '[[ $out == *"entry present and current"* ]]'
run "$cfg" --key-file "$tmp/key2" --apply
check "saved hub: new key updates in place" '[[ $rc == 0 && $out == *"update pc-hub"* ]] && ones "$cfg" && grep -q "$key2" "$cfg" && ! grep -q "$key1" "$cfg"'
check "saved hub: update at 8 space dash" 'grep -q "^        - name: pc-hub$" "$cfg"'
run "$cfg" --key-file "$tmp/key2" --apply
check "saved hub: rerun up to date, loads" '[[ $out == *"up to date"* ]] && loads "$cfg"'

# 12. Hand written 4 space file with api-keys but no claude.
cfg="$tmp/c12.yaml"
printf 'config-version: 8\napi-keys:\n    codex:\n        - name: cx\n          keys:\n            - api-key: "cx-placeholder"\nrouting:\n    strategy: fill-first\n' >"$cfg"
run "$cfg" --key-file "$tmp/key1" --apply
check "4 space no claude: adds claude at 4" '[[ $rc == 0 && $out == *"add claude with pc-hub"* ]] && grep -q "^    claude:$" "$cfg" && grep -q "^        - name: pc-hub$" "$cfg"'
check "4 space no claude: before routing, codex kept" '[[ $(grep -n "^    claude:" "$cfg" | cut -d: -f1) -lt $(grep -n "^routing:" "$cfg" | cut -d: -f1) ]] && grep -q "cx-placeholder" "$cfg"'
run "$cfg" --key-file "$tmp/key1" --apply
check "4 space no claude: rerun up to date, loads" '[[ $out == *"up to date"* ]] && ones "$cfg" && loads "$cfg"'

# 13. Hand written 4 space file with a claude list: pc-hub joins it.
cfg="$tmp/c13.yaml"
printf 'config-version: 8\napi-keys:\n    claude:\n        - name: other\n          keys:\n            - api-key: "other-placeholder"\n    codex:\n        - name: cx\n          keys:\n            - api-key: "cx-placeholder"\n' >"$cfg"
run "$cfg" --key-file "$tmp/key1" --apply
check "4 space claude list: pc-hub added at 8" '[[ $rc == 0 && $out == *"add pc-hub to api-keys.claude"* ]] && grep -q "^        - name: pc-hub$" "$cfg"'
check "4 space claude list: before codex" '[[ $(grep -n "name: pc-hub" "$cfg" | cut -d: -f1) -lt $(grep -n "^    codex:" "$cfg" | cut -d: -f1) ]]'
run "$cfg" --key-file "$tmp/key1" --apply
check "4 space claude list: rerun up to date, loads" '[[ $out == *"up to date"* ]] && ones "$cfg" && loads "$cfg"'

# 14. The broken file the old script wrote (a second claude) is refused.
cfg="$tmp/c14.yaml"
{ cat "$tmp/c13.yaml"; printf '  claude:\n    - name: pc-hub\n'; } >"$cfg"; cp "$cfg" "$tmp/c14.orig"
run "$cfg" --key-file "$tmp/key1" --apply
check "duplicate claude refused" '[[ $rc != 0 && $out == *"refused"* ]] && cmp -s "$cfg" "$tmp/c14.orig"'
run "$cfg" --key-file "$tmp/key1" --check
check "check fails on a duplicate claude" '[[ $rc != 0 && $out == *"refused"* ]]'
printf 'config-version: 8\napi-keys:\n    claude:\n        - name: a\napi-keys:\n    codex:\n        - name: b\n' >"$cfg"; cp "$cfg" "$tmp/c14.orig"
run "$cfg" --key-file "$tmp/key1" --apply
check "duplicate api-keys refused" '[[ $rc != 0 && $out == *"twice"* ]] && cmp -s "$cfg" "$tmp/c14.orig"'
printf 'config-version: 8\napi-keys:\n    claude:\n        - name: pc-hub\n        - name: pc-hub\n' >"$cfg"; cp "$cfg" "$tmp/c14.orig"
run "$cfg" --key-file "$tmp/key1" --apply
check "duplicate pc-hub refused" '[[ $rc != 0 && $out == *"pc-hub twice"* ]] && cmp -s "$cfg" "$tmp/c14.orig"'

# 15. With check-config, a merge the proxy cannot load is never swapped in.
if [[ -x $proxy ]]; then
  cfg="$tmp/c15.yaml"
  printf 'config-version: 8\nrouting:\n  session-affinity: not-a-bool\n' >"$cfg"; cp "$cfg" "$tmp/c15.orig"
  run "$cfg" --key-file "$tmp/key1" --apply
  check "validator refuses an unloadable merge" '[[ $rc != 0 && $out == *"proxy cannot load"* ]] && cmp -s "$cfg" "$tmp/c15.orig" && [[ $(backups "$cfg") == 0 ]]'
  check "validator message hides the key" '[[ $out != *"$key1"* ]]'
  before=$(cat "$tmp/c10.yaml")
  run "$tmp/c10.yaml" --key-file "$tmp/key1" --check
  check "check reports the proxy loads it" '[[ $out == *"the proxy loads"* ]]'
  check "validator leaves the checked file alone" '[[ $(cat "$tmp/c10.yaml") == "$before" ]]'
fi

echo "hub-enable_test: $pass passed, $fail failed"
((fail == 0))
