# Release and container image publishing

Moirai publishes one container image per service to GitHub Container Registry.
[`.github/workflows/release.yml`](../.github/workflows/release.yml) is the only
thing that publishes them, and it is the only workflow allowed to; CI never
pushes an image.

## Image names

| Service | Image |
| --- | --- |
| orchestrator | `ghcr.io/alexandre-leites/moirai/orchestrator` |
| api | `ghcr.io/alexandre-leites/moirai/api` |
| runner | `ghcr.io/alexandre-leites/moirai/runner` |
| web | `ghcr.io/alexandre-leites/moirai/web` |

The names are `ghcr.io/<owner>/<repo>/<service>`, derived at runtime from
`github.repository`, so a fork publishes under its own owner without editing the
workflow. (A fork consuming its own images does have to set
`MOIRAI_IMAGE_PREFIX`; Compose cannot read `github.repository`.)
The nested form was chosen over a flat `moirai-<service>` because the
repository is a monorepo of co-released services: nesting keeps all four
packages grouped under the repository they come from, and leaves the owner's
top-level package namespace free for unrelated projects.

Packages inherit the repository's visibility. While the repository is private,
pulling requires `docker login ghcr.io` with a token that has `read:packages`.

Every image carries OCI labels and index annotations for `title`,
`description`, `source`, `url`, `revision`, `version`, and `created`, plus a
`mode=max` provenance attestation and an SBOM.

## Architectures

Every tag is a manifest list covering `linux/amd64` and `linux/arm64`.

The Go builders and the web bundle build run on the native build platform and
cross-compile (`--platform=$BUILDPLATFORM`, `GOARCH=$TARGETARCH`), so only the
runtime stages -- the ones that run `apk`, `apt-get`, `pip`, and `npm` -- are
emulated through QEMU. A release therefore does not pay emulation cost for
compilation.

The `verify` job re-reads every published tag from the registry with
`docker buildx imagetools inspect` and fails the run if either architecture is
missing. A multi-architecture build that silently degrades to one architecture
still pushes successfully, so the manifest lists are checked rather than
trusted.

## Trigger contract

Three triggers, and nothing else, reach this workflow.

| Trigger | Version | Image tags |
| --- | --- | --- |
| Push to `release/X.Y.Z` (or `release/vX.Y.Z`) | `X.Y.Z-rc.<run number>` | `X.Y.Z-rc.<run number>`, `X.Y.Z-rc`, `sha-<short sha>` |
| Published GitHub Release tagged `vX.Y.Z`, not a pre-release, and the newest release | `X.Y.Z` | `X.Y.Z`, `X.Y`, `X`, `latest`, `sha-<short sha>` |
| Published GitHub Release tagged `vX.Y.Z`, not a pre-release, but **not** the newest release | `X.Y.Z` | `X.Y.Z`, `X.Y`, `sha-<short sha>` |
| Published GitHub Release tagged `vX.Y.Z`, flagged pre-release | `X.Y.Z` | `X.Y.Z`, `sha-<short sha>` |
| Published GitHub Release tagged `vX.Y.Z-<identifier>` | `X.Y.Z-<identifier>` | `X.Y.Z-<identifier>`, `sha-<short sha>` |
| Manual `workflow_dispatch` | `0.0.0-dev.<run number>` | builds every image for every architecture and **publishes nothing** |

Consequences worth stating outright:

- **Pushing a git tag publishes nothing.** A GitHub Release has to be published.
  That keeps releasing an intentional act rather than a side effect of tagging,
  and it means a tag can be created, inspected, and deleted without touching the
  registry.
- **A release branch never claims the bare version.** `1.4.0` only ever points at
  the artifact built from the published `v1.4.0` release.
- **Pointers that span release lines only move forward.** Before claiming
  `latest` or the bare major `X`, the workflow asks GitHub which release is
  currently the newest and only claims them when that is this release.
  Publishing a patch for an older line (`v1.3.5` after `v1.4.0`) therefore
  updates `1.3.5` and `1.3` and leaves `1` and `latest` alone. `X.Y` is not
  gated, because a minor pointer is *supposed* to follow the newest patch of its
  own line.
