#!/bin/sh
set -eu

# Materialises secrets where the unprivileged orchestrator user can read them,
# from either a mounted secret file or a plain environment variable.
#
# The environment fallback exists so the image can be deployed where file
# secrets are awkward -- Portainer stacks, plain `docker run`, schedulers that
# only pass environment.
#
# Each variable is unset once captured, for two reasons: the config rejects a
# value and its _FILE twin together, and the secret should not reach the server
# process's environment where anything it spawns would inherit it. Note this
# does not hide the value from `docker inspect` or a UI that renders it --
# Compose set it on the container, and only file secrets avoid that.
#
# Empty counts as absent. A blank file is how "I did not configure this" arrives
# through templating systems that cannot omit a key, and the config treats an
# empty secret as an error rather than as unset.
secret_dir=/run/loop-secrets
mkdir -p "$secret_dir"
chmod 0700 "$secret_dir"

# install <secret-name> <environment-variable> <required>
install_secret() {
  name="$1"
  variable="$2"
  required="$3"
  target="$secret_dir/$name"

  value="$(printenv "$variable" 2>/dev/null || true)"
  # Unset unconditionally, including when empty. The config treats a variable
  # that is present-but-blank as a misconfiguration rather than as unset, so an
  # optional secret left empty by a template would otherwise refuse to start.
  unset "$variable" 2>/dev/null || true

  if [ -f "/run/secrets/$name" ] && [ -s "/run/secrets/$name" ]; then
    cp "/run/secrets/$name" "$target"
  elif [ -n "$value" ]; then
    printf '%s' "$value" > "$target"
  elif [ "$required" = "required" ]; then
    echo "orchestrator: provide $name as the secret file /run/secrets/$name or as $variable" >&2
    exit 1
  else
    return 0
  fi

  chmod 0400 "$target"
  chown loop:loop "$target"
  return 0
}

install_secret database_url LOOP_DATABASE_URL required
install_secret initial_admin_password LOOP_INITIAL_ADMIN_PASSWORD required
install_secret runner_registration_token RUNNER_REGISTRATION_TOKEN required
install_secret github_token LOOP_GITHUB_TOKEN optional

chown loop:loop "$secret_dir"
export LOOP_DATABASE_URL_FILE="$secret_dir/database_url"
export LOOP_INITIAL_ADMIN_PASSWORD_FILE="$secret_dir/initial_admin_password"
export RUNNER_REGISTRATION_TOKEN_FILE="$secret_dir/runner_registration_token"
# Only when one was actually supplied: the token is optional, and pointing the
# variable at a file that does not exist would turn "no token" into a crash.
if [ -f "$secret_dir/github_token" ]; then
  export LOOP_GITHUB_TOKEN_FILE="$secret_dir/github_token"
fi

exec gosu loop python -m moirai.main "$@"
