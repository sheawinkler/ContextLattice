#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="${1:-.env}"
PROFILE_SET="${2:-}"

if [[ -z "${PROFILE_SET}" ]]; then
  echo "Usage: $0 <env-file> <profiles>" >&2
  exit 2
fi

tmp_file="$(mktemp "${ENV_FILE}.tmp.XXXXXX")"

if [[ -f "${ENV_FILE}" ]]; then
  awk -v profiles="${PROFILE_SET}" '
    BEGIN { updated = 0 }
    /^COMPOSE_PROFILES=/ {
      print "COMPOSE_PROFILES=" profiles
      updated = 1
      next
    }
    { print }
    END {
      if (!updated) {
        print "COMPOSE_PROFILES=" profiles
      }
    }
  ' "${ENV_FILE}" > "${tmp_file}"
else
  printf 'COMPOSE_PROFILES=%s\n' "${PROFILE_SET}" > "${tmp_file}"
fi

mv "${tmp_file}" "${ENV_FILE}"
echo "Set COMPOSE_PROFILES=${PROFILE_SET} in ${ENV_FILE}"
