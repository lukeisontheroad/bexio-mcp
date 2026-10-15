package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

func jwt(exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, exp.Unix()))
	return "eyJhbGciOiJub25lIn0." + payload + ".sig"
}

func TestServer(t *testing.T) {
	var got *http.Request
	var gotBody string
	bexio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got, gotBody = r, string(body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"id":1}]`)
	}))
	defer bexio.Close()
	apiURL = bexio.URL

	srv := httptest.NewServer(newHandler("https://mcp.example.com"))
	defer srv.Close()

	// Missing and expired tokens start the sign-in.
	for _, token := range []string{"", jwt(time.Now().Add(-time.Minute))} {
		req, _ := http.NewRequest("POST", srv.URL+"/mcp/acme/contacts/readonly", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		want := `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp/acme/contacts/readonly", ` +
			`scope="openid profile email offline_access company_profile contact_show"`
		if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != want {
			t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}

	if resp, _ := http.Post(srv.URL+"/mcp/acme/payroll", "application/json", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown module answered %d", resp.StatusCode)
	}

	resp, err := http.Get(srv.URL + prmPath + "/mcp/acme")
	if err != nil {
		t.Fatal(err)
	}
	var prm oauthex.ProtectedResourceMetadata
	json.NewDecoder(resp.Body).Decode(&prm)
	if prm.Resource != "https://mcp.example.com/mcp/acme" || prm.AuthorizationServers[0] != issuer {
		t.Fatalf("metadata %+v", prm)
	}

	token := jwt(time.Now().Add(time.Hour))
	connect := func(path string) *mcp.ClientSession {
		t.Helper()
		session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(),
			&mcp.StreamableClientTransport{Endpoint: srv.URL + path, HTTPClient: &http.Client{Transport: bearer(token)}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Close() })
		return session
	}
	session := connect("/mcp/acme")
	callTool := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	text := func(res *mcp.CallToolResult) string { return res.Content[0].(*mcp.TextContent).Text }

	res := callTool("bexio_read", map[string]any{
		"operation": "v2SearchContact", "query": map[string]any{"limit": 2000, "fields[]": []any{"id", "name_1"}},
		"body": []any{map[string]any{"field": "name_1", "value": "%acme%", "criteria": "like"}},
	})
	if res.IsError || text(res) != `[{"id":1}]` {
		t.Fatalf("search: %v", text(res))
	}
	if got.Method != "POST" || got.URL.Path != "/2.0/contact/search" || got.URL.RawQuery != "fields%5B%5D=id&fields%5B%5D=name_1&limit=2000" ||
		got.Header.Get("Authorization") != "Bearer "+token || !strings.Contains(gotBody, `"%acme%"`) {
		t.Fatalf("forwarded %s %s body %s auth %q", got.Method, got.URL, gotBody, got.Header.Get("Authorization"))
	}

	callTool("bexio_read", map[string]any{"operation": "v2ShowContact", "path_params": map[string]any{"contact_id": 1234567}})
	if got.URL.Path != "/2.0/contact/1234567" {
		t.Fatalf("path %s", got.URL.Path)
	}

	if res := callTool("bexio_read", map[string]any{"operation": "v2ShowContact", "path_params": map[string]any{"contact_id": ".."}}); !res.IsError {
		t.Fatal("'..' path parameter was forwarded")
	}

	if res := callTool("bexio_read", map[string]any{"operation": "v2DeleteContact", "path_params": map[string]any{"contact_id": 1}}); !res.IsError {
		t.Fatal("bexio_read ran a write operation")
	}

	if res := callTool("bexio_docs", map[string]any{}); !strings.Contains(text(res), "Contacts (") || strings.Contains(text(res), "Employees") {
		t.Fatalf("index: %s", text(res))
	}

	tools, err := connect("/mcp/acme/readonly").ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 2 || slices.ContainsFunc(tools.Tools, func(t *mcp.Tool) bool { return t.Name == "bexio_write" }) {
		t.Fatalf("read-only connector tools: %v %v", tools, err)
	}
}

func TestDocs(t *testing.T) {
	a := loadAPI()
	for id := range a.ops {
		if doc, err := a.doc(id); err != nil || !strings.Contains(doc, id) {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if _, err := a.doc("payments"); err == nil {
		t.Fatal("banking payments are documented")
	}
	doc, _ := a.doc("v2ShowContact")
	if !strings.Contains(doc, "contact_id (path, required)") {
		t.Fatalf("path-level parameter missing:\n%s", doc)
	}
	for _, m := range regexp.MustCompile("`((?:v[23])?[A-Z]?[a-z]+[A-Z]\\w+)`").FindAllStringSubmatch(guide, -1) {
		if a.ops[m[1]] == nil {
			t.Errorf("guide.md names unknown operation %s", m[1])
		}
	}
}
