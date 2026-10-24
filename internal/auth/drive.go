package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"icloud-reminders/internal/storage"
)

var ErrDriveAccessRequired = errors.New("Drive web access requires explicit device approval; rerun drive-export with --approve-web-access")

func safeDriveEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host != "icloud.com" && !strings.HasSuffix(host, ".icloud.com") {
		return false
	}
	return u.Path == "" || u.Path == "/"
}

func optionalDriveEndpoint(services map[string]interface{}, key string) string {
	service, ok := services[key].(map[string]interface{})
	if !ok {
		return ""
	}
	raw, _ := service["url"].(string)
	if !safeDriveEndpoint(raw) {
		return ""
	}
	return strings.TrimRight(raw, "/")
}

// PrepareDriveSession reuses the Go login; it cannot request credentials,
// start SRP/2FA or send device notifications. Only administrative CLI code
// calls it while holding the existing data-directory lease.
func (a *Authenticator) PrepareDriveSession(sessionFile string) (*SessionData, error) {
	saved, err := loadSessionFile(sessionFile)
	if err != nil || saved.SessionToken == "" && len(saved.Cookies) == 0 {
		return nil, ErrAuthRequired
	}
	a.data = *saved
	a.sessionID, a.scnt = saved.SessionID, saved.Scnt
	a.authToken, a.trustToken = saved.SessionToken, saved.TrustToken
	a.restoreCookies(saved.Cookies)
	client := *a.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	a.client = &client
	// Legacy sessions contain only CKBaseURL. Resolve all service roots from
	// accountLogin using the saved token, even when Reminders PCS has expired.
	if !safeDriveEndpoint(a.data.DriveBaseURL) || !safeDriveEndpoint(a.data.DocBaseURL) || a.data.DSID == "" {
		ck, err := a.accountLogin()
		if err != nil {
			return nil, fmt.Errorf("cannot discover Drive using the saved Go login; run reminders auth if the login expired: %w", err)
		}
		a.data.CKBaseURL = ck
	}
	if !safeDriveEndpoint(a.data.DriveBaseURL) || !safeDriveEndpoint(a.data.DocBaseURL) || a.data.DSID == "" {
		return nil, errors.New("the saved account did not advertise supported Drive/document endpoints")
	}
	if a.data.DriveClientID == "" {
		a.data.DriveClientID = a.frameID
	}
	a.data.Cookies = a.extractCookies()
	if err := a.saveSession(sessionFile); err != nil {
		return nil, errors.New("cannot save Drive discovery to the private Go session")
	}
	return &a.data, nil
}

