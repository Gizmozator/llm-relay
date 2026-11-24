#!/usr/bin/env bash
# e2e-federation.sh — full-stack federation smoke test (plan
# 2026-11-22-relay-federation, task 6). Manual operator tool: NOT wired into
# `make ci` (CI stays bounded; this boots real relays and waits on real
# reconnects).
#
# It builds the real binary, starts TWO real relay instances and two mock
# OpenAI engines, all on 127.0.0.1:18xxx, and drives the whole lifecycle:
# enroll spoke → share models → serve (stream + non-stream) → HRW stability
# → offline/restart/disable churn → loop guard → served/consumed ledger on
# both sides → plain-local regression. Every check prints PASS/FAIL; any
# failure exits non-zero. All children die on exit (trap); the workdir is
# kept for post-mortem whenever anything failed.
#
# Usage:  scripts/e2e-federation.sh 2>&1 | tee ~/.cache/llm-relay/fed-e2e.log
set -u
cd "$(dirname "$0")/.."

# ---- topology (localhost only, 18xxx, never a deployed instance) ----------
HUB_RELAY=18080  HUB_ADMIN=18090  HUB_TLS=18443
SPOKE_RELAY=18081 SPOKE_ADMIN=18091
ENG_HUB=18180    ENG_SPOKE=18181
ADMIN_PW='e2e-adminpass'
# The spoke dials the hub's federation channel over TLS (plan
# 2026-11-24-remote-tls-v1): the hub's TLS listener port + its self-signed
# cert fingerprint (pinned as hub_pin). The plain relay URL stays for /v1.
HUB_URL="http://127.0.0.1:${HUB_RELAY}"
HUB_TLS_URL="https://127.0.0.1:${HUB_TLS}"

PASS=0; FAIL=0; FAILED=(); FATAL=0
PIDS=()
WORK=$(mktemp -d "${TMPDIR:-/tmp}/fed-e2e.XXXXXX")

say() { printf '%s\n' "$*"; }
ok()  { PASS=$((PASS+1)); say "PASS  $1"; }
bad() { FAIL=$((FAIL+1)); FAILED+=("$1"); say "FAIL  $1${2:+ — $2}"; }
# die — abort the whole run (setup that cannot go on). Diagnostics go to
# stderr so they never pollute a captured function's stdout.
die() { FATAL=1; say "FATAL: $*" >&2; exit 1; }

