# Repository Guidelines

## Project Structure & Module Organization

Spot is a small self-hosted hosting platform. The main Go service lives in `server/` and includes HTTP routing, identity resolution, deploy handling, SQLite metadata, storage adapters, realtime websockets, and tests. Browser-facing static assets and the client SDK live in `sdk/`; generated embedded copies are under `server/static_assets/sdk/`. The shell CLI is `cli/spot`. Deployment configuration is in `docker-compose*.yml`, `caddy/`, `scripts/`, and `justfile`. Example sites are in `examples/`.

## Build, Test, and Development Commands

Use `just` where possible:

```sh
just up                 # start local Docker stack at http://spot.localhost:8080
just down               # stop local stack
just build              # run go build ./... in server/
just test               # run go vet ./... and go test ./...
just test-integration   # run Go integration tests with the integration tag
just e2e                # run stack, deploy demo, exercise serving and APIs
just deploy-demo        # deploy examples/demo to the local stack
```

For single binary work, run `cd server && go build -o spot-api .`.

## Coding Style & Naming Conventions

Go code should be formatted with `gofmt` and kept in package `main` unless there is a strong reason to split packages. Prefer small, explicit interfaces like `SiteStorage`, `FileStorage`, and `DeployAuthorizer` over broad abstractions. Test files follow Go naming conventions: `*_test.go`, with tests named `TestFeatureBehavior`. Shell scripts should stay POSIX `sh` compatible unless already using another shell.

## Testing Guidelines

Run `just test` before submitting changes. Add focused unit tests next to the code they cover in `server/`. Use integration tests with the `integration` build tag for behavior that needs real SQLite or storage flows. For deploy, routing, identity, policy, or security-sensitive changes, cover both allowed and denied paths.

## Commit & Pull Request Guidelines

Recent history uses short imperative commits, often conventional prefixes such as `feat(server): ...`, `fix(deploy): ...`, and `docs(readme): ...`. Keep commits scoped and descriptive.

PRs should include a concise summary, test results, and any config or migration notes. Include screenshots for visible UI changes in `sdk/` or platform pages. Link related issues or plans when applicable.

## Security & Configuration Tips

Do not commit `.env` or real provider tokens. Shared deployments must use a real mesh provider or `SPOT_AUTH_MODE=single-user`; local development may use `SPOT_DEV_IDENTITY_EMAIL`. Treat `_access.json`, trusted proxy settings, and identity resolution as security boundaries.
