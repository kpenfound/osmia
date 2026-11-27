#!/bin/sh
set -eu

fixture=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$fixture/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$fixture"

fail() {
  echo "web-driver fixture check: $*" >&2
  exit 1
}

lever() {
  dagger --workspace "$fixture" call verification-fixture "$@"
}

run() {
  id=$1
  shift
  lever --run-id "$id" verify "$@" export --path "$work/$id" >"$work/$id.out" 2>&1 || {
    cat "$work/$id.out" >&2
    fail "run $id did not complete"
  }
}

png() {
  [ "$(head -c 8 "$1" | od -An -tx1 | tr -d ' \n')" = 89504e470d0a1a0a ]
}

token() {
  grep -ho 'code \[ref=e[0-9]*\]: [0-9a-f]\{16\}' "$1"/snapshots/*.yml | head -1 | sed 's/.*: //'
}

evidence_exists() {
  dir=$1
  jq -e 'all(.claims[].evidence[]; (startswith("/") or contains("..")) | not)' "$dir/report.json" >/dev/null ||
    fail "$dir names an absolute or escaping evidence path"
  jq -r '.claims[].evidence[]' "$dir/report.json" | sort -u | while read -r path; do
    [ -s "$dir/$path" ] || fail "$dir names evidence $path that is missing or empty"
  done
}

grep -Eq 'playwright:v[0-9.]+-[a-z]+@sha256:[0-9a-f]{64}' "$root/modules/web-driver/main.go" ||
  fail "the Playwright image is not pinned by digest"
if grep -Eq '\.Up\(|Tunnel\(' "$root/modules/web-driver/main.go" "$fixture/main.go"; then
  fail "the Service must stay inside the driver container, never published to the host"
fi

flows=$(lever list)
echo "$flows" | jq -e 'type == "array" and length > 0 and all(type == "string")' >/dev/null ||
  fail "list did not return a JSON array of flow names: $flows"
for file in "$fixture"/docs/verification/*.md; do
  [ "$(basename "$file")" = README.md ] && continue
  for section in 'What exists' 'How a user reaches it' 'How to drive it' 'What usually breaks'; do
    grep -q "^## $section\$" "$file" || fail "$file lacks the section $section"
  done
  named=$(awk '/^---$/ {n++; next} n == 1 && /^flows:/ {on = 1; next} n == 1 && /^[a-z]+:/ {on = 0} n == 1 && on && /^  - / {sub(/^  - /, ""); print}' "$file")
  [ -n "$named" ] || fail "$file names no flows"
  for flow in $named; do
    echo "$flows" | jq -e --arg flow "$flow" 'index($flow) != null' >/dev/null ||
      fail "$file names the flow $flow, which list does not return"
  done
done

run flow-a --flow=home-page
run flow-b --flow=home-page
for id in flow-a flow-b; do
  dir=$work/$id
  jq -e '.version == 1 and (.claims | length) > 0 and all(.claims[]; .status == "pass" and (.detail | length) > 0 and .flow == "home-page" and (.evidence | length) > 0)' \
    "$dir/report.json" >/dev/null || fail "the home-page report of $id is not a version 1 report of flow-tagged passing claims"
  evidence_exists "$dir"
  set -- "$dir"/screenshots/*.png
  png "$1" || fail "$id has no PNG screenshot"
  set -- "$dir"/snapshots/*.yml
  grep -q 'ref=e1\]' "$1" || fail "$id has no accessibility snapshot with element refs"
  [ -s "$dir/log.txt" ] || fail "$id has no text log"
done
first=$(token "$work/flow-a")
second=$(token "$work/flow-b")
[ -n "$first" ] && [ -n "$second" ] || fail "an instance token is missing from the snapshots"
[ "$first" != "$second" ] || fail "two run IDs showed the same instance token $first: the service or the browser did not run anew"

run missing '--steps=goto .; click text=No such control; snapshot'
jq -e '([.claims[] | select(.status == "fail" and (.detail | length) > 0)] | length) == 1 and ([.claims[] | select(.status == "pass")] | length) == 1 and all(.claims[]; has("flow") | not)' \
  "$work/missing/report.json" >/dev/null || fail "clicking a missing element did not give one failing claim with a detail"
evidence_exists "$work/missing"

run adhoc '--steps=goto notes; snapshot; fill id=note | Ada%3A first; click e10; snapshot'
jq -e 'all(.claims[]; .status == "pass" and has("flow") == false)' "$work/adhoc/report.json" >/dev/null ||
  fail "the ad-hoc step list did not pass every claim"
evidence_exists "$work/adhoc"
grep -q 'Ada: first' "$work/adhoc"/snapshots/05-snapshot.yml || fail "clicking a snapshot ref did not add the note"

echo "web-driver fixture check passed: tokens $first and $second differ"
