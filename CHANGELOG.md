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

- Repository skeleton: Go module, `serve` / `healthcheck` / `version` commands, `/healthz` and
  `/readyz`, distroless Docker image and CI (`ci.yml`, `dev-images.yml`, `release.yml`).
