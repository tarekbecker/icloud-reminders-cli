// Package auth implements native iCloud authentication for CloudKit access using SRP.
//
// Flow (based on Go-iClient):
//  1. authStart - Initialize session with frame_id
//  2. authFederate - Submit email only
//  3. authInit - Get SRP salt and B from server
//  4. authComplete - Send SRP proof (M1/M2)
//  5. Handle 2FA if required (409 response)
//  6. getTrust - Get session and trust tokens
//  7. authenticateWeb - Get CK URL via accountLogin
//  8. Save session.json with cookies + tokens + CK base URL
package auth

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/term"
	"icloud-reminders/srp"
)

// Apple authentication constants.
const (
	AuthEndpoint  = "https://idmsa.apple.com/appleauth/auth"
	SetupEndpoint = "https://setup.icloud.com/setup/ws/1"
	HomeEndpoint  = "https://www.icloud.com"

	// WidgetKey matches pyicloud and Go-iClient.
	WidgetKey = "d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d"
)

// authDomains are the Apple domains whose cookies we save/restore.
var authDomains = []string{
	"https://idmsa.apple.com",
	"https://appleid.apple.com",
	"https://www.icloud.com",
	"https://setup.icloud.com",
	"https://www.apple.com",
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

// Authenticator manages iCloud authentication state using SRP.
type Authenticator struct {
	username   string
	password   string
	clientID   string
	frameID    string
	authAttr   string
	sessionID  string
	scnt       string
	authToken  string
	trustToken string
	jar        *cookiejar.Jar
	client     *http.Client
	data       SessionData
}

// New creates an Authenticator without credentials (interactive mode).
func New() *Authenticator {
	jar, _ := cookiejar.New(nil)
	frameID := strings.ToLower(uuid.New().String())
	return &Authenticator{
		clientID: "auth-" + frameID,
		frameID:  frameID,
		jar:      jar,
		client:   &http.Client{Jar: jar},
	}
}

// --- Public API ---

// EnsureSession loads a saved session and validates it, or runs full auth flow.
// Returns the final SessionData with a valid CK base URL.
func (a *Authenticator) EnsureSession(sessionFile string, forceReauth bool) (*SessionData, error) {
	if !forceReauth {
		// Try to reuse saved session
		if saved, err := loadSessionFile(sessionFile); err == nil && saved.CKBaseURL != "" {
			log.Println("Trying saved session...")
			a.data = *saved // must be set before restoreCookies so CKBaseURL is included
			a.sessionID = saved.SessionID
			a.scnt = saved.Scnt
			a.authToken = saved.SessionToken
			a.trustToken = saved.TrustToken
			a.restoreCookies(saved.Cookies) // CKBaseURL is now set, so CK host is included

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

	// Full SRP authentication
	return a.fullAuth(sessionFile)
}

// probeCloudKit makes a lightweight test call to verify access.
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

// fullAuth runs the complete SRP signin flow.
func (a *Authenticator) fullAuth(sessionFile string) (*SessionData, error) {
	fmt.Fprintln(os.Stderr, "Signing in to iCloud (SRP)...")

	// 1. Environment variables (set by credentials file or the caller)
	if a.username == "" {
		a.username = os.Getenv("ICLOUD_USERNAME")
	}
	if a.password == "" {
		a.password = os.Getenv("ICLOUD_PASSWORD")
	}

	// 2. Credentials file (~/.config/icloud-reminders/credentials)
	if a.username == "" || a.password == "" {
		if user, pass, err := loadCredentialsFile(); err == nil {
			if a.username == "" {
				a.username = user
			}
			if a.password == "" {
				a.password = pass
			}
		}
	}

	// 3. Interactive prompt (fallback)
	if a.username == "" {
		a.username = promptUser("Apple ID: ")
	}
	if a.password == "" {
		a.password = promptUserPassword("Password: ")
	}

	// Reset state
	a.jar, _ = cookiejar.New(nil)
	a.client = &http.Client{Jar: a.jar}
	a.data = SessionData{}

	// Step 1: Initialize auth session
	if err := a.authStart(); err != nil {
		return nil, fmt.Errorf("authStart: %w", err)
	}

	// Step 2: Submit email
	if err := a.authFederate(); err != nil {
		return nil, fmt.Errorf("authFederate: %w", err)
	}

	// Step 3-4: SRP handshake
	needs2FA, err := a.srpAuth()
	if err != nil {
		return nil, fmt.Errorf("srpAuth: %w", err)
	}

	// Step 5: Handle 2FA if required
	if needs2FA {
		fmt.Fprintln(os.Stderr, "Two-factor authentication required.")
		code := promptUser("Enter 2FA code: ")
		if err := a.submitTwoFactor(code); err != nil {
			return nil, fmt.Errorf("2FA verification: %w", err)
		}
		fmt.Fprintln(os.Stderr, "2FA accepted.")
	}

	// Step 6: Get trust tokens
	if err := a.getTrust(); err != nil {
		log.Printf("Warning: getTrust failed: %v", err)
	}

	// Step 7: Get webservices URL
	ckURL, err := a.accountLogin()
	if err != nil {
		return nil, fmt.Errorf("accountLogin: %w", err)
	}

	a.data.CKBaseURL = ckURL
	a.data.SessionToken = a.authToken
	a.data.TrustToken = a.trustToken
	a.data.SessionID = a.sessionID
	a.data.Scnt = a.scnt
	a.data.Cookies = a.extractCookies()
	a.data.CreatedAt = time.Now().Format(time.RFC3339)

	if err := a.saveSession(sessionFile); err != nil {
		log.Printf("Warning: failed to save session: %v", err)
	}

	fmt.Fprintf(os.Stderr, "✅ Authenticated. CK base: %s\n", ckURL)
	return &a.data, nil
}

// --- SRP Authentication Steps ---

// authStart initializes the authentication session.
func (a *Authenticator) authStart() error {
	url := fmt.Sprintf("%s/authorize/signin?frame_id=%s&language=en_US&skVersion=7&iframeId=%s&client_id=%s&redirect_uri=https://www.icloud.com&response_type=code&response_mode=web_message&state=%s&authVersion=latest",
		AuthEndpoint, a.clientID, a.clientID, WidgetKey, a.clientID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("authStart failed: HTTP %d", resp.StatusCode)
	}

	a.authAttr = resp.Header.Get("X-Apple-Auth-Attributes")
	return nil
}

// authFederate submits the email address.
func (a *Authenticator) authFederate() error {
	body := fmt.Sprintf(`{"accountName":"%s","rememberMe":true}`, a.username)

	req, err := http.NewRequest("POST", AuthEndpoint+"/federate?isRememberMeEnabled=true", strings.NewReader(body))
	if err != nil {
		return err
	}

	req.Header = a.updateAuthHeaders(req.Header)

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("authFederate failed: HTTP %d - %s", resp.StatusCode, string(body))
	}

	return nil
}

// srpAuth performs the SRP handshake.
// Returns true if 2FA is required.
func (a *Authenticator) srpAuth() (bool, error) {
	// Initialize SRP client
	params := srp.GetParams(2048)
	params.NoUserNameInX = true // Required for Apple's implementation

	srpClient := srp.NewSRPClient(params, nil)

	// Get salt and B from server
	authInitResp, err := a.authInit(base64.StdEncoding.EncodeToString(srpClient.GetABytes()))
	if err != nil {
		return false, err
	}

	// Decode salt and B
	salt, err := base64.StdEncoding.DecodeString(authInitResp.Salt)
	if err != nil {
		return false, fmt.Errorf("decode salt: %w", err)
	}

	bBytes, err := base64.StdEncoding.DecodeString(authInitResp.B)
	if err != nil {
		return false, fmt.Errorf("decode B: %w", err)
	}

	// Generate password key using PBKDF2
	passHash := sha256.Sum256([]byte(a.password))
	passKey := pbkdf2.Key(passHash[:], salt, authInitResp.Iteration, 32, sha256.New)

	// Process challenge
	srpClient.ProcessClientChanllenge([]byte(a.username), passKey, salt, bBytes)

	// Complete auth
	return a.authComplete(authInitResp.C,
		base64.StdEncoding.EncodeToString(srpClient.M1),
		base64.StdEncoding.EncodeToString(srpClient.M2))
}

// authInit gets the SRP salt and B from the server.
type authInitResp struct {
	Iteration int    `json:"iteration"`
	Salt      string `json:"salt"`
	Protocol  string `json:"protocol"`
	B         string `json:"b"`
	C         string `json:"c"`
}

func (a *Authenticator) authInit(aVal string) (*authInitResp, error) {
	body := map[string]interface{}{
		"a":           aVal,
		"accountName": a.username,
		"protocols":   []string{"s2k", "s2k_fo"},
	}

	bodyJSON, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", AuthEndpoint+"/signin/init", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, err
	}

	req.Header = a.updateAuthHeaders(req.Header)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("authInit failed: HTTP %d - %s", resp.StatusCode, string(body))
	}

	var result authInitResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode authInit response: %w", err)
	}

	return &result, nil
}

