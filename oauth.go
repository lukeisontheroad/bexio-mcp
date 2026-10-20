package main

import (
	"cmp"
	"context"
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
	"unicode/utf8"
)

// bexio's Keycloak; tests point them at a fake.
var (
	bexioAuthURL  = "https://auth.bexio.com/realms/bexio/protocol/openid-connect/auth"
	bexioTokenURL = "https://auth.bexio.com/realms/bexio/protocol/openid-connect/token"
)

// baseScopes are requested on every sign-in: identity, refresh tokens and
// the company profile.
const baseScopes = "openid profile email offline_access company_profile"

type module struct {
	Section, Tag, Scopes string // consent-page heading, bexio_docs group, the scopes its operations need
	Doc                  string // document type whose positions and comments belong here
}

// Name is the module's form field and key in sealed access maps.
func (m module) Name() string { return slug(m.Tag) }

// Writable reports whether bexio has operations that change this group.
func (m module) Writable() bool {
	return slices.ContainsFunc(bexioAPI.byTag[m.Tag], func(o *operation) bool { return !o.readOnly() })
}

// modules are the rows of the consent page, one per bexio_docs group, each
// off, read or read and write. The server enforces that per operation: bexio
// has no read scope for accounting, files or stock, and master data needs no
// scope at all. Banking payments and payroll are deliberately absent, so no
// client can move money or read salary data. One unknown scope makes bexio
// reject the whole sign-in: only add verified scope names.
var modules = []module{
	{"Contacts", "Contacts", "contact_show contact_edit", ""},
	{"Contacts", "Additional Addresses", "contact_show contact_edit", ""},
	{"Contacts", "Contact Relations", "contact_show contact_edit", ""},
	{"Sales", "Quotes", "kb_offer_show kb_offer_edit", "kb_offer"},
	{"Sales", "Orders", "kb_order_show kb_order_edit", "kb_order"},
	{"Sales", "Invoices", "kb_invoice_show kb_invoice_edit", "kb_invoice"},
	{"Sales", "Deliveries", "kb_delivery_show kb_delivery_edit", ""},
	{"Items and stock", "Items", "article_show article_edit", ""},
	{"Items and stock", "Stock Areas", "stock_edit", ""},
	{"Items and stock", "Stock locations", "stock_edit", ""},
	{"Purchase", "Bills", "contact_show kb_bill_show", ""},
	{"Purchase", "Expenses", "contact_show kb_expense_show", ""},
	{"Purchase", "Purchase Orders", "kb_article_order_show kb_article_order_edit", ""},
	{"Projects", "Projects", "project_show project_edit", ""},
	{"Projects", "Timesheets", "monitoring_show monitoring_edit", ""},
	{"Notes, tasks and files", "Notes", "note_show note_edit", ""},
	{"Notes, tasks and files", "Tasks", "task_show task_edit", ""},
	{"Notes, tasks and files", "Files", "file", ""},
	{"Accounting", "Accounts", "accounting", ""},
	{"Accounting", "Account Groups", "accounting", ""},
	{"Accounting", "Business Years", "accounting", ""},
	{"Accounting", "Calendar Years", "accounting", ""},
	{"Accounting", "Manual Entries", "accounting", ""},
	{"Accounting", "Reports", "accounting", ""},
	{"Accounting", "Vat Periods", "accounting", ""},
	{"Accounting", "Bank Accounts", "bank_account_show", ""},
	{"Master data", "Business Activities", "", ""},
	{"Master data", "Communication Types", "", ""},
	{"Master data", "Company Profile", "", ""},
	{"Master data", "Contact Groups", "", ""},
	{"Master data", "Contact Sectors", "", ""},
	{"Master data", "Countries", "", ""},
	{"Master data", "Currencies", "", ""},
	{"Master data", "Document Settings", "", ""},
	{"Master data", "Document templates", "", ""},
	{"Master data", "Languages", "", ""},
	{"Master data", "Payment Types", "", ""},
	{"Master data", "Permissions", "", ""},
	{"Master data", "Salutations", "", ""},
	{"Master data", "Taxes", "", ""},
	{"Master data", "Titles", "", ""},
	{"Master data", "Units", "", ""},
	{"Master data", "User Management", "", ""},
}

