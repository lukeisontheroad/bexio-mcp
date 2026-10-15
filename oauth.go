package main

import (
	"cmp"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
)

// bexio's Keycloak; tests point them at a fake.
var (
	bexioAuthURL  = "https://auth.bexio.com/realms/bexio/protocol/openid-connect/auth"
	bexioTokenURL = "https://auth.bexio.com/realms/bexio/protocol/openid-connect/token"
)

// baseScopes are requested on every sign-in: identity, refresh tokens and
// the company profile.
const baseScopes = "openid profile email offline_access company_profile"

type module struct{ Name, Label, Scopes string }

// modules are the areas offered on the consent page. Banking payments and
// payroll are deliberately absent, so no client can move money or read salary
// data. One unknown scope makes bexio reject the whole sign-in: only add
// verified scope names.
var modules = []module{
	{"contacts", "Contacts", "contact_show contact_edit"},
	{"quotes", "Quotes", "kb_offer_show kb_offer_edit kb_order_edit kb_invoice_edit"},
	{"orders", "Orders", "kb_order_show kb_order_edit kb_invoice_edit kb_delivery_edit"},
	{"invoices", "Invoices", "kb_invoice_show kb_invoice_edit"},
	{"items", "Items, stock and deliveries", "article_show article_edit stock_edit kb_delivery_show kb_delivery_edit"},
	{"projects", "Projects and time tracking", "project_show project_edit monitoring_show monitoring_edit"},
	{"notes-tasks", "Notes and tasks", "note_show note_edit task_show task_edit"},
	{"purchase", "Purchase: bills, expenses, purchase orders", "contact_show kb_bill_show kb_expense_show kb_article_order_show kb_article_order_edit"},
	{"accounting", "Accounting", "accounting"},
	{"files", "Files", "file"},
	{"master-data", "Bank accounts (read only)", "bank_account_show"},
}

// scopesFor turns the areas picked on the consent page into bexio scopes;
// read-only drops every write scope.
func scopesFor(picked []string, readOnly bool) string {
	scopes := strings.Fields(baseScopes)
	for _, m := range modules {
		if !slices.Contains(picked, m.Name) {
			continue
		}
		for _, s := range strings.Fields(m.Scopes) {
			// stock_edit is the only scope that reads stock, so it stays.
			if !slices.Contains(scopes, s) && !(readOnly && strings.HasSuffix(s, "_edit") && s != "stock_edit") {
				scopes = append(scopes, s)
			}
		}
	}
	return strings.Join(scopes, " ")
}

// sealed is every value the server hands out and later reads back: client
// ids, consent requests, the state sent to bexio, authorization codes and
// tokens. Encrypting them keeps the server stateless. Kind stops one kind
// from being replayed as another.
type sealed struct {
	Kind      string   `json:"k"`
	Exp       int64    `json:"e,omitempty"`
	Client    string   `json:"c,omitempty"` // hash of the client_id it was issued to
	URIs      []string `json:"u,omitempty"` // a client's registered redirect URIs
	Name      string   `json:"n,omitempty"`
	Redirect  string   `json:"r,omitempty"`
	State     string   `json:"s,omitempty"`
	Challenge string   `json:"p,omitempty"` // the client's PKCE challenge
	Verifier  string   `json:"v,omitempty"` // this server's PKCE verifier towards bexio
	Nonce     string   `json:"o,omitempty"`
	Code      string   `json:"d,omitempty"` // bexio authorization code
	Token     string   `json:"t,omitempty"` // bexio access or refresh token
	ReadOnly  bool     `json:"ro,omitempty"`
}

// authServer is the OAuth authorization server clients see. bexio cannot
// register clients, so this server registers them itself, asks the user
// which areas to share, and signs in at bexio with its own client, whose
// secret never leaves the server.
type authServer struct {
	publicURL, clientID, clientSecret string
	aead                              cipher.AEAD
}

func newAuthServer(publicURL, clientID, clientSecret string) *authServer {
	// Derived from the client secret: rotating it signs everyone out.
	key := sha256.Sum256([]byte("bexio-hosted-mcp seal\x00" + clientSecret))
	block, _ := aes.NewCipher(key[:])
	aead, _ := cipher.NewGCM(block)
	return &authServer{publicURL, clientID, clientSecret, aead}
}

