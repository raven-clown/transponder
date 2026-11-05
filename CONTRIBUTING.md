# Contributing

Thanks for helping out. Bug reports, docs fixes, tests and new features are all welcome, and every feature ships in the free build, so there is no closed part you can't touch.

## Finding something to work on

- Issues labeled `good first issue` are small and say exactly what to change.
- Issues labeled `help wanted` are bigger. Each one describes the proposed API, the behavior and the tests it needs.
- The milestones group issues by release. Start with the earliest one.

Before you start, leave a comment on the issue so nobody else picks it up. If you go quiet for two weeks, someone else may take it over.

For a new feature that has no issue yet, open one first, so we can agree on the API before you write the code. Bug fixes and docs can go straight to a pull request.

Use the issue forms: a bug report, a feature request, a new adapter or a docs problem. Each one says what to include.

## Development setup

You need Go 1.26 or newer and [golangci-lint](https://golangci-lint.run/) v2.

```shell
git clone https://github.com/<your-username>/transponder.git
cd transponder

go test -race ./...      # all tests
golangci-lint run ./...  # linters
make check               # both, plus the coverage check CI runs
```

No cgo is needed. The SQLite and Postgres drivers are pure Go.

## Tests

Every change comes with tests. A pull request without them is only merged when there is nothing to test, such as a typo.

- A bug fix starts with a test that fails without the fix.
- A feature is covered for the normal case, the error cases and the edges the issue lists.
- Prefer table-driven tests with `t.Run` subtests and clear names.
- Test behavior through the public API. Don't test private helpers unless they hold real logic.
- Tests must pass with `-race` and must not depend on timing. Use channels or polling with a deadline, never a bare `time.Sleep`.
- No network calls in unit tests. Use `httptest` servers or the fakes in `internal/testutil`.
- Tests that need Docker or a real service go behind the `integration` build tag.
- CI fails if total coverage drops below 80%.

## Code style

- `gofmt` and `goimports`. The linters in `.golangci.yml` must pass.
- Wrap errors with context: `fmt.Errorf("loading config: %w", err)`. Return sentinel errors for cases callers check with `errors.Is`.
- Keep comments short and about why, not what. Every exported name has a doc comment.
- No new dependency without a reason in the pull request.
- No global state. Pass dependencies in.

## Commits and pull requests

- One change per pull request. A refactor and a feature are two pull requests.
- The pull request title follows [Conventional Commits](https://www.conventionalcommits.org/), such as `fix(auth): reject expired tokens` or `feat(admin): list audit records`. Pull requests are squash merged, so the title becomes the commit on `main`.
- The pull request links its issue with `Closes #123`.
- Describe what changed, why, and how you tested it. Say what you didn't test.
- Keep the branch up to date with `main` and respond to review. A maintainer aims to give a first review within three days.

## How a pull request gets merged

`main` is protected. A pull request is merged only when all of these pass:

- a review approval from a code owner, given after the last push
- every review thread resolved
- the branch up to date with `main`
- these checks green: `test` on Linux, macOS and Windows, `coverage`, `lint`, `analyze` (CodeQL), `dependency review` and `pr title`

Workflows from a fork wait for a maintainer to approve them before they run. Dependabot opens weekly pull requests for Go modules and GitHub Actions, and they go through the same checks.

## Backward compatibility

Until 1.0, a minor release may change the API, and the changelog says how to migrate. After 1.0, nothing public breaks without a major version.

Every user-visible change adds a line to `CHANGELOG.md` under Unreleased.

## Reporting a security problem

Don't open a public issue. Use GitHub's private vulnerability reporting on the repository instead.

## Code of conduct

Be kind and assume good intent. This project follows the [Contributor Covenant](https://www.contributor-covenant.org/version/2/1/code_of_conduct/).
