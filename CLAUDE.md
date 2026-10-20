# BexTrackMCP (bexio-mcp)

Remote MCP server for the bexio API, run as a Docker container. Standalone
project with no code shared with other repos. Functionality, security and
user-facing setup are documented in README.md; keep it current with every
change.

API reference: https://docs.bexio.com/ (pre-rendered Redoc; the full
OpenAPI spec is embedded in `__redoc_state`, see `scripts/fetch-spec.py`).

## Commands

```sh
go test ./...                          # includes end-to-end sign-in and MCP tests
docker build -t bexio-mcp .
python3 scripts/fetch-spec.py          # regenerate openapi.json
```

## Branches, commits, releases

- Work on a branch off `dev`, merge into `dev` by pull request. `main` only
  receives pull requests from `dev`. Both are protected (PR + green CI, no
  force push); never push to them directly.
- Conventional Commits are enforced in CI and drive the version
  (release-please, `bump-minor-pre-major`). Never tag manually.
- Merging release-please's PR on `main` tags `vX.Y.Z` and the `release`
  workflow publishes `ghcr.io/<owner>/bexio-mcp:{X.Y.Z,X.Y,latest}` for
  amd64 and arm64, with the version baked in (`-X main.version`).
- Release steps: PR `dev` → `main` (merge commit, not squash, so
  release-please sees every conventional commit); release-please opens its
  release PR. PRs opened by GITHUB_TOKEN start no workflows, so close and
  reopen it (`gh pr close N && gh pr reopen N`) to run the required checks,
  then merge. Afterwards bring `main` back into `dev` with a PR, so the
  release commit and CHANGELOG are on `dev` too.

## Deploy

Production: https://mcp.bextrack.com on the Raspberry Pi (`ssh pi`), in
`~/dockerapps/mcp.bextrack.com` (compose file and `.env`, chmod 600, with
`PUBLIC_URL`, `BEXIO_CLIENT_ID`, `BEXIO_CLIENT_SECRET`, `HOST_PORT=8011`,
`IMAGE`, `SENTRY_DSN` (sentry.io project, platform Go) and
`SENTRY_ENVIRONMENT=production`; the user maintains the secret there, never
write it yourself). The
Pi's Cloudflare tunnel (dashboard-managed) routes the hostname to
`http://localhost:8011` and terminates TLS. Deploying a release means
setting `IMAGE` to the new version's tag and
`docker compose pull && docker compose up -d`. Deploys need the user's go.

## Layout

- `main.go`: HTTP server, 401 and protected resource metadata, the three
  MCP tools, request forwarding to api.bexio.com.
- `oauth.go`: the authorization server clients see: registration, consent
  page (areas + read only), bexio callback, token endpoint, sealing.
- `spec.go`: loads the embedded `openapi.json`, filters operations, renders
  `bexio_docs` output.
- `guide.md`: API conventions shown first by `bexio_docs`; fix recurring
  model mistakes here.
- `openapi.json`: generated, never hand-edited.
- `bextrack-logo.svg`: the bextrack app icon, shown on the consent page.
- Files embedded with `go:embed` must be in the Dockerfile's `COPY` line
  and allowed in `.dockerignore` (an allowlist), or the image build fails.

## Rules

- Stateless: never store, log or cache tokens, codes or sealed values. Log
  paths only, never full URLs or bodies (`/oauth/*` queries carry codes).
- The bexio client secret stays on the server; clients only ever get sealed
  values. bexio's tokens are unwrapped solely to call api.bexio.com. The
  seal key derivation string must not change: it would sign everyone out.
- Every authorization goes through the consent page with its CSRF cookie:
  with open registration, skipping it lets anyone phish a signed-in user's
  bexio access. Redirect URIs are validated before anything redirects.
- Bank payments and payroll stay excluded (`servable` in spec.go, absent
  from `modules`): a chat connector must not move money or read salary data.
- `modules` (oauth.go) maps consent-page areas to OAuth scopes. One scope
  bexio doesn't know makes it reject the whole sign-in (e.g. `general` is
  documented but not requestable). Add only verified names.
- bexio tokens are bound to one company. Several companies means several
  connectors (`/mcp/<label>`, the label is free text); there is no company
  parameter. `/mcp/<label>/<company-id>` makes the callback refuse other
  companies (`company_id` claim of bexio's id token, needs `company_profile`).
- Use `example.com`/Acme placeholders in docs and tests, never real company
  names or personal domains.
