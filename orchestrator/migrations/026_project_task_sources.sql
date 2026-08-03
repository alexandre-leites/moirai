-- Issue #293: a project draws work from 1..N task sources (GitHub, Linear,
-- Jira, a local file directory, ...), not exactly one. Before this, a
-- project's tracker was a single scalar (app.projects.issue_tracker_type),
-- which meant "GitHub and Linear together", "two GitHub repositories", and
-- "no tracker at all" were all inexpressible. This migration introduces the
-- first-class app.project_task_sources row #293 proposed and re-keys the two
-- tables that used to assume one source per project onto it.
--
-- app.issues already coexists across providers under one project (it was
-- keyed UNIQUE(project_id, provider, external_id)); this migration sharpens
-- that to UNIQUE(task_source_id, external_id), which additionally allows two
-- *instances* of the same provider (two GitHub repos) without collision --
-- something the old key could not express since it only ever recorded one
-- row's worth of "provider" per project.

CREATE TABLE IF NOT EXISTS app.project_task_sources (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES app.projects(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  name TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT true,
  -- Provider-specific connection details (repo slug, JQL, team id, local
  -- path, ...), read by the adapter that provider selects. #294's descriptor
  -- gives this shape meaning field-by-field; until then it is opaque to
  -- everything except the adapter that reads it.
  configuration JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Deliberately NOT unique on (project_id, provider): two GitHub
  -- repositories feeding one project is a reasonable thing to want, and a
  -- stable id per row (rather than "the GitHub one") is what makes that
  -- expressible. name is the operator-facing handle that tells two sources
  -- of the same provider apart.
  UNIQUE (project_id, name)
);
CREATE INDEX IF NOT EXISTS project_task_sources_project_id_idx ON app.project_task_sources (project_id);

-- Data migration: every project that exists today has exactly one implied
-- source (its issue_tracker_type, defaulting to 'github'), reading from
-- whatever repository_url held -- syncProject passed that column verbatim as
-- "ref" regardless of provider (including for issue_tracker_type='local_file'
-- projects, which stored a directory path there; see integration_test.go's
-- local_file coverage predating this migration). Materialising one row per
-- project with configuration->>'ref' equal to that same value is what lets
-- an existing deployment keep syncing with zero manual intervention: nothing
-- else in this migration depends on an operator doing anything.
INSERT INTO app.project_task_sources (id, project_id, provider, name, enabled, configuration, created_at, updated_at)
SELECT gen_random_uuid(), p.id, COALESCE(NULLIF(p.issue_tracker_type, ''), 'github'), 'default', true,
       jsonb_build_object('ref', p.repository_url), now(), now()
FROM app.projects p;

-- app.issues: add task_source_id, backfill from the single source each
-- project now has (unambiguous: the data migration above created exactly
-- one row per project_id), then require it and replace the old key.
ALTER TABLE app.issues ADD COLUMN IF NOT EXISTS task_source_id UUID REFERENCES app.project_task_sources(id) ON DELETE CASCADE;

UPDATE app.issues i
SET task_source_id = ts.id
FROM app.project_task_sources ts
WHERE ts.project_id = i.project_id AND i.task_source_id IS NULL;

ALTER TABLE app.issues ALTER COLUMN task_source_id SET NOT NULL;

ALTER TABLE app.issues DROP CONSTRAINT IF EXISTS issues_project_id_provider_external_id_key;
ALTER TABLE app.issues ADD CONSTRAINT issues_task_source_id_external_id_key UNIQUE (task_source_id, external_id);
CREATE INDEX IF NOT EXISTS issues_task_source_id_idx ON app.issues (task_source_id);

-- project_id stays on app.issues (denormalised, still NOT NULL): every
-- scheduling query (ListQueueEntries, GetSchedulerSnapshot,
-- ClaimSchedulableIssue, IssueSyncStatusEntries) joins issues to projects
-- directly and has no reason to care which source an issue came from, so
-- re-keying does not have to touch any of them. provider also stays, for the
-- same reason app.issues kept it before this migration: display, without a
-- join back to project_task_sources.

-- app.issue_sync_state: same treatment. Re-keying onto task_source_id is
-- what makes health, backoff and last-error per source instead of per
-- project -- one source (an expired Jira token) backing off no longer hides
-- in the same row as a healthy sibling GitHub source on the same project.
ALTER TABLE app.issue_sync_state ADD COLUMN IF NOT EXISTS task_source_id UUID REFERENCES app.project_task_sources(id) ON DELETE CASCADE;

UPDATE app.issue_sync_state s
SET task_source_id = ts.id
FROM app.project_task_sources ts
WHERE ts.project_id = s.project_id AND s.task_source_id IS NULL;

ALTER TABLE app.issue_sync_state DROP CONSTRAINT IF EXISTS issue_sync_state_pkey;
ALTER TABLE app.issue_sync_state ALTER COLUMN task_source_id SET NOT NULL;
ALTER TABLE app.issue_sync_state ADD PRIMARY KEY (task_source_id);
-- project_id stays too (denormalised): IssueSyncStatusEntries reports a
-- per-project aggregate across a project's sources (the console's sync
-- panel is out of scope for #293 -- see PR description -- and continues to
-- read one row per project), and that aggregate is a straight GROUP BY on
-- this column.

