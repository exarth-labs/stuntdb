# Contributing to stuntdb

Thanks for your interest in stuntdb! The project is in early development and changes quickly. If you plan a larger change, please open an issue to discuss it first. It saves everyone time.

## Development setup

You need:

| Tool | Version | Install |
|---|---|---|
| Go | 1.26 or newer | https://go.dev/dl/ |
| Task (command runner) | 3.x | https://taskfile.dev/installation/ |
| golangci-lint (linter) | 2.x | https://golangci-lint.run/welcome/install/ |

Then, from the repository root:

| Command | What it does |
|---|---|
| `task build` | Builds `bin/stuntdb` |
| `task test` | Runs the tests |
| `task test:race` | Runs the tests with the race detector. Needs a C compiler; CI always runs it. |
| `task lint` | Runs the linters |
| `task fmt` | Formats the code |
| `task check` | Runs lint, tests and build together. Run this before opening a pull request. |

Run `task` on its own to list every command.

## Making a change

1. Fork the repository and create a branch from `main`.
2. Make your change, with tests.
3. Run `task check`.
4. Open a pull request and fill in the template.

**Commit messages** follow [Conventional Commits](https://www.conventionalcommits.org/), for example `feat: …`, `fix: …`, `docs: …`, `test: …`, `refactor: …` or `chore: …`. The changelog is built from them.

## Design rules

These rules keep stuntdb safe, and able to support many databases:

- **The core never depends on a specific database.** Database-specific code (SQL, type names, drivers) lives only in `dialects/<engine>/`, and the linter enforces this.
- **Never log or print real data values**, whether in errors, logs or test output.
- **The source database is read-only.** stuntdb must never write to it.
- **Command output is a contract.** Results go to stdout, messages go to stderr, and the exit codes are documented. Discuss any change to them in an issue first.

## Reporting bugs and requesting features

Use the issue templates.

**Never paste real production data** (names, emails, connection strings, passwords) into an issue.

For security problems, follow [SECURITY.md](SECURITY.md) instead of opening a public issue.

## License

By contributing, you agree that your contributions are licensed under the [Apache License 2.0](LICENSE).
