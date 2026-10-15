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
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
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

var client = &http.Client{Timeout: time.Minute}

// baseScopes are requested by every connector: identity, refresh tokens and
// the company profile.
const baseScopes = "openid profile email offline_access company_profile"

// modules group bexio's OAuth scopes; a connector URL can pick a subset.
// Banking payments and payroll are deliberately absent, so no connector can
// move money or read salary data. One unknown scope makes bexio reject the
// whole sign-in: only add verified scope names.
var modules = map[string]string{
	"contacts":    "contact_show contact_edit",
	"quotes":      "kb_offer_show kb_offer_edit kb_order_edit kb_invoice_edit",
	"orders":      "kb_order_show kb_order_edit kb_invoice_edit kb_delivery_edit",
	"invoices":    "kb_invoice_show kb_invoice_edit",
	"items":       "article_show article_edit stock_edit kb_delivery_show kb_delivery_edit",
	"projects":    "project_show project_edit monitoring_show monitoring_edit",
	"notes-tasks": "note_show note_edit task_show task_edit",
	"master-data": "bank_account_show",
	"files":       "file",
	"accounting":  "accounting",
	"purchase":    "contact_show kb_bill_show kb_expense_show kb_article_order_show kb_article_order_edit",
}

func main() {
	publicURL := strings.TrimSuffix(os.Getenv("PUBLIC_URL"), "/")
	if publicURL == "" {
		log.Fatal("PUBLIC_URL is required, e.g. https://bexio-mcp.example.com")
	}
	addr := ":" + cmp.Or(os.Getenv("PORT"), "8080")
	log.Printf("listening on %s, connector URL %s/mcp", addr, publicURL)
	srv := &http.Server{
		Addr: addr, Handler: newHandler(publicURL),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute, // bexio calls time out after 1 minute
	}
	log.Fatal(srv.ListenAndServe())
}

// connectorConfig reads the options in a connector URL,
// /mcp[/<label>][/<module,module,...>][/readonly]: the optional label only
// names the connector, no modules means all, and readonly drops every write
// scope and the bexio_write tool. A first segment that reads as an option
// is one, so /mcp/readonly is read-only, not a connector named "readonly".
func connectorConfig(path string) (scopes []string, readOnly bool, err error) {
	picked := map[string]bool{}
	segments := strings.Split(strings.Trim(strings.TrimPrefix(path, "/mcp"), "/"), "/")
	for i, segment := range segments {
		names := strings.Split(segment, ",")
		switch {
		case segment == "readonly":
			readOnly = true
		case !slices.ContainsFunc(names, func(m string) bool { return modules[m] == "" }):
			for _, m := range names {
				picked[m] = true
			}
		case i > 0 || len(names) > 1: // a comma list is never a label
			return nil, false, fmt.Errorf("unknown module in %q, known: %s", segment, strings.Join(slices.Sorted(maps.Keys(modules)), ", "))
		}
	}
	scopes = strings.Fields(baseScopes)
	for _, m := range slices.Sorted(maps.Keys(modules)) {
		if len(picked) > 0 && !picked[m] {
			continue
		}
		for _, s := range strings.Fields(modules[m]) {
			// stock_edit is the only scope that reads stock, so it stays.
			if !slices.Contains(scopes, s) && !(readOnly && strings.HasSuffix(s, "_edit") && s != "stock_edit") {
				scopes = append(scopes, s)
			}
		}
	}
	return scopes, readOnly, nil
}

