#!/bin/sh
set -eu

if [ "$#" -eq 0 ]; then
  set -- .
fi

set +e
rg --quiet --pcre2 --hidden --no-messages --glob '!.git/**' --glob '!vendor/**' --glob '!.idea/**' --glob '!**/*_test.go' \
  -e '-----BEGIN(?: [A-Z]+)? PRIVATE KEY-----' \
  -e '(?i)authorization:[[:space:]]*(?:bearer|token|basic)[[:space:]]+[A-Za-z0-9._~+/-]+' \
  -e '\bgh[pous]_[A-Za-z0-9]{20,}\b' \
  -e '\bgithub_pat_[A-Za-z0-9_]{20,}\b' \
  -e '\blin_api_[A-Za-z0-9_-]{20,}\b' \
  -- "$@"
status=$?
set -e

case "$status" in
  0)
    printf '%s\n' 'sensitive_output_scan:prohibited_material_detected' >&2
    exit 1
    ;;
  1)
    exit 0
    ;;
  *)
    printf '%s\n' 'sensitive_output_scan:scanner_failure' >&2
    exit 2
    ;;
esac
