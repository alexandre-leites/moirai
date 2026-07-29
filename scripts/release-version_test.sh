#!/bin/sh
# Executable specification for scripts/release-version.sh.
#
# The release workflow runs this before it derives anything, so a change that
# breaks the documented trigger -> tag mapping fails the release run instead of
# publishing images under the wrong tags. Run locally with `make test-release-tags`.
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
subject="$script_dir/release-version.sh"

sha_a=0123456789abcdef0123456789abcdef01234567
sha_b=fedcba9876543210fedcba9876543210fedcba98

failures=0
checks=0

# expect <name> <expected-output> [VAR=VALUE ...]
expect() {
	name=$1
	expected=$2
	shift 2
	checks=$((checks + 1))
	if ! actual=$(env "$@" "$subject" 2>&1); then
		printf 'FAIL %s: exited non-zero\n%s\n' "$name" "$actual" >&2
		failures=$((failures + 1))
		return 0
	fi
	if [ "$actual" != "$expected" ]; then
		printf 'FAIL %s\n  expected:\n%s\n  actual:\n%s\n' "$name" "$expected" "$actual" >&2
		failures=$((failures + 1))
	fi
}

# reject <name> <expected-message-substring> [VAR=VALUE ...]
reject() {
	name=$1
	needle=$2
	shift 2
	checks=$((checks + 1))
	if actual=$(env "$@" "$subject" 2>&1); then
		printf 'FAIL %s: expected a non-zero exit, got:\n%s\n' "$name" "$actual" >&2
		failures=$((failures + 1))
		return 0
	fi
	case "$actual" in
	*"$needle"*) ;;
	*)
		printf 'FAIL %s: expected message containing %s, got:\n%s\n' "$name" "$needle" "$actual" >&2
		failures=$((failures + 1))
		;;
	esac
}

# --- published GitHub Release, stable ----------------------------------------

expect 'stable release publishes the full moving-tag ladder' \
	'version=1.4.0
channel=stable
push=true
tags=1.4.0,1.4,1,latest,sha-0123456' \
	EVENT_NAME=release REF=refs/tags/v1.4.0 SHA="$sha_a" RUN_NUMBER=42 \
	PRERELEASE=false MAKE_LATEST=true

# Publishing v1.3.5 after v1.4.0. The minor pointer follows the newest patch of
# its own line, but neither `1` nor `latest` may be dragged backwards.
expect 'a stable release that is not the newest moves neither latest nor the major' \
	'version=1.3.5
channel=stable
push=true
tags=1.3.5,1.3,sha-0123456' \
	EVENT_NAME=release REF=refs/tags/v1.3.5 SHA="$sha_a" RUN_NUMBER=42 \
	PRERELEASE=false MAKE_LATEST=false

expect 'MAKE_LATEST defaults to false, so losing the value cannot move a pointer' \
	'version=1.3.5
channel=stable
push=true
tags=1.3.5,1.3,sha-0123456' \
	EVENT_NAME=release REF=refs/tags/v1.3.5 SHA="$sha_a" RUN_NUMBER=42 \
	PRERELEASE=false

expect 'a 0.x stable release still gets its major and minor pointers' \
	'version=0.1.0
channel=stable
push=true
tags=0.1.0,0.1,0,latest,sha-fedcba9' \
	EVENT_NAME=release REF=refs/tags/v0.1.0 SHA="$sha_b" RUN_NUMBER=1 \
	PRERELEASE=false MAKE_LATEST=true

expect 'a patch release moves its minor and major pointers' \
	'version=2.10.3
channel=stable
push=true
tags=2.10.3,2.10,2,latest,sha-0123456' \
	EVENT_NAME=release REF=refs/tags/v2.10.3 SHA="$sha_a" RUN_NUMBER=7 \
	PRERELEASE=false MAKE_LATEST=true

# --- published GitHub Release, pre-release -----------------------------------

expect 'a release flagged pre-release publishes only its exact version' \
	'version=1.4.0
