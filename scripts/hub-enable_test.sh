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

run() { # config, extra args...; sets out and rc
  local cfg=$1; shift
  rc=0
  out=$("$script" --config "$cfg" --hub-url http://127.0.0.1:9 "$@" 2>&1) || rc=$?
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
  out=$("$script" --config "$tmp/c7.yaml" --hub-url "http://127.0.0.1:$port" --key-file "$tmp/key1" --check 2>&1) || rc=$?
  check "check passes on a live hub" '[[ $rc == 1 && $out == *"answered 200"* ]]' # c7 has 127.0.0.1:9, so the entry is out of date
  "$script" --config "$tmp/c7.yaml" --hub-url "http://127.0.0.1:$port" --key-file "$tmp/key1" --apply >/dev/null
  rc=0
  out=$("$script" --config "$tmp/c7.yaml" --hub-url "http://127.0.0.1:$port" --key-file "$tmp/key1" --check 2>&1) || rc=$?
  check "check exits 0 when entry and hub are good" '[[ $rc == 0 ]]'
else
  echo "skip: python3 missing, live hub check not run"
fi

echo "hub-enable_test: $pass passed, $fail failed"
((fail == 0))
