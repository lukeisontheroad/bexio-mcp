# bexio-hosted-mcp

Remote MCP server (Streamable HTTP) that gives Claude (claude.ai, Desktop,
mobile) access to the [bexio API](https://docs.bexio.com/).

- **Stateless, no stored credentials.** claude.ai signs the user in at
  bexio's own OAuth server (`auth.bexio.com`) and sends the bexio access
  token with every request; the server forwards it to `api.bexio.com`.
  Claude handles token refresh.
- **Multi-tenant.** Every request carries its own token, so one deployment
  serves any number of users and bexio companies at the same time.
- **Three tools instead of 300.** The bexio OpenAPI spec is embedded and
  served on demand:
  - `bexio_docs`: API conventions, operation groups, and per-operation
    parameters and body
  - `bexio_read`: runs GET and search operations by operationId
  - `bexio_write`: runs everything else
- **Excluded:** bank payments and payroll (never callable, scopes never
  requested), PDF/file downloads and multipart uploads.

API reference: https://docs.bexio.com/

## Run

```sh
docker build -t bexio-hosted-mcp .
docker run -p 8080:8080 -e PUBLIC_URL=https://bexio-mcp.example.com bexio-hosted-mcp
```

Put it behind TLS on a public URL. claude.ai connects from Anthropic's
cloud (`160.79.104.0/21`), not from your machine.

| Variable     | Default  | Meaning                                       |
|--------------|----------|-----------------------------------------------|
| `PUBLIC_URL` | required | External base URL, used in the OAuth metadata |
| `PORT`       | `8080`   | Listen port                                   |

Endpoints: `/mcp` and `/mcp/<label>` (MCP), and
`/.well-known/oauth-protected-resource[/mcp...]` (OAuth metadata; also
answers 200 for health checks).

## Connect Claude

1. In your bexio app on [developer.bexio.com](https://developer.bexio.com),
   add the redirect URI `https://claude.ai/api/mcp/auth_callback`. Note the
   client ID and secret.
2. In claude.ai go to **Customize > Connectors > Add custom connector**:
   - URL: `https://bexio-mcp.example.com/mcp/acme`
   - **Advanced settings**: OAuth client ID and secret from step 1
3. Connect, sign in to bexio, and pick the company.

**Choosing scopes:** the connector URL picks what bexio is asked to grant:
`/mcp[/<label>][/<modules>][/readonly]`, every part optional. The label
is any name (it only makes each connector's URL unique); a first segment
that is `readonly` or a module list counts as an option, not a label.

| URL                                    | Access                                   |
|----------------------------------------|------------------------------------------|
| `/mcp`                                 | all modules, read and write              |
| `/mcp/readonly`                        | all modules, read only                   |
| `/mcp/acme`                            | all modules, read and write              |
| `/mcp/acme/contacts,invoices`          | contacts and invoices, read and write    |
| `/mcp/accounting/readonly`             | accounting, read only                    |

Modules: `contacts`, `quotes`, `orders`, `invoices`, `items`, `projects`,
`notes-tasks`, `master-data`, `files`, `accounting`, `purchase`. `readonly`
requests no write scopes and removes the `bexio_write` tool. That matters
for `files`, `accounting` and stock, where bexio has no separate read
scope. Bank payments and payroll are never available. After changing a
connector's modules, reconnect it so bexio issues a token with the new
scopes.

**Several companies:** add one connector per company, each with its own
label (`/mcp/acme`, `/mcp/globex`, ...), and sign each one in to a
different company. All of them can be enabled in the same chat. The label
is just a name; the token decides the company.

**Other MCP clients** use the same bexio app; register each client's
redirect URI there too (as of 2026-10):

| Client      | Redirect URI                                                                 |
|-------------|------------------------------------------------------------------------------|
| claude.ai / Desktop / mobile | `https://claude.ai/api/mcp/auth_callback`                   |
| ChatGPT     | shown when creating the app: `https://chatgpt.com/connector/oauth/<id>` (legacy: `https://chatgpt.com/connector_platform_oauth_redirect`) |
| Claude Code | `http://localhost:<port>/callback`, with `claude mcp add --transport http --client-id <id> --client-secret --callback-port <port> bexio-acme https://bexio-mcp.example.com/mcp/acme` |
| VS Code     | `https://vscode.dev/redirect` and `http://127.0.0.1:33418/`                  |
| Cursor      | `cursor://anysphere.cursor-mcp/oauth/callback`                               |

**Safety:** keep `bexio_write` on "Needs approval" in the connector's tool
permissions; it can issue and send documents and delete records.

## Develop

```sh
go test ./...
python3 scripts/fetch-spec.py   # refresh openapi.json from docs.bexio.com
```

`guide.md` holds the API conventions Claude sees first (`bexio_docs` without
a topic). It is the place to fix recurring mistakes. The test fails if it
names an operationId that doesn't exist.
