# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versioning: [SemVer](https://semver.org/).

## Versioning policy

The connector has a single version number, the one of the `vX.Y.Z` tag. It is not written in any code
file: the binary receives it at build time (`ARG VERSION` in the `Dockerfile`) and
`jiku-github-sync version` prints it.

| Bump      | When |
| --------- | ---- |
| **major** | A breaking change in the configuration (mapping format, environment variables) or in the state kept in the KV that needs a migration. |
| **minor** | New functionality, backwards compatible. |
| **patch** | Fixes and internal changes with no effect on configuration or state. |

### Cutting a release

`[Unreleased]` is moved to a `## [X.Y.Z] - YYYY-MM-DD` section, committed, brought to `main` and tagged:

```sh
git tag vX.Y.Z && git push origin vX.Y.Z
```

Pushing the tag is what publishes. `release.yml` checks that the CHANGELOG has the tag's section, runs vet,
lint and the tests, and only then publishes `gravadigital/jiku-github-sync` to Docker Hub with the tags
`X.Y.Z`, `X.Y` and `latest`. A tag without its section fails before anything is built. The step by step
for agents is in [CLAUDE.md](CLAUDE.md#cutting-a-release).

## [Unreleased]

### Added

- **`githubProject` accepts what the Project's page shows.** Besides the node id (`PVT_…`, which
  only the API or `gh` gave), it now takes `owner/number` (`acme-org/12`) or the Project's URL
  (`https://github.com/orgs/acme-org/projects/12`, `users/<login>/…` too). The connector looks the
  node id up with the project's own credentials on start and on every reload; an unchanged reference
  is not looked up again on a reload. If it cannot be found (wrong number, or the credentials do not
  see it), the service does not start, and a reload keeps the previous mapping and logs why. With a
  GitHub App credential the Project's owner must be the organization of the project's repos.
  Existing mappings with a node id keep working unchanged.

## [1.1.0] - 2026-11-17

Tasks of the requirements, synced with sub-issues of their issues. Off until a project turns it on:
an installation upgraded from 1.0.0 keeps working exactly as before, on the same state.

### Added

- **Tasks ↔ sub-issues (opt-in per project, `tasks.enabled`).** A public task of a synced requirement
  becomes a sub-issue of the requirement's issue, in the same repository; a new sub-issue of a synced
  issue becomes a public task of its requirement (state `backlog`, no responsible people). Title,
  description, state, priority (when the sub-issue is on the Project), comments and attachments are
  synced as for requirements, the latest change winning field by field. `finalizado` / `cancelado`
  close the sub-issue as completed / not planned; reopening goes back to the last open state.
- **Hierarchy both ways.** Moving a task to another synced requirement moves its sub-issue under that
  requirement's issue, and moving a sub-issue under another synced issue moves the task; when both
  moved, the latest change wins (GitHub's timeline says when the parent changed).
- **What stays out, and how pairs stop syncing.** Tasks without a requirement, internal tasks and
  tasks of a requirement that is not synced never leave Jiku; sub-issues of sub-issues are ignored
  (Jiku has no subtasks). A task that becomes internal, or leaves the synced requirements, stops
  syncing with a note on the sub-issue (moved to an unsynced requirement, the sub-issue is also
  taken out of its parent). A sub-issue detached on GitHub, or moved under an issue that is not
  synced, stops syncing with an internal note on the task, which stays where it is in Jiku; each
  pair is revived only by the side that unlinked it. A requirement that stops syncing takes its
  tasks with it, and brings them back when it is revived; a requirement that starts syncing brings
  its public tasks.
- **An issue already synced as a requirement that becomes a sub-issue stays a requirement** (Jiku
  cannot turn one into a task): an internal note is left on it once.
- **Tasks' initial sync** (`tasks.initialSync`: `plan` by default, `apply`, `off`): once tasks are
  turned on, the open public tasks of synced requirements and the open sub-issues of their issues
  are linked (same title, unique under that requirement, or the connector's marker) or created on
  the other side. It runs inside `serve`, after the requirements' initial sync, and is logged one
  line per item like the requirements' plan.
- **`taskStatus` in a field preset**: the Status mapping for tasks' states (`backlog`, `activo`,
  `en_revision`, `finalizado`, `cancelado`). Without it the sub-issues are not added to the Project
  and only open/closed is synced. `tasks.issueType` sets the issue type of the sub-issues created
  from tasks. `check-config` and the mapping check validate `taskStatus` against the Project.
- **Parents sweep.** Linking or unlinking a sub-issue does not change either issue's `updated_at`, so
  the poller reads the parents of each repo's open issues (one GraphQL query per 100 issues, only for
  projects with tasks) and syncs the ones whose parent changed. The webhook receiver takes the
  `sub_issues` event.
- **Verification** compares task pairs too (fields, state, column, parent, comments) and lists open
  public tasks of synced requirements without a sub-issue and open sub-issues of synced issues
  without a task.

