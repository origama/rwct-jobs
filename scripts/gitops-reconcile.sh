#!/usr/bin/env bash
# Usage (cron):
#   * * * * * REPO_DIR=/opt/rwct-jobs BRANCH=main /opt/rwct-jobs/scripts/gitops-reconcile.sh >> /var/log/gitops-reconcile.log 2>&1
#
# Usage (systemd):
#   1) Create /etc/systemd/system/gitops-reconcile.service:
#      [Unit]
#      Description=GitOps reconcile (git pull + docker compose)
#      After=network-online.target docker.service
#      Wants=network-online.target
#
#      [Service]
#      Type=oneshot
#      WorkingDirectory=/opt/rwct-jobs
#      Environment=REPO_DIR=/opt/rwct-jobs
#      Environment=BRANCH=main
#      # Optional examples:
#      # Environment=COMPOSE_FILES=docker-compose.yml:docker-compose.prod.yml
#      # Environment=COMPOSE_PROFILES=prod
#      ExecStart=/opt/rwct-jobs/scripts/gitops-reconcile.sh
#
#      [Install]
#      WantedBy=multi-user.target
#
#   2) Create /etc/systemd/system/gitops-reconcile.timer:
#      [Unit]
#      Description=Run GitOps reconcile every minute
#
#      [Timer]
#      OnBootSec=1m
#      OnUnitActiveSec=1m
#      Persistent=true
#
#      [Install]
#      WantedBy=timers.target
#
#   3) Enable/start timer:
#      sudo systemctl daemon-reload
#      sudo systemctl enable --now gitops-reconcile.timer
#      systemctl list-timers --all | grep gitops-reconcile
#
# Useful environment variables:
#   REPO_DIR, REMOTE, BRANCH, LOCK_FILE, ALWAYS_RECONCILE,
#   COMPOSE_FILES, COMPOSE_PROFILES, COMPOSE_PROJECT_NAME, COMPOSE_BIN,
#   COMPOSE_SCALE (e.g. "job-analyzer=2,message-dispatcher=1"),
#   COMPOSE_BUILD (auto|always|never)
set -Eeuo pipefail

# GitOps pull + Docker Compose reconciliation for a single Linux host.
# Intended for cron/systemd timer execution.

REPO_DIR="${REPO_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
REMOTE="${REMOTE:-origin}"
BRANCH="${BRANCH:-main}"
LOCK_FILE="${LOCK_FILE:-/tmp/gitops-reconcile.lock}"
ALWAYS_RECONCILE="${ALWAYS_RECONCILE:-true}"

# Compose options
COMPOSE_FILES="${COMPOSE_FILES:-}"          # colon-separated, e.g. docker-compose.yml:docker-compose.prod.yml
COMPOSE_PROFILES="${COMPOSE_PROFILES:-}"    # comma-separated, e.g. prod,monitoring
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-}"
COMPOSE_BIN="${COMPOSE_BIN:-}"              # optional override, e.g. "docker compose" or "docker-compose"
COMPOSE_SCALE="${COMPOSE_SCALE:-}"          # comma-separated, e.g. job-analyzer=2,message-dispatcher=1
COMPOSE_BUILD="${COMPOSE_BUILD:-auto}"      # auto=build on git changes; always=always build; never=never build

log() {
  printf '[%s] %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*"
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    log "ERROR: command not found: $1"
    exit 1
  }
}

run_compose() {
  local changed="${1:-false}"
  local -a args
  local -a up_scale_args
  local -a up_build_args
  IFS=' ' read -r -a compose_cmd <<<"${COMPOSE_BIN}"

  if [[ -n "${COMPOSE_FILES}" ]]; then
    IFS=':' read -r -a files <<<"${COMPOSE_FILES}"
    for f in "${files[@]}"; do
      [[ -n "${f}" ]] && args+=("-f" "${f}")
    done
  fi

  if [[ -n "${COMPOSE_PROFILES}" ]]; then
    IFS=',' read -r -a profiles <<<"${COMPOSE_PROFILES}"
    for p in "${profiles[@]}"; do
      [[ -n "${p}" ]] && args+=("--profile" "${p}")
    done
  fi

  if [[ -n "${COMPOSE_PROJECT_NAME}" ]]; then
    args+=("--project-name" "${COMPOSE_PROJECT_NAME}")
  fi

  up_scale_args=()
  if [[ -n "${COMPOSE_SCALE}" ]]; then
    IFS=',' read -r -a scales <<<"${COMPOSE_SCALE}"
    for s in "${scales[@]}"; do
      [[ -n "${s}" ]] && up_scale_args+=("--scale" "${s}")
    done
  fi

  up_build_args=()
  case "${COMPOSE_BUILD}" in
    always)
      up_build_args+=("--build")
      ;;
    auto)
      if [[ "${changed}" == "true" ]]; then
        up_build_args+=("--build")
      fi
      ;;
    never)
      ;;
    *)
      log "ERROR: invalid COMPOSE_BUILD=${COMPOSE_BUILD} (allowed: auto|always|never)"
      exit 1
      ;;
  esac

  "${compose_cmd[@]}" "${args[@]}" pull
  "${compose_cmd[@]}" "${args[@]}" up "${up_scale_args[@]}" "${up_build_args[@]}" -d --remove-orphans
}

resolve_compose_bin() {
  if [[ -n "${COMPOSE_BIN:-}" ]]; then
    return 0
  fi
  if docker compose version >/dev/null 2>&1; then
    COMPOSE_BIN="docker compose"
    return 0
  fi
  if command -v docker-compose >/dev/null 2>&1; then
    COMPOSE_BIN="docker-compose"
    return 0
  fi
  return 1
}

main() {
  require_cmd git
  require_cmd docker
  require_cmd flock

  if ! resolve_compose_bin; then
    log "ERROR: no Compose CLI found (docker compose / docker-compose)"
    exit 1
  fi

  exec 200>"${LOCK_FILE}"
  if ! flock -n 200; then
    log "Another reconciliation is already running, skipping."
    exit 0
  fi

  cd "${REPO_DIR}"

  if [[ ! -d .git ]]; then
    log "ERROR: REPO_DIR is not a git repository: ${REPO_DIR}"
    exit 1
  fi

  if ! git remote get-url "${REMOTE}" >/dev/null 2>&1; then
    log "ERROR: git remote '${REMOTE}' not found"
    exit 1
  fi

  if ! git diff --quiet || ! git diff --cached --quiet; then
    log "ERROR: local git changes detected in ${REPO_DIR}; refusing to deploy"
    exit 1
  fi

  log "Fetching ${REMOTE}/${BRANCH}"
  git fetch --prune "${REMOTE}" "${BRANCH}"

  if git show-ref --verify --quiet "refs/heads/${BRANCH}"; then
    git checkout -q "${BRANCH}"
  else
    git checkout -q -b "${BRANCH}" "${REMOTE}/${BRANCH}"
  fi

  local before after changed
  before="$(git rev-parse HEAD)"
  log "Pulling latest commits (ff-only)"
  git pull --ff-only "${REMOTE}" "${BRANCH}"
  after="$(git rev-parse HEAD)"

  changed=false
  if [[ "${before}" != "${after}" ]]; then
    changed=true
    log "Repository updated: ${before} -> ${after}"
  else
    log "Repository already up to date at ${after}"
  fi

  if [[ "${ALWAYS_RECONCILE}" == "true" || "${changed}" == "true" ]]; then
    log "Reconciling Docker Compose"
    run_compose "${changed}"
    log "Reconciliation completed"
  else
    log "No new commits and ALWAYS_RECONCILE=false, skipping compose"
  fi
}

main "$@"
