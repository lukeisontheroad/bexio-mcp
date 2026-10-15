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

| Variable       | Default        | Meaning                                                     |
|----------------|----------------|-------------------------------------------------------------|
| `PUBLIC_URL`   | required       | External base URL, used in the OAuth metadata               |
| `PORT`         | `8080`         | Listen port                                                 |
| `BEXIO_SCOPES` | all but payroll/banking | Space-separated OAuth scopes requested at sign-in. One unknown scope makes bexio reject the whole sign-in. |

Endpoints: `/mcp` and `/mcp/<label>` (MCP), `/.well-known/oauth-protected-resource[/mcp...]`,
`/healthz`.

## Connect Claude

1. In your bexio app on [developer.bexio.com](https://developer.bexio.com),
   add the redirect URI `https://claude.ai/api/mcp/auth_callback`. Note the
   client ID and secret.
2. In claude.ai go to **Customize > Connectors > Add custom connector**:
   - URL: `https://bexio-mcp.example.com/mcp/acme`
   - **Advanced settings**: OAuth client ID and secret from step 1
3. Connect, sign in to bexio, and pick the company.

**Several companies:** add one connector per company, each with its own
label (`/mcp/acme`, `/mcp/globex`, ...), and sign each one in to a
different company. All of them can be enabled in the same chat. The label
is just a name; the token decides the company.

**Safety:** keep `bexio_write` on "Needs approval" in the connector's tool
permissions; it can issue and send documents and delete records. Set it to
"Blocked" for read-only use.

## Develop

```sh
go test ./...
python3 scripts/fetch-spec.py   # refresh openapi.json from docs.bexio.com
```

`guide.md` holds the API conventions Claude sees first (`bexio_docs` without
a topic). It is the place to fix recurring mistakes. The test fails if it
names an operationId that doesn't exist.
