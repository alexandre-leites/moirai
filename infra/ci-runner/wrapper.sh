#!/usr/bin/env bash
# Root-side boot for running the ci-runner image outside the autoscaler (e.g.
# `run-container.sh`, or a manual `docker run --privileged`). It mirrors the
# autoscaler's wrapped-mode wrapper: start dockerd, then drop to the `runner`
# user and exec the contract entrypoint.
#
# The autoscaler NEVER invokes this -- it overrides USER/ENTRYPOINT and installs
# its own wrapper -- so this exists for local testing of the DinD path and as a
# runnable example of the contract's wrapped mode.
#
# Run as root (e.g. `--user root:root --entrypoint /usr/local/bin/ci-runner-wrapper
# --privileged`); the entrypoint itself must run as the non-root `runner` user.
set -euo pipefail

: "${REPO_URL:?REPO_URL must be set}"
: "${RUNNER_NAME:?RUNNER_NAME must be set}"
: "${RUNNER_TOKEN:?RUNNER_TOKEN must be set}"

if ! docker info >/dev/null 2>&1; then
  mkdir -p /etc/docker
  if [ ! -f /etc/docker/daemon.json ]; then
    # Same configuration the autoscaler's wrapper writes.
    printf '{"features":{"containerd-snapshotter":false}}\n' > /etc/docker/daemon.json
  fi
  echo "starting dockerd (vfs)"
  dockerd --storage-driver=vfs --data-root "${DOCKER_DATA_ROOT:-/var/lib/docker}" \
    >/var/log/dockerd.log 2>&1 &
  DOCKERD_PID=$!
  for _ in $(seq 1 60); do
    docker info >/dev/null 2>&1 && break
    sleep 1
  done
  if ! docker info >/dev/null 2>&1; then
    echo "dockerd failed to become ready" >&2
    tail -50 /var/log/dockerd.log >&2 || true
    exit 1
  fi
fi

# The runner must use the Docker CLI without sudo (contract). The docker group
# comes from the docker-ce deb; ensure it exists and the runner is a member.
if ! getent group docker >/dev/null; then
  groupadd docker
fi
if ! id -nG runner | grep -qw docker; then
  usermod -aG docker runner
fi

echo "dropping to 'runner' and starting the contract entrypoint"
exec su -s /bin/bash runner -c '/usr/local/bin/ci-runner-entrypoint'
