# jiku-github-sync

Jiku connector for GitHub: it keeps the requirements of a Jiku project in sync with the issues of
its GitHub repos and with a GitHub Project (v2). Creation, title, description, state, priority,
type and comments travel both ways.

It is an **external** connector: Jiku works the same without it and needs no change to include
it. All the mapping and sync state live in the connector's configuration and in its own space of
Jiku's KV; it talks to Jiku only through the public bus contract, as a machine user with the
`internal-app` role. Nothing is written into Jiku's data for the connector's sake (no tags, no
properties).

> **Status: proof of concept.** Everything below works end to end against a local Jiku and a
> test GitHub repo and Project. Receiving webhooks needs a GitHub App installed on the
> organization (see [Limitations](#known-limitations)).

## What it syncs

| # | Rule | Direction |
|---|---|---|
| R1 | Every **public** requirement of a mapped project creates its issue in the project's `defaultRepo`. Internal ones never leave Jiku | Jiku → GitHub |
| R2 | Every **new issue** of a mapped repo creates a **public** requirement | GitHub → Jiku |
| R3 | When the project maps a GitHub Project, the issue is added to it | GitHub |
| R4 | A requirement that becomes internal stops syncing (a note is left on the issue); one that becomes public creates its issue, or revives its old pair | Jiku → GitHub |
| R5 | Title, description, state, priority, type: **the latest change wins** | ↔ |
| R6 | Public Jiku comments → issue comments, from the connector's account, with "**Name** comentó en Jiku:" | Jiku → GitHub |
| R7 | Issue comments → public Jiku comments, with "**@login** comentó en [GitHub](link):" | GitHub → Jiku |
| R8 | Comment edits follow their source; a comment deleted on GitHub is marked as deleted in Jiku (Jiku has no delete) | ↔ |
| R10 | A mapped repo that is public is reported on startup and by `check-config` | — |

States: `resuelto` / `cancelado` close the issue as `completed` / `not_planned`; the open states
map to columns of the Project's Status field. Closing the issue of an `incidencia` resolves it in
Jiku with `resolutionType: otro` and a conclusion naming who closed it. Reopening goes back to the
last open state. A column with no mapping changes nothing in Jiku and leaves an internal comment;
a column can be mapped to a state plus an internal comment (e.g. a "Blocked" column → `en_cola`).
A card left without a column (e.g. after a Project's options were replaced) is put back where
Jiku's state says.

Only issues and requirements **created after the connector started watching a project** are
synced automatically; what already existed is brought in once by the **initial sync**, which the
service itself runs when the project's `initialSync` is `apply` (see below).

## How it works

The engine is **state-based**: whatever triggers it — a Jiku event, a GitHub poll, a webhook, the
periodic reconciliation — it reads both sides as they are now and compares each field with what
both sides looked like right after the last sync (stored per pair). The side that changed is
pushed; if both changed, the most recent change wins. That makes every run idempotent: duplicated
or out-of-order triggers find nothing to do, and the connector's own writes never echo back.

```
   GitHub (org)                                         Jiku
   repos · Project v2                     jiku-commands · jiku-queries · JIKU_EVENTS · JIKU_KV
        │  polling (issues, comments, item values)            ▲   │ requirement events
        │  webhooks (optional, GitHub App)                    │   ▼
        ▼                                                     │
   ┌──────────────────────────── jiku-github-sync ───────────────────────────────┐
   │ one deduplicating queue → one worker → engine (per pair: read, compare,       │
   │ write) · periodic reconciliation · hot-reloaded mapping · /healthz /readyz   │
   └──────────────────────────────────────────────────────────────────────────────┘
```

- **Anti-loop / recovery:** a hidden marker on everything the connector writes on GitHub
  (`<!-- jiku-github-sync:… -->`), the attribution link on everything it writes in Jiku, and a
  "pending" record before every creation, so a crash between creating something and storing its
  link never produces a duplicate.
- **Who moved a card** is read from the issue's timeline (`ProjectV2ItemStatusChangedEvent`): a
  Project field value's `creator` does not change on later edits. Changes by
  `github-project-automation` are not taken as a person's decision.
- **GitHub limits:** content-creating writes are spaced (`SYNC_WRITE_INTERVAL`) and rate-limit
  answers are waited out.

## The mapping file

Everything about a GitHub Project is configuration, resolved **by name** at runtime: a Project
with other columns uses another preset, and a Project whose columns change only needs its names
updated (the file is reloaded while running). See [`config.example.yaml`](config.example.yaml).

```yaml
fieldMappings:
  my-board:            # status / priority / type, each optional
    status:   { field: Status, toJiku: {...}, toGithub: {...} }
    priority: { field: Priority, map: {...} }
    type:     { sources: [issueType, label], issueType: {...}, label: {...}, toGithub: {...} }
projects:
  - jikuProject: 123
    repos: [acme-org/example-api, acme-org/example-web]
    defaultRepo: acme-org/example-api
    githubProject: PVT_xxxxxxxx     # optional: without it only open/closed is synced
    fieldMapping: my-board
```

## Running as a service

The connector is meant to run as a long-lived container; nothing needs a command:

- **On start and on every reload of the mapping** it checks the mapping against GitHub (repos,
  fields, options, issue types) and logs the findings (`mapping check`, ERROR / WARN).
- **Initial sync**, per project, from the mapping (`initialSync`):
  - `plan` (default): the plan is logged one line per item (`initial sync plan: …`) with a
    summary (what is linked, created, skipped, comments, writes, estimate). Nothing is written.
  - `apply`: the plan is run once, in the same serial queue as the regular sync; when it ends
    without errors it is recorded in the state and never repeated (with errors, it is retried on
    the next start or reload; it is idempotent).
  - Typical flow: map the project, read the plan in the logs, switch to `apply` in the file.
    Manual links go in the project's `links`.
- **After each reconciliation** it logs a read-only verification of every pair
  (`verification`, differences as WARN). `SYNC_VERIFY=false` turns it off.

```sh
docker logs -f jiku-github-sync | jq -r 'select(.msg|startswith("initial sync")) | [.msg, .title // .reason // ""] | @tsv'
```

## Commands

The same operations exist as subcommands, for diagnosis (`docker exec … jiku-github-sync verify 12`):


```
jiku-github-sync [serve]                         runs the worker (default)
jiku-github-sync check-config [<project>]        compares the mapping with GitHub (exit 1 on errors)
jiku-github-sync sync-inicial <project> [--dry-run] [--links <file>]
jiku-github-sync verify <project>                read-only: compares both sides of every pair
jiku-github-sync healthcheck                     queries /readyz (Docker HEALTHCHECK)
jiku-github-sync version
```

- **`check-config`**: that every repo is readable (and whether it is public), that every field
  and option the mapping names exists, which Project options have no mapping, which issue types
  the organization has. Run it when mapping a project and after any change to its board.
- **`sync-inicial`**: one-time sync of everything **open** that already exists, both ways, with its
  comments (their original date goes in the attribution). It prints a plan first — what is
  linked (same marker, a manual link, or a title that is unique on both sides), created, skipped,
  how many writes and how long given GitHub's limits — and `--dry-run` stops there. Manual links:
  a YAML list `[{requirement: 12, issue: owner/repo#3}]`. Running it again is safe. Every comment
  copied into Jiku notifies the requirement's subscribers by mail: the plan says how many.

- **`verify`**: read-only. For every linked pair it compares title, description, state and
  column, priority, type and that every comment has its copy on the other side, and it lists the
  open public requirements and open issues that have no pair. Run it after `sync-inicial`, or
  whenever drift is suspected (exit 1 on differences).

## Running it

### What it needs

- **In Jiku:** a Zitadel machine user for the connector with the `internal-app` role and Access
  Token Type = JWT. Nothing else: no change in Jiku. The first connection creates the identity's
  row in Jiku; the connector waits for it.
- **On GitHub:** a token with access to the mapped repos and Projects. For a trial, a user token
  with `repo` + `project`; in production, a GitHub App (Issues read/write, Organization Projects
  read/write, Metadata read) installed on the organization, which also delivers webhooks.
  Everything the connector writes appears under that identity.

### Configuration

Environment variables: see [`.env.dist`](.env.dist). The essential ones: `JIKU_SERVERS`,
`JIKU_INSTANCE`, `JIKU_CREDS`, `JIKU_KEY_FILE` (or `JIKU_KEY_B64`), `JIKU_PROJECT_ID`,
`GITHUB_TOKEN`, `SYNC_CONFIG`.

### Next to Jiku's local stack

```sh
cp .env.dist .env                                  # identity and bus settings
mkdir -p .local && cp config.example.yaml .local/config.yaml   # adapt it; put the key in .local/
export GITHUB_TOKEN=...                            # passed through, never written to disk
docker compose -f docker-compose.local.yml up -d --build
curl -s localhost:8095/readyz
docker compose -f docker-compose.local.yml exec jiku-github-sync jiku-github-sync check-config
```

### Operations

- Logs are JSON on stderr: every write says which pair, which side and which fields. Tokens,
  bodies and comments never go to the logs.
- `/healthz` (process alive) and `/readyz` (bus connected and a recent successful GitHub poll).
- `POST /webhooks/github` when `GITHUB_WEBHOOK_SECRET` is set (HMAC-verified; `issues`,
  `issue_comment`, `projects_v2_item`). Webhooks are only hints: polling and reconciliation keep
  running, because GitHub does not retry failed deliveries.

## Known limitations

- **Authorship.** Comments from GitHub appear in Jiku under the connector, with the GitHub login in
  the text; comments from Jiku appear on GitHub under the token's account. Jiku has no way for a
  connector to write on behalf of a person, and the connector does not change Jiku.
- **Mails.** Comments and resolutions that arrive from GitHub notify subscribers like any other.
- **Jiku events.** The `internal-app` role cannot ack JetStream messages, so the event consumer is
  ephemeral: events published while the connector is down are not replayed; the periodic
  reconciliation picks up those changes.
- **Webhooks** need a GitHub App installed by an owner of the organization. Without it, changes
  arrive by polling (`SYNC_POLL_INTERVAL`).
- **Attachments** are not copied: GitHub has no API to upload them.
- **Replacing a Project's options through the API** (`updateProjectV2Field`) gives every option a
  new id and empties every card; the connector puts linked cards back from Jiku, but cards of
  unlinked issues stay empty.
- Project fields other than the mapped single-selects (Size, Iteration, dates, …) and sub-issues
  are not synced.

## Development

```sh
go build ./... && go vet ./... && golangci-lint run ./...
go test -race ./...
docker build --build-arg VERSION=local -t gravadigital/jiku-github-sync:local .
```

- Unit tests cover the mapping, the GitHub client (against `httptest`), the webhook receiver, the
  initial-sync plan and, above all, the engine: every rule above runs against in-memory fakes of
  GitHub and Jiku that reproduce GitHub's real behaviour (Project workflows, first-creator field
  values, an asynchronous timeline), and each scenario ends asserting that a second sync writes
  nothing.
- [`test/e2e/e2e.sh`](test/e2e/e2e.sh) drives a **running** connector against a real Jiku and a
  real test repo and Project, acting as a person on both sides. Its header lists the variables.
  Never point it at production data.

## CI and images

`.github/workflows/`:

- `ci.yml`: build, vet, lint, tests and `docker build` on every push to `main`/`dev` and on PRs.
- `dev-images.yml`: publishes `gravadigital/jiku-github-sync:dev` and `:dev-<sha>` on every push to `dev`.
- `release.yml`: a `vX.Y.Z` tag publishes `X.Y.Z`, `X.Y` and `latest`; it requires the CHANGELOG section
  (see [`CHANGELOG.md`](CHANGELOG.md#cutting-a-release)).

Repository secrets: `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN`.

## License

[Apache-2.0](LICENSE) © Grava Digital.