// authComplete sends the SRP proof to the server.
// Returns true if 2FA is required (409 response).
func (a *Authenticator) authComplete(c, m1, m2 string) (bool, error) {
	body := map[string]interface{}{
		"accountName": a.username,
		"rememberMe":  true,
		"trustTokens": []string{},
		"m1":          m1,
		"c":           c,
		"m2":          m2,
	}

	if a.trustToken != "" {
		body["trustTokens"] = []string{a.trustToken}
	}

	bodyJSON, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", AuthEndpoint+"/signin/complete?isRememberMeEnabled=true", bytes.NewReader(bodyJSON))
	if err != nil {
		return false, err
	}

	req.Header = a.updateAuthHeaders(req.Header)

	resp, err := a.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	// Capture session headers
	a.sessionID = resp.Header.Get("X-Apple-ID-Session-Id")
	a.scnt = resp.Header.Get("scnt")

	switch resp.StatusCode {
	case 200:
		return false, nil // Success, no 2FA
	case 409:
		// 2FA required - capture headers from response
		a.sessionID = resp.Header.Get("X-Apple-ID-Session-Id")
		a.scnt = resp.Header.Get("scnt")
		return true, nil
	case 403:
		return false, fmt.Errorf("invalid username or password")
	case 401:
		return false, fmt.Errorf("unauthorized - check credentials")
	case 412:
		return false, fmt.Errorf("privacy acknowledgment required - visit https://appleid.apple.com")
	default:
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("authComplete failed: HTTP %d - %s", resp.StatusCode, string(body))
	}
}

