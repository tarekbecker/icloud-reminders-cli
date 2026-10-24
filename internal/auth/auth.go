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
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"icloud-reminders/internal/logger"
	"icloud-reminders/internal/storage"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/term"
	"icloud-reminders/internal/srp"
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
	DriveBaseURL   string   `json:"drive_base_url,omitempty"`
	DocBaseURL     string   `json:"doc_base_url,omitempty"`
	DriveClientID  string   `json:"drive_client_id,omitempty"`
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
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Expires  int64  `json:"expires"`
	Secure   bool   `json:"secure"`
	HostOnly bool   `json:"host_only,omitempty"`
}

// Authenticator manages iCloud authentication state using SRP.
type Authenticator struct {
	ctx                  context.Context
	interactive          bool
	username             string
	password             string
	clientID             string
	frameID              string
	authAttr             string
	sessionID            string
	scnt                 string
	authToken            string
	trustToken           string
	jar                  *sessionJar
	client               *http.Client
	data                 SessionData
	approvalPollInterval time.Duration
}

// New creates an Authenticator without credentials (interactive mode).
func New() *Authenticator {
	return NewWithContext(context.Background())
}

// NewWithContext creates an interactive authenticator with cancellation.
func NewWithContext(ctx context.Context) *Authenticator {
	jar := newSessionJar()
	frameID := strings.ToLower(uuid.New().String())
	return &Authenticator{
		ctx:                  ctx,
		interactive:          true,
		clientID:             "auth-" + frameID,
		frameID:              frameID,
		jar:                  jar,
		client:               &http.Client{Jar: jar, Timeout: 30 * time.Second},
		approvalPollInterval: 5 * time.Second,
	}
}

// ErrAuthRequired means an administrator must authenticate with the CLI.
var ErrAuthRequired = errors.New("authentication required; run reminders auth")

// NewNonInteractive only reuses or refreshes a saved session. It never reads
// credentials, initiates password authentication, or prompts on stdin.
func NewNonInteractive(ctx context.Context) *Authenticator {
	a := New()
	a.ctx = ctx
	a.interactive = false
	return a
}

// --- Public API ---

// EnsureSession loads a saved session and validates it, or runs full auth flow.
// Returns the final SessionData with a valid CK base URL.
func (a *Authenticator) EnsureSession(sessionFile string, forceReauth bool) (*SessionData, error) {
	if !forceReauth {
		// Try to reuse saved session
		if saved, err := loadSessionFile(sessionFile); err == nil && saved.CKBaseURL != "" {
			logger.Info("Trying saved session...")
			if saved.CreatedAt != "" {
				logger.Debugf("Session created at: %s", saved.CreatedAt)
			}
			a.data = *saved // must be set before restoreCookies so CKBaseURL is included
			a.sessionID = saved.SessionID
			a.scnt = saved.Scnt
			a.authToken = saved.SessionToken
			a.trustToken = saved.TrustToken
			a.restoreCookies(saved.Cookies) // CKBaseURL is now set, so CK host is included

			// Probe: try the CK base URL directly
			logger.Debugf("Probing CloudKit: %s", saved.CKBaseURL)
			ok, err := a.probeCloudKit(saved.CKBaseURL)
			if err != nil {
				return nil, err
			}
			if ok {
				logger.Info("Session reused OK.")
				return &a.data, nil
			}

			// Cookies stale — try accountLogin to refresh them
			logger.Info("Session stale, refreshing via accountLogin...")
			if ckURL, err := a.accountLogin(); err == nil {
				a.data.CKBaseURL = ckURL
				a.data.Cookies = a.extractCookies()
				a.data.CreatedAt = time.Now().Format(time.RFC3339)
				_ = a.saveSession(sessionFile)
				logger.Infof("Session refreshed. CK host: %s", hostOf(ckURL))
				return &a.data, nil
			} else {
				logger.Infof("accountLogin failed (%v), doing full re-auth...", err)
			}
		}
	}

	// Full SRP authentication
	if !a.interactive {
		return nil, ErrAuthRequired
	}
	return a.fullAuth(sessionFile)
}