-- app.project_credentials: the piece #293 flagged as needing a decision, not
-- just a schema change. A project-level credential (github_token,
-- ssh_private_key, agent:*) and a source-scoped one (a second GitHub source's
-- own github_token) both need to fit in the same table without colliding.
--
-- Decision: add a nullable task_source_id rather than making every credential
-- source-scoped. NULL means "project-level", exactly today's meaning and
-- exactly what every existing row already is (this column is added nullable
-- with no backfill, so every pre-existing credential keeps meaning what it
-- always meant). A non-NULL task_source_id scopes a credential to one
-- source, which is what lets two sources of the same provider each hold their
-- own github_token without fighting over the one project-level slot -- the
-- collision #293 describes.
--
-- Postgres treats NULLs as distinct in a UNIQUE constraint, so a plain
-- UNIQUE(project_id, kind, task_source_id) would let the same project store
-- two different project-level (task_source_id NULL) secrets of the same
-- kind, which is exactly the ambiguity a single credential slot is supposed
-- to prevent. Two partial unique indexes instead: one enforces "at most one
-- project-level credential per kind", the other "at most one credential per
-- kind per source" -- each row falls into exactly one of the two, so they
-- never have to agree on how to treat a NULL together.
ALTER TABLE app.project_credentials ADD COLUMN IF NOT EXISTS task_source_id UUID REFERENCES app.project_task_sources(id) ON DELETE CASCADE;

ALTER TABLE app.project_credentials DROP CONSTRAINT IF EXISTS project_credentials_pkey;

CREATE UNIQUE INDEX IF NOT EXISTS project_credentials_project_kind_global_uidx
  ON app.project_credentials (project_id, kind) WHERE task_source_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS project_credentials_project_kind_source_uidx
  ON app.project_credentials (project_id, kind, task_source_id) WHERE task_source_id IS NOT NULL;

-- app.projects.issue_tracker_type is superseded entirely: every project now
-- has its sources in app.project_task_sources (at least one, thanks to the
-- data migration above), and nothing reads this column any more once
-- server.go's sync path is re-pointed at the new table. It was never
-- settable through the API to begin with (neither ProjectConfiguration nor
-- Project exposes it in proto/control_plane.proto), so dropping it changes
-- no external behaviour.
ALTER TABLE app.projects DROP COLUMN IF EXISTS issue_tracker_type;

-- app.projects.code_host_type is left exactly as-is: a project delivers to
-- 0..1 code hosts, not 1..N (see #291/#293's "task sources are plural; the
-- code host is not"), so it stays a scalar column. This migration's
-- multiplicity work is scoped to task sources only.