// submitTwoFactor submits the 2FA code.
func (a *Authenticator) submitTwoFactor(code string) error {
	body := map[string]interface{}{
		"securityCode": map[string]string{"code": strings.TrimSpace(code)},
	}

	bodyJSON, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/verify/trusteddevice/securitycode", AuthEndpoint)
	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyJSON))
	if err != nil {
		return err
	}

	req.Header = a.updateAuthHeaders(req.Header)
	req.Header.Set("X-Apple-ID-Session-Id", a.sessionID)
	req.Header.Set("scnt", a.scnt)

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("2FA submission failed: HTTP %d - %s", resp.StatusCode, string(body))
	}

	// Update scnt from response
	if newScnt := resp.Header.Get("scnt"); newScnt != "" {
		a.scnt = newScnt
	}

	return nil
}

// getTrust gets the session and trust tokens.
func (a *Authenticator) getTrust() error {
	req, err := http.NewRequest("GET", AuthEndpoint+"/2sv/trust", nil)
	if err != nil {
		return err
	}

	req.Header = a.updateAuthHeaders(req.Header)
	req.Header.Set("X-Apple-ID-Session-Id", a.sessionID)
	req.Header.Set("scnt", a.scnt)

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 204 {
		return fmt.Errorf("getTrust failed: HTTP %d", resp.StatusCode)
	}

	a.authToken = resp.Header.Get("X-Apple-Session-Token")
	a.trustToken = resp.Header.Get("X-Apple-TwoSV-Trust-Token")

	return nil
}

// accountLogin calls the iCloud setup endpoint to get webservices URLs.
func (a *Authenticator) accountLogin() (string, error) {
	token := a.authToken
	if token == "" {
		// Fallback to cookie
		token = a.sessionTokenFromCookies()
	}

	body := map[string]interface{}{
		"dsWebAuthToken": token,
		"extended_login": true,
	}

	if a.data.AccountCountry != "" {
		body["accountCountryCode"] = a.data.AccountCountry
	}
	if a.data.DSID != "" {
		body["dsPrsId"] = a.data.DSID
	}

	bodyJSON, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", SetupEndpoint+"/accountLogin", bytes.NewReader(bodyJSON))
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
		return "", fmt.Errorf("accountLogin HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("accountLogin JSON parse: %w", err)
	}

	// Extract DSID
	if dsInfo, ok := result["dsInfo"].(map[string]interface{}); ok {
		if dsid, ok := dsInfo["dsid"].(string); ok {
			a.data.DSID = dsid
		}
	}

	// Extract CK URL
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

	return ckURL, nil
}

