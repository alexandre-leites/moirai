#!/usr/bin/env bash
# Entrypoint for the Moirai CI runner image, conforming to the autoscaler
# runner-image contract.
#
# The autoscaler starts one container per job with the environment variables
# below and AutoRemove; this image must register against GitHub and run exactly
# ONE job, then exit (the exit code is what the autoscaler observes). This
# script runs as the non-root `runner` user.
#
# In wrapped mode (project `pre_script` and/or `docker_dind`) the autoscaler
# overrides USER/ENTRYPOINT with its own root wrapper that runs the pre-script,
# starts dockerd and then drops to this image's USER and execs this entrypoint.
# This script therefore NEVER starts dockerd: it only uses a daemon that is
# already running, when one is reachable.
#
# Environment (every variable the contract always injects):
#   REPO_URL          repository URL to register against (required)
#   RUNNER_NAME       unique runner name, equals the container name (required)
#   RUNNER_TOKEN      short-lived registration token, use once (required)
#   RUNNER_SCOPE      always "repo"
#   LABELS            comma-separated labels (default: self-hosted,linux)
#   RUNNER_WORKDIR    work directory (default: /runner/_work)
#   EPHEMERAL         always true
#   GITHUB_REPOSITORY owner/repo
# Optional variables from the project `environment` map are appended by the
# autoscaler and pass through to job processes untouched.
set -euo pipefail

: "${REPO_URL:?REPO_URL must be set}"
: "${RUNNER_NAME:?RUNNER_NAME must be set}"
: "${RUNNER_TOKEN:?RUNNER_TOKEN must be set}"

LABELS="${LABELS:-self-hosted,linux}"
RUNNER_WORKDIR="${RUNNER_WORKDIR:-/runner/_work}"

mkdir -p "${RUNNER_WORKDIR}"

cd /runner
config_args=(
  ./config.sh --unattended
  --url "${REPO_URL}"
  --token "${RUNNER_TOKEN}"
  --name "${RUNNER_NAME}"
  --labels "${LABELS}"
  --work "${RUNNER_WORKDIR}"
  --replace
  --ephemeral
)

echo "configuring runner '${RUNNER_NAME}' against ${REPO_URL}"
"${config_args[@]}"

# --ephemeral makes run.sh unregister and exit after a single job, which is
# what the AutoRemove container relies on to be cleaned up.
echo "starting runner '${RUNNER_NAME}' (ephemeral, one job)"
exec tini -s -- ./run.sh