func (a *Authenticator) probeDrive() (bool, error) {
	if !safeDriveEndpoint(a.data.DriveBaseURL) {
		return false, ErrAuthRequired
	}
	u, _ := url.Parse(a.data.DriveBaseURL + "/retrieveItemDetailsInFolders")
	params := url.Values{"dsid": {a.data.DSID}, "clientId": {a.data.DriveClientID}}
	u.RawQuery = params.Encode()
	payload := []byte(`[{"drivewsid":"FOLDER::com.apple.CloudDocs::root","partialData":false}]`)
	req, err := http.NewRequestWithContext(a.ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return false, errors.New("cannot prepare Drive access probe")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", HomeEndpoint)
	req.Header.Set("Referer", HomeEndpoint+"/")
	resp, err := a.client.Do(req)
	if err != nil {
		return false, errors.New("Drive access probe could not complete")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 421 {
		return false, ErrAuthRequired
	}
	if resp.StatusCode == 403 || resp.StatusCode == 450 {
		return false, ErrDriveAccessRequired
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("Drive access probe returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return false, errors.New("Drive access probe response was unreadable or oversized")
	}
	var nodes []struct {
		ID     string `json:"drivewsid"`
		Type   string `json:"type"`
		Status string `json:"status"`
	}
	if json.Unmarshal(data, &nodes) != nil || len(nodes) != 1 || nodes[0].ID != "FOLDER::com.apple.CloudDocs::root" || nodes[0].Type != "FOLDER" || nodes[0].Status != "" && nodes[0].Status != "OK" {
		return false, errors.New("Drive did not confirm root-folder access")
	}
	return true, nil
}

// ApproveDriveAccess is separate from Reminders approval. It reuses login,
// device-consent polling and cookie persistence with appName=iclouddrive.
func (a *Authenticator) ApproveDriveAccess(sessionFile string, timeout time.Duration, progress io.Writer) (*SessionData, error) {
	if !a.interactive || !safeDriveEndpoint(a.data.DriveBaseURL) || !safeDriveEndpoint(a.data.DocBaseURL) {
		return nil, ErrAuthRequired
	}
	return a.approveWebAccess(sessionFile, timeout, progress, "iclouddrive", "Drive", a.probeDrive)
}

type driveSnapshot struct {
	Version     int               `json:"version"`
	AccountKey  string            `json:"account_key"`
	IDKey       string            `json:"id_key"`
	DriveURL    string            `json:"drive_url"`
	DocumentURL string            `json:"document_url"`
	Params      map[string]string `json:"params"`
	Cookies     []driveCookie     `json:"cookies"`
	UserAgent   string            `json:"user_agent"`
}

type driveCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Expires  *int64 `json:"expires"`
	Secure   bool   `json:"secure"`
	HostOnly bool   `json:"host_only"`
}

// ExportDriveSession writes a password-free snapshot for the private Python
// reader. It never prints cookie/token data or exposes this through MCP.
func (a *Authenticator) ExportDriveSession(sessionFile, outputFile string) error {
	source, sourceErr := filepath.Abs(sessionFile)
	destination, destinationErr := filepath.Abs(outputFile)
	if sourceErr != nil || destinationErr != nil || source == destination {
		return errors.New("Drive export must not replace the original Go login session")
	}
	if original, err := os.Stat(sessionFile); err == nil {
		if existing, err := os.Stat(outputFile); err == nil && os.SameFile(original, existing) {
			return errors.New("Drive export must not replace the original Go login session")
		}
	}
	if ok, err := a.probeDrive(); !ok {
		if err != nil {
			return err
		}
		return ErrDriveAccessRequired
	}
	if filepath.Base(outputFile) != "session.json" {
		return errors.New("Drive output must be a private session.json path")
	}
	account := sha256.Sum256([]byte(a.data.DSID))
	snapshot := driveSnapshot{Version: 1, AccountKey: hex.EncodeToString(account[:]),
		DriveURL: a.data.DriveBaseURL, DocumentURL: a.data.DocBaseURL,
		Params: map[string]string{"dsid": a.data.DSID, "clientId": a.data.DriveClientID}, UserAgent: "Go-http-client/1.1"}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return errors.New("cannot generate private Drive reference key")
	}
	snapshot.IDKey = base64.URLEncoding.EncodeToString(key)
	if file, err := os.Open(outputFile); err == nil {
		previous, readErr := io.ReadAll(io.LimitReader(file, 1_000_001))
		file.Close()
		var old driveSnapshot
		if readErr == nil && len(previous) <= 1_000_000 && json.Unmarshal(previous, &old) == nil && old.AccountKey == snapshot.AccountKey {
			if decoded, err := base64.URLEncoding.DecodeString(old.IDKey); err == nil && len(decoded) == 32 {
				snapshot.IDKey = old.IDKey
			}
		}
	}
	for _, cookie := range a.extractCookies() {
		domain := strings.TrimPrefix(cookie.Domain, ".")
		if domain != "icloud.com" && !strings.HasSuffix(domain, ".icloud.com") {
			continue
		}
		if !cookie.HostOnly {
			domain = "." + domain
		}
		item := driveCookie{Name: cookie.Name, Value: cookie.Value, Domain: domain, Path: cookie.Path,
			Secure: cookie.Secure, HostOnly: cookie.HostOnly}
		if cookie.Expires != 0 {
			expires := cookie.Expires
			item.Expires = &expires
		}
		snapshot.Cookies = append(snapshot.Cookies, item)
	}
	if len(snapshot.Cookies) == 0 || len(snapshot.Cookies) > 100 {
		return errors.New("Drive export needs a bounded cookie snapshot")
	}
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) > 1_000_000 {
		return errors.New("cannot prepare bounded Drive session export")
	}
	if err := storage.WritePrivate(outputFile, data); err != nil {
		return errors.New("cannot write private Drive export; check output-directory permissions")
	}
	return nil
}