// scopesFor turns each area's access into bexio scopes: read drops the
// area's write scopes. Converting a document needs both areas' write scopes.
func scopesFor(access map[string]bool) string {
	scopes := strings.Fields(baseScopes)
	for _, m := range modules {
		write, ok := access[m.Name()]
		for _, s := range strings.Fields(m.Scopes) {
			// stock_edit is the only scope that reads stock, so it stays.
			if ok && !slices.Contains(scopes, s) && (write || !strings.HasSuffix(s, "_edit") || s == "stock_edit") {
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
	Kind      string          `json:"k"`
	Exp       int64           `json:"e,omitempty"`
	Client    string          `json:"c,omitempty"` // hash of the client_id it was issued to
	URIs      []string        `json:"u,omitempty"` // a client's registered redirect URIs
	Name      string          `json:"n,omitempty"`
	Redirect  string          `json:"r,omitempty"`
	State     string          `json:"s,omitempty"`
	Challenge string          `json:"p,omitempty"` // the client's PKCE challenge
	Verifier  string          `json:"v,omitempty"` // this server's PKCE verifier towards bexio
	Nonce     string          `json:"o,omitempty"`
	Token     string          `json:"t,omitempty"`  // bexio access or refresh token
	Access    map[string]bool `json:"a,omitempty"`  // area name: true to read and write, false to read; others are off
	Company   string          `json:"co,omitempty"` // slug of the company name the connector URL pins
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
		q.Get("code_challenge_method") != "S256" || len(challenge) != 43 || len(q.Get("state")) > 512 || len(q.Get("resource")) > 512 {
		showError(w, http.StatusBadRequest, page{Heading: "Invalid sign-in request", Lines: []string{"Remove the connector and add it again."}})
		return
	}
	// A connector URL /mcp/<company-name> pins the company; clients send it as
	// the resource (RFC 8707).
	company := ""
	if rest, ok := strings.CutPrefix(q.Get("resource"), s.publicURL+"/mcp/"); ok {
		if r, err := url.PathUnescape(rest); err == nil {
			rest = r
		}
		company = slug(rest)
	}
	// A cookie per sign-in ties both the consent form and bexio's callback to
	// this browser: another site can neither submit the form for the user nor
	// hand the user a bexio link that ends at its own client. Lax, because
	// bexio's redirect back is a cross-site navigation.
	nonce := randomString()
	http.SetCookie(w, s.flowCookie(nonce, 600))
	u, _ := url.Parse(redirect)
	target := u.Host
	if u.Scheme != "https" {
		target = u.Scheme + "://" + u.Host
	}
	render(w, http.StatusOK, "consent", page{
		Name: client.Name, Target: target, Local: u.Scheme != "https", Modules: modules, Company: companyTitle(company),
		Request: s.seal(sealed{
			Kind: "consent", Exp: time.Now().Add(10 * time.Minute).Unix(), Client: clientHash(q.Get("client_id")),
			Redirect: redirect, State: q.Get("state"), Challenge: challenge, Nonce: nonce, Company: company,
		}),
	})
}

// consent handles the consent form: deny goes back to the client, allow
// signs in at bexio with the scopes for the picked areas.
func (s *authServer) consent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	req, ok := s.open(r.PostFormValue("request"), "consent")
	if _, err := r.Cookie(s.flowCookie(req.Nonce, 0).Name); !ok || err != nil {
		showError(w, http.StatusForbidden, expired)
		return
	}
	if r.PostFormValue("action") != "allow" {
		// No redirect: Cancel must not send anyone to a stranger's URI.
		http.SetCookie(w, s.flowCookie(req.Nonce, -1))
		showError(w, http.StatusOK, page{Heading: "Cancelled", Lines: []string{"Nothing was shared. You can close this window."}})
		return
	}
	access := map[string]bool{}
	for _, m := range modules {
		switch r.PostFormValue(m.Name()) {
		case "write":
			access[m.Name()] = m.Writable()
		case "read":
			access[m.Name()] = false
		}
	}
	if len(access) == 0 {
		showError(w, http.StatusBadRequest, page{Heading: "Nothing to share", Lines: []string{"Set at least one row to Read: go back and try again."}})
		return
	}
	verifier := randomString()
	http.SetCookie(w, s.flowCookie(req.Nonce, 600)) // the bexio login may take a while
	state := s.seal(sealed{
		Kind: "state", Exp: time.Now().Add(10 * time.Minute).Unix(), Client: req.Client, Redirect: req.Redirect,
		State: req.State, Challenge: req.Challenge, Verifier: verifier, Nonce: req.Nonce, Access: access, Company: req.Company,
	})
	http.Redirect(w, r, bexioAuthURL+"?"+url.Values{
		"client_id":             {s.clientID},
		"redirect_uri":          {s.publicURL + "/oauth/callback"},
		"response_type":         {"code"},
		"scope":                 {scopesFor(access)},
		"state":                 {state},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}.Encode(), http.StatusSeeOther)
}

// callback redeems bexio's code, refuses a company other than the one the
// connector URL pins, and hands the client a code carrying bexio's refresh
// token. Unlike bexio's code it is not single use: for its 2 minutes the
// client's PKCE verifier is what stops replays.
func (s *authServer) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	st, ok := s.open(q.Get("state"), "state")
	if _, err := r.Cookie(s.flowCookie(st.Nonce, 0).Name); !ok || err != nil {
		showError(w, http.StatusBadRequest, expired)
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
	t, err := s.bexioToken(r.Context(), url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")},
		"code_verifier": {st.Verifier}, "redirect_uri": {s.publicURL + "/oauth/callback"}})
	if err != nil {
		log.Printf("callback: bexio refused: %v", err)
		showError(w, http.StatusBadRequest, page{Heading: "Sign-in not finished", Lines: []string{"bexio did not finish the sign-in. Start it again from your app."}})
		return
	}
	if name := companyName(t.IDToken); st.Company != "" && !sameCompany(st.Company, name) {
		log.Print("callback: other company than the connector URL pins")
		want := companyTitle(st.Company)
		p := page{Heading: "Wrong bexio company", Lines: []string{
			"This connector is for " + want + ", but you picked " + cmp.Or(name, "another company") + " at bexio.",
			"Start the sign-in again from your app and pick " + want + "."}}
		if name != "" {
			p.Hint, p.URL = "If "+name+" is the company you want, use this connector URL instead:", s.publicURL+"/mcp/"+slug(name)
		}
		showError(w, http.StatusForbidden, p)
		return
	}
	code := s.seal(sealed{
		Kind: "code", Exp: time.Now().Add(2 * time.Minute).Unix(), Client: st.Client, Redirect: st.Redirect,
		Challenge: st.Challenge, Token: t.RefreshToken, Access: st.Access,
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

// token redeems codes and refresh tokens and returns bexio's tokens sealed,
// so they only work through here.
func (s *authServer) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	grantType, caller := r.PostFormValue("grant_type"), clientHash(r.PostFormValue("client_id"))
	var grant sealed
	switch grantType {
	case "authorization_code":
		c, ok := s.open(r.PostFormValue("code"), "code")
		if redirect := r.PostFormValue("redirect_uri"); !ok || c.Client != caller || redirect != "" && redirect != c.Redirect ||
			pkceChallenge(r.PostFormValue("code_verifier")) != c.Challenge {
			oauthError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		grant = c
	case "refresh_token":
		rt, ok := s.open(r.PostFormValue("refresh_token"), "refresh")
		if !ok || rt.Client != caller {
			oauthError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		grant = rt
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}

	// A code carries the refresh token bexio issued at the callback, so both
	// grants are a refresh at bexio.
	t, err := s.bexioToken(r.Context(), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {grant.Token}})
	if err != nil {
		log.Printf("token %s: bexio refused: %v", grantType, err)
		oauthError(w, http.StatusBadRequest, "invalid_grant") // Claude expects exactly this to restart sign-in
		return
	}
	expiresIn := cmp.Or(t.ExpiresIn, 300)
	log.Printf("token %s: ok, expires_in %d", grantType, expiresIn)
	out := map[string]any{
		"access_token": s.seal(sealed{Kind: "access", Exp: time.Now().Unix() + expiresIn, Token: t.AccessToken, Access: grant.Access}),
		"token_type":   "Bearer",
		"expires_in":   expiresIn,
	}
	if refresh := cmp.Or(t.RefreshToken, grant.Token); refresh != "" { // bexio may not rotate
		// Expires unless used: each refresh issues a new one, so active clients never notice.
		out["refresh_token"] = s.seal(sealed{Kind: "refresh", Exp: time.Now().AddDate(0, 0, 90).Unix(), Client: grant.Client, Token: refresh, Access: grant.Access})
	}
	writeJSON(w, http.StatusOK, out)
}

type bexioTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	IDToken      string `json:"id_token"`
}

// bexioToken calls bexio's token endpoint with this server's client secret.
func (s *authServer) bexioToken(ctx context.Context, form url.Values) (bexioTokens, error) {
	var t bexioTokens
	form.Set("client_id", s.clientID)
	form.Set("client_secret", s.clientSecret)
	req, _ := http.NewRequestWithContext(ctx, "POST", bexioTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if err = json.NewDecoder(resp.Body).Decode(&t); resp.StatusCode != http.StatusOK {
			err = errors.New(resp.Status)
		}
	}
	if ctx.Err() == nil && (resp == nil || resp.StatusCode >= 500) { // bexio down, not a stale code or a closed tab
		report("bexio token %s: %v", form.Get("grant_type"), cause(err))
	}
	if err == nil && t.AccessToken == "" {
		err = errors.New("no access token")
	}
	return t, err
}

// companyName reads the company_name claim (company_profile scope) of bexio's
// id token. It came straight from bexio's token endpoint over TLS, so its
// signature needs no check (OIDC Core 3.1.3.7).
func companyName(idToken string) string {
	var claims struct {
		CompanyName string `json:"company_name"`
	}
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(idToken+"..", ".")[1])
	json.Unmarshal(payload, &claims)
	return claims.CompanyName
}

// slug turns a name into a connector URL label or form field name: lower case
// letters and digits, anything else one "-" ("Acme Group, AG" is "acme-group-ag").
func slug(name string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), "-")
}