// sessionTokenFromCookies extracts session token from cookie jar.
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

// --- HTTP Helpers ---

// updateAuthHeaders sets the standard Apple OAuth headers.
func (a *Authenticator) updateAuthHeaders(h http.Header) http.Header {
	if a.scnt != "" {
		h.Set("scnt", a.scnt)
	}
	if a.sessionID != "" {
		h.Set("X-Apple-ID-Session-Id", a.sessionID)
	}

	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Referer", "https://idmsa.apple.com/")
	h.Set("Origin", "https://idmsa.apple.com")
	h.Set("X-Apple-Widget-Key", WidgetKey)
	h.Set("X-Apple-I-Require-UE", "true")
	h.Set("X-Apple-Auth-Attributes", a.authAttr)
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	h.Set("X-Apple-Mandate-Security-Upgrade", "0")
	h.Set("X-Apple-Oauth-Client-Id", WidgetKey)
	h.Set("X-Apple-Oauth-Client-Type", "firstPartyAuth")
	h.Set("X-Apple-Oauth-Redirect-URI", "https://www.icloud.com")
	h.Set("X-Apple-Oauth-Require-Grant-Code", "true")
	h.Set("X-Apple-Oauth-Response-Mode", "web_message")
	h.Set("X-Apple-Oauth-Response-Type", "code")
	h.Set("X-Apple-Oauth-State", a.clientID)
	h.Set("X-Apple-Offer-Security-Upgrade", "1")
	h.Set("X-Apple-Frame-Id", a.clientID)
	h.Set("X-Apple-I-FD-Client-Info", `{"U":"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36","L":"en-US","Z":"GMT-04:00","V":"1.1","F":".ta44j1e3NlY5BNlY5BSs5uQ32SCVgdI.AqWJ4EKKw0fVD_DJhCizgzH_y3EjNklY_ia4WFL264HRe4FSr_JzC1zJ6rgNNlY5BNp55BNlan0Os5Apw.BS1"}`)

	return h
}

// --- Cookie Helpers ---

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

func unquoteCookieValue(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

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

	setURLs := []string{
		"https://www.icloud.com",
		"https://setup.icloud.com",
		"https://idmsa.apple.com",
		"https://appleid.apple.com",
		"https://www.apple.com",
	}
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

// --- Session Persistence ---

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

// loadCredentialsFile reads ICLOUD_USERNAME and ICLOUD_PASSWORD from
// ~/.config/icloud-reminders/credentials (shell export format).
// Returns an error if the file doesn't exist or values are missing.
func loadCredentialsFile() (username, password string, err error) {
	home := os.Getenv("HOME")
	if home == "" {
		return "", "", fmt.Errorf("HOME not set")
	}
	path := filepath.Join(home, ".config", "icloud-reminders", "credentials")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		// Support: export ICLOUD_USERNAME="value"  or  ICLOUD_USERNAME=value
		line = strings.TrimPrefix(line, "export ")
		if strings.HasPrefix(line, "ICLOUD_USERNAME=") {
			username = strings.Trim(strings.TrimPrefix(line, "ICLOUD_USERNAME="), `"'`)
		}
		if strings.HasPrefix(line, "ICLOUD_PASSWORD=") {
			password = strings.Trim(strings.TrimPrefix(line, "ICLOUD_PASSWORD="), `"'`)
		}
	}
	if username == "" || password == "" {
		return "", "", fmt.Errorf("credentials not found in %s", path)
	}
	return username, password, nil
}

func promptUser(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text())
	}
	return ""
}

// promptUserPassword reads a password from stdin without echoing.
func promptUserPassword(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	bytePassword, err := term.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return ""
	}
	fmt.Fprintln(os.Stderr)
	return strings.TrimSpace(string(bytePassword))
}