CLEANED=0
cleanup() {
  [ "$CLEANED" -eq 1 ] && return
  CLEANED=1
  local p
  # First SIGTERM: relays flush logs and close the WS channel cleanly.
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
  # Bounded wait, then escalate: a wedged child must not hang the script
  # forever (an unwaited `wait` would).
  local deadline=$((SECONDS + 5))
  for p in "${PIDS[@]:-}"; do
    [ -n "$p" ] || continue
    while kill -0 "$p" 2>/dev/null && [ "$SECONDS" -lt "$deadline" ]; do sleep 0.2; done
    if kill -0 "$p" 2>/dev/null; then
      say "note: pid $p ignored SIGTERM — sending SIGKILL"
      kill -9 "$p" 2>/dev/null
    fi
    wait "$p" 2>/dev/null
  done
  # Keep the evidence when anything went wrong; the printed pointers must
  # not point into a deleted directory.
  if [ "$FAIL" -gt 0 ] || [ "$FATAL" -gt 0 ]; then
    say "note: workdir kept for post-mortem: $WORK"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT
# Ctrl-C / kill must END the run after cleanup, not resume it mid-flight.
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

# req METHOD URL [BODY] [TOKEN] → "<body>\n<status>" (status on the last
# line); curl's exit status is preserved for the caller. curl's stderr goes
# to the run log instead of /dev/null so transport errors are visible.
# Tokens ride on the command line (visible in `ps`): acceptable here — this
# is a local-only script and every port is bound to 127.0.0.1.
req() {
  local method=$1 url=$2 body=${3:-} token=${4:-}
  local args=(-sS -m 25 -X "$method" -H 'Content-Type: application/json' -w '\n%{http_code}')
  [ -n "$body" ] && args+=(--data "$body")
  [ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
  curl "${args[@]}" "$url" 2>>"$WORK/curl.err"
}
code_of() { printf '%s' "$1" | tail -n1; }
body_of() { printf '%s' "$1" | sed '$d'; }

# api METHOD URL OUT_VAR [BODY] [TOKEN] → status in $REPLY_CODE, body in $OUT_VAR.
# A transport failure is reported as REPLY_CODE=000 with an explanatory body,
# never as an empty reply that later jq calls would silently swallow.
api() {
  local method=$1 url=$2 out=$3 body=${4:-} token=${5:-}
  local r rc
  r=$(req "$method" "$url" "$body" "$token"); rc=$?
  if [ "$rc" -ne 0 ]; then
    REPLY_CODE=000
    printf -v "$out" '%s' "curl transport error (exit $rc; see $WORK/curl.err)"
    return 0
  fi
  REPLY_CODE=$(code_of "$r")
  printf -v "$out" '%s' "$(body_of "$r")"
}

# extract OUT JQ_EXPR BODY [SECRET] — jq -er extraction into $OUT; FATAL when
# the result is empty/null/error, so a failed API call never rides on as an
# empty id inside a later URL or JSON body. Pass SECRET (any non-empty word)
# for credential responses (keys, tokens): on failure the body is WITHHELD
# from the log — only the jq error is printed — so a partial secret can never
# leak into the tee'd transcript.
extract() {
  local out=$1 expr=$2 body=$3 secret=${4:-} val err
  if val=$(printf '%s' "$body" | jq -er "$expr" 2>/dev/null) && [ -n "$val" ]; then
    printf -v "$out" '%s' "$val"
    return 0
  fi
  err=$(printf '%s' "$body" | jq -er "$expr" 2>&1 >/dev/null)
  [ -n "$err" ] || err='empty or null result'
  if [ -n "$secret" ]; then
    die "extract [$expr] found nothing (body withheld — credential response); jq: $err"
  fi
  die "extract [$expr] found nothing; body: $(printf '%s' "$body" | head -c 300); jq: $err"
}

# check NAME JQ_EXPR BODY — PASS when jq -e holds, else FAIL with the body tail
check() {
  local name=$1 expr=$2 body=$3
  if printf '%s' "$body" | jq -e "$expr" >/dev/null 2>&1; then
    ok "$name"
  else
    bad "$name" "jq [$expr] failed; body: $(printf '%s' "$body" | head -c 300)"
  fi
}

# expect NAME WANT — assert $REPLY_CODE of the last api call. A mutating call
# (POST/PATCH/PUT) must answer the code it promises: a 4xx/5xx whose error
# body happens to survive a later extract/check must not sail through.
expect() {
  local name=$1 want=$2
  if [ "$REPLY_CODE" = "$want" ]; then
    ok "$name"
  else
    bad "$name" "HTTP $REPLY_CODE, want $want; body: $(printf '%s' "${b:-}" | head -c 200)"
  fi
}

# wait_for DESC LIMIT_SECS CMD... — poll every 0.5 s until the command exits 0
wait_for() {
  local desc=$1 limit=$2; shift 2
  local deadline=$((SECONDS + limit))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if "$@" >/dev/null 2>&1; then ok "$desc"; return 0; fi
    sleep 0.5
  done
  bad "$desc" "not satisfied within ${limit}s"
  return 1
}

login() { # login ADMIN_URL → token on stdout; stdout stays EMPTY on failure
  local url=$1 r body tok
  r=$(req POST "$url/api/auth/login" "{\"username\":\"admin\",\"password\":\"$ADMIN_PW\"}")
  body=$(body_of "$r")
  # stderr: our stdout is captured by the caller (HUB=$(login ...)), so a
  # diagnostic printed with say() would become the "token".
  # jq -er prints "null" and exits 1 when .token is absent/null (a JSON error
  # body), so capture, reject empty AND "null", and print nothing on stdout.
  tok=$(printf '%s' "$body" | jq -er .token 2>/dev/null) || tok=""
  if [ -z "$tok" ] || [ "$tok" = "null" ]; then
    say "login at $url failed (HTTP $(code_of "$r")): $body" >&2
  else
    printf '%s' "$tok"
  fi
}

# start_relay NAME RELAY_PORT ADMIN_PORT DB TLS_PORT → pid in $RELAY_PID.
# TLS_PORT=0 disables the TLS listener (the spoke needs none: it only dials
# out). Called plainly, never inside $(...): the relay must be a child of
# THIS shell or the cleanup trap's `wait` cannot reap it.
start_relay() {
  local name=$1 rp=$2 ap=$3 db=$4 tls=$5
  LISTEN_ADDR="127.0.0.1:$rp" ADMIN_ADDR="127.0.0.1:$ap" DB_PATH="$db" \
  TLS_PORT="$tls" \
  JWT_SECRET="e2e-secret-not-for-production-xx" \
  ADMIN_USERNAME=admin ADMIN_PASSWORD="$ADMIN_PW" \
    "./llm-relay" >"$WORK/$name.log" 2>&1 &
  RELAY_PID=$!
}

# hub_catalog OUT — GET the hub /v1/models; succeeds ONLY on HTTP 200 with a
# well-formed .data array. A dead hub, a 5xx or a non-JSON error page must
# never be mistaken for "the model list is empty".
hub_catalog() {
  local r code body
  r=$(req GET "$HUB_URL/v1/models" "" "$HUB_KEY")
  code=$(code_of "$r")
  [ "$code" = "200" ] || return 1
  body=$(body_of "$r")
  printf '%s' "$body" | jq -e '.data | type == "array"' >/dev/null 2>&1 || return 1
  printf -v "$1" '%s' "$body"
}
# hub_has/hub_lacks MODEL — membership predicates over a PROVEN-valid catalog.
hub_has()   { local c; hub_catalog c && printf '%s' "$c" | jq -e --arg m "$1" 'any(.data[]; .id == $m)' >/dev/null; }
hub_lacks() { local c; hub_catalog c && printf '%s' "$c" | jq -e --arg m "$1" '(any(.data[]; .id == $m) | not)' >/dev/null; }

say "== federation e2e — workdir $WORK =="

# ---- preflight: every port must be free ------------------------------------
# A foreign listener on 18xxx would make relays fail to bind (or worse, let
# checks talk to the wrong instance). Fail loudly up front, naming the port.
for p in "$HUB_RELAY" "$HUB_ADMIN" "$HUB_TLS" "$SPOKE_RELAY" "$SPOKE_ADMIN" "$ENG_HUB" "$ENG_SPOKE"; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
    die "preflight: 127.0.0.1:$p is already in use — free it before running the e2e"
  fi
done

# ---- build + boot ---------------------------------------------------------
say '-- building the real binary (make build)'
if ! make build >"$WORK/build.log" 2>&1; then
  say "make build failed:" >&2; tail -20 "$WORK/build.log" >&2
  die "make build failed"
fi

command -v python3 >/dev/null 2>&1 || die "python3 not found — the mock engines need it"
python3 scripts/mock-engine.py "$ENG_SPOKE" mock-a mock-b >"$WORK/eng-spoke.log" 2>&1 &
PIDS+=($!); ENG_SPOKE_PID=$!
python3 scripts/mock-engine.py "$ENG_HUB" mock-hub >"$WORK/eng-hub.log" 2>&1 &
PIDS+=($!); ENG_HUB_PID=$!
for ep in "$ENG_HUB" "$ENG_SPOKE"; do
  wait_for "mock engine :$ep answers /v1/models" 15 \
    curl -sf -m 2 "http://127.0.0.1:$ep/v1/models" || {
      say "engine log tails:" >&2; tail -n 20 "$WORK"/eng-*.log >&2
      die "mock engine :$ep did not start"
    }
done

start_relay hub "$HUB_RELAY" "$HUB_ADMIN" "$WORK/hub.db" "$HUB_TLS"; HUB_PID=$RELAY_PID; PIDS+=("$HUB_PID")
start_relay spoke "$SPOKE_RELAY" "$SPOKE_ADMIN" "$WORK/spoke.db" 0; SPOKE_PID=$RELAY_PID; PIDS+=("$SPOKE_PID")

for hp in "$HUB_ADMIN" "$SPOKE_ADMIN"; do
  wait_for "relay admin :$hp answers /healthz" 30 \
    curl -sf -m 2 "http://127.0.0.1:$hp/healthz" || exit 1
done
wait_for "hub relay :$HUB_RELAY answers /healthz" 30 \
  curl -sf -m 2 "http://127.0.0.1:$HUB_RELAY/healthz" || exit 1

HUB=$(login "http://127.0.0.1:$HUB_ADMIN")
SPOKE=$(login "http://127.0.0.1:$SPOKE_ADMIN")
if [ -z "$HUB" ] || [ -z "$SPOKE" ]; then
  die "admin login failed (relay logs in $WORK)"
fi
say '-- both relays up, admins logged in'

# ---- wiring: hub role, backends, sharing, enrollment ----------------------
api PUT "http://127.0.0.1:$HUB_ADMIN/api/admin/federation" b '{"role":"hub"}' "$HUB"
expect 'setup: hub role PUT accepted' 200
check 'setup: hub role accepted' '.role == "hub"' "$b"

api POST "http://127.0.0.1:$HUB_ADMIN/api/admin/backends" b \
  "{\"name\":\"hub-engine\",\"base_url\":\"http://127.0.0.1:$ENG_HUB\"}" "$HUB"
expect 'setup: hub backend created' 201
extract HUB_BE '.id' "$b"
api POST "http://127.0.0.1:$HUB_ADMIN/api/admin/backends/$HUB_BE/models" b \
  '{"model_ids":["mock-hub"]}' "$HUB"
expect 'setup: hub models imported' 201

api POST "http://127.0.0.1:$SPOKE_ADMIN/api/admin/backends" b \
  "{\"name\":\"spoke-engine\",\"base_url\":\"http://127.0.0.1:$ENG_SPOKE\"}" "$SPOKE"
expect 'setup: spoke backend created' 201
extract SPOKE_BE '.id' "$b"
api POST "http://127.0.0.1:$SPOKE_ADMIN/api/admin/backends/$SPOKE_BE/models" b \
  '{"model_ids":["mock-a","mock-b"]}' "$SPOKE"
expect 'setup: spoke models imported' 201
api GET "http://127.0.0.1:$SPOKE_ADMIN/api/admin/models" b '' "$SPOKE"
SPOKE_MODELS=$b   # keep: the PATCH responses overwrite $b each round
for m in mock-a mock-b; do
  extract MID ".models[] | select(.name==\"$m\") | .id" "$SPOKE_MODELS"
  api PATCH "http://127.0.0.1:$SPOKE_ADMIN/api/admin/models/$MID" b \
    '{"fed_visibility":"all"}' "$SPOKE"
  expect "setup: model PATCH shares $m" 200
done
api GET "http://127.0.0.1:$SPOKE_ADMIN/api/admin/models" b '' "$SPOKE"
check 'setup: spoke shares mock-a and mock-b' \
  '[.models[] | select(.fed_visibility=="all") | .name] | sort == ["mock-a","mock-b"]' "$b"

api POST "http://127.0.0.1:$HUB_ADMIN/api/keys" b '{"name":"e2e-client"}' "$HUB"
expect 'setup: hub client key created' 201
extract HUB_KEY '.key' "$b" secret
api POST "http://127.0.0.1:$HUB_ADMIN/api/peers" b '{"name":"e2e-spoke"}' "$HUB"
expect 'setup: peer enrolled' 201
extract FED_TOKEN '.token' "$b" secret
extract PEER_ID '.peer.id' "$b" secret

# Generate the hub's self-signed TLS cert (plan 2026-11-24-remote-tls-v1):
# the spoke dials the hub's TLS listener and pins this fingerprint. The
# pinned dialer skips hostname checks, so 127.0.0.1 as the SAN is enough.
api POST "http://127.0.0.1:$HUB_ADMIN/api/admin/tls" b '{"hosts":["127.0.0.1"]}' "$HUB"
expect 'setup: hub TLS cert generated' 200
extract HUB_PIN '.tls.fingerprint' "$b"
check 'setup: hub cert fingerprint present' '.tls.configured == true and (.tls.fingerprint|startswith("sha256:"))' "$b"

api PUT "http://127.0.0.1:$SPOKE_ADMIN/api/admin/federation" b \
  "{\"role\":\"spoke\",\"hub_url\":\"$HUB_TLS_URL\",\"hub_token\":\"$FED_TOKEN\",\"hub_pin\":\"$HUB_PIN\"}" "$SPOKE"
expect 'setup: spoke role PUT accepted' 200
wait_for 'enroll: spoke channel connects to the hub' 30 \
  bash -c "curl -s -H 'Authorization: Bearer $SPOKE' \
    http://127.0.0.1:$SPOKE_ADMIN/api/admin/federation | jq -e '.spoke.connected == true'"

# ---- a: spoke models materialize on the hub -------------------------------
# NB: membership must use any(); `jq -e '.data[]? .== x'` exits on the LAST
# output only, which silently fails (or worse, passes) on multi-row lists.
wait_for 'a: mock-a materializes in the hub catalog' 30 hub_has mock-a
api GET "http://127.0.0.1:$HUB_RELAY/v1/models" b '' "$HUB_KEY"
check 'a: mock-b materializes too' 'any(.data[]; .id == "mock-b")' "$b"

# ---- b: hub user runs the spoke model (non-stream + stream) ----------------
api POST "http://127.0.0.1:$HUB_RELAY/v1/chat/completions" b \
  '{"model":"mock-a","messages":[{"role":"user","content":"hi"}]}' "$HUB_KEY"
check 'b: non-stream chat served by the spoke' \
  '.choices[0].message.content == "mock reply from mock-a" and .usage.prompt_tokens == 10' "$b"

STREAM=$(curl -sS -N -m 25 "http://127.0.0.1:$HUB_RELAY/v1/chat/completions" \
  -H "Authorization: Bearer $HUB_KEY" -H 'Content-Type: application/json' \
  --data '{"model":"mock-a","messages":[{"role":"user","content":"hi"}],"stream":true}' 2>>"$WORK/curl.err")
if printf '%s' "$STREAM" | grep -q 'data:' && printf '%s' "$STREAM" | grep -q '\[DONE\]' \
   && printf '%s' "$STREAM" | grep -q 'stream from mock-a'; then
  ok 'b: SSE stream chunks arrive through the tunnel'
else
  bad 'b: SSE stream chunks arrive through the tunnel' "stream body: $(printf '%s' "$STREAM" | head -c 300)"
fi
# Metering lands asynchronously (the recorder batches); wait on the counter
# itself instead of a fixed sleep — fast on a good run, honest on a slow one.
metered_on_hub() {
  api GET "http://127.0.0.1:$HUB_ADMIN/api/admin/usage/detail?page_size=20" b '' "$HUB"
  printf '%s' "$b" | jq -e \
    '[.rows[] | select(.model_requested=="mock-a") | select(.prompt_tokens==10)] | length >= 2' >/dev/null
}
wait_for 'b: both spoke calls metered on the hub (usage counted)' 15 metered_on_hub

# ---- d: spoke process dies → models vanish from the hub --------------------
# hub_lacks proves the catalog request itself succeeded (200 + .data array)
# before asserting absence — a hub outage must FAIL the check, not pass it.
kill "$SPOKE_PID" 2>/dev/null; wait "$SPOKE_PID" 2>/dev/null
wait_for 'd: spoke offline → mock-a disappears from /v1/models' 30 hub_lacks mock-a

# ---- e: spoke restarts → models return; peer disable drops the channel ----
start_relay spoke "$SPOKE_RELAY" "$SPOKE_ADMIN" "$WORK/spoke.db" 0; SPOKE_PID=$RELAY_PID; PIDS+=("$SPOKE_PID")
wait_for 'e: restarted spoke answers /healthz' 30 \
  curl -sf -m 2 "http://127.0.0.1:$SPOKE_ADMIN/healthz" || exit 1
SPOKE=$(login "http://127.0.0.1:$SPOKE_ADMIN")
[ -n "$SPOKE" ] || die "spoke login after restart failed"
wait_for 'e: spoke restart → mock-a returns to the hub catalog' 60 hub_has mock-a
api PATCH "http://127.0.0.1:$HUB_ADMIN/api/admin/peers/$PEER_ID" b '{"enabled":false}' "$HUB"
expect 'setup: peer disable accepted' 200
wait_for 'e: peer disabled via admin API → channel drops (models disabled)' 30 hub_lacks mock-a
api PATCH "http://127.0.0.1:$HUB_ADMIN/api/admin/peers/$PEER_ID" b '{"enabled":true}' "$HUB"
expect 'setup: peer re-enable accepted' 200
wait_for 'e: peer re-enabled → spoke reconnects' 60 hub_has mock-a

# ---- c: HRW stability — one session pins one member of the fed group ------
api GET "http://127.0.0.1:$HUB_ADMIN/api/admin/models" b '' "$HUB"
extract MA '.models[] | select(.name=="mock-a") | .id' "$b"
extract MB '.models[] | select(.name=="mock-b") | .id' "$b"
api POST "http://127.0.0.1:$HUB_ADMIN/api/admin/groups" b '{"name":"fedpool"}' "$HUB"
expect 'setup: fed group created' 201
extract GROUP_ID '.id' "$b"
api PATCH "http://127.0.0.1:$HUB_ADMIN/api/admin/groups/$GROUP_ID" b \
  "{\"members\":[{\"model_id\":$MA,\"weight\":1},{\"model_id\":$MB,\"weight\":1}],\"enabled\":true}" "$HUB"
expect 'setup: fed group members PATCH' 200
check 'c: fed group with two synthetic members enabled' '.enabled == true' "$b"
for i in 1 2 3 4 5; do
  api POST "http://127.0.0.1:$HUB_RELAY/v1/chat/completions" b \
    '{"model":"fedpool","messages":[{"role":"user","content":"hi"}]}' "$HUB_KEY"
  # the group member answers with its own model name ($b is already the body)
  printf '%s' "$b" | jq -er '.model' >/dev/null 2>&1 \
    || bad "c: group call $i failed" "HTTP $REPLY_CODE; body: $(printf '%s' "$b" | head -c 200)"
  sleep 0.3
done
# Same as b: wait on the metered rows instead of a fixed sleep. The
# non-null guard matters: five null upstreams would otherwise be
# "exactly one unique member" and pass vacuously.
hrw_pinned() {
  api GET "http://127.0.0.1:$HUB_ADMIN/api/admin/usage/detail?page_size=30" b '' "$HUB"
  printf '%s' "$b" | jq -e \
    '[.rows[] | select(.model_requested=="fedpool") | .model_upstream]
     | (length >= 5) and (all(. != null)) and ((unique | length) == 1)' >/dev/null
}
wait_for 'c: same session → same member across 5 group calls' 15 hrw_pinned

# ---- f: loop guard — spoke-origin traffic never bounces back ----------------
api POST "http://127.0.0.1:$SPOKE_ADMIN/api/keys" b '{"name":"e2e-spoke-client"}' "$SPOKE"
expect 'setup: spoke client key created' 201
extract SPOKE_KEY '.key' "$b" secret
START=$SECONDS
api POST "http://127.0.0.1:$SPOKE_RELAY/v1/chat/completions" b \
  '{"model":"mock-a","messages":[{"role":"user","content":"hi"}]}' "$SPOKE_KEY"
check 'f: spoke user requesting its own shared model is served locally' \
  '.choices[0].message.content == "mock reply from mock-a"' "$b"
[ $((SECONDS - START)) -lt 15 ] && ok 'f: no loop-back hang on the local request' \
  || bad 'f: no loop-back hang on the local request' "took $((SECONDS - START))s"
# fedpool exists only on the hub and its members are the spoke's own models —
# the hub never echoes them to their origin, so the spoke cannot route a
# request back through the hub to itself: a plain unknown-model 404 (a 403
# from the hub gate would be equally guard-honest), and it must be fast.
api POST "http://127.0.0.1:$SPOKE_RELAY/v1/chat/completions" b \
  '{"model":"fedpool","messages":[{"role":"user","content":"hi"}]}' "$SPOKE_KEY"
case "$REPLY_CODE" in
  404|403) ok 'f: spoke request for a spoke-hosted-via-hub group → no loop (HTTP '"$REPLY_CODE"')' ;;
  *)       bad 'f: spoke request for a spoke-hosted-via-hub group' "HTTP $REPLY_CODE, want 404/403" ;;