func (s *authServer) seal(v sealed) string {
	plain, _ := json.Marshal(v)
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(s.aead.Seal(nonce, nonce, plain, nil))
}

// open returns a sealed value that is intact, of the given kind and unexpired.
func (s *authServer) open(text, kind string) (sealed, bool) {
	var v sealed
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return v, false
	}
	n := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil || json.Unmarshal(plain, &v) != nil || v.Kind != kind || (v.Exp != 0 && time.Now().Unix() > v.Exp) {
		return sealed{}, false
	}
	return v, true
}

func (s *authServer) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.metadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.metadata)
	mux.HandleFunc("POST /oauth/register", s.register)
	mux.HandleFunc("GET /oauth/authorize", s.authorize)
	mux.HandleFunc("POST /oauth/authorize", s.consent)
	mux.HandleFunc("GET /oauth/callback", s.callback)
	mux.HandleFunc("POST /oauth/token", s.token)
}

func (s *authServer) metadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         s.publicURL,
		"authorization_endpoint":                         s.publicURL + "/oauth/authorize",
		"token_endpoint":                                 s.publicURL + "/oauth/token",
		"registration_endpoint":                          s.publicURL + "/oauth/register",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"authorization_response_iss_parameter_supported": true,
	})
}

// register is dynamic client registration (RFC 7591). The client_id is the
// sealed registration itself, so nothing is stored.
func (s *authServer) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil || len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata")
		return
	}
	if slices.ContainsFunc(req.RedirectURIs, func(u string) bool { return !redirectAllowed(u) }) {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri")
		return
	}
	name := req.ClientName
	if r := []rune(name); len(r) > 100 {
		name = string(r[:100])
	}
	log.Print("register")
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  s.seal(sealed{Kind: "client", URIs: req.RedirectURIs, Name: name}),
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// redirectAllowed accepts https, http on loopback (native apps, RFC 8252)
// and the editors' own schemes. Any other scheme can launch an arbitrary OS
// handler, and non-ASCII hosts could pass for another host on the consent
// page.
func redirectAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2000 || u.Fragment != "" || strings.ContainsFunc(u.Host, func(r rune) bool { return r > unicode.MaxASCII }) {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		return isLoopback(u.Hostname())
	case "cursor", "vscode", "vscode-insiders":
		return true
	}
	return false
}

func isLoopback(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }

// redirectRegistered matches exactly, except that loopback redirects may use
// any port: native apps pick a fresh one per sign-in (RFC 8252 section 7.3).
func redirectRegistered(uris []string, got string) bool {
	g, err := url.Parse(got)
	return err == nil && slices.ContainsFunc(uris, func(r string) bool {
		u, _ := url.Parse(r)
		return r == got || u.Scheme == "http" && g.Scheme == "http" && isLoopback(u.Hostname()) &&
			u.Hostname() == g.Hostname() && u.Path == g.Path && u.RawQuery == g.RawQuery
	})
}

// authorize validates the request and shows the consent page. Errors are
// shown here, never redirected: before the user consents, this server must
// not send anyone to a URI a stranger registered.
func (s *authServer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	client, ok := s.open(q.Get("client_id"), "client")
	redirect, challenge := q.Get("redirect_uri"), q.Get("code_challenge")
	if !ok || !redirectAllowed(redirect) || !redirectRegistered(client.URIs, redirect) || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || len(challenge) != 43 || len(q.Get("state")) > 512 {
		http.Error(w, "Invalid sign-in request. Remove the connector and add it again.", http.StatusBadRequest)
		return
	}
	// A cookie per sign-in ties both the consent form and bexio's callback to
	// this browser: another site can neither submit the form for the user nor
	// hand the user a bexio link that ends at its own client. Lax, because
	// bexio's redirect back is a cross-site navigation.
	nonce := randomString()
	http.SetCookie(w, s.flowCookie(nonce, 600))
	u, _ := url.Parse(redirect)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	target := u.Host
	if u.Scheme != "https" {
		target = u.Scheme + "://" + u.Host
	}
	consentPage.Execute(w, map[string]any{
		"Version":    version,
		"HeaderLogo": headerLogo,
		"BadgeLogo":  badgeLogo,
		"Name":       client.Name,
		"Target":     target,
		"Local":      u.Scheme != "https",
		"Modules":    modules,
		"Request": s.seal(sealed{
			Kind: "consent", Exp: time.Now().Add(10 * time.Minute).Unix(), Client: clientHash(q.Get("client_id")),
			Redirect: redirect, State: q.Get("state"), Challenge: challenge, Nonce: nonce,
		}),
	})
}

