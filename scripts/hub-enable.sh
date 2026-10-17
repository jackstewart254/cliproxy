#!/usr/bin/env bash
# Route this Mac's Claude traffic through the PC hub, with the Mac's own logins
# as fallback. Keeps one api-keys.claude group "pc-hub" (scripts/hub-entry.yaml)
# in the proxy config. Dry run unless --apply. The key is read from --key-file
# and never printed. See docs/hub-routing.md.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: hub-enable.sh [--apply] [--check] --key-file <path>
                     [--hub-url http://jacks-desktop.taila5e4cd.ts.net]
                     [--config ~/.cli-proxy-api/config.yaml]
                     [--proxy-bin ~/cliproxy/bin/cliproxy-dash]
  (default)  show what would change; nothing is written
  --apply    back up the config, then add or update the pc-hub group
  --check    report whether the group is current, the config loads and the
             hub /healthz answers; exits 1 unless all hold
  A config the proxy binary (when it has check-config) cannot load is never
  swapped in.
EOF
}

die() { echo "hub-enable: $*" >&2; exit 1; }

apply=0 check=0 key_file="" hub_url="http://jacks-desktop.taila5e4cd.ts.net"
config="$HOME/.cli-proxy-api/config.yaml" proxy_bin="$HOME/cliproxy/bin/cliproxy-dash"
while (($#)); do
  case "$1" in
    --apply) apply=1 ;;
    --check) check=1 ;;
    --key-file) [[ $# -ge 2 ]] || die "--key-file needs a path"; key_file=$2; shift ;;
    --hub-url) [[ $# -ge 2 ]] || die "--hub-url needs a URL"; hub_url=$2; shift ;;
    --config) [[ $# -ge 2 ]] || die "--config needs a path"; config=$2; shift ;;
    --proxy-bin) [[ $# -ge 2 ]] || die "--proxy-bin needs a path"; proxy_bin=$2; shift ;;
    -h | --help) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
  shift
done

[[ -n $key_file ]] || { usage >&2; die "--key-file is required"; }
hub_url=${hub_url%/}
[[ $hub_url =~ ^https?://[A-Za-z0-9.-]+(:[0-9]+)?$ ]] || die "--hub-url must be http(s)://host[:port] with no path"
[[ -f $config && ! -L $config ]] || die "config $config is missing or a symlink"
grep -Eq '^config-version:[[:space:]]*8[[:space:]]*$' "$config" || die "$config is not a config-version 8 file; add the group by hand (docs/hub-routing.md)"
[[ -r $key_file ]] || die "key file $key_file is not readable"
key=$(tr -d '\r\n' <"$key_file")
[[ -n $key ]] || die "key file $key_file is empty"
# Keeps the key safe inside a double-quoted YAML string and in the edits below.
[[ $key =~ ^[A-Za-z0-9._~+/=-]+$ ]] || die "key in $key_file has unexpected characters"

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
template="$here/hub-entry.yaml"
[[ -r $template ]] || die "template $template is missing"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Writes the group with the given key text; bash builtins only, so the key
# never shows up in a process listing.
render() {
  local line
  while IFS= read -r line || [[ -n $line ]]; do
    [[ $line == \#* ]] && continue
    line=${line//__HUB_URL__/$hub_url}
    line=${line//__HUB_KEY__/$1}
    printf '%s\n' "$line"
  done <"$template"
}
render "$key" >"$work/group"
render "<from $key_file>" >"$work/group.masked"

# Reads a config on stdin, writes it with the pc-hub group in place to stdout
# and one line to actionfile: "current" (nothing to change), what it changed,
# or "refused: why" with exit 3 and no output. Indents follow the file: the
# proxy's own saves write 4 spaces, hand edits often 2. The template's dash
# column (4) moves to the file's. A second api-keys, a second claude under it
# or a second pc-hub is refused, so a broken file is never built on.
merge_prog='
  function ind(s) { match(s, /^ */); return RLENGTH }
  function skip(s) { return s ~ /^[[:space:]]*(#.*)?$/ }
  # first line from "from" on that is not blank or a comment, with indent <= limit
  function next_at(from, limit,   j) {
    for (j = from; j <= n; j++) if (!skip(line[j]) && ind(line[j]) <= limit) return j
    return n + 1
  }
  # back up over blank and comment lines that end a section
  function trim_end(end, start) {
    while (end - 1 > start && skip(line[end - 1])) end--
    return end
  }
  function emit_group(dash,   k, g) {
    for (k = 1; k <= gn; k++) {
      g = group[k]
      if (dash > 4) g = sprintf("%" (dash - 4) "s", "") g
      else if (dash < 4) g = substr(g, 5 - dash)
      print g
    }
  }
  function print_lines(from, to,   i) { for (i = from; i <= to; i++) print line[i] }
  # indent step of the file: the first child of a top-level mapping key
  function file_unit(   i, j) {
    for (i = 1; i < n; i++) {
      if (line[i] !~ /^[A-Za-z0-9_-]+:[[:space:]]*(#.*)?$/) continue
      for (j = i + 1; j <= n && skip(line[j]); j++) {}
      if (j <= n && ind(line[j]) > 0 && line[j] !~ /^ *-/) return ind(line[j])
    }
    return 2
  }
  # counts the facts of one group line into cnt, ignoring indent, quotes,
  # comments, list dashes and flow versus block lists
  function canon(s, sign,   k, m, items) {
    if (skip(s)) return
    sub(/[[:space:]]+#.*$/, "", s)
    gsub(/["\047]/, "", s)
    sub(/^[[:space:]]+/, "", s)
    sub(/[[:space:]]+$/, "", s)
    while (s ~ /^-( |$)/) sub(/^-( +|$)/, "", s)
    if (s == "") return
    if (s ~ /^[A-Za-z0-9_-]+: *\[.*\]$/) {
      k = index(s, ":")
      cnt[substr(s, 1, k)] += sign
      s = substr(s, k + 1)
      sub(/^ *\[/, "", s)
      sub(/\] *$/, "", s)
      m = split(s, items, / *, */)
      for (k = 1; k <= m; k++) if (items[k] != "") cnt[items[k]] += sign
      return
    }
    cnt[s] += sign
  }
  function refuse(why) { print "refused: " why > actionfile; exit 3 }
  function done(what) { print what > actionfile; exit 0 }
  BEGIN { while ((getline l < groupfile) > 0) group[++gn] = l; close(groupfile) }
  { line[++n] = $0 }
  END {
    for (i = 1; i <= n; i++) if (line[i] ~ /^api-keys:/) {
      if (a) refuse("api-keys appears twice at the top level")
      a = i
    }
    if (!a) {
      u = file_unit()
      print_lines(1, n)
      print ""
      print "api-keys:"
      printf "%" u "s%s\n", "", "claude:"
      emit_group(2 * u)
      done("append api-keys.claude with pc-hub at the end")
    }
    if (line[a] !~ /^api-keys:[[:space:]]*(#.*)?$/) refuse("api-keys is not a block mapping")
    ae = next_at(a + 1, 0)
    if (ae <= n && line[ae] ~ /^-/) refuse("api-keys is a list, not the v8 mapping")
    for (i = a + 1; i < ae; i++) {
      if (skip(line[i])) continue
      if (!C) C = ind(line[i])
      if (ind(line[i]) < C) refuse("api-keys children are not evenly indented")
      if (ind(line[i]) > C) continue
      if (line[i] ~ /^ *-/) refuse("api-keys is a list, not the v8 mapping")
      if (line[i] ~ /^ *claude:/) {
        if (c) refuse("api-keys has claude twice")
        c = i
      }
    }
    if (!c) {
      if (!C) C = file_unit()
      at = trim_end(ae, a)
      print_lines(1, at - 1)
      printf "%" C "s%s\n", "", "claude:"
      emit_group(2 * C)
      print_lines(at, n)
      done("add claude with pc-hub under api-keys at line " at)
    }
    if (line[c] !~ /^ *claude:[[:space:]]*(#.*)?$/) refuse("api-keys.claude is not a block list")
    ce = next_at(c + 1, C)
    for (i = c + 1; i < ce; i++) if (!skip(line[i])) { first = i; break }
    if (!first || line[first] !~ /^ *- /) refuse("api-keys.claude is not an indented block list")
    D = ind(line[first])
    # pc-hub is the item whose own name field (first or later key) says so
    for (i = c + 1; i < ce; i++) {
      if (skip(line[i])) continue
      if (ind(line[i]) < D) refuse("api-keys.claude items are not evenly indented")
      if (line[i] !~ /^ *(- +)?name:[[:space:]]*["\047]?pc-hub["\047]?[[:space:]]*(#.*)?$/) continue
      st = 0
      if (line[i] ~ /^ *- / && ind(line[i]) == D) st = i
      else if (line[i] ~ /^ *name:/ && ind(line[i]) == D + 2) {
        for (j = i - 1; j > c; j--) if (line[j] ~ /^ *- / && ind(line[j]) == D) { st = j; break }
        if (!st) refuse("cannot find the list item holding name: pc-hub")
      } else continue
      if (s) refuse("api-keys.claude has pc-hub twice")
      s = st
    }
    if (s) {
      e = trim_end(next_at(s + 1, D), s)
      for (i = s; i < e; i++) canon(line[i], 1)
      for (k = 1; k <= gn; k++) canon(group[k], -1)
      same = 1
      for (t in cnt) if (cnt[t] != 0) same = 0
      if (same) {
        print_lines(1, n)
        done("current")
      }
      print_lines(1, s - 1)
      emit_group(D)
      print_lines(e, n)
      done("update pc-hub at lines " s "-" (e - 1))
    }
    at = trim_end(ce, c)
    print_lines(1, at - 1)
    emit_group(D)
    print_lines(at, n)
    done("add pc-hub to api-keys.claude at line " at)
  }
'

# merge <in> <out> <actionfile>: exit 0 merged, 3 refused
merge() { awk -v groupfile="$work/group" -v actionfile="$3" "$merge_prog" <"$1" >"$2"; }

# The proxy binary loads a config the way the server does when it has the
# check-config command (look for its marker; an older binary would start a
# server instead). It may rewrite the file it loads, so it gets a copy.
validator=""
if [[ -x $proxy_bin ]] && LC_ALL=C grep -aqF 'check-config: ok' "$proxy_bin"; then validator=$proxy_bin; fi
vmsg=""
loads() {
  local out
  cp "$1" "$work/validate.yaml"
  if out=$("$validator" check-config "$work/validate.yaml" 2>&1); then return 0; fi
  vmsg=$(printf '%s\n' "$out" | tail -n 1 | cut -c1-240)
  vmsg=${vmsg//$key/<key>}
  return 1
}

status=0
merge "$config" "$work/new" "$work/action" || status=$?
action=$(cat "$work/action" 2>/dev/null || true)
((status == 0)) || die "${action:-could not read $config}; nothing written"

if [[ $action == current ]]; then
  echo "pc-hub in $config is up to date"
  current=1
else
  # The result must read back as one current pc-hub group, and load.
  again=0
  merge "$work/new" "$work/again" "$work/action2" || again=$?
  ((again == 0)) && [[ $(cat "$work/action2") == current ]] && cmp -s "$work/new" "$work/again" ||
    die "the merged config does not read back as current ($(cat "$work/action2" 2>/dev/null)); nothing written"
  if [[ -n $validator ]]; then
    loads "$work/new" || die "the proxy cannot load the merged config: $vmsg; nothing written"
  else
    echo "note: no proxy binary with check-config at $proxy_bin; checked the layout only"
  fi
  if ((apply)); then
    backup="$config.bak-hub-$(date +%Y%m%d-%H%M%S)"
    [[ ! -e $backup ]] || backup="$backup-$$"
    [[ ! -e $backup ]] || die "backup $backup already exists"
    cp -p "$config" "$backup"
    # Same directory, so the rename is atomic; mktemp makes it mode 600.
    next=$(mktemp "$(dirname "$config")/.hub-enable.XXXXXX")
    cat "$work/new" >"$next" && mv -f "$next" "$config" || { rm -f "$next"; die "could not replace $config; the backup is $backup"; }
    echo "applied: $action"
    echo "backup: $backup"
    echo "the proxy reloads the config by itself; see docs/hub-routing.md to verify"
    current=1
  else
    echo "dry run: would $action in $config, after a backup:"
    sed 's/^/  | /' "$work/group.masked"
    echo "run again with --apply to write it"
    current=0
  fi
fi

if ((check)); then
  ok=1
  if ((current)); then
    echo "check: pc-hub entry present and current"
  else
    echo "check: pc-hub entry missing or out of date"
    ok=0
  fi
  if [[ -z $validator ]]; then
    echo "check: no proxy binary with check-config at $proxy_bin; config load not checked"
  elif loads "$config"; then
    echo "check: the proxy loads $config"
  else
    echo "check: the proxy cannot load $config: $vmsg"
    ok=0
  fi
  code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' "$hub_url/healthz" || true)
  if [[ $code == 200 ]]; then
    echo "check: hub $hub_url/healthz answered 200"
  else
    echo "check: hub $hub_url/healthz did not answer 200 (got ${code:-nothing})"
    ok=0
  fi
  ((ok)) || exit 1
fi
