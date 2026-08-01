#!/bin/sh
# Renders compose.tls-stack.yaml from compose.yaml + compose.tls.yaml.
#
# Portainer's web editor takes a single file and has no equivalent of
# `-f a -f b`, so the TLS stack has to exist as one document. Generating it
# rather than maintaining a second copy by hand is the point: two full stacks
# drift, and the way you find out is a deployment that behaves differently from
# the one you tested.
#
#   sh scripts/render-tls-stack.sh            rewrite the file
#   sh scripts/render-tls-stack.sh --check    fail if it is out of date
#
# --no-interpolate keeps ${MOIRAI_...} placeholders unexpanded, so the generated
# file stays configurable through Portainer's environment-variable form. Without
# it, every default would be baked in and overriding anything would mean editing
# the YAML.
set -eu

target=compose.tls-stack.yaml
check=false
[ "${1:-}" = "--check" ] && check=true

render() {
	cat <<'HEADER'
# Moirai with an encrypted control stream — one file, for Portainer.
#
# GENERATED. Do not edit: `make compose-tls-stack` renders it from
# compose.yaml + compose.tls.yaml, and `make compose-overlays` fails if the two
# have drifted. Edit those, then regenerate.
#
# Use this when you paste a stack into Portainer's web editor, which takes one
# file. On a machine with a shell, prefer
#
#   docker compose -f compose.yaml -f compose.tls.yaml up -d
#
# What the encryption is for: the orchestrator refuses to hand a runner a
# project's own credential over a channel it cannot encrypt. With this stack a
# runner needs no GitHub token of its own — each job is given its project's
# credential, for as long as it holds that job's lease. Without it, runners fall
# back to the shared GITHUB_TOKEN below, which is the same credential for every
# project.
#
# This does NOT put the console behind TLS. That is still plain HTTP on the
# published port, and is a separate concern.
#
# The certificate is self-signed, generated on first start into a volume, and
# names only "orchestrator" — the service name on the internal network, which is
# the only name anything dials it by.
HEADER
	docker compose -f compose.yaml -f compose.tls.yaml config --no-interpolate
}

if [ "$check" = true ]; then
	rendered=$(render)
	if [ "$rendered" != "$(cat "$target")" ]; then
		echo "$target is out of date; run: make compose-tls-stack" >&2
		exit 1
	fi
	echo "$target is up to date"
	exit 0
fi

render >"$target"
echo "wrote $target"
