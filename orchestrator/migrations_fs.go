// Package orchestrator holds assets that must ship inside the orchestrator
// binary rather than be discovered on disk next to it.
package orchestrator

import "embed"

// Migrations embeds every file under migrations/ so applying migrations no
// longer depends on the process working directory: previously
// migrate.Apply(ctx, url, os.DirFS(".")) only worked because the Docker image
// happens to set WORKDIR /app and copy migrations there. Any other
// invocation - go run from the repo root, a systemd unit without
// WorkingDirectory - failed at startup with an opaque "load migration
// source" error. The embedded binary is self-contained: this directory
// listing is baked in at compile time, so where the process runs from no
// longer matters.
//
//go:embed migrations/*.sql
var Migrations embed.FS