// consent handles the consent form: deny goes back to the client, allow
// signs in at bexio with the scopes for the picked areas.
func (s *authServer) consent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	req, ok := s.open(r.PostFormValue("request"), "consent")
	if _, err := r.Cookie(s.flowCookie(req.Nonce, 0).Name); !ok || err != nil {
		http.Error(w, "This sign-in expired or was not started in this browser. Start it again from your app.", http.StatusForbidden)
		return
	}
	if r.PostFormValue("action") != "allow" {
		// No redirect: Cancel must not send anyone to a stranger's URI.
		http.SetCookie(w, s.flowCookie(req.Nonce, -1))
		http.Error(w, "Cancelled. You can close this window.", http.StatusOK)
		return
	}
	picked := r.PostForm["module"]
	if len(picked) == 0 {
		http.Error(w, "Pick at least one area: go back and try again.", http.StatusBadRequest)
		return
	}
	readOnly := r.PostFormValue("readonly") != ""
	verifier := randomString()
	http.SetCookie(w, s.flowCookie(req.Nonce, 600)) // the bexio login may take a while
	state := s.seal(sealed{
		Kind: "state", Exp: time.Now().Add(10 * time.Minute).Unix(), Client: req.Client, Redirect: req.Redirect,
		State: req.State, Challenge: req.Challenge, Verifier: verifier, Nonce: req.Nonce, ReadOnly: readOnly,
	})
	http.Redirect(w, r, bexioAuthURL+"?"+url.Values{
		"client_id":             {s.clientID},
		"redirect_uri":          {s.publicURL + "/oauth/callback"},
		"response_type":         {"code"},
		"scope":                 {scopesFor(picked, readOnly)},
		"state":                 {state},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}.Encode(), http.StatusSeeOther)
}

// callback receives bexio's answer and hands the client a code wrapping
// bexio's. The bexio code is redeemed only at the token endpoint, after the
// client proved its PKCE verifier; bexio accepts it once, so replays fail.
func (s *authServer) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	st, ok := s.open(q.Get("state"), "state")
	if _, err := r.Cookie(s.flowCookie(st.Nonce, 0).Name); !ok || err != nil {
		http.Error(w, "This sign-in expired or was not started in this browser. Start it again from your app.", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, s.flowCookie(st.Nonce, -1))
	if q.Get("code") == "" {
		e := q.Get("error")
		if !slices.Contains([]string{"invalid_request", "unauthorized_client", "invalid_scope", "server_error", "temporarily_unavailable"}, e) {
			e = "access_denied"
		}
		log.Printf("callback: bexio error %s", e)
		s.redirectClient(w, r, st.Redirect, url.Values{"error": {e}, "state": {st.State}})
		return
	}
	code := s.seal(sealed{
		Kind: "code", Exp: time.Now().Add(2 * time.Minute).Unix(), Client: st.Client, Redirect: st.Redirect,
		Challenge: st.Challenge, Verifier: st.Verifier, Code: q.Get("code"), ReadOnly: st.ReadOnly,
	})
	s.redirectClient(w, r, st.Redirect, url.Values{"code": {code}, "state": {st.State}})
}

