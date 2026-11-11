# jiku-github-sync

Jiku connector for GitHub: it keeps the requirements of a Jiku project in sync with the issues of its
GitHub repos and the Status of a GitHub Project (v2). Creation, state, priority, type and comments
travel both ways.

It is an **external** connector: Jiku works the same without it and needs no change to include it. All the
mapping and sync state live in the connector's configuration and its KV space; it talks to Jiku only
through the public bus contract, as a machine user with the `internal-app` role.

> **Status: proof of concept in progress.**

## Commands

```
jiku-github-sync [serve]       runs the worker (default)
jiku-github-sync healthcheck   queries /readyz of the local process (for the Docker HEALTHCHECK)
jiku-github-sync version
```

Configuration is read from the environment: see [`.env.dist`](.env.dist).

## Development

```sh
go build ./... && go vet ./... && golangci-lint run ./...
go test -race ./...
docker build --build-arg VERSION=local -t gravadigital/jiku-github-sync:local .
docker run --rm -p 8080:8080 gravadigital/jiku-github-sync:local   # curl localhost:8080/readyz
```

## CI and images

`.github/workflows/`:

- `ci.yml`: build, vet, lint, tests and `docker build` on every push to `main`/`dev` and on PRs.
- `dev-images.yml`: publishes `gravadigital/jiku-github-sync:dev` and `:dev-<sha>` on every push to `dev`.
- `release.yml`: a `vX.Y.Z` tag publishes `X.Y.Z`, `X.Y` and `latest`; it requires the CHANGELOG section
  (see [`CHANGELOG.md`](CHANGELOG.md#cutting-a-release)).

Repository secrets: `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN`.

## License

[Apache-2.0](LICENSE) © Grava Digital.
