// Command bexio-hosted-mcp is a remote MCP server for the bexio API.
//
// It holds no state and no credentials: claude.ai signs the user in at
// bexio's own OAuth server and sends the bexio access token with every MCP
// request, which this server forwards to api.bexio.com.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

const (
	issuer  = "https://auth.bexio.com/realms/bexio"
	prmPath = "/.well-known/oauth-protected-resource"
	// ponytail: hard cap per tool result; a 2000-row list would flood the
	// chat context. Claude pages with limit/offset when it sees the note.
	maxResult = 100_000
)

// apiURL is the bexio API base; tests point it at a fake.
var apiURL = "https://api.bexio.com"

// defaultScopes cover every area except banking payments and payroll, so a
// chat connector can never move money or read salary data. One unknown scope
// makes bexio reject the whole sign-in, so only add verified scope names.
const defaultScopes = "openid profile email offline_access company_profile " +
	"contact_show contact_edit kb_offer_show kb_offer_edit kb_order_show kb_order_edit " +
	"kb_invoice_show kb_invoice_edit kb_delivery_show kb_delivery_edit " +
	"article_show article_edit stock_edit project_show project_edit monitoring_show monitoring_edit " +
	"note_show note_edit task_show task_edit bank_account_show file accounting " +
	"kb_bill_show kb_expense_show kb_article_order_show kb_article_order_edit"

func main() {
	publicURL := strings.TrimSuffix(os.Getenv("PUBLIC_URL"), "/")
	if publicURL == "" {
		log.Fatal("PUBLIC_URL is required, e.g. https://bexio-mcp.example.com")
	}
	scopes := strings.Fields(cmp.Or(os.Getenv("BEXIO_SCOPES"), defaultScopes))
	addr := ":" + cmp.Or(os.Getenv("PORT"), "8080")
	log.Printf("listening on %s, connector URL %s/mcp", addr, publicURL)
	log.Fatal(http.ListenAndServe(addr, newHandler(publicURL, scopes)))
}

func newHandler(publicURL string, scopes []string) http.Handler {
	server := newServer(loadAPI())
	mcpHandler := requireBearer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true},
	), publicURL, scopes)

	// The token decides the bexio company. /mcp/<label> serves the same thing
	// under its own URL, so each company can be its own claude.ai connector.
	metadata := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(&oauthex.ProtectedResourceMetadata{
			Resource:               publicURL + cmp.Or(strings.TrimPrefix(r.URL.Path, prmPath), "/mcp"),
			AuthorizationServers:   []string{issuer},
			ScopesSupported:        scopes,
			BearerMethodsSupported: []string{"header"},
			ResourceName:           "bexio",
			ResourceDocumentation:  "https://docs.bexio.com/",
		})
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)
	mux.HandleFunc("GET "+prmPath, metadata)
	mux.HandleFunc("GET "+prmPath+"/mcp", metadata)
	mux.HandleFunc("GET "+prmPath+"/mcp/", metadata)
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	return mux
}

// requireBearer answers requests without a usable token with the 401 that
// starts claude.ai's sign-in: it points at the protected resource metadata,
// which names bexio as the authorization server.
func requireBearer(next http.Handler, publicURL string, scopes []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tokenUsable(r.Header.Get("Authorization")) {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q, scope=%q`,
				publicURL+prmPath+r.URL.Path, strings.Join(scopes, " ")))
			http.Error(w, "sign in to bexio", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenUsable checks for a bearer token that has not expired. The token is
// deliberately not verified: it is only ever forwarded to bexio's own API,
// which validates it (this server owns no data to protect). Reading the
// unverified exp claim lets Claude refresh before bexio rejects calls.
func tokenUsable(header string) bool {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return true // not a JWT: let bexio decide
	}
	var claims struct{ Exp int64 }
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return true
	}
	return time.Now().Unix() < claims.Exp
}

type docsInput struct {
	Topic string `json:"topic,omitempty" jsonschema:"empty for conventions and the group index, a group name (e.g. Invoices) for its operations, or an operationId (e.g. v2CreateInvoice) for parameters and body"`
}

type callInput struct {
	Operation  string         `json:"operation" jsonschema:"operationId from bexio_docs, e.g. v2SearchContact"`
	PathParams map[string]any `json:"path_params,omitempty" jsonschema:"values for the {placeholders} in the operation path, e.g. {\"contact_id\": 42}"`
	Query      map[string]any `json:"query,omitempty" jsonschema:"query string parameters, e.g. {\"limit\": 20, \"order_by\": \"id_desc\"}"`
	Body       any            `json:"body,omitempty" jsonschema:"JSON request body as documented by bexio_docs"`
}

func newServer(a *api) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "bexio", Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions: "Live access to one bexio company (accounting, CRM, invoicing). Call bexio_docs first: " +
			"it explains the API conventions and lists the operations. Writes change real books: " +
			"confirm with the user before creating, changing, deleting, issuing or sending anything.",
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "bexio_docs",
		Description: "bexio API reference. Start here: no topic gives conventions and operation groups; a group lists its operations; an operationId shows its parameters and request body.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in docsInput) (*mcp.CallToolResult, any, error) {
		text, err := a.doc(in.Topic)
		return textResult(text), nil, err
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "bexio_read",
		Description: "Run a read-only bexio operation (GET or search) by operationId. Look up its parameters with bexio_docs first.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, a.caller(true))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "bexio_write",
		Description: "Run a bexio operation that changes data (create, edit, delete, issue, send, ...) by operationId. Changes the company's real books: confirm with the user first.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(true)},
	}, a.caller(false))
	return s
}

func (a *api) caller(readOnly bool) mcp.ToolHandlerFor[callInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in callInput) (*mcp.CallToolResult, any, error) {
		op := a.ops[in.Operation]
		if op == nil {
			return nil, nil, fmt.Errorf("unknown operation %q: look it up with bexio_docs", in.Operation)
		}
		if readOnly && !op.readOnly() {
			return nil, nil, fmt.Errorf("%s changes data: use bexio_write", op.ID)
		}
		text, err := call(ctx, req.Extra.Header.Get("Authorization"), op, in)
		return textResult(text), nil, err
	}
}

func call(ctx context.Context, authorization string, op *operation, in callInput) (string, error) {
	path := op.Path
	for k, v := range in.PathParams {
		path = strings.ReplaceAll(path, "{"+k+"}", url.PathEscape(param(v)))
	}
	if strings.Contains(path, "{") {
		return "", fmt.Errorf("missing path parameter in %s", path)
	}
	query := url.Values{}
	for k, v := range in.Query {
		query.Set(k, param(v))
	}
	var body io.Reader
	if in.Body != nil {
		b, err := json.Marshal(in.Body)
		if err != nil {
			return "", err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, op.Method, apiURL+path+"?"+query.Encode(), body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResult+1))
	switch {
	case err != nil:
		return "", err
	case resp.StatusCode == http.StatusUnauthorized:
		return "", errors.New("bexio rejected the token: reconnect the connector")
	case resp.StatusCode >= 300:
		return "", fmt.Errorf("bexio answered %s: %s", resp.Status, data)
	case len(data) == 0:
		return resp.Status, nil // e.g. 204 after a 4.0 delete
	case len(data) > maxResult:
		return string(data[:maxResult]) + "\n[truncated: narrow the request with limit/offset or a search]", nil
	}
	return string(data), nil
}

// param formats a JSON value for a URL; JSON numbers arrive as float64,
// which fmt would print as 1.234567e+06.
func param(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}
