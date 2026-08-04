#!/usr/bin/env bash
# Manual helper for running the ci-runner image the way the autoscaler does: one
# container that registers with GitHub and runs a single job, then exits.
#
#   usage: REPO_URL=https://github.com/alexandre-leites/moirai \
#            RUNNER_TOKEN=<registration-token-or-PAT> \
#            ./run-container.sh
#
# Defaults to wrapped mode (root boot that starts dockerd then drops to the
# `runner` user -- the image's purpose is DinD-heavy jobs), so it needs
# --privileged. Set CI_RUNNER_PLAIN=1 to run the image's own non-root entrypoint
# instead (no DinD).
#
# This is a local-testing convenience. Production runner containers are started
# by the autoscaler, which passes exactly the environment variables below.
set -euo pipefail

IMAGE="${CI_RUNNER_IMAGE:-ghcr.io/alexandre-leites/moirai/ci-runner:latest}"
CONTAINER_NAME="${CI_RUNNER_CONTAINER_NAME:-ci-runner-test}"

: "${REPO_URL:?REPO_URL must be set}"
: "${RUNNER_TOKEN:?RUNNER_TOKEN must be set}"

if [ "${CI_RUNNER_PLAIN:-}" = "1" ]; then
  entry=()
  extra_user=()
else
  entry=(--entrypoint /usr/local/bin/ci-runner-wrapper)
  extra_user=(--user root:root --privileged)
fi

docker run --rm --name "${CONTAINER_NAME}" \
  "${extra_user[@]}" \
  "${entry[@]}" \
  -e REPO_URL="${REPO_URL}" \
  -e RUNNER_NAME="${RUNNER_NAME:-${CONTAINER_NAME}}" \
  -e RUNNER_TOKEN="${RUNNER_TOKEN}" \
  -e LABELS="${LABELS:-self-hosted,linux}" \
  -e RUNNER_WORKDIR="${RUNNER_WORKDIR:-/runner/_work}" \
  "${IMAGE}"