### Changed

- The Jiku event consumer subscribes to every event type and keeps requirement and task events
  (task events only for projects with tasks turned on), so turning tasks on by reloading the mapping
  needs no restart.

### Notes for existing installations

- **Nothing changes until a project turns tasks on.** Without a `tasks` section the connector syncs
  exactly as before (a new sub-issue is still imported as a requirement), and the state written by
  1.0.0 is read as it is: no migration.
- **To turn tasks on**, add a `taskStatus` to the project's field preset (optional) and
  `tasks: { enabled: true }` to the project. The tasks' initial sync starts in `plan`: read the plan in
  the logs (`tasks initial sync plan`), then set `tasks.initialSync: apply`. New tasks and sub-issues
  are synced from the moment tasks are turned on, whatever the mode.
- **Sub-issues created while running 1.0.0 were imported as requirements**, and stay requirements
  (each gets an internal note when tasks are turned on; the plan lists them as skipped).
- **GitHub permissions:** none new. Sub-issues are written with the same Issues read/write
  permission (`repo` scope for a token).
- **Jiku permissions:** none new. The `internal-app` role already allows `tasks.new`,
  `tasks.{id}.edit`, `tasks.{id}.comment` and the tasks and attachments queries. Comments copied to a
  task notify nobody (Jiku has no task subscriptions).
- **State in the KV**: new keys `link.t.*`, `link.ti.*`, `pending.t.*`, `pending.ti.*` and new
  optional fields in `project.*` and in links. Existing keys keep their shape.

## [1.0.0] - 2026-11-12

First release: a service that keeps the requirements of Jiku projects and the issues of GitHub
repositories (and their Project v2) in sync, from outside Jiku.

### Added

- **Sync both ways between Jiku requirements and GitHub issues.** Every public requirement of a
  mapped project gets its issue; every new issue of a mapped repository becomes a public requirement
  and is added to the project's GitHub Project. Title, description, state (open/closed and the
  Project's Status column), priority and type are synced, and the latest change wins, field by
  field. Internal requirements never leave Jiku; a requirement that becomes internal stops syncing
  (a note is left on the issue) and is revived if it becomes public again.
- **States.** `resuelto` / `cancelado` close the issue as completed / not planned; the open states
  map to Status columns. Closing the issue of an `incidencia` resolves it with
  `resolutionType: otro`; reopening goes back to the last open state. A column can map to a state
  plus an internal comment (e.g. a blocked column → `en_cola`); an unmapped column changes nothing
  and leaves an internal comment. Cards left without a column are put back from Jiku.
- **Comments.** Public Jiku comments go to GitHub from the connector's account with an attribution
  line; GitHub comments become public Jiku comments with a link to the original. Edits follow their
  source; a comment deleted on GitHub is marked as deleted in Jiku.
- **Attachments of descriptions and comments.** Jiku images become embedded GitHub attachments;
  other Jiku files are named on GitHub. GitHub attachments become Jiku files of the requirement or
  comment, or a note with a link when Jiku refuses them. Each pair remembers the files present on
  both sides, so texts edited back and forth do not copy them again.
- **Mapping file** (`SYNC_CONFIG`): projects, repositories, the GitHub Project, and reusable field
  presets resolved by name at runtime (states, priorities, types), so a Project's options can change
  without a code change. Hot reload; a reload that does not validate is rejected and the previous
  mapping keeps working.
- **GitHub credentials per client**: a named token (from an environment variable or a file) or the
  connector's GitHub App with one installation per organization. Each credential is its own client
  with its own rate limits; the identity of each one is logged on startup.
- **Initial sync** of what already exists (`initialSync: plan | apply | off`), run by the service
  itself: open requirements and issues with their comments; existing items are linked only when
  their titles are exactly the same, anything else is created on the other side. `plan` logs the
  plan without writing.
- **Service operation**: GitHub polling (issues, comments, Project item values), Jiku's
  requirement events, a periodic reconciliation that catches whatever a missed event or poll left
  behind (including requirements that became public or were reopened while the connector was down),
  a periodic read-only verification of every pair, an HMAC-verified webhook receiver, `/healthz` and
  `/readyz`. Fresh changes are synced before background work.
- **Recovery**: creations interrupted between the write and storing the pair are found again by
  their markers, never duplicated; the connector's own writes never echo back.
- **State** in the connector's own space of Jiku's KV: no change in Jiku, no tags.
- Startup and `check-config` report a mapped repository that is public.
- Diagnostic commands: `check-config`, `sync-inicial [--dry-run]`, `verify`, `healthcheck`,
  `version`.
- Distroless Docker image (`gravadigital/jiku-github-sync`), CI (`ci.yml`, `dev-images.yml`,
  `release.yml`) and `docker-compose.local.yml` to run next to Jiku's local stack.