esac

# ---- g: served/consumed ledger on BOTH sides -------------------------------
# Traffic so far: 2 direct mock-a calls + 5 group calls = 7 spoke-served.
# (The ≥7 checks fail closed: a missing admin/peer entry makes jq compare
# null >= 7, which is false.)
api GET "http://127.0.0.1:$HUB_ADMIN/api/admin/federation/stats" b '' "$HUB"
check 'g: hub ledger — admin consumed ≥ 7 requests' \
  '[.users[] | select(.user=="admin")][0].consumed.requests >= 7' "$b"
check 'g: hub ledger — mirror served ≥ 7 requests' \
  '[.users[] | select(.user=="admin")][0].served.requests >= 7' "$b"
check 'g: hub ledger — peer e2e-spoke served ≥ 7 requests' \
  '[.peers[] | select(.peer=="e2e-spoke")][0].served.requests >= 7' "$b"
extract HUB_CONSUMED_BEFORE '[.users[] | select(.user=="admin")][0].consumed.requests' "$b"
api GET "http://127.0.0.1:$SPOKE_ADMIN/api/admin/federation/stats" b '' "$SPOKE"
check 'g: spoke ledger — served_by_source names the hub user' \
  '[.served_by_source[] | select(.fed_user=="admin")][0].counts.requests >= 7' "$b"