// probeCloudKit makes a lightweight test call to verify access.
func (a *Authenticator) probeCloudKit(ckBase string) (bool, error) {
	if ckBase == "" {
		return false, nil
	}
	base := ckBase
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	probeURL := base + "database/1/com.apple.reminders/production/private/zones/list"

	req, err := http.NewRequestWithContext(a.ctx, "POST", probeURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", HomeEndpoint)
	req.Header.Set("Referer", HomeEndpoint+"/")

	resp, err := a.client.Do(req)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if IsWebAccessDisabled(resp.StatusCode, body) {
		return false, ErrWebAccessDisabled
	}
	return resp.StatusCode >= 200 && resp.StatusCode < 300, nil
}

// fullAuth runs the complete SRP signin flow.
func (a *Authenticator) fullAuth(sessionFile string) (*SessionData, error) {
	fmt.Fprintln(os.Stderr, "Signing in to iCloud (SRP)...")
	logger.Infof("Authenticating as: %s", a.username)

	// 1. Environment variables (set by credentials file or the caller)
	if a.username == "" {
		if v := os.Getenv("ICLOUD_USERNAME"); v != "" {
			a.username = v
			logger.Debug("Credentials: username from env ICLOUD_USERNAME")
		}
	}
	if a.password == "" {
		if v := os.Getenv("ICLOUD_PASSWORD"); v != "" {
			a.password = v
			logger.Debug("Credentials: password from env ICLOUD_PASSWORD")
		}
	}

	// 2. Credentials file (~/.config/icloud-reminders/credentials)
	if a.username == "" || a.password == "" {
		if user, pass, err := loadCredentialsFile(sessionDir(sessionFile)); err == nil {
			if a.username == "" {
				a.username = user
				logger.Debug("Credentials: username from credentials file")
			}
			if a.password == "" {
				a.password = pass
				logger.Debug("Credentials: password from credentials file")
			}
		}
	}

	// 3. Interactive prompt (fallback)
	if a.username == "" {
		logger.Debug("Credentials: username via interactive prompt")
		a.username = promptUser("Apple ID: ")
	}
	if a.password == "" {
		logger.Debug("Credentials: password via interactive prompt")
		a.password = promptUserPassword("Password: ")
	}

	// Reset state
	a.jar = newSessionJar()
	a.client = &http.Client{Jar: a.jar, Timeout: 30 * time.Second}
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
		if err := a.verifyTwoFactor(os.Stdin, os.Stderr); err != nil {
			return nil, err
		}
	} else {
		logger.Debug("2FA skipped (trust token accepted)")
	}

	// Step 6: Get trust tokens
	if err := a.getTrust(); err != nil {
		logger.Debugf("getTrust failed (non-fatal): %v", err)
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
		logger.Infof("Warning: failed to save session: %v", err)
	}

	fmt.Fprintf(os.Stderr, "✅ Authenticated. CK base: %s\n", ckURL)
	logger.Infof("Session saved to: %s", sessionFile)
	logger.Debugf("Trust token present: %v", a.trustToken != "")
	return &a.data, nil
}

// --- SRP Authentication Steps ---

