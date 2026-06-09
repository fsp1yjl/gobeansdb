#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:7990}"
KEY="${KEY:-test_http_key}"
TEST_FILE="${TEST_FILE:-/tmp/gobeansdb_http_test.bin}"
TOKEN="${TOKEN:-}"
FLAG="${FLAG:-7}"
EXPTIME="${EXPTIME:-0}"

BODY_FILE="$(mktemp)"
HEADER_FILE="$(mktemp)"
cleanup() {
  rm -f "$BODY_FILE" "$HEADER_FILE"
}
trap cleanup EXIT

PASS=0
FAIL=0

print_pass() {
  PASS=$((PASS + 1))
  echo "[PASS] $1"
}

print_fail() {
  FAIL=$((FAIL + 1))
  echo "[FAIL] $1"
}

assert_eq() {
  local actual="$1"
  local expected="$2"
  local desc="$3"
  if [[ "$actual" == "$expected" ]]; then
    print_pass "$desc"
  else
    print_fail "$desc (expected=$expected, got=$actual)"
  fi
}

run_curl() {
  local method="$1"
  local url="$2"
  local with_auth="$3"
  local data_file="${4:-}"

  local auth_args=()
  if [[ "$with_auth" == "true" && -n "$TOKEN" ]]; then
    auth_args=(-H "X-Beansdb-Token: $TOKEN")
  fi

  if [[ -n "$data_file" ]]; then
    curl -sS -X "$method" "${auth_args[@]}" --data-binary "@$data_file" -D "$HEADER_FILE" -o "$BODY_FILE" -w "%{http_code}" "$url"
  else
    curl -sS -X "$method" "${auth_args[@]}" -D "$HEADER_FILE" -o "$BODY_FILE" -w "%{http_code}" "$url"
  fi
}

echo "hello-http-crud" > "$TEST_FILE"

PUT_URL="$BASE_URL/api/v1/object/$KEY?flag=$FLAG&exptime=$EXPTIME"
GET_URL="$BASE_URL/api/v1/object/$KEY"
METRICS_URL="$BASE_URL/metrics"

if [[ -n "$TOKEN" ]]; then
  code="$(run_curl "PUT" "$PUT_URL" "false" "$TEST_FILE")"
  assert_eq "$code" "401" "PUT without token should return 401"
fi

code="$(run_curl "PUT" "$PUT_URL" "true" "$TEST_FILE")"
assert_eq "$code" "200" "PUT upload should return 200"

if grep -q '"ok":true' "$BODY_FILE" && grep -q "\"key\":\"$KEY\"" "$BODY_FILE"; then
  print_pass "PUT response body contains ok=true and key"
else
  print_fail "PUT response body validation"
fi

code="$(run_curl "GET" "$GET_URL" "true")"
assert_eq "$code" "200" "GET should return 200"

get_body="$(cat "$BODY_FILE")"
if [[ "$get_body" == "hello-http-crud" ]]; then
  print_pass "GET response body matches uploaded content"
else
  print_fail "GET response body mismatch"
fi

if grep -qi "^X-Beansdb-Flag: $FLAG" "$HEADER_FILE"; then
  print_pass "GET response header contains X-Beansdb-Flag"
else
  print_fail "GET response header missing X-Beansdb-Flag"
fi

code="$(run_curl "DELETE" "$GET_URL" "true")"
assert_eq "$code" "200" "DELETE should return 200"

code="$(run_curl "GET" "$GET_URL" "true")"
assert_eq "$code" "404" "GET after DELETE should return 404"

code="$(run_curl "GET" "$METRICS_URL" "true")"
assert_eq "$code" "200" "GET /metrics should return 200"

if grep -q '"total_requests"' "$BODY_FILE" \
  && grep -q '"qps_1m"' "$BODY_FILE" \
  && grep -q '"status_codes"' "$BODY_FILE" \
  && grep -q '"latency_buckets"' "$BODY_FILE"; then
  print_pass "metrics payload contains required fields"
else
  print_fail "metrics payload missing required fields"
fi

echo
echo "Summary: PASS=$PASS FAIL=$FAIL"
if [[ "$FAIL" -gt 0 ]]; then
  exit 1
fi
