#!/usr/bin/env bash
# test-registry-recovery.sh — prove release.sh recovers from an expired
# mcp-publisher token, and that it does NOT swallow other failures.
#
# The functions are extracted from release.sh rather than re-implemented, so
# this tests the real code. mcp-publisher, gh and curl are stubbed on PATH.

set -uo pipefail
cd "$(dirname "$0")/.."
root=$(pwd)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Extract only the function definitions under test, so this exercises the real
# code. Taking a line range would also pick up the top-level statements between
# them, which would run at eval time.
extract_fn() {
    awk -v fn="$1() {" '
        $0 == fn { inside = 1 }
        inside    { print }
        inside && $0 == "}" { exit }
    ' "$root/scripts/release.sh"
}

eval "$(extract_fn registry_sha_for_version)"
eval "$(extract_fn registry_sha_wait)"
eval "$(extract_fn publish_registry_record)"

# release.sh declares version before these functions use it.
version=0.0.0
export version
export VERSION_UNDER_TEST="$version"
export CURL_CALLS="$work/curl.calls"

# Every function under test must have been found; a rename would otherwise
# make this script pass while testing nothing.
for fn in registry_sha_for_version registry_sha_wait publish_registry_record; do
    declare -F "$fn" >/dev/null || fail "could not extract $fn from release.sh"
done

# --- stub PATH ---------------------------------------------------------------
mkdir -p "$work/bin"

# curl: the registry read. Mutable so tests can simulate replica lag.
cat > "$work/bin/curl" <<'STUB'
#!/usr/bin/env bash
calls_file=${CURL_CALLS:?CURL_CALLS must be set}
n=$(( $(cat "$calls_file" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$calls_file"
# SHA_VISIBLE_AFTER: the read returns empty until this many calls have happened.
visible=${SHA_VISIBLE_AFTER:-1}
if [ "$n" -ge "$visible" ] && [ -n "${SHA_VALUE:-}" ]; then
  printf '{"servers":[{"server":{"version":"%s","packages":[{"fileSha256":"%s"}]}}]}' \
    "${VERSION_UNDER_TEST:-0.0.0}" "$SHA_VALUE"
else
  printf '{"servers":[]}'
fi
STUB
chmod +x "$work/bin/curl"

# gh: mints a token.
cat > "$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
[ "$1" = "auth" ] && [ "$2" = "token" ] && { echo "gho_stub"; exit 0; }
exit 0
STUB
chmod +x "$work/bin/gh"

# mcp-publisher: PUBLISH_MODE drives its behaviour.
#
# login and publish are handled separately: an earlier version of this stub
# applied the publish mode to the login call too, so the re-authentication
# itself failed and the test reported a false negative.
cat > "$work/bin/mcp-publisher" <<'STUB'
#!/usr/bin/env bash
log=${PUBLISH_LOG:-/dev/null}
echo "$*" >> "$log"

# count_pattern always prints exactly one integer.
# `grep -c ... || echo 0` is wrong here: grep already prints 0 when nothing
# matches, so the fallback appends a second line and the arithmetic test
# breaks with "integer expression expected".
count_pattern() {
    local n
    n=$(grep -c "$1" "$log" 2>/dev/null)
    printf '%s' "${n:-0}"
}

case "$1" in
  login)
    token=""
    prev=""
    for arg in "$@"; do
      [ "$prev" = "--token" ] && token=$arg
      prev=$arg
    done
    # An empty token means gh could not mint one, so login must fail.
    if [ -z "$token" ] || [ "${PUBLISH_MODE:-ok}" = "login-fails" ]; then
      echo "device flow failed" >&2
      exit 1
    fi
    echo "✓ Successfully logged in"
    exit 0
    ;;
  publish)
    case "${PUBLISH_MODE:-ok}" in
      ok)
        echo "✓ Successfully published"; exit 0 ;;
      expired-then-ok)
        # The first publish expires; only a login since then lets it succeed.
        if [ "$(count_pattern '^login')" -eq 0 ]; then
          echo "Error: publish failed: server returned status 401: {\"detail\":\"Invalid or expired Registry JWT token\"}" >&2
          echo "failed to parse token: token has invalid claims: token is expired" >&2
          exit 1
        fi
        echo "✓ Successfully published"; exit 0 ;;
      forbidden)
        echo "Error: publish failed: server returned status 403: namespace mismatch" >&2
        exit 1 ;;
      expired-always)
        echo "Error: Invalid or expired Registry JWT token" >&2
        exit 1 ;;
    esac
    ;;
esac
exit 0
STUB
chmod +x "$work/bin/mcp-publisher"

export PATH="$work/bin:$PATH"

run_case() {
  local name=$1 mode=$2 sha=${3:-} lag=${4:-1} login=${5:-yes}
  : > "$work/publish.log"
  export PUBLISH_MODE="$mode" PUBLISH_LOG="$work/publish.log"
  export SHA_VALUE="$sha" SHA_VISIBLE_AFTER="$lag"
  export CURL_CALLS="$work/curl.calls"
  rm -f "$work/curl.calls"

  if [ "$login" = "no" ]; then
    # Make gh unusable so login cannot read a token.
    cat > "$work/bin/gh" <<'S'
#!/usr/bin/env bash
exit 1
S
    chmod +x "$work/bin/gh"
  else
    cat > "$work/bin/gh" <<'S'
#!/usr/bin/env bash
[ "$1" = "auth" ] && [ "$2" = "token" ] && { echo "gho_stub"; exit 0; }
exit 0
S
    chmod +x "$work/bin/gh"
  fi

  out=$(publish_registry_record 2>&1); rc=$?
  logins=$(grep -c '^login' "$work/publish.log" 2>/dev/null || true)
  publishes=$(grep -c '^publish' "$work/publish.log" 2>/dev/null || true)
  logins=${logins:-0}
  publishes=${publishes:-0}

  printf '%s\n' "$name"
  printf '    rc=%s logins=%s publishes=%s\n' "$rc" "$logins" "$publishes"
}

