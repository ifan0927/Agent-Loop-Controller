#!/bin/sh
set -eu

if [ "$#" -eq 0 ]; then
  set -- .
fi

if rg --pcre2 --hidden --no-messages --glob '!.git/**' --glob '!vendor/**' --glob '!.idea/**' --glob '!**/*_test.go' -n \
  -e '-----BEGIN(?: [A-Z]+)? PRIVATE KEY-----' \
  -e '(?i)authorization:[[:space:]]*(?:bearer|token|basic)[[:space:]]+[A-Za-z0-9._~+/-]+' \
  -e '\bgh[pous]_[A-Za-z0-9]{20,}\b' \
  -e '\bgithub_pat_[A-Za-z0-9_]{20,}\b' \
  -e '\blin_api_[A-Za-z0-9_-]{20,}\b' \
  "$@"; then
  printf '%s\n' 'sensitive credential-like material detected' >&2
  exit 1
fi
