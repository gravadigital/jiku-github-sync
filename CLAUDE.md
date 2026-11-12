# jiku-github-sync — repository conventions

- Everything in this repository is English: documentation, code, comments, logs, test output and commit
  messages. The exception is what the connector **writes into Jiku and GitHub** for people to read
  (attribution lines such as "**Name** comentó en Jiku:", notes such as "📎 informe.pdf (adjunto en Jiku)",
  notices): it is Spanish, like the teams that read it. Identifiers (Jiku states, env vars, field names)
  are not prose and do not change.
- Go 1.25 (`go.mod` at `go 1.25.0`): do not upgrade dependencies that require a newer version.
- **The connector is external to Jiku.** Jiku must work without it and needs no change for it: no tags,
  fields or code in Jiku to hold the connector's mapping or state. The mapping lives in the connector's
  configuration; the state lives in the connector's own space of Jiku's KV.
- **It runs as an always-on container.** Every feature must work inside `serve` (initial sync, mapping check,
  verification, reloads); the CLI subcommands are diagnostics, never a required step of operation.
- **GitHub credentials are per client** (`credentials` in the mapping), never per project.
- **No real names in tracked files.** No client, organization, repository, Project, person or id of a real
  deployment in code, tests, docs, examples or commit messages: use invented ones (`acme-org`,
  `example-repo`). Real configuration lives in `.local/`, research notes with client data in `_research/`;
  both are ignored and never committed or copied out.
- **Secrets never touch the repo or the logs.** Tokens are passed through environment variables (or mounted
  secret files referenced from the mapping), never written into tracked files, never printed. Machine user
  keys (`*.key.json`) and database dumps (`*.sql`, `*.sql.gz`, `*.dump`) are ignored and never committed.
- **Live tests write only to the test organization and repositories** configured in `.local/`, never to a
  client's real repositories or Projects. Mails from a local Jiku go to a local catcher, never to people.
- The state in the KV (`internal/state`) must stay readable by the next version: a new field is added with
  a fallback for links written before it (see `Snapshot.JikuBody`). A change that needs a migration is a
  **major** (see the versioning policy).
- Tests: `go test -race ./...`, synthetic fixtures only. `test/e2e/e2e.sh` runs against a live local Jiku and
  the test organization (variables in `.local/e2e.env`).
- Conventional commits in English, one per concern, on `dev`. **Never `git push`.** Never `git add -A` or
  `git add .` at the root: stage by name, and never `.local/`, `_research/`, `.env`, keys or dumps.
- Before finishing: `go vet ./...`, `golangci-lint run ./...`, `go test -race ./...`, `docker build .`.

---

## Cutting a release

**Triggers:** "versiona", "versionar", "cortá una release", "nueva versión", "bump de versión",
"versiona y listo", "release", "cut a release", "new version", "version bump", "bump the version",
"release and done", or any variant, in Spanish or English. When one appears, follow this procedure in full
and in order. It is the same as the one in the jiku-docs repo, adapted to this connector.

### What is versioned

| Version | Where it lives | Who moves it |
| --- | --- | --- |
| **The product**, what "version it" means | The `vX.Y.Z` tag + the `## [X.Y.Z]` section of `CHANGELOG.md` | This procedure |

