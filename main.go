// Command bexio-mcp (BexTrackMCP) is a remote MCP server for the bexio API.
//
// It is its own OAuth authorization server towards MCP clients (bexio cannot
// register them) and signs users in at bexio with one confidential client.
// It stores nothing: everything it issues is sealed, and bexio's tokens are
// only unwrapped to forward a request to api.bexio.com.
package main

import (
	"bytes"
	"cmp"
	"context"
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
	prmPath = "/.well-known/oauth-protected-resource"
	// ponytail: hard cap per tool result; a 2000-row list would flood the
	// chat context. Claude pages with limit/offset when it sees the note.
	maxResult = 100_000
)

// version is set at build time (-ldflags "-X main.version=1.2.3").
var version = "dev"

// apiURL is the bexio API base; tests point it at a fake.
var apiURL = "https://api.bexio.com"

var client = &http.Client{Timeout: time.Minute}

func main() {
	publicURL := strings.TrimSuffix(os.Getenv("PUBLIC_URL"), "/")
	clientID, clientSecret := os.Getenv("BEXIO_CLIENT_ID"), os.Getenv("BEXIO_CLIENT_SECRET")
	if publicURL == "" || clientID == "" || clientSecret == "" {
		log.Fatal("PUBLIC_URL, BEXIO_CLIENT_ID and BEXIO_CLIENT_SECRET are required")
	}
	addr := ":" + cmp.Or(os.Getenv("PORT"), "8080")
	log.Printf("BexTrackMCP %s listening on %s, connector URL %s/mcp", version, addr, publicURL)
	srv := &http.Server{
		Addr: addr, Handler: newHandler(newAuthServer(publicURL, clientID, clientSecret)),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute, // bexio calls time out after 1 minute
	}
	log.Fatal(srv.ListenAndServe())
}

type readOnlyKey struct{}

func newHandler(auth *authServer) http.Handler {
	a := loadAPI()
	servers := map[bool]*mcp.Server{false: newServer(a, false), true: newServer(a, true)}
	mcpHandler := requireBearer(mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return servers[r.Context().Value(readOnlyKey{}) == true]
	}, &mcp.StreamableHTTPOptions{Stateless: true}), auth)

	// /mcp/<label> serves the same thing under its own URL, so several
	// connectors (one per bexio company) can coexist in one client.
	metadata := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, &oauthex.ProtectedResourceMetadata{
			Resource:               auth.publicURL + cmp.Or(strings.TrimPrefix(r.URL.Path, prmPath), "/mcp"),
			AuthorizationServers:   []string{auth.publicURL},
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
	auth.routes(mux)
	return mux
}

// requireBearer unwraps the server's own access token into the bexio token
// it carries. Without a valid one it answers the 401 that starts the
// client's sign-in, pointing at the protected resource metadata.
func requireBearer(next http.Handler, auth *authServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Access log with paths only: queries and bodies carry codes and tokens.
		scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		access, ok := auth.open(token, "access")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			log.Printf("%s %s 401 (bearer sent: %t) %q", r.Method, r.URL.Path, token != "", r.UserAgent())
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, auth.publicURL+prmPath+r.URL.Path))
			http.Error(w, "sign in to bexio", http.StatusUnauthorized)
			return
		}
		log.Printf("%s %s %s %q", r.Method, r.URL.Path, r.Header.Get("Mcp-Method"), r.UserAgent())
		r.Header.Set("Authorization", "Bearer "+access.Token)
		r = r.WithContext(context.WithValue(r.Context(), readOnlyKey{}, access.ReadOnly))
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // tool calls are small JSON-RPC messages
		next.ServeHTTP(w, r)
	})
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
	s := mcp.NewServer(&mcp.Implementation{Name: "BexTrackMCP", Version: version}, &mcp.ServerOptions{Instructions: instructions})
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
