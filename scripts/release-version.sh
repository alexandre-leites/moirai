#!/bin/sh
# Derive the release version, publication channel, and container image tag list
# for one release run.
#
# The release workflow (.github/workflows/release.yml) is the only caller. The
# derivation lives here, not inline in the workflow, so the trigger -> tag
# mapping is executable and can be tested without cutting a real release; see
# scripts/release-version_test.sh.
#
# Inputs (environment):
#   EVENT_NAME         push | release | workflow_dispatch   (github.event_name)
#   REF                refs/heads/release/X.Y.Z or refs/tags/vX.Y.Z
#   SHA                40-character commit sha               (github.sha)
#   RUN_NUMBER         monotonic workflow run counter        (github.run_number)
#   PRERELEASE         true|false, release events only       (github.event.release.prerelease)
#   MAKE_LATEST        true|false, release events only. Gates the tags that must
#                      never move backwards: `latest` and the bare major `X`.
#                      Defaults to false, because every way of losing this value
#                      should leave those pointers where they are.
#
# Output (stdout): `key=value` lines, directly appendable to $GITHUB_OUTPUT.
#   version=   semantic version carried in org.opencontainers.image.version
#   channel=   stable | prerelease | dev
#   push=      true|false, whether the run publishes to the registry
#   tags=      comma-separated image tag suffixes, never empty
#
# Any input that does not match the documented contract is a hard failure. A
# release run must never guess at a version.
set -eu

fail() {
	printf 'release-version: %s\n' "$1" >&2
	exit 1
}

EVENT_NAME=${EVENT_NAME:-}
REF=${REF:-}
SHA=${SHA:-}
RUN_NUMBER=${RUN_NUMBER:-}
PRERELEASE=${PRERELEASE:-false}
MAKE_LATEST=${MAKE_LATEST:-false}

[ -n "$EVENT_NAME" ] || fail 'EVENT_NAME is required'
[ -n "$REF" ] || fail 'REF is required'
printf '%s' "$SHA" | grep -Eq '^[0-9a-f]{40}$' ||
	fail "SHA must be a 40-character commit sha (got '$SHA')"
printf '%s' "$RUN_NUMBER" | grep -Eq '^[0-9]+$' ||
	fail "RUN_NUMBER must be a non-negative integer (got '$RUN_NUMBER')"
case "$PRERELEASE" in
true | false) ;;
*) fail "PRERELEASE must be true or false (got '$PRERELEASE')" ;;
esac
case "$MAKE_LATEST" in
true | false) ;;
*) fail "MAKE_LATEST must be true or false (got '$MAKE_LATEST')" ;;
esac

short_sha=$(printf '%s' "$SHA" | cut -c1-7)

# X.Y.Z with no leading zeroes, matching semver 2.0.0 core versions.
core_pattern='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
# X.Y.Z optionally followed by a dot-separated pre-release identifier.
tag_pattern='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'

case "$EVENT_NAME" in
release)
	case "$REF" in
	refs/tags/*) tag=${REF#refs/tags/} ;;
	*) fail "a release event must carry a refs/tags/* ref (got '$REF')" ;;
	esac
	printf '%s' "$tag" | grep -Eq "$tag_pattern" ||
		fail "release tag must be vX.Y.Z or vX.Y.Z-<prerelease> (got '$tag')"
	version=${tag#v}
	case "$version" in
	*-*)
		# A pre-release identifier in the tag is authoritative on its own: it
		# pins exactly one immutable tag and never moves a channel pointer.
		channel=prerelease
		tags=$version
		;;
	*)
		if [ "$PRERELEASE" = true ]; then
			channel=prerelease
			tags=$version
		else
			channel=stable
			major=${version%%.*}
			rest=${version#*.}
			minor=${rest%%.*}
			# `X.Y` always follows the newest patch of its own minor line, which
			# is what a minor pointer means. `X` and `latest` span lines, so a
			# patch published for an older line (v1.3.5 after v1.4.0) must not
			# claim them -- that would move a pointer backwards.
			tags="$version,$major.$minor"
			if [ "$MAKE_LATEST" = true ]; then
				tags="$tags,$major,latest"
			fi
		fi
		;;
	esac
	push=true
	;;
push)
	case "$REF" in
	refs/heads/release/*) branch_version=${REF#refs/heads/release/} ;;
	*) fail "a push event must target refs/heads/release/* (got '$REF')" ;;
	esac
	branch_version=${branch_version#v}
	printf '%s' "$branch_version" | grep -Eq "$core_pattern" ||
		fail "release branch must be named release/X.Y.Z or release/vX.Y.Z (got '$REF')"
	# Release-branch builds are release candidates for the version the branch
	# names. They are never `latest` and never claim the bare version, which
	# stays reserved for the published GitHub Release.
	version="$branch_version-rc.$RUN_NUMBER"
	channel=prerelease
	tags="$version,$branch_version-rc"
	push=true
	;;
workflow_dispatch)
	# Manual runs exist to prove the pipeline builds. They never publish, so
	# they can carry no version claim beyond the commit they came from.
	version="0.0.0-dev.$RUN_NUMBER"
	channel=dev
	tags=''
	push=false
	;;
*)
	fail "unsupported event '$EVENT_NAME'"
	;;
esac

# Every run gets an immutable commit-addressed tag, so any published digest can
# be traced back to a revision even when the moving tags have advanced.
tags="${tags:+$tags,}sha-$short_sha"

printf 'version=%s\n' "$version"
printf 'channel=%s\n' "$channel"
printf 'push=%s\n' "$push"
printf 'tags=%s\n' "$tags"
