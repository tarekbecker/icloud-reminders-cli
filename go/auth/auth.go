// Package auth implements native iCloud authentication for CloudKit access.
//
// Flow:
//  1. Try to reuse saved session (session.json) → call accountLogin → get fresh CK URL
//  2. If that fails, do full signin (username/password → optional 2FA → trust → accountLogin)
//  3. Write updated session.json with cookies + tokens + CK base URL
package auth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"
)

// Apple authentication constants (from pyicloud / public Apple auth docs).
const (
	AuthEndpoint  = "https://idmsa.apple.com/appleauth/auth"
	SetupEndpoint = "https://setup.icloud.com/setup/ws/1"
	HomeEndpoint  = "https://www.icloud.com"

	// WidgetKey is the public Apple OAuth client ID used by iCloud.com.
	WidgetKey = "d39ba9916b7251055b22c7f910e2ea796ee65e98119eed3b1d46c30b3d14ce39"
)

// authDomains are the Apple domains whose cookies we save/restore.
var authDomains = []string{
	"https://idmsa.apple.com",
	"https://appleid.apple.com",
	"https://www.icloud.com",
	"https://setup.icloud.com",
	"https://www.apple.com",
}

// httpError is an error that carries an HTTP status code.
type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string   { return e.msg }
func (e httpError) HTTPStatus() int { return e.status }

// retryableHTTPStatus returns true for transient errors that should be retried.
func retryableHTTPStatus(code int) bool {
	switch code {
	case 429, 500, 502, 503, 504:
		return true
	}
	return false
}

// doWithRetry executes fn with exponential backoff on transient failures.
// maxRetries is the maximum number of retry attempts.
func doWithRetry(maxRetries int, fn func() error) error {
	var err error
	for i := 0; i <= maxRetries; i++ {
		err = fn()
		if err == nil {
			return nil
		}
		// Check if it's a retryable HTTP error
		if httpErr, ok := err.(interface{ HTTPStatus() int }); ok {
			if !retryableHTTPStatus(httpErr.HTTPStatus()) {
				return err
			}
		} else if i == maxRetries {
			return err
		}
		// Exponential backoff: 1s, 2s, 4s...
		delay := time.Duration(1<<i) * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		log.Printf("Retry %d/%d after %v: %v", i+1, maxRetries, delay, err)
		time.Sleep(delay)
	}
	return err
}

// SessionData holds the persisted authentication state.
type SessionData struct {
	CKBaseURL      string   `json:"ck_base_url"`
	SessionToken   string   `json:"session_token,omitempty"`
	TrustToken     string   `json:"trust_token,omitempty"`
	AccountCountry string   `json:"account_country,omitempty"`
	SessionID      string   `json:"session_id,omitempty"`
	Scnt           string   `json:"scnt,omitempty"`
	DSID           string   `json:"dsid,omitempty"`
	Cookies        []Cookie `json:"cookies"`
	CreatedAt      string   `json:"created_at,omitempty"`

	// Legacy field from Python bootstrap — ignored in new sessions
	Headers map[string]string `json:"headers,omitempty"`
}

// Cookie is a serializable HTTP cookie.
type Cookie struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Domain  string `json:"domain"`
	Path    string `json:"path"`
	Expires int64  `json:"expires"`
	Secure  bool   `json:"secure"`
}

// Authenticator manages iCloud authentication state.
type Authenticator struct {
	username string
	password string
	jar      *cookiejar.Jar
	client   *http.Client
	data     SessionData
}

// New creates an Authenticator for the given credentials.
func New(username, password string) *Authenticator {
	jar, _ := cookiejar.New(nil)
	return &Authenticator{
		username: username,
		password: password,
		jar:      jar,
		client:   &http.Client{Jar: jar},
	}
}

// --- Public API ---

// EnsureSession loads a saved session and validates it, or runs full auth flow.
// Returns the final SessionData with a valid CK base URL.
func (a *Authenticator) EnsureSession(sessionFile string, forceReauth bool) (*SessionData, error) {
	if !forceReauth {
		// Try to reuse saved session by using its cookies + CK base URL directly.
		// We probe with a lightweight CloudKit zones/list call rather than
		// accountLogin (which requires a valid dsWebAuthToken that may not be
		// stored in Python-bootstrapped sessions).
		if saved, err := loadSessionFile(sessionFile); err == nil && saved.CKBaseURL != "" {
			log.Println("Trying saved session...")
			a.restoreCookies(saved.Cookies)
			a.data = *saved

			// Probe: try the CK base URL directly
			if ok := a.probeCloudKit(saved.CKBaseURL); ok {
				log.Println("Session reused OK.")
				return &a.data, nil
			}

			// Cookies stale — try accountLogin to refresh them
			log.Println("Probing failed, trying accountLogin refresh...")
			if ckURL, err := a.accountLogin(); err == nil {
				a.data.CKBaseURL = ckURL
				a.data.Cookies = a.extractCookies()
				a.data.CreatedAt = time.Now().Format(time.RFC3339)
				_ = a.saveSession(sessionFile)
				log.Println("Session refreshed via accountLogin.")
				return &a.data, nil
			} else {
				log.Printf("accountLogin failed (%v), doing full re-auth...", err)
			}
		}
	}

	// Full interactive authentication
	return a.fullAuth(sessionFile)
}