- **Every run produces `sha-<short sha>`.** Any published digest can be traced
  back to a revision even after the moving tags have advanced.
- **A malformed trigger fails the run.** `release/next`, `release/1.4`,
  `refs/tags/1.4.0` (no `v`), and a non-semver release tag are all rejected
  before anything is built. The workflow never guesses a version.
- **Promoting an existing pre-release does not republish.** The workflow listens
  for `release: types: [published]` only, so flipping the "pre-release" checkbox
  off afterwards fires `released`, not `published`, and nothing happens. Adding
  `released` would make every ordinary release build twice. If you need to
  promote, delete the release and publish it again, which fires `published`.
  Note that this only changes the outcome for a tag with no pre-release
  identifier: `v1.4.0-rc.1` publishes `1.4.0-rc.1` whether the checkbox is set
  or not, because an identifier in the tag is authoritative.

The derivation is [`scripts/release-version.sh`](../scripts/release-version.sh),
not inline workflow YAML, so the contract is executable.
[`scripts/release-version_test.sh`](../scripts/release-version_test.sh) is its
specification; run it with `make test-release-tags`. The `plan` job runs it
before deriving anything, so a change that breaks the mapping fails the release
instead of publishing images under the wrong tags.

## Cutting a release

Release candidates, optional:

```bash
git switch -c release/1.4.0
git push -u origin release/1.4.0
```

Every push to that branch publishes `1.4.0-rc.<run number>` and moves
`1.4.0-rc`. Deploy `1.4.0-rc` in staging and keep pushing fixes to the branch.

The release itself:

```bash
gh release create v1.4.0 --target release/1.4.0 --generate-notes
```

`gh release create` publishes the release, which fires the workflow. Add
`--prerelease` to publish under the exact version only, with no `latest`, `X.Y`,
or `X` pointers.

To rehearse the pipeline without publishing anything, run the workflow manually
from the Actions tab: a `workflow_dispatch` run builds all four images for both
architectures and pushes nothing.

## Runners

The release workflow runs on GitHub-hosted `ubuntu-latest`, the same as CI. The
`[self-hosted, linux]` lines are kept commented out beside each `runs-on` so the
pool can be switched back in one edit.

That is a change from the original design, which used a self-hosted pool because
the repository is private and GitHub-hosted minutes are billable. The pool had
nothing online, so every queued run was cancelled by the next push and neither
workflow had executed for over a week. Hosted runners are the cheaper trade while
that is true: a release that does not run is worth less than the minutes it saves.

The workflow deliberately uses no `actions/setup-*` step -- it needs Docker and
nothing else. On a self-hosted runner that means:

- Docker Engine (already required by the CI `compose-smoke` job).
- Permission to run privileged containers. `docker/setup-qemu-action` registers
  the aarch64 emulator by running `tonistiigi/binfmt` with `--privileged`. If it
  cannot, the workflow fails at that step rather than publishing an amd64-only
  image. The image is pinned to `qemu-v9.2.2`: a later build of it crashed
  `npm install` under arm64 emulation with SIGILL, which presents as an
  unexplained failure inside the web image's runtime stage.
- Outbound network access to `ghcr.io`, plus the package registries the images
  build from (Debian, Alpine, npm, PyPI, and the GitHub CLI release archive).

`docker/setup-buildx-action` downloads buildx itself, so no runner needs the
buildx plugin preinstalled.

Layer cache is stored in the GitHub Actions cache, scoped per service
(`type=gha,scope=release-<service>`), so a release reuses the previous release's
layers.

## Running the published images

`compose.yaml` pulls the published images by default, so consuming a release is just
`docker compose up -d`. `MOIRAI_IMAGE_TAG` selects the version (`latest` by
default, which moves) and `MOIRAI_IMAGE_PREFIX` points at a fork or mirror.
Building from the checkout instead is the `compose.build.yaml` overlay.

## Credentials

The workflow authenticates to `ghcr.io` with the built-in `GITHUB_TOKEN` and
`permissions: packages: write`, granted only to the job that needs it. No
release secret is provisioned for this repository, and none should be. The
token is passed to `docker/login-action` and never reaches a build argument, a
build context, or an image layer.
