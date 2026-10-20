package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

const publicURL = "https://mcp.example.com"

// fakeBexio records what reached bexio's token endpoint and API.
type fakeBexio struct {
	api     *http.Request
	apiBody string
	token   url.Values
	company string // company_name in the id token
}

func setup(t *testing.T) (*httptest.Server, *authServer, *fakeBexio) {
	fake := &fakeBexio{}
	bexio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/token" {
			fake.api, fake.apiBody = r, string(body)
			io.WriteString(w, `[{"id":1}]`)
			return
		}
		fake.token, _ = url.ParseQuery(string(body))
		if fake.token.Get("client_secret") != "bexio-secret" || fake.token.Has("code") && fake.token.Get("code") != "bexio-code" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		idToken := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"company_name":"`+fake.company+`"}`)) + ".sig"
		io.WriteString(w, `{"access_token":"bexio-access","refresh_token":"bexio-refresh","expires_in":300,"id_token":"`+idToken+`"}`)
	}))
	t.Cleanup(bexio.Close)
	apiURL, bexioTokenURL = bexio.URL, bexio.URL+"/token"
	auth := newAuthServer(publicURL, "bexio-client", "bexio-secret")
	srv := httptest.NewServer(newHandler(auth))
	t.Cleanup(srv.Close)
	return srv, auth, fake
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearer(token)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestServer(t *testing.T) {
	srv, auth, fake := setup(t)

	// Anything but a valid access token of this server starts the sign-in:
	// nothing, a raw bexio token, an expired token, another sealed kind.
	expired := auth.seal(sealed{Kind: "access", Exp: time.Now().Add(-time.Minute).Unix(), Token: "bexio-access"})
	legacy := auth.seal(sealed{Kind: "access", Exp: time.Now().Add(time.Hour).Unix(), Token: "bexio-access"}) // no areas yet
	for _, token := range []string{"", "bexio-access", expired, legacy, auth.seal(sealed{Kind: "refresh", Token: "bexio-access"})} {
		req, _ := http.NewRequest("POST", srv.URL+"/mcp/acme", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		want := `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp/acme"`
		if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != want {
			t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}

	resp, err := http.Get(srv.URL + prmPath + "/mcp/acme")
	if err != nil {
		t.Fatal(err)
	}
	var prm oauthex.ProtectedResourceMetadata
	json.NewDecoder(resp.Body).Decode(&prm)
	if prm.Resource != publicURL+"/mcp/acme" || prm.AuthorizationServers[0] != publicURL {
		t.Fatalf("metadata %+v", prm)
	}

	all := map[string]bool{}
	for _, m := range modules {
		all[m.Name] = !m.ReadOnly
	}
	token := auth.seal(sealed{Kind: "access", Exp: time.Now().Add(time.Hour).Unix(), Token: "bexio-access", Access: all})
	session := connect(t, srv.URL+"/mcp/acme", token)
	callOn := func(session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	callTool := func(name string, args map[string]any) *mcp.CallToolResult { return callOn(session, name, args) }
	text := func(res *mcp.CallToolResult) string { return res.Content[0].(*mcp.TextContent).Text }

	res := callTool("bexio_read", map[string]any{
		"operation": "v2SearchContact", "query": map[string]any{"limit": 2000, "fields[]": []any{"id", "name_1"}},
		"body": []any{map[string]any{"field": "name_1", "value": "%acme%", "criteria": "like"}},
	})
	got := fake.api
	if res.IsError || text(res) != `[{"id":1}]` {
		t.Fatalf("search: %v", text(res))
	}
	if got.Method != "POST" || got.URL.Path != "/2.0/contact/search" || got.URL.RawQuery != "fields%5B%5D=id&fields%5B%5D=name_1&limit=2000" ||
		got.Header.Get("Authorization") != "Bearer bexio-access" || !strings.Contains(fake.apiBody, `"%acme%"`) {
		t.Fatalf("forwarded %s %s body %s auth %q", got.Method, got.URL, fake.apiBody, got.Header.Get("Authorization"))
	}

	callTool("bexio_read", map[string]any{"operation": "v2ShowContact", "path_params": map[string]any{"contact_id": 1234567}})
	if fake.api.URL.Path != "/2.0/contact/1234567" {
		t.Fatalf("path %s", fake.api.URL.Path)
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

	// Per area: contacts read only, quotes read and write, the rest off.
	some := connect(t, srv.URL+"/mcp/acme", auth.seal(sealed{Kind: "access", Exp: time.Now().Add(time.Hour).Unix(), Token: "bexio-access",
		Access: map[string]bool{"contacts": false, "quotes": true}}))
	position := func(docType string) map[string]any {
		return map[string]any{"operation": "v2CreateDefaultPosition", "path_params": map[string]any{"kb_document_type": docType, "document_id": 1}}
	}
	for name, res := range map[string]*mcp.CallToolResult{
		"write to a read-only area":       callOn(some, "bexio_write", map[string]any{"operation": "v2DeleteContact", "path_params": map[string]any{"contact_id": 1}}),
		"read an area that is off":        callOn(some, "bexio_read", map[string]any{"operation": "v2ListInvoices"}),
		"invoice position, invoices off":  callOn(some, "bexio_write", position("kb_invoice")),
		"position of an unknown document": callOn(some, "bexio_write", position("contact")),
		"group name as document type":     callOn(some, "bexio_write", position("Quotes")),
		"path climbing into another area": callOn(some, "bexio_write", map[string]any{"operation": "v2EditQuote",
			"path_params": map[string]any{"quote_id": "../../3.0/accounting/manual_entries"}}),
		"master data off, which needs no scope": callOn(some, "bexio_read", map[string]any{"operation": "v2ListUnits"}),
	} {
		if !res.IsError {
			t.Errorf("%s was forwarded", name)
		}
	}
	if res := callOn(some, "bexio_write", position("kb_offer")); res.IsError || fake.api.URL.Path != "/2.0/kb_offer/1/kb_position_custom" {
		t.Fatalf("quote position: %s, forwarded %s", text(res), fake.api.URL.Path)
	}
	if res := callOn(some, "bexio_read", map[string]any{"operation": "v2ShowContact", "path_params": map[string]any{"contact_id": 2}}); res.IsError {
		t.Fatalf("read a read-only area: %s", text(res))
	}
}

// TestSignIn walks a client through registration, consent, bexio's callback,
// the code exchange and a refresh, and checks the attacks each step stops.
func TestSignIn(t *testing.T) {
	srv, auth, fake := setup(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	postJSON := func(path, body string) *http.Response {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	tokenRequest := func(form url.Values) (int, map[string]any) {
		resp, err := http.PostForm(srv.URL+"/oauth/token", form)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	var meta struct{ Issuer string }
	resp, _ := http.Get(srv.URL + "/.well-known/oauth-authorization-server")
	json.NewDecoder(resp.Body).Decode(&meta)
	if meta.Issuer != publicURL {
		t.Fatalf("issuer %q", meta.Issuer)
	}

	// Plain http, OS protocol handlers and lookalike hosts are refused.
	for _, uri := range []string{"http://evil.example.com/cb", "ms-msdt:id", "https://\u0441laude.ai/cb"} {
		if resp := postJSON("/oauth/register", `{"redirect_uris":["`+uri+`"]}`); resp.StatusCode != 400 {
			t.Fatalf("%s registered: %d", uri, resp.StatusCode)
		}
	}
	resp = postJSON("/oauth/register", `{"redirect_uris":["https://client.example.com/cb","http://127.0.0.1/callback",`+
		`"cursor://anysphere.cursor-mcp/oauth/callback","vscode://cb"],"client_name":"Test <b>"}`)
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	if resp.StatusCode != 201 || reg.ClientID == "" {
		t.Fatalf("register: %d", resp.StatusCode)
	}

	verifier := "client-verifier-0123456789012345678901234567890"
	authorize := func(redirect, responseType, resource string) *http.Response {
		resp, err := noRedirect.Get(srv.URL + "/oauth/authorize?" + url.Values{
			"client_id": {reg.ClientID}, "redirect_uri": {redirect}, "response_type": {responseType},
			"state": {"xyz"}, "code_challenge": {pkceChallenge(verifier)}, "code_challenge_method": {"S256"},
			"resource": {resource},
		}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	// Before consent nothing redirects, not even to a registered URI.
	pinned := publicURL + "/mcp/Acme%20AG"
	for _, resp := range []*http.Response{authorize("https://evil.example.com/cb", "code", pinned), authorize("https://client.example.com/cb", "token", pinned)} {
		if resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
			t.Fatalf("bad authorize request: %d %q", resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// Plain /mcp pins nothing.
	if resp := authorize("http://127.0.0.1:53682/callback", "code", publicURL+"/mcp"); resp.StatusCode != 200 {
		t.Fatalf("loopback on another port: %d", resp.StatusCode)
	} else if page, _ := io.ReadAll(resp.Body); strings.Contains(string(page), "This connector is for") {
		t.Fatalf("unpinned connector shows a company: %s", page)
	}

	resp = authorize("https://client.example.com/cb", "code", pinned)
	page, _ := io.ReadAll(resp.Body)
	request := regexp.MustCompile(`name="request" value="([\w-]+)"`).FindSubmatch(page)
	cookies := resp.Cookies()
	if resp.StatusCode != 200 || request == nil || len(cookies) != 1 || strings.Contains(string(page), "<b>") || !strings.Contains(string(page), "This connector is for <span>acme-ag</span>") ||
		!strings.Contains(string(page), `name="contacts" value="write" checked`) {
		t.Fatalf("consent page: %d %d cookies\n%s", resp.StatusCode, len(cookies), page)
	}
	consent := func(withCookie bool, action string) *http.Response {
		form := url.Values{"request": {string(request[1])}, "contacts": {"write"}, "readonly": {"1"}, "action": {action}}
		req, _ := http.NewRequest("POST", srv.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if withCookie {
			req.AddCookie(cookies[0])
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := consent(false, "allow"); resp.StatusCode != 403 {
		t.Fatalf("consent without the browser's cookie: %d", resp.StatusCode)
	}
	if resp := consent(true, "deny"); resp.StatusCode != 200 || resp.Header.Get("Location") != "" {
		t.Fatalf("cancel redirected: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp = consent(true, "allow")
	toBexio, _ := url.Parse(resp.Header.Get("Location"))
	if q := toBexio.Query(); resp.StatusCode != 303 || q.Get("client_id") != "bexio-client" || q.Get("redirect_uri") != publicURL+"/oauth/callback" ||
		q.Get("scope") != "openid profile email offline_access company_profile contact_show" {
		t.Fatalf("redirect to bexio: %d %s", resp.StatusCode, toBexio)
	}

	callback := func(withCookie bool) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+"/oauth/callback?"+url.Values{"code": {"bexio-code"}, "state": {toBexio.Query().Get("state")}}.Encode(), nil)
		if withCookie {
			req.AddCookie(cookies[0])
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	// A bexio link from someone else's consent must not complete in this browser.
	if resp := callback(false); resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
		t.Fatalf("callback without the browser's cookie: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// The connector URL pins Acme AG: a sign-in to another company is refused
	// with a page naming it and its connector URL.
	fake.company = "Acme Group AG"
	if resp := callback(true); resp.StatusCode != 403 || resp.Header.Get("Location") != "" {
		t.Fatalf("other company: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	} else if page, _ := io.ReadAll(resp.Body); !strings.Contains(string(page), "This connector is for acme-ag, but you picked Acme Group AG") ||
		!strings.Contains(string(page), publicURL+"/mcp/acme-group-ag") {
		t.Fatalf("other company page: %s", page)
	}
	// Without a company in the connector URL any company is fine.
	unpinned, _ := http.NewRequest("GET", srv.URL+"/oauth/callback?"+url.Values{"code": {"bexio-code"}, "state": {auth.seal(sealed{
		Kind: "state", Exp: time.Now().Add(time.Minute).Unix(), Redirect: "https://client.example.com/cb", Nonce: "n"})}}.Encode(), nil)
	unpinned.AddCookie(&http.Cookie{Name: auth.flowCookie("n", 0).Name, Value: "1"})
	if resp, err := noRedirect.Do(unpinned); err != nil || resp.StatusCode != 302 {
		t.Fatalf("unpinned sign-in: %v %v", resp, err)
	}
	fake.company = "ACME ag"
	resp = callback(true)
	if fake.token.Get("code") != "bexio-code" || fake.token.Get("code_verifier") == "" || fake.token.Get("client_secret") != "bexio-secret" {
		t.Fatalf("bexio got %v", fake.token)
	}
	back, _ := url.Parse(resp.Header.Get("Location"))
	code := back.Query().Get("code")
	if back.Host != "client.example.com" || back.Query().Get("state") != "xyz" || back.Query().Get("iss") != publicURL || code == "" {
		t.Fatalf("redirect to client: %s", back)
	}

	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {reg.ClientID},
		"redirect_uri": {"https://client.example.com/cb"}, "code_verifier": {verifier}}
	for name, change := range map[string][2]string{
		"wrong verifier":     {"code_verifier", "guess"},
		"other client":       {"client_id", "someone-else"},
		"client_id as code":  {"code", reg.ClientID},
		"different redirect": {"redirect_uri", "https://client.example.com/other"},
	} {
		bad := url.Values{}
		for k, v := range exchange {
			bad[k] = v
		}
		bad.Set(change[0], change[1])
		if status, body := tokenRequest(bad); status != 400 || body["error"] != "invalid_grant" {
			t.Fatalf("%s: %d %v", name, status, body)
		}
	}
	status, body := tokenRequest(exchange)
	if status != 200 || fake.token.Get("refresh_token") != "bexio-refresh" || fake.token.Get("client_secret") != "bexio-secret" {
		t.Fatalf("exchange: %d %v, bexio got %v", status, body, fake.token)
	}

	// The issued token is read-only, as consented, and unwraps to bexio's.
	session := connect(t, srv.URL+"/mcp/acme", body["access_token"].(string))
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 2 || slices.ContainsFunc(tools.Tools, func(t *mcp.Tool) bool { return t.Name == "bexio_write" }) {
		t.Fatalf("read-only tools: %v %v", tools, err)
	}
	session.CallTool(context.Background(), &mcp.CallToolParams{Name: "bexio_read", Arguments: map[string]any{"operation": "v2ShowContact", "path_params": map[string]any{"contact_id": 1}}})
	if fake.api.Header.Get("Authorization") != "Bearer bexio-access" {
		t.Fatalf("forwarded %q", fake.api.Header.Get("Authorization"))
	}

	status, refreshed := tokenRequest(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {body["refresh_token"].(string)}, "client_id": {reg.ClientID}})
	if status != 200 || fake.token.Get("refresh_token") != "bexio-refresh" || refreshed["access_token"] == nil {
		t.Fatalf("refresh: %d %v", status, refreshed)
	}
}

func TestScopes(t *testing.T) {
	base := "openid profile email offline_access company_profile "
	for want, access := range map[string]map[string]bool{
		"contact_show contact_edit kb_invoice_show": {"contacts": true, "invoices": false},
		"article_show stock_edit kb_delivery_show":  {"items": false},
		"": {"master-data": true},
	} {
		if got := scopesFor(access); got != strings.TrimSpace(base+want) {
			t.Errorf("scopesFor(%v) = %q", access, got)
		}
	}
}

func TestSlug(t *testing.T) {
	for name, want := range map[string]string{"Acme AG": "acme-ag", " Acme Group, AG ": "acme-group-ag", "Müller & Söhne AG": "müller-söhne-ag", "--": ""} {
		if got := slug(name); got != want {
			t.Errorf("slug(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestScrubEvent(t *testing.T) {
	event := scrubEvent(&sentry.Event{
		Request: &sentry.Request{Method: "GET", URL: "https://mcp.example.com/oauth/callback", QueryString: "code=secret",
			Headers: map[string]string{"Authorization": "Bearer secret"}, Cookies: "flow=secret", Data: "secret"},
		User: sentry.User{IPAddress: "192.0.2.1"},
	}, nil)
	if r := event.Request; r.QueryString != "" || r.Headers != nil || r.Cookies != "" || r.Data != "" || r.URL == "" || event.User.IPAddress != "" {
		t.Fatalf("not scrubbed: %+v %+v", event.Request, event.User)
	}
}

func TestDocs(t *testing.T) {
	a := loadAPI()
	for id := range a.ops {
		if doc, err := a.doc(id); err != nil || !strings.Contains(doc, id) {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if _, err := a.doc("payments"); err == nil || a.ops["ApiOutgoingPayment_POST"] != nil {
		t.Fatal("bank payments are documented")
	}
	for id, op := range a.ops {
		if op.area(callInput{PathParams: map[string]any{"kb_document_type": "kb_offer"}}).Name == "" {
			t.Errorf("%s (%s) is in no consent-page area", id, op.Tag)
		}
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
