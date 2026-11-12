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
