// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/basmulder03/repo-keeper/internal/clock"
	"github.com/basmulder03/repo-keeper/internal/httpx"
	"github.com/basmulder03/repo-keeper/internal/secrets"
)

// DeviceCode is what the user must enter at VerificationURI.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// Device runs the OAuth device flow (no client secret needed, so nothing sensitive ships in the binary).
type Device struct {
	HTTP     *httpx.Client
	Clock    clock.Clock
	WebBase  string // default https://github.com (GHES: https://host)
	ClientID string
	Scope    string // empty for GitHub Apps (permissions come from the app)
}

func (d *Device) base() string {
	if d.WebBase == "" {
		return "https://github.com"
	}
	return strings.TrimRight(d.WebBase, "/")
}

func (d *Device) post(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base()+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := d.HTTP.Do(ctx, req)
	if err != nil {
		return err
	}
	if resp.Status != http.StatusOK {
		return &APIError{Status: resp.Status, Message: message(resp.Body)}
	}
	return json.Unmarshal(resp.Body, out)
}

// Start requests a device code.
func (d *Device) Start(ctx context.Context) (DeviceCode, error) {
	form := url.Values{"client_id": {d.ClientID}}
	if d.Scope != "" {
		form.Set("scope", d.Scope)
	}
	var dc DeviceCode
	if err := d.post(ctx, "/login/device/code", form, &dc); err != nil {
		return dc, err
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return dc, errors.New("github: device flow response incomplete (is the client id valid and device flow enabled?)")
	}
	if dc.Interval < 1 {
		dc.Interval = 5
	}
	return dc, nil
}

// Poll waits for the user to authorise, honouring the server's interval and slow_down requests.
func (d *Device) Poll(ctx context.Context, dc DeviceCode) (secrets.Token, error) {
	interval := time.Duration(dc.Interval) * time.Second
	deadline := d.Clock.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	for {
		select {
		case <-ctx.Done():
			return secrets.Token{}, ctx.Err()
		case <-d.Clock.After(interval):
		}
		if dc.ExpiresIn > 0 && d.Clock.Now().After(deadline) {
			return secrets.Token{}, errors.New("github: device code expired; run the login again")
		}
		var r struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
			Description string `json:"error_description"`
			Interval    int    `json:"interval"`
		}
		err := d.post(ctx, "/login/oauth/access_token", url.Values{
			"client_id": {d.ClientID}, "device_code": {dc.DeviceCode},
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
		}, &r)
		if err != nil {
			return secrets.Token{}, err
		}
		switch r.Error {
		case "":
			if r.AccessToken == "" {
				return secrets.Token{}, errors.New("github: empty access token in response")
			}
			return secrets.New(r.AccessToken), nil
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
			if r.Interval > 0 {
				interval = time.Duration(r.Interval) * time.Second
			}
		case "expired_token":
			return secrets.Token{}, errors.New("github: device code expired; run the login again")
		case "access_denied":
			return secrets.Token{}, errors.New("github: authorisation denied in the browser")
		default:
			return secrets.Token{}, fmt.Errorf("github: device flow error %q: %s", r.Error, r.Description)
		}
	}
}