channel=prerelease
push=true
tags=1.4.0,sha-0123456' \
	EVENT_NAME=release REF=refs/tags/v1.4.0 SHA="$sha_a" RUN_NUMBER=42 \
	PRERELEASE=true MAKE_LATEST=true

expect 'a pre-release identifier in the tag is authoritative on its own' \
	'version=1.4.0-rc.1
channel=prerelease
push=true
tags=1.4.0-rc.1,sha-0123456' \
	EVENT_NAME=release REF=refs/tags/v1.4.0-rc.1 SHA="$sha_a" RUN_NUMBER=42 \
	PRERELEASE=false MAKE_LATEST=true

# --- release branch pushes ---------------------------------------------------

expect 'a release branch push publishes run-numbered release candidates' \
	'version=1.4.0-rc.42
channel=prerelease
push=true
tags=1.4.0-rc.42,1.4.0-rc,sha-0123456' \
	EVENT_NAME=push REF=refs/heads/release/1.4.0 SHA="$sha_a" RUN_NUMBER=42

expect 'a release branch may spell its version with a leading v' \
	'version=1.4.0-rc.42
channel=prerelease
push=true
tags=1.4.0-rc.42,1.4.0-rc,sha-0123456' \
	EVENT_NAME=push REF=refs/heads/release/v1.4.0 SHA="$sha_a" RUN_NUMBER=42

# --- manual dispatch ---------------------------------------------------------

expect 'a manual dispatch builds without publishing' \
	'version=0.0.0-dev.9
channel=dev
push=false
tags=sha-fedcba9' \
	EVENT_NAME=workflow_dispatch REF=refs/heads/main SHA="$sha_b" RUN_NUMBER=9

# --- rejected inputs ---------------------------------------------------------

reject 'a release branch without a version is rejected' \
	'release branch must be named' \
	EVENT_NAME=push REF=refs/heads/release/next SHA="$sha_a" RUN_NUMBER=1

reject 'a two-component release branch is rejected' \
	'release branch must be named' \
	EVENT_NAME=push REF=refs/heads/release/1.4 SHA="$sha_a" RUN_NUMBER=1

reject 'a zero-padded release branch is rejected' \
	'release branch must be named' \
	EVENT_NAME=push REF=refs/heads/release/01.4.0 SHA="$sha_a" RUN_NUMBER=1

reject 'a push outside release/* is rejected' \
	'must target refs/heads/release/' \
	EVENT_NAME=push REF=refs/heads/main SHA="$sha_a" RUN_NUMBER=1

reject 'a release tag without the v prefix is rejected' \
	'release tag must be' \
	EVENT_NAME=release REF=refs/tags/1.4.0 SHA="$sha_a" RUN_NUMBER=1

reject 'a non-semver release tag is rejected' \
	'release tag must be' \
	EVENT_NAME=release REF=refs/tags/v1.4 SHA="$sha_a" RUN_NUMBER=1

reject 'a release event on a branch ref is rejected' \
	'must carry a refs/tags/' \
	EVENT_NAME=release REF=refs/heads/release/1.4.0 SHA="$sha_a" RUN_NUMBER=1

reject 'a short sha is rejected' \
	'40-character commit sha' \
	EVENT_NAME=release REF=refs/tags/v1.4.0 SHA=0123456 RUN_NUMBER=1

reject 'a non-numeric run number is rejected' \
	'RUN_NUMBER must be' \
	EVENT_NAME=push REF=refs/heads/release/1.4.0 SHA="$sha_a" RUN_NUMBER=abc

reject 'an unknown event is rejected' \
	"unsupported event 'schedule'" \
	EVENT_NAME=schedule REF=refs/heads/main SHA="$sha_a" RUN_NUMBER=1

reject 'a non-boolean prerelease flag is rejected' \
	'PRERELEASE must be' \
	EVENT_NAME=release REF=refs/tags/v1.4.0 SHA="$sha_a" RUN_NUMBER=1 PRERELEASE=yes

if [ "$failures" -ne 0 ]; then
	printf '%s of %s release tag mapping checks failed\n' "$failures" "$checks" >&2
	exit 1
fi
printf 'all %s release tag mapping checks passed\n' "$checks"