# ---- h: plain local hub traffic stays out of the ledger --------------------
api POST "http://127.0.0.1:$HUB_RELAY/v1/chat/completions" b \
  '{"model":"mock-hub","messages":[{"role":"user","content":"hi"}]}' "$HUB_KEY"
check 'h: plain local hub request served' '.choices[0].message.content == "mock reply from mock-hub"' "$b"
# A deliberate fixed pause, not a wait_for: this check asserts the ledger did
# NOT move — there is no arrival condition to poll for, so the pause only
# gives a hypothetical mis-stamped row time to show up before the compare.
sleep 1
api GET "http://127.0.0.1:$HUB_ADMIN/api/admin/federation/stats" b '' "$HUB"
# extract (jq -e + non-empty) — a plain jq would print "null" for a missing
# admin entry and BEFORE==AFTER=="null" would pass the comparison vacuously.
extract HUB_CONSUMED_AFTER '[.users[] | select(.user=="admin")][0].consumed.requests' "$b"
[ "$HUB_CONSUMED_BEFORE" = "$HUB_CONSUMED_AFTER" ] \
  && ok 'h: local request did not move the federation ledger (no fed stamp)' \
  || bad 'h: local request did not move the federation ledger' "consumed $HUB_CONSUMED_BEFORE → $HUB_CONSUMED_AFTER"

# ---- summary ---------------------------------------------------------------
say "== summary: $PASS passed, $FAIL failed =="
for f in "${FAILED[@]:-}"; do [ -n "$f" ] && say "  FAILED: $f"; done
[ "$FAIL" -eq 0 ] || exit 1
exit 0
