#!/bin/sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)"
scanner="$repo_root/scripts/scan-sensitive-output.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/ifan-loop-sensitive-scan.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

failure_code='sensitive_output_scan:prohibited_material_detected'
scanner_error_code='sensitive_output_scan:scanner_failure'
case_number=0

fail() {
  printf '%s\n' "sensitive-output scanner regression failed: $1" >&2
  exit 1
}

assert_absent() {
  output_file=$1
  forbidden=$2
  label=$3
  if grep -F -- "$forbidden" "$output_file" >/dev/null 2>&1; then
    fail "$label disclosed prohibited bytes"
  fi
}

assert_detected_without_disclosure() {
  label=$1
  payload=$2
  forbidden=$3
  case_number=$((case_number + 1))
  case_root="$test_root/case-$case_number"
  mkdir "$case_root"
  printf '%s\n' "$payload" >"$case_root/evidence.txt"

  set +e
  "$scanner" "$case_root" >"$case_root/stdout" 2>"$case_root/stderr"
  status=$?
  set -e

  [ "$status" -eq 1 ] || fail "$label returned status $status instead of 1"
  [ ! -s "$case_root/stdout" ] || fail "$label wrote to stdout"
  [ "$(cat "$case_root/stderr")" = "$failure_code" ] || fail "$label returned non-sanitized stderr"
  assert_absent "$case_root/stdout" "$forbidden" "$label stdout"
  assert_absent "$case_root/stderr" "$forbidden" "$label stderr"
}

classic_suffix='0123456789abcdefghijklmnopqrstuvwxyz'
fine_grained_suffix='0123456789_abcdefghijklmnopqrstuvwxyz'
linear_suffix='0123456789-abcdefghijklmnopqrstuvwxyz'
header_secret='header-secret-0123456789'
private_key_marker="-----BE""GIN PRIVATE KEY-----"
rsa_private_key_marker="-----BE""GIN RSA PRIVATE KEY-----"

assert_detected_without_disclosure "private key" "$private_key_marker" "PRIVATE KEY"
assert_detected_without_disclosure "RSA private key" "$rsa_private_key_marker" "RSA PRIVATE KEY"
assert_detected_without_disclosure "Bearer authorization" "Author""ization: Bearer $header_secret" "$header_secret"
assert_detected_without_disclosure "Token authorization" "Author""ization: Token $header_secret" "$header_secret"
assert_detected_without_disclosure "Basic authorization" "Author""ization: Basic $header_secret" "$header_secret"
assert_detected_without_disclosure "GitHub ghp token" "ghp_$classic_suffix" "$classic_suffix"
assert_detected_without_disclosure "GitHub gho token" "gho_$classic_suffix" "$classic_suffix"
assert_detected_without_disclosure "GitHub ghu token" "ghu_$classic_suffix" "$classic_suffix"
assert_detected_without_disclosure "GitHub ghs token" "ghs_$classic_suffix" "$classic_suffix"
assert_detected_without_disclosure "GitHub fine-grained token" "github_pat_$fine_grained_suffix" "$fine_grained_suffix"
assert_detected_without_disclosure "Linear token" "lin_api_$linear_suffix" "$linear_suffix"

clean_root="$test_root/clean"
mkdir "$clean_root"
printf '%s\n' 'sanitized evidence' >"$clean_root/evidence.txt"
"$scanner" "$clean_root" >"$clean_root/stdout" 2>"$clean_root/stderr" ||
  fail "clean evidence returned nonzero"
[ ! -s "$clean_root/stdout" ] || fail "clean evidence wrote to stdout"
[ ! -s "$clean_root/stderr" ] || fail "clean evidence wrote to stderr"

missing_marker="lin_api_$linear_suffix"
missing_root="$test_root/$missing_marker"
set +e
"$scanner" "$missing_root" >"$test_root/missing.stdout" 2>"$test_root/missing.stderr"
status=$?
set -e
[ "$status" -eq 2 ] || fail "scanner error returned status $status instead of 2"
[ ! -s "$test_root/missing.stdout" ] || fail "scanner error wrote to stdout"
[ "$(cat "$test_root/missing.stderr")" = "$scanner_error_code" ] ||
  fail "scanner error returned non-sanitized stderr"
assert_absent "$test_root/missing.stderr" "$missing_marker" "scanner error"

missing_rg_root="$test_root/private-$missing_marker"
set +e
PATH=/nonexistent /bin/sh "$scanner" "$missing_rg_root" \
  >"$test_root/missing-rg.stdout" 2>"$test_root/missing-rg.stderr"
status=$?
set -e
[ "$status" -eq 2 ] || fail "missing rg returned status $status instead of 2"
[ ! -s "$test_root/missing-rg.stdout" ] || fail "missing rg wrote to stdout"
[ "$(cat "$test_root/missing-rg.stderr")" = "$scanner_error_code" ] ||
  fail "missing rg returned non-sanitized stderr"
assert_absent "$test_root/missing-rg.stderr" "$missing_marker" "missing rg secret path"
assert_absent "$test_root/missing-rg.stderr" "$scanner" "missing rg absolute script path"
assert_absent "$test_root/missing-rg.stderr" "$missing_rg_root" "missing rg absolute input path"
