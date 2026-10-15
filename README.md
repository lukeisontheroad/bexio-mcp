# bexio-hosted-mcp

Remote MCP server (Streamable HTTP) that gives MCP clients (claude.ai,
Claude Desktop/Code, ChatGPT, Cursor, VS Code, ...) access to the
[bexio API](https://docs.bexio.com/).

- **Just the URL.** The server is its own OAuth authorization server: clients
  register themselves automatically (dynamic client registration), so nobody
  enters a client ID or secret. It signs users in at bexio with one bexio
  app whose secret stays on the server.
- **Consent page.** Before the bexio login, the user sees which client asks
  for access and picks the areas it may use (contacts, invoices, accounting,
  ...) and whether it is read only.
- **Stateless and multi-tenant.** Nothing is stored. Client IDs, codes and
  tokens are encrypted with a key derived from the bexio client secret, and
  every request carries its own token, so one deployment serves any number
  of users and bexio companies.
- **Three tools instead of 300.** The bexio OpenAPI spec is embedded and
  served on demand:
  - `bexio_docs`: API conventions, operation groups, and per-operation
    parameters and body
  - `bexio_read`: runs GET and search operations by operationId
  - `bexio_write`: runs everything else (absent on read-only connections)
- **Excluded:** bank payments and payroll (never callable, scopes never
  requested), PDF/file downloads and multipart uploads.

API reference: https://docs.bexio.com/

## Hosted instance

```
https://mcp.bextrack.com/mcp
```

Add it as a remote MCP server in any client (see [Connect a client](#connect-a-client))
and sign in with your own bexio account. Nothing is stored on the server.

## Run your own

```sh
docker build -t bexio-hosted-mcp .
docker run -p 8080:8080 -e PUBLIC_URL=https://bexio-mcp.example.com \
  -e BEXIO_CLIENT_ID=... -e BEXIO_CLIENT_SECRET=... bexio-hosted-mcp
```

or `docker compose up -d` with a `.env` (chmod 600) holding the same
variables. Put it behind TLS on a public URL.

| Variable              | Meaning                                                  |
|-----------------------|----------------------------------------------------------|
| `PUBLIC_URL`          | External base URL; the OAuth issuer and resource         |
| `BEXIO_CLIENT_ID`     | Client ID of the bexio app on developer.bexio.com        |
| `BEXIO_CLIENT_SECRET` | Its secret. Rotating it signs every user out             |
| `PORT`                | Listen port, default `8080`                              |

The bexio app needs exactly one redirect URI:
`https://bexio-mcp.example.com/oauth/callback`.

Endpoints: `/mcp` and `/mcp/<label>` (MCP), `/oauth/*` and
`/.well-known/oauth-authorization-server` (authorization server),
`/.well-known/oauth-protected-resource[/mcp...]` (also fine for health
checks).

## Connect a client

Add `https://bexio-mcp.example.com/mcp` as a remote MCP server; in claude.ai
choose **Register automatically** under OAuth client. Connecting opens the
consent page, then the bexio login, where you pick the company.

**Several companies:** add one connector per company with a label of your
choice (`/mcp/acme`, `/mcp/globex`, ...) and sign each in to a different
company. The label only makes the URLs unique; the sign-in decides the
company.

**Safety:** keep `bexio_write` on "Needs approval" in the client's tool
permissions; it can issue and send documents and delete records.

## Develop

```sh
go test ./...
python3 scripts/fetch-spec.py   # refresh openapi.json from docs.bexio.com
```

`guide.md` holds the API conventions Claude sees first (`bexio_docs` without
a topic). It is the place to fix recurring mistakes. The test fails if it
names an operationId that doesn't exist.