func newHandler(publicURL string) http.Handler {
	a := loadAPI()
	servers := map[bool]*mcp.Server{false: newServer(a, false), true: newServer(a, true)}
	mcpHandler := requireBearer(mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		_, readOnly, _ := connectorConfig(r.URL.Path) // validated by requireBearer
		return servers[readOnly]
	}, &mcp.StreamableHTTPOptions{Stateless: true}), publicURL)

	// The token decides the bexio company; the URL only picks scopes, so each
	// company (or scope set) is its own claude.ai connector.
	metadata := func(w http.ResponseWriter, r *http.Request) {
		path := cmp.Or(strings.TrimPrefix(r.URL.Path, prmPath), "/mcp")
		scopes, _, err := connectorConfig(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(&oauthex.ProtectedResourceMetadata{
			Resource:               publicURL + path,
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
	mux.HandleFunc("GET "+prmPath+"/", metadata)
	return mux
}

// requireBearer answers requests without a usable token with the 401 that
// starts claude.ai's sign-in: it points at the protected resource metadata,
// which names bexio as the authorization server, and asks for the scopes the
// connector URL selects.
func requireBearer(next http.Handler, publicURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scopes, _, err := connectorConfig(r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		// Access log without tokens or bodies: enough to trace a client's sign-in.
		authorization := r.Header.Get("Authorization")
		if !tokenUsable(authorization) {
			log.Printf("%s %s 401 (bearer sent: %t) %q", r.Method, r.URL.Path, authorization != "", r.UserAgent())
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q, scope=%q`,
				publicURL+prmPath+r.URL.Path, strings.Join(scopes, " ")))
			http.Error(w, "sign in to bexio", http.StatusUnauthorized)
			return
		}
		log.Printf("%s %s %s %q", r.Method, r.URL.Path, r.Header.Get("Mcp-Method"), r.UserAgent())
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // tool calls are small JSON-RPC messages
		next.ServeHTTP(w, r)
	})
}

// tokenUsable checks for a bearer token that has not expired. The token is
// deliberately not verified: it is only ever forwarded to bexio's own API,
// which validates it (this server owns no data to protect). Reading the
// unverified exp claim lets Claude refresh before bexio rejects calls.
func tokenUsable(header string) bool {
	scheme, token, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, "Bearer") || token == "" {
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
	Query      map[string]any `json:"query,omitempty" jsonschema:"query string parameters, e.g. {\"limit\": 20, \"order_by\": \"id_desc\"}; arrays repeat the parameter"`
	Body       any            `json:"body,omitempty" jsonschema:"JSON request body as documented by bexio_docs"`
}

func newServer(a *api, readOnly bool) *mcp.Server {
	instructions := "Live access to one bexio company (accounting, CRM, invoicing). Call bexio_docs first: " +
		"it explains the API conventions and lists the operations. "
	if readOnly {
		instructions += "This connector is read-only."
	} else {
		instructions += "Writes change real books: confirm with the user before creating, changing, deleting, issuing or sending anything."
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "bexio", Version: "0.1.0"}, &mcp.ServerOptions{Instructions: instructions})
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
	if !readOnly {
		mcp.AddTool(s, &mcp.Tool{
			Name:        "bexio_write",
			Description: "Run a bexio operation that changes data (create, edit, delete, issue, send, ...) by operationId. Changes the company's real books: confirm with the user first.",
			Annotations: &mcp.ToolAnnotations{DestructiveHint: new(true)},
		}, a.caller(false))
	}
	return s
}

func (a *api) caller(readOnly bool) mcp.ToolHandlerFor[callInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in callInput) (*mcp.CallToolResult, any, error) {
		op := a.ops[in.Operation]
		if op == nil {
			return nil, nil, fmt.Errorf("unknown operation %q: look it up with bexio_docs", in.Operation)
		}
		if readOnly && !op.readOnly() {
			return nil, nil, fmt.Errorf("%s changes data: it needs bexio_write, which read-only connectors don't have", op.ID)
		}
		text, err := call(ctx, req.Extra.Header.Get("Authorization"), op, in)
		return textResult(text), nil, err
	}
}

func call(ctx context.Context, authorization string, op *operation, in callInput) (string, error) {
	path := op.Path
	for k, v := range in.PathParams {
		// "." and ".." would climb out of the operation's path.
		if p := param(v); p != "" && p != "." && p != ".." {
			path = strings.ReplaceAll(path, "{"+k+"}", url.PathEscape(p))
		}
	}
	if strings.Contains(path, "{") {
		return "", fmt.Errorf("missing or invalid path parameter in %s", path)
	}
	query := url.Values{}
	for k, v := range in.Query {
		values, ok := v.([]any)
		if !ok {
			values = []any{v}
		}
		for _, v := range values {
			if v != nil {
				query.Add(k, param(v))
			}
		}
	}
	var body io.Reader
	if in.Body != nil {
		b, _ := json.Marshal(in.Body) // decoded from JSON, so it re-encodes
		if s, ok := in.Body.(string); ok && json.Valid([]byte(s)) {
			b = []byte(s) // the model sent the body JSON-encoded as a string
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
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	log.Printf("bexio %s %d", op.ID, resp.StatusCode)
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
		return strings.ToValidUTF8(string(data[:maxResult]), "") + "\n[truncated: narrow the request with limit/offset or a search]", nil
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
