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
`X.Y.Z`, `X.Y` and `latest`. A tag without its section fails before anything is built.

## [Unreleased]

### Added

- Repository skeleton: Go module, distroless Docker image, CI (`ci.yml`, `dev-images.yml`,
  `release.yml`) and `docker-compose.local.yml` to run next to Jiku's local stack.
- State-based sync engine between Jiku requirements and GitHub issues + a Project v2: creation both
  ways (public requirements and new issues), title, description, state (open/closed and the
  Project's Status), priority, type, comments with edits and deletions, visibility changes, issue
  transfers and recovery of interrupted creations.
- Mapping file with reusable presets resolved by name at runtime and hot reload; `check-config`.
- Worker: GitHub polling (issues, comments, Project item values), Jiku's requirement events,
  periodic reconciliation, HMAC-verified webhook receiver, `/healthz` and `/readyz`.
- `sync-inicial`: one-time sync of what already exists, with a reviewable plan and `--dry-run`.
- State in the connector's space of Jiku's KV (no Jiku change, no tags).
- GitHub credentials per client in the mapping (token from an environment variable or a file, or
  the connector's GitHub App with one installation per organization); each credential is its own
  client with its own rate limits; reloads with unusable credentials are rejected.
- The service runs the initial sync (`initialSync: plan | apply | off`), the mapping check and a
  periodic verification itself; fresh changes are synced before background work.
- Attachments of descriptions and comments both ways: Jiku images become embedded GitHub
  attachments, other Jiku files are named; GitHub attachments become Jiku files (or a note with a
  link when Jiku refuses them). Each pair remembers the files present on both sides.

### Changed

- The periodic verification runs apart from the sync queue, so it no longer delays fresh changes
  for as long as it takes to read every pair.
- The state keeps one description hash per side (`jBody`, `gBody`); links written before keep
  working through the old `bodyHash`.
