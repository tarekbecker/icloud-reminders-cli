package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type approvalFixture struct {
	t               *testing.T
	a               *Authenticator
	sessionFile     string
	original        []byte
	mode            string
	workflow        string
	consentRequests int
	stateRequests   int
	keyActions      []bool
	cancel          context.CancelFunc
}

func newApproval(t *testing.T, mode string) *approvalFixture {
	t.Helper()
	f := &approvalFixture{t: t, mode: mode, sessionFile: filepath.Join(t.TempDir(), "session.json")}
	f.a = New()
	f.a.approvalPollInterval = time.Millisecond
	f.a.data.CKBaseURL = "https://p1-ckdatabasews.icloud.com"
	u, _ := url.Parse(SetupEndpoint)
	f.a.jar.SetCookies(u, []*http.Cookie{{Name: "login", Value: "private-login", Domain: ".icloud.com", Path: "/", Secure: true}})
	f.a.data.Cookies = f.a.extractCookies()
	if err := f.a.saveSession(f.sessionFile); err != nil {
		t.Fatal(err)
	}
	f.original, _ = os.ReadFile(f.sessionFile)
	f.a.client.Transport = authTransport(f.respond)
	return f
}

func (f *approvalFixture) respond(r *http.Request) (*http.Response, error) {
	f.t.Helper()
	if r.URL.Host == "p1-ckdatabasews.icloud.com" {
		if _, err := r.Cookie("X-APPLE-WEBAUTH-PCS-Test"); err == nil && f.mode != "incomplete" {
			return authResponse(200, http.Header{}, "{}"), nil
		}
		return authResponse(403, http.Header{}, `{"serverErrorCode":"ACCESS_DENIED","reason":"private db access disabled for this account"}`), nil
	}
	if r.URL.Host != "setup.icloud.com" || r.Method != http.MethodPost {
		f.t.Fatal("unexpected approval host or method")
	}
	if _, err := r.Cookie("login"); err != nil {
		f.t.Fatal("approval lost the existing login session")
	}
	var body struct {
		Workflow   string `json:"workflowUuid"`
		AppName    string `json:"appName"`
		UserAction bool   `json:"derivedFromUserAction"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.Workflow == "" {
		f.t.Fatal("approval workflow missing")
	}
	if f.workflow == "" {
		f.workflow = body.Workflow
	}
	if f.workflow != body.Workflow {
		f.t.Fatal("approval polling changed workflows")
	}
	switch r.URL.Path {
	case "/setup/ws/1/requestWebAccessState":
		f.stateRequests++
		if f.mode == "auth-expired" {
			return authResponse(401, http.Header{}, "private-upstream-body"), nil
		}
		if f.mode == "malformed" {
			return authResponse(200, http.Header{}, "private-upstream-body"), nil
		}
		allowed := f.mode != "web-disabled"
		consented := f.stateRequests >= 3 && f.mode != "timeout"
		if f.mode == "armed" || f.mode == "key-timeout" {
			consented = true
		}
		payload, _ := json.Marshal(map[string]any{"isWebAccessAllowed": allowed, "isICDRSDisabled": f.mode != "standard", "isDeviceConsentedForPCS": consented})
		return authResponse(200, http.Header{}, string(payload)), nil
	case "/setup/ws/1/enableDeviceConsentForPCS":
		f.consentRequests++
		if f.consentRequests != 1 {
			f.t.Fatal("device consent notification was resent during polling")
		}
		if f.cancel != nil {
			f.cancel()
		}
		payload, _ := json.Marshal(map[string]any{"isWebAccessAllowed": true, "isDeviceConsentedForPCS": false,
			"isDeviceConsentNotificationSent": f.mode != "no-notification", "deviceNotSupported": f.mode == "unsupported"})
		return authResponse(200, http.Header{}, string(payload)), nil
	case "/setup/ws/1/requestPCS":
		if body.AppName != "reminders" {
			f.t.Fatal("requested keys for the wrong app")
		}
		f.keyActions = append(f.keyActions, body.UserAction)
		if len(f.keyActions) == 1 || f.mode == "key-timeout" {
			return authResponse(200, http.Header{}, `{"status":"pending","message":"private-upstream-message"}`), nil
		}
		return authResponse(200, http.Header{"Set-Cookie": {"X-APPLE-WEBAUTH-PCS-Test=approved; Domain=.icloud.com; Path=/; Secure; Max-Age=600"}}, `{"status":"success"}`), nil
	default:
		f.t.Fatal("unexpected approval endpoint")
		return nil, errors.New("unexpected request")
	}
}

func TestADPApprovalPersistsScopedSessionAndSurvivesRestart(t *testing.T) {
	f := newApproval(t, "success")
	var output bytes.Buffer
	session, err := f.a.ApproveWebAccess(f.sessionFile, time.Second, &output)
	if err != nil {
		t.Fatal(err)
	}
	if f.consentRequests != 1 || f.stateRequests != 3 || len(f.keyActions) != 2 || !f.keyActions[0] || f.keyActions[1] {
		t.Fatal("expected one consent request, bounded state polling and one user-initiated key request")
	}
	if strings.Contains(output.String(), "private-") || strings.Contains(output.String(), "approved;") || strings.Contains(output.String(), "https://") {
		t.Fatal("approval output leaked account or upstream details")
	}
	found := false
	for _, c := range session.Cookies {
		if c.Name == "X-APPLE-WEBAUTH-PCS-Test" {
			found = c.Domain == "icloud.com" && !c.HostOnly && c.Path == "/" && c.Secure && c.Expires > time.Now().Unix()
		}
	}
	if !found {
		t.Fatal("approved cookie scope or expiry was lost")
	}
	info, err := os.Stat(f.sessionFile)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("approved session should remain private")
	}
	restarted := NewNonInteractive(context.Background())
	restarted.client.Transport = authTransport(f.respond)
	if _, err := restarted.EnsureSession(f.sessionFile, false); err != nil {
		t.Fatal("saved approval did not survive restart")
	}
	if f.consentRequests != 1 {
		t.Fatal("server reuse must not request another notification")
	}
}

func TestADPAlreadyArmedSessionDoesNotRequestAnotherConsent(t *testing.T) {
	f := newApproval(t, "armed")
	if _, err := f.a.ApproveWebAccess(f.sessionFile, time.Second, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.consentRequests != 0 {
		t.Fatal("an already armed device should not be notified again")
	}
	before := len(f.keyActions)
	if _, err := f.a.ApproveWebAccess(f.sessionFile, time.Second, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.keyActions) != before {
		t.Fatal("already available data should not request more keys")
	}
}

func TestStandardProtectionDoesNotArmADevice(t *testing.T) {
	f := newApproval(t, "standard")
	if _, err := f.a.ApproveWebAccess(f.sessionFile, time.Second, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.consentRequests != 0 {
		t.Fatal("standard protection should not request device consent")
	}
}

func TestADPFailuresPreserveSessionAndDoNotLeakDetails(t *testing.T) {
	for _, mode := range []string{"web-disabled", "timeout", "key-timeout", "unsupported", "no-notification", "auth-expired", "malformed", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			f := newApproval(t, mode)
			var output bytes.Buffer
			_, err := f.a.ApproveWebAccess(f.sessionFile, 30*time.Millisecond, &output)
			if err == nil {
				t.Fatal("incomplete or denied approval must not succeed")
			}
			if strings.Contains(err.Error(), "private-") || strings.Contains(output.String(), "private-") || strings.Contains(err.Error(), "https://") {
				t.Fatal("approval error leaked private details")
			}
			if mode == "web-disabled" && (!errors.Is(err, ErrWebAccessDisabled) || f.consentRequests != 0 || len(f.keyActions) != 0) {
				t.Fatal("disabled web access must stop before notifying devices")
			}
			if (mode == "timeout" || mode == "key-timeout") && !errors.Is(err, ErrApprovalTimeout) {
				t.Fatal("approval wait should have a bounded timeout")
			}
			if mode == "auth-expired" && !errors.Is(err, ErrAuthRequired) {
				t.Fatal("expired login should request authentication")
			}
			if mode == "incomplete" && !errors.Is(err, ErrApprovalIncomplete) {
				t.Fatal("server success alone must not prove data access")
			}
			after, _ := os.ReadFile(f.sessionFile)
			if !bytes.Equal(f.original, after) {
				t.Fatal("failed approval changed the saved session")
			}
			for index, action := range f.keyActions {
				if action != (index == 0) {
					t.Fatal("key polling must not resend user actions")
				}
			}
		})
	}
}

func TestADPApprovalCancellationAndNonInteractiveGuard(t *testing.T) {
	f := newApproval(t, "success")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.a.ctx, f.cancel = ctx, cancel
	if _, err := f.a.ApproveWebAccess(f.sessionFile, time.Minute, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal("device approval should observe cancellation")
	}
	f.a.interactive = false
	before := f.consentRequests
	if _, err := f.a.ApproveWebAccess(f.sessionFile, time.Second, io.Discard); !errors.Is(err, ErrAuthRequired) || f.consentRequests != before {
		t.Fatal("noninteractive server must not initiate device approval")
	}
}