The number **is not written in any code file**: the binary receives it at build time
(`-ldflags "-X main.version=..."`, `ARG VERSION` in the `Dockerfile`), and `release.yml` takes it from the
tag. That is why there is no `set-version.sh` here: applying the number means writing the CHANGELOG section
and creating the tag. The policy is in [CHANGELOG.md](CHANGELOG.md#versioning-policy); this is the
step by step.

### Step 1: choose the cut point

`main` is the release branch; work comes in through `dev`.

```sh
git describe --tags --abbrev=0            # latest published version (nothing: this is the first one)
git rev-list --left-right --count main...dev
git status --porcelain
```

**The cut lands on finished work.** Never in the middle of a half-implemented change, nor with
`[Unreleased]` describing something the code does not do yet. If `dev` has open work, the cut is `main` (or
the last finished commit of `dev`), and **the user is told**, it is not resolved silently. If
`origin/<branch>` is behind or has diverged, report it too, but **do not push** (see step 8).

**First release:** if `main` does not exist locally, it is created at the cut point
(`git branch main <commit>`), without pushing. If `origin/main` holds history that is not in `dev` (for
example the "Initial commit" GitHub makes when a repository is created with a license), **do not merge,
rebase or rewrite anything**: tag the local `main` and tell the user, in the report, that `origin/main`
does not descend from it and that how to reconcile it (replacing it, or merging the unrelated histories) is
their decision.

### Step 2: gather the real changes

The CHANGELOG entry covers **everything** that came in since the last tag (since the first commit, for the
first release). Commit subjects are not enough: read the bodies.

```sh
git log v<latest>..HEAD --oneline
git log v<latest>..HEAD --format='%h%n%B'      # the bodies explain the why
git diff --stat v<latest>..HEAD
```

Sources, from richest to poorest:

1. `[Unreleased]` in `CHANGELOG.md`, which feature commits already fed.
2. The README (what it syncs, known limitations) and `config.example.yaml`.
3. The commit bodies.

And always review these diffs, which are the ones that produce entries nobody wrote:

```sh
git diff v<latest>..HEAD -- .env.dist                          # new or renamed variables
git diff v<latest>..HEAD -- config.example.yaml internal/mapping/   # the mapping format
git diff v<latest>..HEAD -- internal/state/                    # the state kept in Jiku's KV
git diff v<latest>..HEAD -- Dockerfile docker-compose.local.yml .github/workflows/
git diff v<latest>..HEAD --diff-filter=D --name-only           # what was deleted
```

> **The rule that matters: every entry must be verifiable in the tree being tagged.** If the cut is from
> `main`, a change that is only in `dev` **does not go** in the entry. Check with
> `git branch --contains <sha>` before claiming anything.

### Step 3: propose the number and WAIT

| Bump | When |
| --- | --- |
| **major** | Breaks the configuration (the mapping format, environment variables) or the state kept in the KV in a way that needs a migration |
| **minor** | New functionality, backwards compatible |
| **patch** | Fixes and internal changes, with no effect on configuration or state |

The first release is `1.0.0` when the connector is meant for production use, `0.1.0` otherwise: propose one
and say why.

Present to the user: **the proposed number, the reasoning and the list of breaking changes found**.

**Do not set the version without explicit confirmation.** If the user corrects the number, it is their
decision: take it. If the correction contradicts the table (a `minor` with breaking changes, for example),
say so **in one line**, keep the breaking changes clearly visible at the top of the entry and go on without
insisting.

### Step 4: write the CHANGELOG entry

In [CHANGELOG.md](CHANGELOG.md), above the previous entry:

- Title `## [X.Y.Z] - YYYY-MM-DD` with **today's real date**.
- `## [Unreleased]` stays empty above; what it had moves under the new title.
- Keep a Changelog sections, only those that apply: `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`,
  `Security`.
- **`### Notes for existing installations`** at the end. It belongs to this repo, not to Keep a Changelog,
  and it is **mandatory** when there are: new required environment variables, renamed or removed variables,
  changes to the mapping format, state in the KV that changes shape, a Jiku role or permission the machine
  user needs, or GitHub token scopes / App permissions that change.
- In English, like the rest of the file. Bold at the start of the important entries. Say **what breaks and
  what whoever operates the installation has to do**, not only what changed.
- No real names (see the conventions above), also here.
- Never delete or rewrite entries of already published versions.

### Step 5: update the README and the examples if needed

- **[README.md](README.md)**: if what it syncs, how it runs, the commands, the requirements or the known
  limitations changed. **It carries no version number, on purpose: do not add one.**
- `.env.dist`: it must have every variable, with its comment.
- `config.example.yaml`: it must show every key the mapping accepts.

### Step 6: verify the number

```sh
grep -n '^## \[X.Y.Z\] - ' CHANGELOG.md          # the same check release.yml requires
go vet ./... && golangci-lint run ./... && go test -race ./...
docker build --build-arg VERSION=X.Y.Z -t jiku-github-sync:release-check . \
  && docker run --rm jiku-github-sync:release-check version   # must print X.Y.Z
docker rmi jiku-github-sync:release-check
```

It runs before tagging because `release.yml` will run it anyway, and by then the tag is already in place.

### Step 7: commit

On the cut branch, with the CHANGELOG and whatever was touched in the README, `.env.dist` or
`config.example.yaml`.

**Never `git add -A` or `git add .`**: `.local/`, `_research/`, `.env` and dumps live at the root and are
not committed. Stage by name:

```sh
git add CHANGELOG.md README.md .env.dist config.example.yaml   # only what applies
git status --porcelain          # confirm there is nothing extra
git commit -m "chore(release): X.Y.Z"
```

End the message with the usual `Co-Authored-By:` line. If the cut was on `dev`, bring `main` to the release
commit (`git switch main && git merge --ff-only dev`, or create it if it is the first one).

### Step 8: local tag, and nothing else

```sh
git tag vX.Y.Z          # on main
```

> ### NEVER PUSH ANYTHING
>
> Not the branch, not the tag, not `git push --tags`, not `git push --follow-tags`, not
> `git push origin vX.Y.Z`. **Not even when it looks like the obvious next step**, nor when the user says
> "versiona y listo" (release and done).
>
> The reason is concrete: pushing the tag is what publishes `gravadigital/jiku-github-sync` to Docker Hub
> (`release.yml` is triggered by `push: tags: ['v*.*.*']`, with the tags `X.Y.Z`, `X.Y` and `latest`).
> That action belongs to the user, not to the agent.
>
> Finish by telling them the exact command, as information, without running it:
>
> ```sh
> git push origin main dev && git push origin vX.Y.Z
> ```

### Step 9: report

- Which version was tagged, on which branch and on which commit.
- What was left out of the release and why (typically: `dev` work still open).
- If `origin/<branch>` is behind or has diverged, say so. If the GitHub repo does not exist yet, say that
  too.
- The push command, without running it.
- That `release.yml` checks the tag against the CHANGELOG again and runs vet, lint and tests before
  publishing, so a wrongly placed tag never reaches the registry.

### Hard rules

1. **Never `git push`** anything, at any step, for any reason.
2. **Never set the version without** the user's explicit confirmation.
3. **Never put a change in the entry that is not in the tree being tagged.**
4. **Never version on top of a half-implemented change** without warning first.
5. **Never rewrite entries of already published versions.**
6. **Never rewrite `origin` history** (no force, no rebase of published branches): diverged remotes are
   reported, not fixed.
7. If the tag already exists and needs fixing: `git tag -d vX.Y.Z` and create it again. This is safe **only
   because it is never pushed**.