// sameCompany compares a connector URL's company with bexio's company name;
// separators don't matter ("acmeag" is "Acme AG").
func sameCompany(pin, name string) bool {
	return strings.ReplaceAll(pin, "-", "") == strings.ReplaceAll(slug(name), "-", "")
}

var legalForms = map[string]string{"ag": "AG", "sa": "SA", "kg": "KG", "gmbh": "GmbH"}

// companyTitle turns a company slug back into a name for people: "acme-ag" is
// "Acme AG". The URL has no capitals, so this is a best guess.
func companyTitle(pin string) string {
	words := strings.FieldsFunc(pin, func(r rune) bool { return r == '-' })
	for i, w := range words {
		r, n := utf8.DecodeRuneInString(w)
		words[i] = cmp.Or(legalForms[w], string(unicode.ToUpper(r))+w[n:])
	}
	return strings.Join(words, " ")
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

// The official store badges, linking to the BexTrack apps.
var (
	//go:embed app-store-badge.svg
	appStoreBadge string
	//go:embed google-play-badge.png
	googlePlayBadge string
)

// dataURL inlines an image: the pages' CSP allows no other source.
func dataURL(mime, data string) template.URL {
	return template.URL("data:" + mime + ";base64," + base64.StdEncoding.EncodeToString([]byte(data)))
}

func logo(outline string) template.URL {
	return dataURL("image/svg+xml", strings.ReplaceAll(bextrackLogo, "#0d2f3b", outline))
}

var (
	headerLogo, badgeLogo = logo("#c5d0d3"), logo("#0d2f3b")
	appStore, googlePlay  = dataURL("image/svg+xml", appStoreBadge), dataURL("image/png", googlePlayBadge)
)

// page is what the templates show: the consent page, or the error page that
// ends a request here (Heading, Lines and an optional URL to use instead).
type page struct {
	Heading, Hint, URL             string
	Lines                          []string
	Name, Target, Company, Request string
	Local                          bool
	Modules                        []module
	Version                        string
	HeaderLogo, BadgeLogo          template.URL
	AppStore, GooglePlay           template.URL
}

var expired = page{Heading: "Sign-in expired", Lines: []string{"This sign-in expired or was not started in this browser. Start it again from your app."}}

// render writes a page in the look of bexio's own sign-in pages (light card,
// dark header, round badge, list rows) in bextrack's colours.
func render(w http.ResponseWriter, status int, name string, p page) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	p.Version, p.HeaderLogo, p.BadgeLogo, p.AppStore, p.GooglePlay = version, headerLogo, badgeLogo, appStore, googlePlay
	pages.ExecuteTemplate(w, name, p)
}