// probeCloudKit makes a lightweight test call to the CloudKit zones/list endpoint
// to verify that the current cookie jar grants access.
func (a *Authenticator) probeCloudKit(ckBase string) bool {
	if ckBase == "" {
		return false
	}
	base := ckBase
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	probeURL := base + "database/1/com.apple.reminders/production/private/zones/list"

	req, err := http.NewRequest("POST", probeURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", HomeEndpoint)
	req.Header.Set("Referer", HomeEndpoint+"/")

	resp, err := a.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// fullAuth runs the complete signin flow interactively.
func (a *Authenticator) fullAuth(sessionFile string) (*SessionData, error) {
	fmt.Fprintln(os.Stderr, "Signing in to iCloud...")

	// Reset cookie jar
	a.jar, _ = cookiejar.New(nil)
	a.client = &http.Client{Jar: a.jar}
	a.data = SessionData{}

	// Step 1: Sign in (with retry on transient errors)
	var needs2FA bool
	err := doWithRetry(3, func() error {
		var err error
		needs2FA, err = a.signin()
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("signin: %w", err)
	}

	// Step 2: Handle 2FA if required
	if needs2FA {
		fmt.Fprintln(os.Stderr, "Two-factor authentication required.")
		code := promptUser("Enter 2FA code: ")
		if err := a.verify2FA(code); err != nil {
			return nil, fmt.Errorf("2FA verification: %w", err)
		}
		fmt.Fprintln(os.Stderr, "2FA accepted.")

		if err := a.trustSession(); err != nil {
			log.Printf("Warning: session trust failed: %v", err)
		}
	}

	// Step 3: Get webservices URL via accountLogin
	ckURL, err := a.accountLogin()
	if err != nil {
		return nil, fmt.Errorf("accountLogin: %w", err)
	}

	a.data.CKBaseURL = ckURL
	a.data.Cookies = a.extractCookies()
	a.data.CreatedAt = time.Now().Format(time.RFC3339)

	if err := a.saveSession(sessionFile); err != nil {
		log.Printf("Warning: failed to save session: %v", err)
	}
	fmt.Fprintf(os.Stderr, "✅ Authenticated. CK base: %s\n", ckURL)
	return &a.data, nil
}

// --- Internal auth steps ---

// signin POSTs credentials to the Apple auth endpoint.
// Returns true if 2FA is required, false if already trusted.
func (a *Authenticator) signin() (bool, error) {
	body := map[string]interface{}{
		"accountName":  a.username,
		"password":     a.password,
		"rememberMe":   true,
		"trustTokens":  []string{},
	}
	if a.data.TrustToken != "" {
		body["trustTokens"] = []string{a.data.TrustToken}
	}

	resp, err := a.authPost(AuthEndpoint+"/signin", body, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	// Capture session data from response headers
	a.data.SessionToken   = resp.Header.Get("X-Apple-Session-Token")
	a.data.SessionID      = resp.Header.Get("X-Apple-ID-Session-Id")
	a.data.Scnt           = resp.Header.Get("scnt")
	a.data.AccountCountry = resp.Header.Get("X-Apple-ID-Account-Country")
	if t := resp.Header.Get("X-Apple-TwoSV-Trust-Token"); t != "" {
		a.data.TrustToken = t
	}

	switch resp.StatusCode {
	case 200:
		return false, nil // signed in, no 2FA needed
	case 409:
		return true, nil // 2FA required
	case 403:
		return false, fmt.Errorf("invalid Apple ID or password")
	case 401:
		return false, fmt.Errorf("unauthorized (check credentials)")
	default:
		return false, httpError{
			status: resp.StatusCode,
			msg:    fmt.Sprintf("signin failed HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 200)),
		}
	}
}

// verify2FA submits the 6-digit code to the trusted-device verification endpoint.
func (a *Authenticator) verify2FA(code string) error {
	body := map[string]interface{}{
		"securityCode": map[string]string{"code": strings.TrimSpace(code)},
	}
	extra := map[string]string{
		"X-Apple-ID-Session-Id": a.data.SessionID,
		"scnt":                  a.data.Scnt,
	}
	resp, err := a.authPost(AuthEndpoint+"/verify/trusteddevice/securitycode", body, extra)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return fmt.Errorf("2FA failed HTTP %d: %s",
			resp.StatusCode, truncate(string(respBody), 200))
	}

	// Refresh session token from response
	if t := resp.Header.Get("X-Apple-Session-Token"); t != "" {
		a.data.SessionToken = t
	}
	return nil
}

// trustSession marks the session as trusted, earning a trust token for future logins.
func (a *Authenticator) trustSession() error {
	extra := map[string]string{
		"X-Apple-ID-Session-Id": a.data.SessionID,
		"scnt":                  a.data.Scnt,
	}
	resp, err := a.authGet(AuthEndpoint+"/2sv/trust", extra)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	if t := resp.Header.Get("X-Apple-TwoSV-Trust-Token"); t != "" {
		a.data.TrustToken = t
	}
	if t := resp.Header.Get("X-Apple-Session-Token"); t != "" {
		a.data.SessionToken = t
	}
	return nil
}

// sessionTokenFromCookies attempts to extract a session token from
// the cookie jar — specifically X-APPLE-DS-WEB-SESSION-TOKEN — as a
// fallback when the token is not stored in session.json (Python-bootstrapped
// sessions omit it).
func (a *Authenticator) sessionTokenFromCookies() string {
	for _, domain := range authDomains {
		u, _ := url.Parse(domain)
		for _, c := range a.jar.Cookies(u) {
			if c.Name == "X-APPLE-DS-WEB-SESSION-TOKEN" {
				return unquoteCookieValue(c.Value)
			}
		}
	}
	return ""
}

// accountLogin calls the iCloud setup endpoint to obtain webservices URLs.
// This is called both after full auth AND when reusing a saved session.
func (a *Authenticator) accountLogin() (string, error) {
	token := a.data.SessionToken
	if token == "" {
		// Python-generated session.json may not include session_token;
		// try to pull it from the X-APPLE-DS-WEB-SESSION-TOKEN cookie.
		token = a.sessionTokenFromCookies()
	}

	body := map[string]interface{}{
		"dsWebAuthToken": token,
		"extended_login": true,
	}
	if a.data.AccountCountry != "" {
		body["accountCountry"] = a.data.AccountCountry
	}
	if a.data.DSID != "" {
		body["dsPrsId"] = a.data.DSID
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	reqURL := SetupEndpoint + "/accountLogin"
	req, err := http.NewRequest("POST", reqURL, bytes.NewReader(bodyJSON))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", HomeEndpoint)
	req.Header.Set("Referer", HomeEndpoint+"/")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("accountLogin request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("accountLogin HTTP %d: %s",
			resp.StatusCode, truncate(string(respBody), 200))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("accountLogin JSON parse: %w", err)
	}

	// Extract DSID from dsInfo
	if dsInfo, ok := result["dsInfo"].(map[string]interface{}); ok {
		if dsid, ok := dsInfo["dsid"].(string); ok {
			a.data.DSID = dsid
		}
	}

	// Extract CloudKit base URL
	webservices, ok := result["webservices"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("no webservices in accountLogin response")
	}
	ckws, ok := webservices["ckdatabasews"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("no ckdatabasews in webservices")
	}
	ckURL, ok := ckws["url"].(string)
	if !ok {
		return "", fmt.Errorf("no url in ckdatabasews")
	}

	// Refresh session cookies after accountLogin (iCloud sets new cookies here)
	a.data.Cookies = a.extractCookies()
	return ckURL, nil
}

// --- HTTP helpers ---

// authHeaders returns the standard Apple OAuth headers.
func authHeaders() map[string]string {
	return map[string]string{
		"Accept":                           "application/json",
		"Content-Type":                     "application/json",
		"X-Apple-OAuth-Client-Id":          WidgetKey,
		"X-Apple-OAuth-Client-Type":        "firstPartyAuth",
		"X-Apple-OAuth-Redirect-URI":       HomeEndpoint,
		"X-Apple-OAuth-Require-Grant-Code": "true",
		"X-Apple-OAuth-Response-Mode":      "form_post",
		"X-Apple-OAuth-Response-Type":      "code",
		"X-Apple-Widget-Key":               WidgetKey,
		"Origin":                           "https://idmsa.apple.com",
		"Referer":                          "https://idmsa.apple.com/",
	}
}

func (a *Authenticator) authPost(rawURL string, body interface{}, extra map[string]string) (*http.Response, error) {
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", rawURL, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, err
	}
	for k, v := range authHeaders() {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	return a.client.Do(req)
}

func (a *Authenticator) authGet(rawURL string, extra map[string]string) (*http.Response, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range authHeaders() {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	return a.client.Do(req)
}

// --- Cookie helpers ---

// extractCookies reads all cookies from the jar for all Apple domains.
func (a *Authenticator) extractCookies() []Cookie {
	seen := make(map[string]bool)
	var result []Cookie
	for _, domain := range authDomains {
		u, _ := url.Parse(domain)
		for _, c := range a.jar.Cookies(u) {
			key := c.Name + "|" + c.Domain + "|" + c.Path
			if seen[key] {
				continue
			}
			seen[key] = true
			exp := int64(0)
			if !c.Expires.IsZero() {
				exp = c.Expires.Unix()
			}
			result = append(result, Cookie{
				Name:    c.Name,
				Value:   c.Value,
				Domain:  c.Domain,
				Path:    c.Path,
				Expires: exp,
				Secure:  c.Secure,
			})
		}
	}
	return result
}

// unquoteCookieValue strips RFC 2109 outer double-quotes from a cookie value.
// Apple sends many cookie values as quoted-strings: "v=1:t=..." — Go's
// strict net/http parser rejects raw '"' bytes and drops the cookie entirely.
// Stripping the outer quotes preserves the actual value.
func unquoteCookieValue(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// restoreCookies loads saved cookies back into the jar.
// Cookies are set against multiple Apple/iCloud URLs so Go's jar (which
// validates host-domain matching) forwards them correctly to subdomains
// like p68-ckdatabasews.icloud.com and setup.icloud.com.
func (a *Authenticator) restoreCookies(cookies []Cookie) {
	var httpCookies []*http.Cookie
	for _, c := range cookies {
		exp := time.Time{}
		if c.Expires > 0 {
			exp = time.Unix(c.Expires, 0)
		}
		httpCookies = append(httpCookies, &http.Cookie{
			Name:    c.Name,
			Value:   unquoteCookieValue(c.Value),
			Domain:  c.Domain,
			Path:    c.Path,
			Expires: exp,
			Secure:  c.Secure,
		})
	}

	// Set cookies against all relevant Apple domains so they're forwarded
	// to any subdomain (setup.icloud.com, p*-ckdatabasews.icloud.com, etc.)
	setURLs := []string{
		"https://www.icloud.com",
		"https://setup.icloud.com",
		"https://idmsa.apple.com",
		"https://appleid.apple.com",
		"https://www.apple.com",
	}
	// Also add the stored CK base URL domain
	if a.data.CKBaseURL != "" {
		setURLs = append(setURLs, a.data.CKBaseURL)
	}

	for _, rawURL := range setURLs {
		u, err := url.Parse(rawURL)
		if err != nil {
			continue
		}
		a.jar.SetCookies(u, httpCookies)
	}
}

// --- Session persistence ---

// saveSession writes the session data to disk.
func (a *Authenticator) saveSession(sessionFile string) error {
	if err := os.MkdirAll(sessionDir(sessionFile), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(&a.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(sessionFile, data, 0600)
}

func sessionDir(sessionFile string) string {
	parts := strings.Split(sessionFile, "/")
	if len(parts) <= 1 {
		return "."
	}
	return strings.Join(parts[:len(parts)-1], "/")
}

// loadSessionFile reads saved session data from disk.
func loadSessionFile(sessionFile string) (*SessionData, error) {
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return nil, err
	}
	var s SessionData
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// --- Misc ---

// promptUser prints a prompt to stderr and reads a line from stdin.
func promptUser(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text())
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// LoadCredentials reads ICLOUD_USERNAME / ICLOUD_PASSWORD from the credentials file
// if not already set in the environment.
func LoadCredentials(configDir string) (username, password string, err error) {
	// Try environment first
	username = os.Getenv("ICLOUD_USERNAME")
	password = os.Getenv("ICLOUD_PASSWORD")
	if username != "" && password != "" {
		return username, password, nil
	}

	// Try credentials file
	credsFile := configDir + "/credentials"
	data, ferr := os.ReadFile(credsFile)
	if ferr != nil {
		if username == "" || password == "" {
			return "", "", fmt.Errorf("ICLOUD_USERNAME / ICLOUD_PASSWORD not set and %s not found", credsFile)
		}
		return username, password, nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "export ") {
			line = line[7:]
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch k {
		case "ICLOUD_USERNAME":
			if username == "" {
				username = v
			}
		case "ICLOUD_PASSWORD":
			if password == "" {
				password = v
			}
		}
	}
	if username == "" || password == "" {
		return "", "", fmt.Errorf("credentials not found in env or %s", credsFile)
	}
	return username, password, nil
}
