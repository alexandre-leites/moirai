#!/bin/sh
set -eu

secret_dir=/run/loop-secrets
mkdir -p "$secret_dir"
cp /run/secrets/runner_registration_token "$secret_dir/runner_registration_token"
chmod 0700 "$secret_dir"
chmod 0400 "$secret_dir/runner_registration_token"
chown loop:loop "$secret_dir/runner_registration_token"
chown loop:loop "$secret_dir"
export LOOP_RUNNER_REGISTRATION_TOKEN_FILE="$secret_dir/runner_registration_token"
exec su-exec loop /runner "$@"