// showError ends a request with the error page: every error a person can see,
// from the sign-in to unknown addresses and crashes.
func showError(w http.ResponseWriter, status int, p page) { render(w, status, "error", p) }

var pages = template.Must(template.New("").Parse(`{{define "top"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{or .Heading "Connect bexio"}}</title>
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
.intro h1 { margin: 0 0 12px; font-size: 20px; }
.intro p { margin: 0 0 8px; }
.target { color: var(--muted); font-size: 13px; }
.target span { font-family: ui-monospace, Menlo, monospace; color: var(--fg); word-break: break-all; }
.warn { background: var(--warn); border-radius: 3px; padding: 8px 12px; margin-top: 12px; font-size: 13px; }
.section { padding: 14px 24px 6px; font-size: 12px; font-weight: 700; text-transform: uppercase; letter-spacing: .6px; color: var(--muted); }
.row { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 8px 16px; padding: 8px 24px; border-top: 1px solid var(--line); }
.seg { display: flex; flex: none; margin-left: auto; border: 1px solid var(--line); border-radius: 3px; overflow: hidden; font-size: 13px; }
.seg label { position: relative; cursor: pointer; }
.seg label + label { border-left: 1px solid var(--line); }
.seg input { position: absolute; opacity: 0; }
.seg span { display: block; padding: 5px 10px; }
.seg input:checked + span { background: var(--teal); color: #fff; }
.seg input:focus-visible + span { outline: 2px solid var(--teal-dark); outline-offset: -2px; }
.md-warn { display: none; margin: 0 24px 10px; }
form:has(.md input[value=off]:checked) .md-warn { display: block; }
.actions { display: flex; flex-direction: row-reverse; align-items: center; gap: 8px; padding: 18px 24px; border-top: 1px solid var(--line); }
button { font: inherit; border: 0; border-radius: 3px; padding: 10px 18px; cursor: pointer; }
button[value=allow] { background: var(--teal); color: #fff; font-weight: 700; }
button[value=allow]:hover { background: var(--teal-dark); }
button[value=deny] { background: transparent; color: var(--muted); }
button:focus-visible { outline: 2px solid var(--teal); outline-offset: 2px; }
.note { max-width: 480px; margin: 12px auto 0; text-align: center; color: var(--muted); font-size: 12px; }
.note strong, .note a { color: var(--fg); }
.stores { display: flex; justify-content: center; align-items: center; gap: 4px; margin: 16px auto 0; }
.stores img { display: block; height: 40px; }
.stores .play { height: 59px; margin: -10px -9px; } /* Google's badge carries its own clear space */
</style>
</head>
<body>
<div class="card">
<header><img src="{{.HeaderLogo}}" alt="">BexTrack<span>MCP</span></header>
<div class="intro">
<div class="badge"><img src="{{.BadgeLogo}}" alt=""></div>
{{end}}

{{define "bottom"}}</div>
<p class="note">Bank payments and payroll are never available through this connector.<br>
<strong>Use this connector at your own risk.</strong> It is provided as is, without warranty or liability.<br>
BexTrackMCP {{.Version}} · <a href="https://github.com/lukeisontheroad/bexio-mcp" target="_blank" rel="noopener">Source code on GitHub</a></p>
<p class="note">Track your time in bexio with the BexTrack app:</p>
<div class="stores"><a href="https://apps.apple.com/app/apple-store/id1534958012?pt=122197988&amp;ct=bextrack-mcp&amp;mt=8" target="_blank" rel="noopener"><img src="{{.AppStore}}" alt="Download BexTrack on the App Store"></a>
<a href="https://play.google.com/store/apps/details?id=xyz.lukeisontheroad.bexio&amp;referrer=utm_source%3Dbextrack-mcp%26utm_medium%3Dfooter" target="_blank" rel="noopener"><img class="play" src="{{.GooglePlay}}" alt="Get BexTrack on Google Play"></a></div>
</body>
</html>
{{end}}

{{define "error"}}{{template "top" .}}<h1>{{.Heading}}</h1>
{{range .Lines}}<p>{{.}}</p>
{{end}}{{if .URL}}<p class="target">{{.Hint}}<br><span>{{.URL}}</span></p>
{{end}}</div>
{{template "bottom" .}}{{end}}

{{define "consent"}}{{template "top" .}}<p><strong>{{or .Name "An application"}}</strong> wants to connect to your bexio account.<br>Choose what it may use.</p>
<p class="target">Afterwards you will be sent back to <span>{{.Target}}</span></p>
{{if .Company}}<p class="target">This connector is for <span>{{.Company}}</span>: pick that company at bexio.</p>{{end}}
{{if .Local}}<p class="warn">That address opens a program on your own computer. Only continue if you just started this sign-in yourself.</p>{{end}}
</div>
<form method="post">
<input type="hidden" name="request" value="{{.Request}}">
{{$section := ""}}{{range .Modules}}{{$md := eq .Section "Master data"}}{{if ne .Section $section}}{{$section = .Section}}<div class="section">{{.Section}}</div>
{{if $md}}<p class="warn md-warn">Contacts, invoices and other records refer to master data: with some of it off, creating them may fail.</p>
{{end}}{{end}}<div class="row{{if $md}} md{{end}}"><span>{{.Tag}}</span><span class="seg" role="radiogroup" aria-label="{{.Tag}}">
<label><input type="radio" name="{{.Name}}" value="off"><span>Off</span></label>
<label><input type="radio" name="{{.Name}}" value="read"{{if not .Writable}} checked{{end}}><span>Read</span></label>
{{if .Writable}}<label><input type="radio" name="{{.Name}}" value="write" checked><span>Read &amp; write</span></label>{{end}}
</span></div>
{{end}}<div class="actions"><button name="action" value="allow">Continue to bexio</button><button name="action" value="deny">Cancel</button></div>
</form>
</div>
{{template "bottom" .}}{{end}}`))
