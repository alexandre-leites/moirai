#!/bin/sh
set -eu

# Materialises the registration token where the unprivileged runner user can
# read it, from either a mounted secret file or a plain environment variable.
#
# The environment fallback exists so the image can be deployed where file
# secrets are awkward -- Portainer stacks, plain `docker run`, schedulers that
# only pass environment. The variable is unset once written, so the token does
# not reach the runner process and cannot be inherited by an agent it spawns.
# It is still visible to `docker inspect` when supplied this way; file secrets
# are the only form that avoids that.
secret_dir=/run/loop-secrets
mkdir -p "$secret_dir"
chmod 0700 "$secret_dir"

token_path="$secret_dir/runner_registration_token"
if [ -f /run/secrets/runner_registration_token ] && [ -s /run/secrets/runner_registration_token ]; then
  cp /run/secrets/runner_registration_token "$token_path"
elif [ -n "${LOOP_RUNNER_REGISTRATION_TOKEN:-}" ]; then
  printf '%s' "$LOOP_RUNNER_REGISTRATION_TOKEN" > "$token_path"
else
  echo "runner: provide the registration token as the secret file /run/secrets/runner_registration_token or as LOOP_RUNNER_REGISTRATION_TOKEN" >&2
  exit 1
fi

unset LOOP_RUNNER_REGISTRATION_TOKEN 2>/dev/null || true
chmod 0400 "$token_path"
chown loop:loop "$token_path"
chown loop:loop "$secret_dir"
export LOOP_RUNNER_REGISTRATION_TOKEN_FILE="$token_path"

# A private key delivered by the control plane is written here. The directory
# has to exist and belong to the runner user *before* privileges are dropped:
# /run is a root-owned tmpfs, so an unprivileged mkdir inside it fails, and it
# failed at the only moment that mattered -- midway through a job, as
# "create key directory: mkdir /run/moirai: permission denied", after the
# execution had already been accepted.
#
# Created here rather than in the image because it lives on a tmpfs that is
# mounted over whatever the image contained.
key_dir="${LOOP_RUNNER_KEY_DIR:-/run/moirai/keys}"
mkdir -p "$key_dir"
chmod 0700 "$key_dir"
chown -R loop:loop "$key_dir"
# The parent too: /run/moirai is created by the mkdir -p above and would
# otherwise stay root-owned, which is enough to stop the runner replacing the
# directory if it ever needs to.
parent_dir=$(dirname "$key_dir")
[ "$parent_dir" = "/run" ] || chown loop:loop "$parent_dir"

exec su-exec loop /runner "$@"