// authStart initializes the authentication session.
func (a *Authenticator) authStart() error {
	url := fmt.Sprintf("%s/authorize/signin?frame_id=%s&language=en_US&skVersion=7&iframeId=%s&client_id=%s&redirect_uri=https://www.icloud.com&response_type=code&response_mode=web_message&state=%s&authVersion=latest",
		AuthEndpoint, a.clientID, a.clientID, WidgetKey, a.clientID)

	req, err := http.NewRequestWithContext(a.ctx, "GET", url, nil)
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

	req, err := http.NewRequestWithContext(a.ctx, "POST", AuthEndpoint+"/federate?isRememberMeEnabled=true", strings.NewReader(body))
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
	req, err := http.NewRequestWithContext(a.ctx, "POST", AuthEndpoint+"/signin/init", bytes.NewReader(bodyJSON))
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
	req, err := http.NewRequestWithContext(a.ctx, "POST", AuthEndpoint+"/signin/complete?isRememberMeEnabled=true", bytes.NewReader(bodyJSON))
	if err != nil {
		return false, err
	}

	req.Header = a.updateAuthHeaders(req.Header)

	resp, err := a.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	a.captureSessionHeaders(resp.Header)

	switch resp.StatusCode {
	case 200:
		return false, nil // Success, no 2FA
	case 409:
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

// verifyTwoFactor explicitly requests delivery before waiting for a code.
// A successful SRP challenge does not always trigger Apple's device notification.
func (a *Authenticator) verifyTwoFactor(input io.Reader, output io.Writer) error {
	fmt.Fprintln(output, "Two-factor authentication required.")
	fmt.Fprintln(output, "Requesting a verification code on your trusted Apple devices...")
	if err := a.requestTwoFactor(); err != nil {
		return err
	}
	fmt.Fprintln(output, "Approve the sign-in notification on a trusted Apple device.")
	fmt.Fprint(output, "Enter 2FA code: ")
	scanner := bufio.NewScanner(input)
	if !scanner.Scan() {
		return errors.New("2FA code input unavailable; run reminders auth in an interactive terminal")
	}
	code := strings.TrimSpace(scanner.Text())
	if len(code) != 6 || strings.IndexFunc(code, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return errors.New("2FA code must contain six digits")
	}
	if err := a.submitTwoFactor(code); err != nil {
		return fmt.Errorf("2FA verification: %w", err)
	}
	fmt.Fprintln(output, "2FA accepted.")
	return nil
}

// requestTwoFactor asks Apple to push a code using the current SRP session.
// This matches rclone's RequestPushNotification implementation; no automatic
// retries are made because each successful request may send another code.
func (a *Authenticator) requestTwoFactor() error {
	req, err := http.NewRequestWithContext(a.ctx, http.MethodPut, AuthEndpoint+"/verify/trusteddevice/securitycode", nil)
	if err != nil {
		return errors.New("cannot prepare 2FA code request")
	}
	req.Header = a.updateAuthHeaders(req.Header)
	resp, err := a.client.Do(req)
	if err != nil {
		if a.ctx.Err() != nil {
			return fmt.Errorf("2FA code request failed: %w", a.ctx.Err())
		}
		return errors.New("2FA code request failed: network error")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("2FA code request failed: HTTP %d; retry reminders auth later", resp.StatusCode)
	}
	a.captureSessionHeaders(resp.Header)
	return nil
}

// submitTwoFactor submits the 2FA code.
func (a *Authenticator) submitTwoFactor(code string) error {
	body := map[string]interface{}{
		"securityCode": map[string]string{"code": strings.TrimSpace(code)},
	}

	bodyJSON, _ := json.Marshal(body)
	url := fmt.Sprintf("%s/verify/trusteddevice/securitycode", AuthEndpoint)
	req, err := http.NewRequestWithContext(a.ctx, "POST", url, bytes.NewReader(bodyJSON))
	if err != nil {
		return err
	}

	req.Header = a.updateAuthHeaders(req.Header)
	req.Header.Set("X-Apple-ID-Session-Id", a.sessionID)
	req.Header.Set("scnt", a.scnt)

	resp, err := a.client.Do(req)
	if err != nil {
		if a.ctx.Err() != nil {
			return a.ctx.Err()
		}
		return errors.New("2FA submission failed: network error")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return fmt.Errorf("2FA submission failed: HTTP %d", resp.StatusCode)
	}

	a.captureSessionHeaders(resp.Header)

	return nil
}

// getTrust gets the session and trust tokens.
func (a *Authenticator) getTrust() error {
	req, err := http.NewRequestWithContext(a.ctx, "GET", AuthEndpoint+"/2sv/trust", nil)
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
	req, err := http.NewRequestWithContext(a.ctx, "POST", SetupEndpoint+"/accountLogin", bytes.NewReader(bodyJSON))
	if err != nil {
		return "", errors.New("cannot prepare accountLogin request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", HomeEndpoint)
	req.Header.Set("Referer", HomeEndpoint+"/")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", errors.New("accountLogin network request failed")
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(respBody) > 1<<20 {
		return "", errors.New("accountLogin returned an unreadable or oversized response")
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("accountLogin HTTP %d", resp.StatusCode)
	}

	var result map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(respBody))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return "", errors.New("accountLogin returned invalid JSON")
	}

	// Extract DSID
	if dsInfo, ok := result["dsInfo"].(map[string]interface{}); ok {
		if dsid, ok := dsInfo["dsid"].(string); ok {
			a.data.DSID = dsid
		} else if dsid, ok := dsInfo["dsid"].(json.Number); ok {
			a.data.DSID = dsid.String()
		}
	}

	// Extract CK URL
	webservices, ok := result["webservices"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("no webservices in accountLogin response")
	}
	// Preserve optional Drive discovery without changing Calendar/Reminders
	// requirements or guessing account/partition addresses.
	a.data.DriveBaseURL = optionalDriveEndpoint(webservices, "drivews")
	a.data.DocBaseURL = optionalDriveEndpoint(webservices, "docws")
	if a.data.DriveClientID == "" {
		a.data.DriveClientID = a.frameID
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

// captureSessionHeaders preserves session continuity when Apple rotates headers.
func (a *Authenticator) captureSessionHeaders(h http.Header) {
	if value := h.Get("X-Apple-ID-Session-Id"); value != "" {
		a.sessionID = value
	}
	if value := h.Get("scnt"); value != "" {
		a.scnt = value
	}
}

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
	return a.jar.snapshot()
}

// hostOf returns just the hostname from a URL string, for concise logging.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Host
}

func unquoteCookieValue(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

func (a *Authenticator) restoreCookies(cookies []Cookie) {
	RestoreCookies(a.jar, cookies, a.data.CKBaseURL)
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
	return storage.WritePrivate(sessionFile, data)
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
func loadCredentialsFile(directory string) (username, password string, err error) {
	path := filepath.Join(directory, "credentials")
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
