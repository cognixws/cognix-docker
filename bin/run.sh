#!/bin/sh
# Runs the prebuilt cognix-docker binary when shipped, else builds once per
# source digest into the extension's cache dir (never go run on every start).
set -eu

EXT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OS=$(uname -s)
ARCH=$(uname -m)
PREBUILT="${EXT_DIR}/bin/cognix-docker-${OS}-${ARCH}"

if [ -x "${PREBUILT}" ]; then
  exec "${PREBUILT}" "$@"
fi

CACHE_BASE="${PICODE_EXT_DATA:-${XDG_CACHE_HOME:-${HOME}/.cache}/cognix-docker}"
BIN_DIR="${CACHE_BASE}/bin"
mkdir -p "${BIN_DIR}"

HASH=$(find "${EXT_DIR}/cmd" "${EXT_DIR}/engine" "${EXT_DIR}/sysadmin" "${EXT_DIR}/server" "${EXT_DIR}/go.mod" -type f 2>/dev/null | LC_ALL=C sort | xargs sha256sum | sha256sum | cut -c1-16)
CACHED="${BIN_DIR}/cognix-docker-${HASH}"

if [ ! -x "${CACHED}" ]; then
  TMP="${CACHED}.tmp.$$"
  (cd "${EXT_DIR}" && go build -trimpath -ldflags="-s -w" -o "${TMP}" ./cmd/cognix-docker)
  chmod 0755 "${TMP}"
  mv -f "${TMP}" "${CACHED}"
  # Prune older cached builds of this extension
  for old in "${BIN_DIR}"/cognix-docker-*; do
    if [ "${old}" != "${CACHED}" ] && [ -f "${old}" ]; then
      rm -f "${old}" 2>/dev/null || true
    fi
  done
fi

exec "${CACHED}" "$@"
