## Architecture

Use only these technologies:

- Go 1.27+ (`go.mod` pins 1.27.1)
- TypeScript, Vite and Svelte 5 for the UI (`ui/`)
- Docker
- Kubernetes (API, e.g. client-go), only in `cmd/kubeswap`
- Markdown
- YAML
- Makefile
- bash

## Testing Changes

- On RSCH-IPAT-D06 there is no host `go`: run Go commands, including `make test-dev` and `make test-all`, in the `golang:1.27` container, and run `make test-ui` and UI builds in `node:24`, per the recipe in `/opt/ai/AGENTS.md`. Keep `simple-responder_*` out of `./build` there, because the `internal/process` ForkedGrandchild tests fail inside the container once it exists.
- Name tests by the type under test: `TestProxy_<name>`, `TestProcessGroup_<name>`, etc.
- Check new tests quickly with `go test -v -run <new tests>`.
- After changing Go source, run `make test-dev` (short tests; its `staticcheck` step is non-fatal and silently skipped where the tool is absent, as in the `golang:1.27` container); after changing `ui/`, run `make test-ui`; before committing, run `make test-all` (race detector, `./internal/...`).
- After pushing, check CI for the pushed commit: `gh run list -R dpeet/llama-swap -c <sha>` until every path-filtered workflow you expect has appeared (Go changes → Linux CI and Windows CI; `ui/` → UI Tests), then `gh run watch <id> -R dpeet/llama-swap --exit-status` for each, and treat a red run as unfinished work, because Windows CI covers a platform the Linux container can't and the local suite passes regardless. Also check `gh run list -R dpeet/llama-swap --event schedule -L 10`; a red scheduled run is yours only if it is newer than the last fix and its workflow is still active (`gh workflow list --all`), because disabling a workflow doesn't erase its old failures.
- After an upstream rebase, check every newly imported `internal/process` test that expects a clean (nil) `Stop` for `CmdStop: testCmdStop` (see its comment in `process_command_test.go`), leaving tests that assert `ErrForcedKill` alone, because upstream's `killProcess` returns no error, so upstream's Windows CI never sees the fork's `ErrForcedKill` and the test arrives red on the fork's Windows CI.
- Build test binaries into `./build`.
- Run `make eval-docs-agent` only when the user asks — it scores the Help page's Docs Agent against a local model after a change to its system prompt, `docs/kb/` content, the MCP tool descriptions, or docs search ranking. See `evals/docs-agent/README.md` for the tuning loop.

## Documentation

- When a change adds, removes, or changes the meaning of a configuration option, add or update its knowledge-base guide under `docs/kb/guides/`, not just `docs/config.example.yaml` and `config-schema.json`. Prefer extending an existing guide on the same topic over a new file for a single setting.
- Follow the frontmatter contract and writing guidelines in `docs/kb/README.md` (required `title`/`summary`/`category`, `config_keys` referencing real schema keys, keep it short, show a working config, say what goes wrong).
- Run `make test-dev` afterward, because `TestKB_FrontmatterIsValid` in `internal/docagent` checks the frontmatter and that `config_keys` resolve.

## Git commits

This is a permanently divergent personal fork (`dpeet/llama-swap`, no upstream PRs or issue tracker in use), so commit straight to it.

- Run `gofmt -w <file>` on changed Go files before committing.
- Subject: `<area>: <what changed, in plain words>`, where the area is the package path or top-level dir touched (`internal/router:`, `ui:`, `docker:`; join two with a comma: `internal/server, ui:`).
- Body: why the change was needed and what it does, as paragraphs written one line each (no hard wrap), because that's how the rest of the owner's tooling renders commit text.