# assert_case runs publish_registry_record and checks the outcome.
#
# The assertions matter: a harness that only prints what happened would have
# reported the expiry recovery as working even when the stub made it fail.
assert_case() {
    local name=$1 mode=$2 want_rc=$3 want_logins=$4 want_publishes=$5

    : > "$work/publish.log"
    export PUBLISH_MODE="$mode" PUBLISH_LOG="$work/publish.log"

    out=$(publish_registry_record 2>&1); local rc=$?
    local logins publishes
    logins=$(count_lines '^login' "$work/publish.log")
    publishes=$(count_lines '^publish' "$work/publish.log")

    if [ "$rc" != "$want_rc" ] || [ "$logins" != "$want_logins" ] \
       || [ "$publishes" != "$want_publishes" ]; then
        printf '  FAIL %s\n' "$name"
        printf '       rc=%s (want %s) logins=%s (want %s) publishes=%s (want %s)\n' \
            "$rc" "$want_rc" "$logins" "$want_logins" "$publishes" "$want_publishes"
        printf '       output: %s\n' "$(printf '%s' "$out" | tr '\n' '|')"
        failures=$((failures + 1))
        return
    fi
    printf '  ok   %s  (rc=%s logins=%s publishes=%s)\n' \
        "$name" "$rc" "$logins" "$publishes"
}

failures=0

# count_lines always prints exactly one integer (see count_pattern above).
count_lines() {
    local n
    n=$(grep -c "$1" "$2" 2>/dev/null)
    printf '%s' "${n:-0}"
}

# A usable gh token unless the case says otherwise.
stub_gh_ok() {
    cat > "$work/bin/gh" <<'S'
#!/usr/bin/env bash
[ "$1" = "auth" ] && [ "$2" = "token" ] && { echo "gho_stub"; exit 0; }
exit 0
S
    chmod +x "$work/bin/gh"
}
stub_gh_fail() {
    cat > "$work/bin/gh" <<'S'
#!/usr/bin/env bash
exit 1
S
    chmod +x "$work/bin/gh"
}

echo "=== publish_registry_record ==="
stub_gh_ok
assert_case "happy path (valid token)"                 ok              0 0 1
assert_case "expired -> re-login -> republish"          expired-then-ok 0 1 2
assert_case "403 namespace mismatch: no retry"         forbidden       1 0 1
assert_case "still expired after re-login: fail"       expired-always  1 1 2
stub_gh_fail
assert_case "re-login fails: surface the failure"      expired-then-ok 1 1 1

# The login must receive the token from gh, not prompt interactively.
: > "$work/publish.log"
stub_gh_ok
export PUBLISH_MODE="expired-then-ok" PUBLISH_LOG="$work/publish.log"
publish_registry_record >/dev/null 2>&1
if grep -q '^login github --token gho_stub$' "$work/publish.log"; then
    printf '  ok   re-login passes the gh token non-interactively\n'
else
    printf '  FAIL re-login did not use the gh token: %s\n' \
        "$(grep '^login' "$work/publish.log" || echo '<no login>')"
    failures=$((failures + 1))
fi

echo
echo "=== registry_sha_wait (replica lag) ==="
export SHA_VALUE="abc123" CURL_CALLS="$work/curl.calls"
for lag in 1 3 6; do
  rm -f "$work/curl.calls"
  export SHA_VISIBLE_AFTER="$lag"
  got=$(registry_sha_wait 6)
  calls=$(cat "$work/curl.calls" 2>/dev/null || echo 0)
  if [ "$got" = "abc123" ]; then
    printf '  ok   lag=%s recovered after %s read(s)\n' "$lag" "$calls"
  else
    printf '  FAIL lag=%s gave up: sha=%s\n' "$lag" "${got:-<empty>}"
    failures=$((failures + 1))
  fi
done

# A record that never appears must be reported, not spun on forever.
export SHA_VALUE=""
rm -f "$work/curl.calls"
got=$(registry_sha_wait 2)
if [ -z "$got" ]; then
  printf '  ok   an absent record eventually returns empty\n'
else
  printf '  FAIL absent record returned %s\n' "$got"
  failures=$((failures + 1))
fi

# Polling must stop as soon as the record is readable.
unset SHA_VISIBLE_AFTER
rm -f "$work/curl.calls"
export SHA_VALUE="abc123"
registry_sha_wait 6 >/dev/null
calls=$(cat "$work/curl.calls" 2>/dev/null || echo 0)
if [ "$calls" -eq 1 ]; then
  printf '  ok   stops polling once the record is visible\n'
else
  printf '  FAIL kept polling: %s reads\n' "$calls"
  failures=$((failures + 1))
fi

printf '\n'
if [ "$failures" -eq 0 ]; then
  echo "all registry-recovery checks passed"
  exit 0
fi
echo "$failures check(s) failed" >&2
exit 1