// flowCookie marks one sign-in in progress in this browser. On https it is a
// __Host- cookie, so sibling subdomains cannot plant one in a victim's
// browser to complete an attacker's sign-in there.
func (s *authServer) flowCookie(nonce string, maxAge int) *http.Cookie {
	c := &http.Cookie{Name: "flow_" + nonce, Value: "1", Path: "/", MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if strings.HasPrefix(s.publicURL, "https:") {
		c.Name, c.Secure = "__Host-"+c.Name, true
	}
	return c
}

func (s *authServer) redirectClient(w http.ResponseWriter, r *http.Request, redirect string, params url.Values) {
	u, _ := url.Parse(redirect) // registered, so it parses
	q := u.Query()
	for k, v := range params {
		if v[0] != "" {
			q[k] = v
		}
	}
	q.Set("iss", s.publicURL) // RFC 9207
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// token redeems codes and refresh tokens at bexio with this server's client
// secret and returns bexio's tokens sealed, so they only work through here.
func (s *authServer) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	grantType, caller := r.PostFormValue("grant_type"), clientHash(r.PostFormValue("client_id"))
	var grant sealed
	form := url.Values{"client_id": {s.clientID}, "client_secret": {s.clientSecret}, "grant_type": {grantType}}
	switch grantType {
	case "authorization_code":
		c, ok := s.open(r.PostFormValue("code"), "code")
		if redirect := r.PostFormValue("redirect_uri"); !ok || c.Client != caller || redirect != "" && redirect != c.Redirect ||
			pkceChallenge(r.PostFormValue("code_verifier")) != c.Challenge {
			oauthError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		grant = c
		form.Set("code", c.Code)
		form.Set("code_verifier", c.Verifier)
		form.Set("redirect_uri", s.publicURL+"/oauth/callback")
	case "refresh_token":
		rt, ok := s.open(r.PostFormValue("refresh_token"), "refresh")
		if !ok || rt.Client != caller {
			oauthError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		grant = rt
		form.Set("refresh_token", rt.Token)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}

	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	req, _ := http.NewRequestWithContext(r.Context(), "POST", bexioTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if err = json.NewDecoder(resp.Body).Decode(&t); resp.StatusCode != http.StatusOK {
			err = errors.New(resp.Status)
		}
	}
	if err != nil || t.AccessToken == "" {
		log.Printf("token %s: bexio refused: %v", grantType, err)
		oauthError(w, http.StatusBadRequest, "invalid_grant") // Claude expects exactly this to restart sign-in
		return
	}
	expiresIn := cmp.Or(t.ExpiresIn, 300)
	log.Printf("token %s: ok, expires_in %d", grantType, expiresIn)
	out := map[string]any{
		"access_token": s.seal(sealed{Kind: "access", Exp: time.Now().Unix() + expiresIn, Token: t.AccessToken, ReadOnly: grant.ReadOnly}),
		"token_type":   "Bearer",
		"expires_in":   expiresIn,
	}
	if refresh := cmp.Or(t.RefreshToken, grant.Token); refresh != "" { // bexio may not rotate
		// Expires unless used: each refresh issues a new one, so active clients never notice.
		out["refresh_token"] = s.seal(sealed{Kind: "refresh", Exp: time.Now().AddDate(0, 0, 90).Unix(), Client: grant.Client, Token: refresh, ReadOnly: grant.ReadOnly})
	}
	writeJSON(w, http.StatusOK, out)
}

func clientHash(clientID string) string {
	h := sha256.Sum256([]byte(clientID))
	return base64.RawURLEncoding.EncodeToString(h[:16])
}

func pkceChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func randomString() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

// bextrackLogo is the bextrack app icon (an hourglass); on the dark header
// its outline is drawn light.
//
//go:embed bextrack-logo.svg
var bextrackLogo string

func logo(outline string) template.URL {
	svg := strings.ReplaceAll(bextrackLogo, "#0d2f3b", outline)
	return template.URL("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg)))
}

var headerLogo, badgeLogo = logo("#c5d0d3"), logo("#0d2f3b")

// consentPage follows the look of bexio's own sign-in pages (light card,
// dark header, round badge, list rows) in bextrack's colours.
var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect bexio</title>
<style>
:root { color-scheme: light; --page: #f4f4f4; --card: #fff; --head: #0d2e3a; --head-fg: #c5d0d3; --intro: #ebebea;
  --teal: #34606c; --teal-dark: #0d2f3b; --lime: #bcdd37; --fg: #1f1f1f; --muted: #6b6b6b; --line: #e6e6e6; --warn: #fff4d6; }
* { box-sizing: border-box; }
body { margin: 0; padding: 16px 16px 32px; background: var(--page); color: var(--fg); font: 15px/1.45 Lato, "Helvetica Neue", Arial, sans-serif; }
.card { max-width: 480px; margin: 0 auto; background: var(--card); border-radius: 3px; box-shadow: 0 1px 4px rgba(0,0,0,.18); overflow: hidden; }
header { background: var(--head); color: var(--head-fg); display: flex; align-items: center; justify-content: center; gap: 6px; padding: 12px 0; }
header { font-size: 22px; letter-spacing: .3px; }
header img { height: 26px; }
header span { font-size: 13px; letter-spacing: 1px; padding-top: 6px; }
.intro { background: var(--intro); text-align: center; padding: 32px 24px 24px; }
.badge { width: 128px; height: 128px; margin: 0 auto 24px; border-radius: 50%; background: var(--teal); color: var(--lime);
  display: flex; align-items: center; justify-content: center; }
.badge img { width: 76px; }
.intro p { margin: 0 0 8px; }
.target { color: var(--muted); font-size: 13px; }
.target span { font-family: ui-monospace, Menlo, monospace; color: var(--fg); word-break: break-all; }
.warn { background: var(--warn); border-radius: 3px; padding: 8px 12px; margin-top: 12px; font-size: 13px; }
.section { padding: 14px 24px 6px; font-size: 12px; font-weight: 700; text-transform: uppercase; letter-spacing: .6px; color: var(--muted); }
label.row { display: flex; align-items: center; justify-content: space-between; gap: 16px; padding: 11px 24px; border-top: 1px solid var(--line); cursor: pointer; }
label.row:hover { background: #fafafa; }
label.row small { display: block; color: var(--muted); font-size: 13px; }
input[type=checkbox] { width: 18px; height: 18px; flex: none; accent-color: var(--teal); }
.actions { display: flex; flex-direction: row-reverse; align-items: center; gap: 8px; padding: 18px 24px; border-top: 1px solid var(--line); }
button { font: inherit; border: 0; border-radius: 3px; padding: 10px 18px; cursor: pointer; }
button[value=allow] { background: var(--teal); color: #fff; font-weight: 700; }
button[value=allow]:hover { background: var(--teal-dark); }
button[value=deny] { background: transparent; color: var(--muted); }
button:focus-visible, input:focus-visible { outline: 2px solid var(--teal); outline-offset: 2px; }
.note { max-width: 480px; margin: 12px auto 0; text-align: center; color: var(--muted); font-size: 12px; }
</style>
</head>
<body>
<div class="card">
<header><img src="{{.HeaderLogo}}" alt="">BexTrack<span>MCP</span></header>
<div class="intro">
<div class="badge"><img src="{{.BadgeLogo}}" alt=""></div>
<p><strong>{{or .Name "An application"}}</strong> wants to connect to your bexio account.<br>Choose what it may use.</p>
<p class="target">Afterwards you will be sent back to <span>{{.Target}}</span></p>
{{if .Local}}<p class="warn">That address opens a program on your own computer. Only continue if you just started this sign-in yourself.</p>{{end}}
</div>
<form method="post">
<input type="hidden" name="request" value="{{.Request}}">
<div class="section">Areas</div>
{{range .Modules}}<label class="row">{{.Label}}<input type="checkbox" name="module" value="{{.Name}}" checked></label>
{{end}}<div class="section">Access</div>
<label class="row"><span>Read only<small>Can look things up, never changes anything</small></span><input type="checkbox" name="readonly" value="1"></label>
<div class="actions"><button name="action" value="allow">Continue to bexio</button><button name="action" value="deny">Cancel</button></div>
</form>
</div>
<p class="note">Bank payments and payroll are never available through this connector.<br>BexTrackMCP {{.Version}}</p>
</body>
</html>
`))
