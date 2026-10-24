package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDriveDiscoveryReusesSavedGoLoginWithoutCloudKitProbeOrSignIn(t *testing.T) {
	a := NewNonInteractive(context.Background())
	path := filepath.Join(t.TempDir(), "session.json")
	original := SessionData{CKBaseURL: "https://p1-ckdatabasews.icloud.com", SessionToken: "private-token",
		Cookies: []Cookie{{Name: "login", Value: "private-cookie", Domain: "icloud.com", Path: "/", Secure: true}}}
	data, _ := json.Marshal(original)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.client.Transport = authTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "setup.icloud.com" || r.URL.Path != "/setup/ws/1/accountLogin" {
			t.Fatal("Drive discovery must only reuse accountLogin, never SRP, 2FA, CloudKit or approval")
		}
		var input map[string]any
		_ = json.NewDecoder(r.Body).Decode(&input)
		if input["dsWebAuthToken"] != "private-token" {
			t.Fatal("saved login token was not reused")
		}
		return authResponse(200, http.Header{}, `{"dsInfo":{"dsid":123456789012345678},"webservices":{"ckdatabasews":{"url":"https://p1-ckdatabasews.icloud.com"},"drivews":{"url":"https://p1-drivews.icloud.com"},"docws":{"url":"https://p1-docws.icloud.com"}}}`), nil
	})
	result, err := a.PrepareDriveSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.DSID != "123456789012345678" || result.DriveBaseURL == "" || result.DocBaseURL == "" || result.CKBaseURL != original.CKBaseURL {
		t.Fatal("Drive discovery did not preserve the Go login and exact account identity")
	}
	loaded, err := loadSessionFile(path)
	if err != nil || loaded.DriveBaseURL != result.DriveBaseURL || loaded.SessionToken != original.SessionToken {
		t.Fatal("updated discovery was not persisted safely")
	}
}

func TestDriveDiscoveryRejectsForeignEndpointsAndPrivateErrors(t *testing.T) {
	for _, raw := range []string{"http://p1-drivews.icloud.com", "https://icloud.com.evil.invalid", "https://user:secret@p1-drivews.icloud.com", "https://p1-drivews.icloud.com/path", "https://p1-drivews.icloud.com:444"} {
		if safeDriveEndpoint(raw) {
			t.Fatal("untrusted Drive endpoint accepted")
		}
	}
	a := New()
	path := filepath.Join(t.TempDir(), "session.json")
	_ = os.WriteFile(path, []byte(`{"session_token":"private-token"}`), 0600)
	a.client.Transport = authTransport(func(r *http.Request) (*http.Response, error) {
		return authResponse(403, http.Header{}, "private-account-response"), nil
	})
	_, err := a.PrepareDriveSession(path)
	if err == nil || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "https://") {
		t.Fatal("discovery did not produce a safe failure")
	}
}

func TestDriveApprovalUsesDriveAppAndPreservesOneNotificationWorkflow(t *testing.T) {
	a := New()
	a.data.CKBaseURL = "https://p1-ckdatabasews.icloud.com"
	a.data.DriveBaseURL = "https://p1-drivews.icloud.com"
	a.data.DocBaseURL = "https://p1-docws.icloud.com"
	a.data.DSID = "12345"
	a.approvalPollInterval = time.Millisecond
	path := filepath.Join(t.TempDir(), "session.json")
	u, _ := url.Parse(SetupEndpoint)
	a.jar.SetCookies(u, []*http.Cookie{{Name: "login", Value: "private-login", Domain: ".icloud.com", Path: "/", Secure: true}})
	requests := 0
	a.client.Transport = authTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "p1-drivews.icloud.com" {
			if _, err := r.Cookie("pcs-drive"); err == nil {
				return authResponse(200, http.Header{}, `[{"drivewsid":"FOLDER::com.apple.CloudDocs::root","type":"FOLDER","status":"OK"}]`), nil
			}
			return authResponse(403, http.Header{}, "private-access-error"), nil
		}
		if r.URL.Path == "/setup/ws/1/requestWebAccessState" {
			return authResponse(200, http.Header{}, `{"isWebAccessAllowed":true,"isICDRSDisabled":true,"isDeviceConsentedForPCS":true}`), nil
		}
		if r.URL.Path != "/setup/ws/1/requestPCS" {
			t.Fatal("an already armed session must not request device consent again")
		}
		var input map[string]any
		_ = json.NewDecoder(r.Body).Decode(&input)
		if input["appName"] != "iclouddrive" || input["derivedFromUserAction"] != (requests == 0) {
			t.Fatal("wrong service keys or repeated user action requested")
		}
		requests++
		if requests == 1 {
			return authResponse(200, http.Header{}, `{"status":"pending"}`), nil
		}
		return authResponse(200, http.Header{"Set-Cookie": {"pcs-drive=private-approved; Domain=.icloud.com; Path=/; Secure"}}, `{"status":"success"}`), nil
	})
	if _, err := a.ApproveDriveAccess(path, time.Second, io.Discard); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatal("expected bounded Drive approval polling")
	}
	if _, err := a.ApproveDriveAccess(path, time.Second, io.Discard); err != nil || requests != 2 {
		t.Fatal("already approved Drive must not resend notifications")
	}
}

func TestDriveExportKeepsReferenceKeysAndCannotOverwriteGoSession(t *testing.T) {
	a := New()
	a.data.DriveBaseURL = "https://p1-drivews.icloud.com"
	a.data.DocBaseURL = "https://p1-docws.icloud.com"
	a.data.DSID = "12345"
	u, _ := url.Parse(SetupEndpoint)
	a.jar.SetCookies(u, []*http.Cookie{{Name: "login", Value: "private-cookie", Domain: ".icloud.com", Path: "/", Secure: true}})
	a.client.Transport = authTransport(func(r *http.Request) (*http.Response, error) {
		return authResponse(200, http.Header{}, `[{"drivewsid":"FOLDER::com.apple.CloudDocs::root","type":"FOLDER"}]`), nil
	})
	base := t.TempDir()
	source := filepath.Join(base, "go", "session.json")
	output := filepath.Join(base, "drive", "session.json")
	if err := a.saveSession(source); err != nil {
		t.Fatal(err)
	}
	if err := a.ExportDriveSession(source, source); err == nil {
		t.Fatal("Drive export must not overwrite Go auth")
	}
	if err := a.ExportDriveSession(source, output); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(output)
	var first driveSnapshot
	_ = json.Unmarshal(data, &first)
	key, _ := base64.URLEncoding.DecodeString(first.IDKey)
	info, _ := os.Stat(output)
	if len(key) != 32 || info.Mode().Perm() != 0600 || len(first.Cookies) != 1 || first.Cookies[0].Domain != ".icloud.com" {
		t.Fatal("export lost private key, permissions or cookie scope")
	}
	if err := a.ExportDriveSession(source, output); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(output)
	var second driveSnapshot
	_ = json.Unmarshal(data, &second)
	if first.IDKey != second.IDKey || first.AccountKey != second.AccountKey {
		t.Fatal("re-export changed stable issued-reference identity")
	}
}
