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
  (default)  show what would change; nothing is written
  --apply    back up the config, then add or update the pc-hub group
  --check    report whether the group is current and the hub /healthz answers;
             exits 1 unless both hold
EOF
}

die() { echo "hub-enable: $*" >&2; exit 1; }

apply=0 check=0 key_file="" hub_url="http://jacks-desktop.taila5e4cd.ts.net"
config="$HOME/.cli-proxy-api/config.yaml"
while (($#)); do
  case "$1" in
    --apply) apply=1 ;;
    --check) check=1 ;;
    --key-file) [[ $# -ge 2 ]] || die "--key-file needs a path"; key_file=$2; shift ;;
    --hub-url) [[ $# -ge 2 ]] || die "--hub-url needs a URL"; hub_url=$2; shift ;;
    --config) [[ $# -ge 2 ]] || die "--config needs a path"; config=$2; shift ;;
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

# Puts the group file ($1) into the config on stdin and writes the result to
# stdout and a one line summary of the change to $2. Handles the layouts the
# proxy writes: no api-keys, api-keys without claude, or a block list under
# api-keys.claude with or without pc-hub. Anything else exits 3, unchanged.
merge() {
  awk -v groupfile="$1" -v actionfile="$2" '
    function ind(s) { match(s, /^ */); return RLENGTH }
    function skip(s) { return s ~ /^[[:space:]]*(#.*)?$/ }
    # first line after from that is not blank or a comment and has indent <= limit
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
    function refuse(why) { print "refused: " why > actionfile; exit 3 }
    BEGIN { while ((getline l < groupfile) > 0) group[++gn] = l; close(groupfile) }
    { line[++n] = $0 }
    END {
      for (i = 1; i <= n; i++) if (line[i] ~ /^api-keys:/) { a = i; break }
      if (!a) {
        for (i = 1; i <= n; i++) print line[i]
        print ""; print "api-keys:"; print "  claude:"; emit_group(4)
        print "append api-keys.claude with pc-hub at the end" > actionfile
        exit 0
      }
      if (line[a] !~ /^api-keys:[[:space:]]*(#.*)?$/) refuse("api-keys is not a block mapping")
      ae = next_at(a + 1, 0)
      if (ae <= n && line[ae] ~ /^-/) refuse("api-keys is a list, not the v8 mapping")
      for (i = a + 1; i < ae; i++) {
        if (skip(line[i]) || ind(line[i]) > 2) continue
        if (ind(line[i]) != 2 || line[i] ~ /^  -/) refuse("api-keys is not a 2-space mapping")
        if (line[i] ~ /^  claude:/) { c = i; break }
      }
      if (!c) {
        at = trim_end(ae, a)
        for (i = 1; i < at; i++) print line[i]
        print "  claude:"; emit_group(4)
        for (i = at; i <= n; i++) print line[i]
        print "add claude with pc-hub under api-keys at line " at > actionfile
        exit 0
      }
      if (line[c] !~ /^  claude:[[:space:]]*(#.*)?$/) refuse("api-keys.claude is not a block list")
      ce = next_at(c + 1, 2)
      for (i = c + 1; i < ce; i++) if (!skip(line[i])) { first = i; break }
      if (!first || line[first] !~ /^ *- /) refuse("api-keys.claude is not a block list")
      dash = ind(line[first])
      # pc-hub is the item whose own name field (first or later key) says so
      for (i = c + 1; i < ce; i++) {
        if (line[i] !~ /^ *(- +)?name:[[:space:]]*["\047]?pc-hub["\047]?[[:space:]]*(#.*)?$/) continue
        if (line[i] ~ /^ *- / && ind(line[i]) == dash) { s = i; break }
        if (line[i] ~ /^ *name:/ && ind(line[i]) == dash + 2) {
          for (j = i - 1; j > c; j--) if (line[j] ~ /^ *- / && ind(line[j]) == dash) { s = j; break }
          if (!s) refuse("cannot find the list item holding name: pc-hub")
          break
        }
      }
      if (s) {
        e = trim_end(next_at(s + 1, dash), s)
        for (i = 1; i < s; i++) print line[i]
        emit_group(dash)
        for (i = e; i <= n; i++) print line[i]
        print "update pc-hub at lines " s "-" (e - 1) > actionfile
        exit 0
      }
      at = trim_end(ce, c)
      for (i = 1; i < at; i++) print line[i]
      emit_group(dash)
      for (i = at; i <= n; i++) print line[i]
      print "add pc-hub to api-keys.claude at line " at > actionfile
    }
  '
}

status=0
merge "$work/group" "$work/action" <"$config" >"$work/new" || status=$?
action=$(cat "$work/action" 2>/dev/null || true)
((status == 0)) || die "${action:-could not read $config}; nothing written"

if cmp -s "$config" "$work/new"; then
  echo "pc-hub in $config is up to date"
  current=1
elif ((apply)); then
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

if ((check)); then
  ok=1
  if ((current)); then
    echo "check: pc-hub entry present and current"
  else
    echo "check: pc-hub entry missing or out of date"
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
