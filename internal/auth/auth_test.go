package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type authTransport func(*http.Request) (*http.Response, error)

func (f authTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func authResponse(status int, headers http.Header, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}

func TestTwoFactorRequestsDeliveryBeforeReadingAndPreservesSession(t *testing.T) {
	a := New()
	step := 0
	a.client.Transport = authTransport(func(r *http.Request) (*http.Response, error) {
		step++
		switch step {
		case 1:
			if r.Method != http.MethodPost || r.URL.Path != "/appleauth/auth/signin/complete" {
				t.Fatal("expected SRP completion first")
			}
			return authResponse(409, http.Header{
				"X-Apple-Id-Session-Id": {"session-1"}, "Scnt": {"scnt-1"},
			}, "{}"), nil
		case 2:
			if r.Method != http.MethodPut || r.URL.Path != "/appleauth/auth/verify/trusteddevice/securitycode" {
				t.Fatal("expected explicit notification request before code validation")
			}
			if r.Header.Get("X-Apple-ID-Session-Id") != "session-1" || r.Header.Get("scnt") != "scnt-1" {
				t.Fatal("delivery request lost the SRP session")
			}
			return authResponse(202, http.Header{
				"X-Apple-Id-Session-Id": {"session-2"}, "Scnt": {"scnt-2"},
			}, ""), nil
		case 3:
			if r.Method != http.MethodPost || r.URL.Path != "/appleauth/auth/verify/trusteddevice/securitycode" {
				t.Fatal("expected code validation after delivery")
			}
			if r.Header.Get("X-Apple-ID-Session-Id") != "session-2" || r.Header.Get("scnt") != "scnt-2" {
				t.Fatal("code validation did not use rotated session headers")
			}
			var input struct {
				SecurityCode struct{ Code string } `json:"securityCode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.SecurityCode.Code != "123456" {
				t.Fatal("code validation did not receive the entered code")
			}
			return authResponse(204, http.Header{"X-Apple-Id-Session-Id": {"session-3"}}, ""), nil
		default:
			t.Fatal("unexpected additional authentication request")
			return nil, errors.New("unexpected request")
		}
	})
	required, err := a.authComplete("challenge", "proof-1", "proof-2")
	if err != nil || !required {
		t.Fatal("SRP completion should require 2FA")
	}
	var output bytes.Buffer
	input := &deliveryAwareReader{t: t, step: &step, input: strings.NewReader(" 123456 \n")}
	if err := a.verifyTwoFactor(input, &output); err != nil {
		t.Fatal(err)
	}
	if step != 3 || a.sessionID != "session-3" || a.scnt != "scnt-2" {
		t.Fatal("2FA session continuity was not preserved")
	}
	if !strings.Contains(output.String(), "2FA accepted.") || strings.Contains(output.String(), "123456") {
		t.Fatal("authentication output should report success without echoing the code")
	}
}

type deliveryAwareReader struct {
	t     *testing.T
	step  *int
	input io.Reader
}

func (r *deliveryAwareReader) Read(p []byte) (int, error) {
	r.t.Helper()
	if *r.step != 2 {
		r.t.Fatal("code input was read before requesting notification delivery")
	}
	return r.input.Read(p)
}

type unexpectedInput struct{ t *testing.T }

func (r unexpectedInput) Read([]byte) (int, error) {
	r.t.Fatal("a failed delivery request must not prompt for or read a code")
	return 0, io.EOF
}

func TestTwoFactorDeliveryFailureStopsWithoutRetryOrPrivateDetails(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a := New()
			calls := 0
			a.client.Transport = authTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return authResponse(status, http.Header{}, "private-account-details"), nil
			})
			var output bytes.Buffer
			err := a.verifyTwoFactor(unexpectedInput{t}, &output)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) || calls != 1 {
				t.Fatal("failed delivery should report status and stop after one request")
			}
			if strings.Contains(err.Error(), "private-account-details") || strings.Contains(output.String(), "Enter 2FA code") {
				t.Fatal("failed delivery leaked details or prompted for an unsent code")
			}
		})
	}
}

func TestTwoFactorDeliveryNetworkErrorAndCancellationAreSafe(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			a := New()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a.ctx = ctx
			if canceled {
				cancel()
			}
			a.client.Transport = authTransport(func(r *http.Request) (*http.Response, error) {
				if canceled {
					return nil, r.Context().Err()
				}
				return nil, errors.New("private-transport-details")
			})
			err := a.verifyTwoFactor(unexpectedInput{t}, io.Discard)
			if err == nil || strings.Contains(err.Error(), "private-transport-details") || strings.Contains(err.Error(), "https://") {
				t.Fatal("network failure should not expose remote details")
			}
			if canceled && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation should be preserved")
			}
		})
	}
}

func TestTwoFactorRejectsInvalidInputWithoutSubmittingIt(t *testing.T) {
	for _, code := range []string{"", "12345\n", "1234567\n", "abcdef\n"} {
		t.Run(fmt.Sprintf("length-%d", len(code)), func(t *testing.T) {
			a := New()
			calls := 0
			a.client.Transport = authTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPut {
					t.Fatal("invalid code must not be submitted")
				}
				return authResponse(200, http.Header{}, "{}"), nil
			})
			if err := a.verifyTwoFactor(strings.NewReader(code), io.Discard); err == nil || calls != 1 {
				t.Fatal("missing or invalid code should stop before validation")
			}
		})
	}
}

func TestTwoFactorSubmissionFailureDoesNotExposeCodeOrResponse(t *testing.T) {
	a := New()
	a.client.Transport = authTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut {
			return authResponse(202, http.Header{}, ""), nil
		}
		return authResponse(403, http.Header{}, "private-account-details 123456"), nil
	})
	var output bytes.Buffer
	err := a.verifyTwoFactor(strings.NewReader("123456\n"), &output)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatal("code validation failure should report the HTTP status")
	}
	if strings.Contains(err.Error(), "private-account-details") || strings.Contains(err.Error(), "123456") || strings.Contains(output.String(), "2FA accepted") {
		t.Fatal("failed validation exposed private details or reported success")
	}
}

func TestBlockedWebAccessDoesNotRefreshOrReauthenticate(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(fmt.Sprint(interactive), func(t *testing.T) {
			a := New()
			a.interactive = interactive
			sessionFile := filepath.Join(t.TempDir(), "session.json")
			original := []byte(`{"ck_base_url":"https://cloudkit.invalid","session_token":"private-session-token"}`)
			if err := os.WriteFile(sessionFile, original, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			a.client.Transport = authTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls != 1 || r.URL.Path != "/database/1/com.apple.reminders/production/private/zones/list" {
					t.Fatal("blocked database access must not refresh or reauthenticate")
				}
				return authResponse(403, http.Header{}, `{"serverErrorCode":"ACCESS_DENIED","reason":"private db access disabled for this account","uuid":"private-request-id"}`), nil
			})
			_, err := a.EnsureSession(sessionFile, false)
			if !errors.Is(err, ErrWebAccessDisabled) || calls != 1 {
				t.Fatal("blocked access should be reported separately from expired authentication")
			}
			if strings.Contains(err.Error(), "private-request-id") || strings.Contains(err.Error(), "private-session-token") {
				t.Fatal("blocked access exposed private data")
			}
			after, err := os.ReadFile(sessionFile)
			if err != nil || !bytes.Equal(original, after) {
				t.Fatal("a denied data read must preserve the existing session")
			}
		})
	}
}
