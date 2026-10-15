# bexio-hosted-mcp

Remote MCP server for the bexio API, run as a Docker container. Standalone
project with no code shared with other repos. User-facing setup is in
README.md.

API reference: https://docs.bexio.com/ (pre-rendered Redoc; the full
OpenAPI spec is embedded in `__redoc_state`, see `scripts/fetch-spec.py`).

## Commands

```sh
go test ./...                          # includes an end-to-end MCP client test
docker build -t bexio-hosted-mcp .
python3 scripts/fetch-spec.py          # regenerate openapi.json
```

## Deploy

Production: https://mcp.bextrack.com on the Raspberry Pi
(`ssh pi`), in `~/dockerapps/mcp.bextrack.com` (compose file,
`.env`, chmod 600, with `PUBLIC_URL`, `BEXIO_CLIENT_ID`,
`BEXIO_CLIENT_SECRET` and `HOST_PORT=8011`; the user maintains the secret
there, never write it yourself). The Pi's Cloudflare tunnel
(dashboard-managed) routes the hostname to `http://localhost:8011` and
terminates TLS. The Pi never builds; ship the image as a tar:

```sh
docker buildx build --platform linux/arm64 -t bexio-hosted-mcp:latest --load .
docker save bexio-hosted-mcp:latest -o bexio-hosted-mcp.tar
scp bexio-hosted-mcp.tar docker-compose.yml pi:dockerapps/mcp.bextrack.com/
ssh pi 'cd ~/dockerapps/mcp.bextrack.com && docker load -i bexio-hosted-mcp.tar && docker compose up -d'
```

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
- Files embedded with `go:embed` must also be in the Dockerfile's explicit
  `COPY` line, or the image build fails.

## Rules

- Stateless: never store, log or cache tokens, codes or sealed values. Log
  paths only, never full URLs or bodies (`/oauth/*` queries carry codes).
- The bexio client secret stays on the server; clients only ever get sealed
  values. bexio's tokens are unwrapped solely to call api.bexio.com.
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
  parameter.
- Use `example.com`/Acme placeholders in docs and tests, never real company
  names.
