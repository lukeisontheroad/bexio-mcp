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

## Layout

- `main.go`: HTTP server, OAuth 401/metadata, the three MCP tools, request
  forwarding to api.bexio.com.
- `spec.go`: loads the embedded `openapi.json`, filters operations, renders
  `bexio_docs` output.
- `guide.md`: API conventions shown first by `bexio_docs`; fix recurring
  model mistakes here.
- `openapi.json`: generated, never hand-edited.

## Rules

- Stateless: never store, log or cache tokens. The bearer token is forwarded
  to api.bexio.com only.
- Bank payments and payroll stay excluded (`excludedTags`, `defaultScopes`):
  a chat connector must not move money or read salary data.
- `defaultScopes`: one scope bexio doesn't know makes it reject the whole
  sign-in (e.g. `general` is documented but not requestable). Add only
  verified names.
- bexio tokens are bound to one company. Several companies means several
  connectors (`/mcp/<label>`); there is no company parameter.
- Use `example.com`/Acme placeholders in docs and tests, never real company
  names.
