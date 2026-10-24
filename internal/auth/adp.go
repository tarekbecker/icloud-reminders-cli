package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

var (
	ErrApprovalTimeout     = errors.New("device approval timed out; retry the administrative command with --approve-web-access")
	ErrApprovalUnsupported = errors.New("no supported trusted device is available for iCloud web approval")
	ErrApprovalIncomplete  = errors.New("Apple reported approval but access to the requested service is still blocked")
)

type approvalState struct {
	Status             string `json:"status"`
	WebAllowed         *bool  `json:"isWebAccessAllowed"`
	ICDRSDisabled      *bool  `json:"isICDRSDisabled"`
	Consented          *bool  `json:"isDeviceConsentedForPCS"`
	NotificationSent   *bool  `json:"isDeviceConsentNotificationSent"`
	DeviceNotSupported bool   `json:"deviceNotSupported"`
}

// ApproveWebAccess obtains temporary Reminders access for this web session.
// Call EnsureSession first. Only an explicit administrative CLI invocation may
// request device notifications; normal MCP requests only reuse saved cookies.
func (a *Authenticator) ApproveWebAccess(sessionFile string, timeout time.Duration, progress io.Writer) (*SessionData, error) {
	if !a.interactive || a.data.CKBaseURL == "" {
		return nil, ErrAuthRequired
	}
	return a.approveWebAccess(sessionFile, timeout, progress, "reminders", "Reminders", func() (bool, error) {
		return a.probeCloudKit(a.data.CKBaseURL)
	})
}

func (a *Authenticator) approveWebAccess(sessionFile string, timeout time.Duration, progress io.Writer, appName, label string, probe func() (bool, error)) (*SessionData, error) {
	if timeout <= 0 || timeout > 15*time.Minute {
		return nil, errors.New("approval timeout must be positive and at most 15 minutes")
	}
	if progress == nil {
		progress = io.Discard
	}
	previousContext := a.ctx
	ctx, cancel := context.WithTimeout(a.ctx, timeout)
	a.ctx = ctx
	defer func() { cancel(); a.ctx = previousContext }()
	if ok, _ := probe(); ok {
		fmt.Fprintf(progress, "%s web access is already available.\n", label)
		return &a.data, nil
	}

	// Each approval attempt has one workflow. Polling does not resend device
	// consent notifications, and only the first key request is a user action.
	workflow := uuid.NewString()
	state, err := a.approvalRequest("requestWebAccessState", map[string]any{"workflowUuid": workflow})
	if err != nil {
		return nil, err
	}
	if state.WebAllowed == nil {
		return nil, errors.New("iCloud returned an unrecognized web access state")
	}
	if !*state.WebAllowed {
		return nil, ErrWebAccessDisabled
	}
	// With standard protection, no device arming is necessary. With ADP, an
	// unarmed session must first obtain explicit consent from a trusted device.
	// With ADP, iCloud Data Recovery Service (ICDRS) is disabled; the web
	// client then requires an armed device to release the requested PCS keys.
	needsDeviceConsent := state.ICDRSDisabled == nil || *state.ICDRSDisabled
	if needsDeviceConsent && (state.Consented == nil || !*state.Consented) {
		fmt.Fprintln(progress, "Requesting iCloud web access. Approve the notification on your trusted Apple device.")
		state, err = a.approvalRequest("enableDeviceConsentForPCS", map[string]any{"workflowUuid": workflow})
		if err != nil {
			return nil, err
		}
		if state.WebAllowed != nil && !*state.WebAllowed {
			return nil, ErrWebAccessDisabled
		}
		if state.DeviceNotSupported {
			return nil, ErrApprovalUnsupported
		}
		if state.Consented == nil {
			return nil, errors.New("iCloud returned an unrecognized device consent state")
		}
		if !*state.Consented && (state.NotificationSent == nil || !*state.NotificationSent) {
			return nil, errors.New("Apple did not send a device approval notification; check your trusted devices")
		}
		for !*state.Consented {
			if err := a.waitForApproval(); err != nil {
				return nil, err
			}
			state, err = a.approvalRequest("requestWebAccessState", map[string]any{"workflowUuid": workflow})
			if err != nil {
				return nil, err
			}
			if state.WebAllowed != nil && !*state.WebAllowed {
				return nil, ErrWebAccessDisabled
			}
			if state.Consented == nil {
				return nil, errors.New("iCloud returned an unrecognized device consent state")
			}
		}
	}

	fmt.Fprintf(progress, "Requesting access to %s. Keep your trusted Apple device unlocked and approve any further notification.\n", label)
	first := true
	for {
		deadline, _ := ctx.Deadline()
		state, err = a.approvalRequest("requestPCS", map[string]any{
			"appName": appName, "derivedFromUserAction": first,
			"isFinalAttempt": time.Until(deadline) <= a.approvalPollInterval,
			"workflowUuid":   workflow,
		})
		first = false
		if err != nil {
			return nil, err
		}
		if state.WebAllowed != nil && !*state.WebAllowed {
			return nil, ErrWebAccessDisabled
		}
		if state.DeviceNotSupported {
			return nil, ErrApprovalUnsupported
		}
		if state.Status == "" {
			return nil, errors.New("iCloud returned an unrecognized service approval response")
		}
		if state.Status == "success" {
			// Success requires a real database read, not merely the response flag
			// or a guessed cookie name. Apple changes its PCS cookie names.
			ok, _ := probe()
			if !ok {
				if ctx.Err() != nil {
					return nil, approvalContextError(ctx)
				}
				return nil, ErrApprovalIncomplete
			}
			a.data.Cookies = a.extractCookies()
			a.data.CreatedAt = time.Now().UTC().Format(time.RFC3339)
			if err := a.saveSession(sessionFile); err != nil {
				return nil, errors.New("cannot save the approved session; check data-directory permissions")
			}
			fmt.Fprintf(progress, "%s web access approved and saved. Approval can expire; rerun this command when required.\n", label)
			return &a.data, nil
		}
		if err := a.waitForApproval(); err != nil {
			return nil, err
		}
	}
}

func (a *Authenticator) approvalRequest(path string, body map[string]any) (approvalState, error) {
	var state approvalState
	payload, err := json.Marshal(body)
	if err != nil {
		return state, errors.New("cannot prepare iCloud web approval request")
	}
	req, err := http.NewRequestWithContext(a.ctx, http.MethodPost, SetupEndpoint+"/"+path, bytes.NewReader(payload))
	if err != nil {
		return state, errors.New("cannot prepare iCloud web approval request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", HomeEndpoint)
	req.Header.Set("Referer", HomeEndpoint+"/")
	resp, err := a.client.Do(req)
	if err != nil {
		if a.ctx.Err() != nil {
			return state, approvalContextError(a.ctx)
		}
		return state, errors.New("iCloud web approval request failed: network error")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return state, ErrAuthRequired
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return state, fmt.Errorf("iCloud web approval request failed: HTTP %d", resp.StatusCode)
	}
	payload, err = io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if a.ctx.Err() != nil {
		return state, approvalContextError(a.ctx)
	}
	if err != nil || len(payload) > 64<<10 || json.Unmarshal(payload, &state) != nil {
		return state, errors.New("iCloud returned an invalid web approval response")
	}
	return state, nil
}

func (a *Authenticator) waitForApproval() error {
	timer := time.NewTimer(a.approvalPollInterval)
	defer timer.Stop()
	select {
	case <-a.ctx.Done():
		return approvalContextError(a.ctx)
	case <-timer.C:
		return nil
	}
}

func approvalContextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrApprovalTimeout, ctx.Err())
	}
	return ctx.Err()
}
