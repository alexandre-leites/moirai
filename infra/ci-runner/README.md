# Moirai CI runner image

The GitHub Actions self-hosted runner for the `self-hosted` / `linux` pool,
with the whole pipeline toolchain baked in. It implements the autoscaler
runner-image contract: the autoscaler polls the GitHub APIs, decides how many
runners are needed, and starts one container per job from this image. Each
container registers against GitHub, runs exactly one job, and exits.

It replaces the previous myoung34-based runner container. The change that
matters: the pool used to mount the host's docker socket, so `compose-smoke`
built the four product images directly on the 40 GB host disk, which ran out of
space mid-build ("no space left on device"). Each job now builds inside its own
fresh, disposable dockerd, so nothing accumulates on the host and the cache
dies with the container.

## Runner-image contract

The authoritative contract lives in the autoscaler project
(`docs/runner-image-contract`). This image conforms to it:

- **One job per container.** The entrypoint registers with `--ephemeral` and
  `run.sh` exits after a single job. The container is `AutoRemove`, so the exit
  code is what the autoscaler observes.
- **Non-root runner user.** The GitHub runner runs as `runner` (uid/gid 1001),
  which owns `/runner` and the work directory. Passwordless sudo is configured
  for pre-scripts and workflows that run `sudo`. `/opt`, `/tmp` and `/runner`
  are owned by `runner` and recursively world-writable, so root and the runner
  user both have full access for installs, scratch files and runner state.
- **Environment variables.** The entrypoint reads the contract variables:
  `REPO_URL`, `RUNNER_NAME`, `RUNNER_TOKEN`, `LABELS`, `RUNNER_WORKDIR`,
  `EPHEMERAL`, `RUNNER_SCOPE`, `GITHUB_REPOSITORY`. Project `environment` map
  entries pass through to job processes untouched.
- **Never starts its own dockerd.** In wrapped mode (`pre_script` and/or
  `docker_dind`) the autoscaler's wrapper starts dockerd and then execs this
  image's entrypoint. The image ships the docker CLI + dockerd so workflow jobs
  can use Docker; the project must set `docker_dind: true` for that.
- **Self-contained.** No volumes, no published ports, nothing mounted or
  injected beyond the environment variables.

## Baked toolchain

| Tool | Version | Source |
| --- | --- | --- |
| GitHub Actions runner | 2.336.0 (self-updates at runtime) | `actions/runner` release tar |
| Go | 1.25.12 | go.dev tar (checksum-verified) |
| Node | 26.6.0 (npm 12.0.2) | nodejs.org tar |
| Docker engine / CLI | 29.7.1 | Docker apt repo, pinned |
| containerd | 2.2.6 | Docker apt repo, pinned |
| docker buildx | 0.36.0 | Docker apt repo, pinned |
| docker compose | 5.4.0 | Docker apt repo, pinned |
| gh | 2.63.2 | cli/cli release (same pin as the orchestrator image) |
| sqlc | 1.29.0 | sqlc-dev/sqlc release (same pin as the Makefile) |
| buf | 1.50.0 | bufbuild/buf release (same pin as the Makefile) |
| govulncheck | 1.6.0 | `go install` from the baked toolchain |
| trivy | 0.73.0 | aquasecurity/trivy release |

Base tooling: `build-essential` (gcc for `go test -race`), `git`, `make`,
`curl`, `bash`, `jq`, `unzip`, `xz-utils`, `tini`, `sudo`, `passwd` (usermod),
`util-linux` (su/setpriv), `libicu70`, `liblttng-ust` (the runner's runtime
dependencies).

## Updating the image

1. Bump the tool versions at the top of `Dockerfile`.
2. Bump `infra/ci-runner/VERSION`.
3. Merge, and the publish workflow rebuilds `latest` + `1.0.0` + `1.0` +
   `sha-<short>`.

The workflow also runs weekly so the baked tools do not silently rot, and on
`workflow_dispatch` for an ad-hoc rebuild.

## Running it

Production containers are started by the autoscaler. For local testing:

```sh
REPO_URL=https://github.com/alexandre-leites/moirai \
RUNNER_TOKEN=<registration-token-or-PAT> \
./run-container.sh
```

`run-container.sh` defaults to wrapped mode (root boot that starts dockerd then
drops to the `runner` user), so it needs `--privileged`. Set `CI_RUNNER_PLAIN=1`
to run the image's own non-root entrypoint without DinD. Health is reported by
the image's `HEALTHCHECK` (runner listener alive, plus dockerd reachable when a
socket is present).

## Relationship to the moirai runner image

`runner/Dockerfile` is the *execution* image — the environment an agent runs in
for a job. This image is the *CI* runner — the environment that builds, tests
and runs the moirai stack. They are different concerns and must not be
conflated; this image is not offered to agents and `runner/toolchain.json` does
not describe it.