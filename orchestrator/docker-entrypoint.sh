#!/bin/sh
set -eu

secret_dir=/run/loop-secrets
mkdir -p "$secret_dir"
chmod 0700 "$secret_dir"
for name in database_url initial_admin_password runner_registration_token; do
  cp "/run/secrets/$name" "$secret_dir/$name"
  chmod 0400 "$secret_dir/$name"
  chown loop:loop "$secret_dir/$name"
done
if [ -f /run/secrets/github_token ]; then
  cp /run/secrets/github_token "$secret_dir/github_token"
  chmod 0400 "$secret_dir/github_token"
  chown loop:loop "$secret_dir/github_token"
  export LOOP_GITHUB_TOKEN_FILE="$secret_dir/github_token"
fi
chown loop:loop "$secret_dir"
export LOOP_DATABASE_URL_FILE="$secret_dir/database_url"
export LOOP_INITIAL_ADMIN_PASSWORD_FILE="$secret_dir/initial_admin_password"
export RUNNER_REGISTRATION_TOKEN_FILE="$secret_dir/runner_registration_token"
exec gosu loop python -m moirai.main "$@"
