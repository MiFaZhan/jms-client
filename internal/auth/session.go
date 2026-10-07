// Package auth performs the JumpServer dual login and MFA handling.
//
// KoKo's WebSocket terminal only accepts the jms_sessionid cookie from a
// Django form login, while the REST API needs a Bearer token — so a
// usable session needs both (DESIGN.md §3.4). The REST transport itself
// lives in internal/api.
//
// Dependency direction: auth depends on api and config.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/pquerna/otp/totp"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/config"
)

// Endpoint paths used by the dual login (DESIGN.md §3.4).
const (
	PathAPILogin     = "/api/v1/authentication/auth/"
	PathMFAChallenge = "/api/v1/authentication/mfa/challenge/"
	PathFormLogin    = "/core/auth/login/"
)

// Cookie and header names.
const (
	CookieSession = "jms_sessionid"
	CookieCSRF    = "jms_csrftoken"
	HeaderCSRF    = "X-CSRFToken"
)

// Sentinel errors. Callers should test them with errors.Is.
var (
	// ErrMFANotProvided is returned when the server demands MFA but no
	// TOTP secret is configured and no prompt callback was supplied.
	ErrMFANotProvided = errors.New("MFA required but no TOTP secret configured")

	// ErrMissingSessionCookie is returned when the form login completed
	// without yielding a jms_sessionid cookie.
	ErrMissingSessionCookie = errors.New("form login returned no session cookie")
)

// Credentials carries the secret material for one login attempt. It is
// deliberately separate from config.ServerConfig: the server entry holds
// metadata only.
type Credentials struct {
	Password  string
	OTPSecret string
}

// Session is an authenticated connection to one JumpServer address.
//
// The address is bound for the session's whole lifetime: REST calls and
// (in later milestones) the KoKo connection both derive from BaseURL
// (DESIGN.md §4.7).
type Session struct {
	Server  *config.ServerConfig
	Client  *api.Client
	BaseURL string
}

// Bearer returns the API bearer token, or "" before a successful login.
func (s *Session) Bearer() string {
	if s == nil || s.Client == nil {
		return ""
	}
	return s.Client.Bearer()
}

// SessionID returns the current jms_sessionid cookie value, or "".
func (s *Session) SessionID() string {
	if s == nil || s.Client == nil {
		return ""
	}
	return s.Client.Cookie(CookieSession)
}

// CSRF returns the current jms_csrftoken cookie value, or "".
func (s *Session) CSRF() string {
	if s == nil || s.Client == nil {
		return ""
	}
	return s.Client.Cookie(CookieCSRF)
}

// Login performs the full dual login against baseURL:
//
//  1. POST /api/v1/authentication/auth/ for the Bearer token, retrying
//     through /api/v1/authentication/mfa/challenge/ when the server asks
//     for MFA
//  2. GET then POST /core/auth/login/ for the jms_sessionid cookie
//
// otpPrompt is consulted only when the server demands MFA and no TOTP
// secret is available; it may be nil, in which case a missing secret
// surfaces as ErrMFANotProvided.
//
// Any failure during the login flow is reported as an *api.AuthError, so
// callers can distinguish "this endpoint rejects the credential" from
// "this endpoint is unreachable" (DESIGN.md §4.7).
func Login(ctx context.Context, srv *config.ServerConfig, baseURL string, creds Credentials,
	otpPrompt func() (string, error)) (*Session, error) {

	client := api.New(baseURL)
	sess := &Session{Server: srv, Client: client, BaseURL: client.BaseURL()}

	if err := apiLogin(ctx, sess, creds, otpPrompt); err != nil {
		return nil, err
	}
	if err := formLogin(ctx, sess, creds); err != nil {
		return nil, err
	}
	return sess, nil
}

// apiLogin obtains the bearer token, handling the MFA challenge.
func apiLogin(ctx context.Context, sess *Session, creds Credentials, otpPrompt func() (string, error)) error {
	payload := map[string]string{
		"username": sess.Server.Username,
		"password": creds.Password,
	}

	var data map[string]any
	if err := sess.Client.Post(ctx, PathAPILogin, payload, &data); err != nil {
		return classifyLoginError(err, "API login")
	}

	if mfaRequired(data) {
		if err := challengeMFA(ctx, sess, creds, otpPrompt); err != nil {
			return err
		}
		data = nil
		if err := sess.Client.Post(ctx, PathAPILogin, payload, &data); err != nil {
			return classifyLoginError(err, "API login after MFA")
		}
	}

	token, _ := data["token"].(string)
	if strings.TrimSpace(token) == "" {
		return &api.AuthError{StatusCode: http.StatusUnauthorized, Body: "API login failed: " + apiMessage(data)}
	}
	sess.Client.SetBearer(token)
	return nil
}

// challengeMFA completes the OTP challenge.
func challengeMFA(ctx context.Context, sess *Session, creds Credentials, otpPrompt func() (string, error)) error {
	code := ""
	if secret := strings.TrimSpace(creds.OTPSecret); secret != "" {
		generated, err := totp.GenerateCode(secret, nowFn())
		if err == nil {
			code = generated
		} else if otpPrompt == nil {
			return fmt.Errorf("configured TOTP secret is invalid: %w", err)
		}
	}
	if code == "" {
		if otpPrompt == nil {
			envName, envErr := config.EnvVarName(sess.Server.Name, config.CredOTP)
			if envErr != nil {
				envName = "<unknown kind>"
			}
			return fmt.Errorf("%w: run `jms config add %s` to store one, or set %s",
				ErrMFANotProvided, sess.Server.Name, envName)
		}
		entered, err := otpPrompt()
		if err != nil {
			return fmt.Errorf("MFA prompt: %w", err)
		}
		if strings.TrimSpace(entered) == "" {
			return fmt.Errorf("%w: no verification code provided", ErrMFANotProvided)
		}
		code = strings.TrimSpace(entered)
	}

	body := map[string]string{"type": "otp", "code": code}
	var ignored map[string]any
	if err := sess.Client.Post(ctx, PathMFAChallenge, body, &ignored); err != nil {
		return classifyLoginError(err, "MFA challenge")
	}
	return nil
}

// formLogin obtains the jms_sessionid cookie through the Django form login.
func formLogin(ctx context.Context, sess *Session, creds Credentials) error {
	loginURL := sess.BaseURL + PathFormLogin

	// Seed the csrf cookie.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loginURL, nil)
	if err != nil {
		return &api.APIError{StatusCode: 0, Body: "form login: " + err.Error()}
	}
	resp, err := sess.Client.HTTP().Do(req)
	if err != nil {
		return &api.APIError{StatusCode: 0, Body: "form login (GET): " + err.Error()}
	}
	// Drain and close so the connection returns to the keep-alive pool;
	// `jms config test` and the pool's re-login path perform several logins
	// per process.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	csrf := sess.CSRF()
	form := url.Values{}
	form.Set("username", sess.Server.Username)
	form.Set("password", creds.Password)
	form.Set("csrfmiddlewaretoken", csrf)

	req, err = http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return &api.APIError{StatusCode: 0, Body: "form login: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", loginURL)
	if csrf != "" {
		req.Header.Set(HeaderCSRF, csrf)
	}
	resp, err = sess.Client.HTTP().Do(req)
	if err != nil {
		return &api.APIError{StatusCode: 0, Body: "form login (POST): " + err.Error()}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if sess.SessionID() == "" {
		// The form login answered but yielded no session cookie: the account
		// or the endpoint configuration is the problem, not the network, so
		// this is terminal for endpoint selection.
		return fmt.Errorf("%w for %q (HTTP %d)", ErrMissingSessionCookie, sess.Server.Username, resp.StatusCode)
	}
	if cookieCSRF := sess.CSRF(); cookieCSRF != "" {
		sess.Client.SetCSRF(cookieCSRF)
	} else {
		sess.Client.SetCSRF(csrf)
	}
	return nil
}

// mfaRequired reports whether a login response asks for an MFA code.
// Field names differ across JumpServer versions, so both are checked
// (DESIGN.md §3.4).
func mfaRequired(data map[string]any) bool {
	if data == nil {
		return false
	}
	if v, ok := data["code"].(string); ok && v == "mfa_required" {
		return true
	}
	if v, ok := data["error"].(string); ok && v == "mfa_required" {
		return true
	}
	return false
}

// apiMessage extracts the human-readable message from an API error body.
func apiMessage(data map[string]any) string {
	if data == nil {
		return "no token in response"
	}
	for _, key := range []string{"detail", "msg", "error"} {
		if v, ok := data[key].(string); ok && v != "" {
			return v
		}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return "no token in response"
	}
	return string(encoded)
}

// credentialRejection statuses are the ones that mean the server saw and
// rejected the presented credential. Only these are terminal for endpoint
// selection: switching to another address cannot fix them.
var credentialRejection = map[int]bool{
	http.StatusBadRequest:          true, // JumpServer reports bad credentials with 400
	http.StatusUnauthorized:        true,
	http.StatusForbidden:           true,
	http.StatusUnprocessableEntity: true,
}

// classifyLoginError maps a failed login step onto the two classes the
// endpoint-selection policy needs (DESIGN.md §4.7 point 2):
//
//   - a credential rejection becomes *api.AuthError and is terminal - a
//     wrong password is not fixed by another address;
//   - a network-class failure (transport error, 5xx) is returned as-is so
//     the caller can fail over to the next candidate (§11.9: TCP up but the
//     service behind it broken is exactly this class).
//
// The Python reference collapses both into AuthError because its failover is
// not endpoint-aware; the Go rewrite must keep the distinction.
func classifyLoginError(err error, context string) error {
	if err == nil {
		return nil
	}
	var authErr *api.AuthError
	if errors.As(err, &authErr) {
		return err
	}
	apiErr, ok := api.AsAPIError(err)
	if ok && credentialRejection[apiErr.StatusCode] {
		return &api.AuthError{StatusCode: apiErr.StatusCode, Body: context + ": " + apiErr.Body}
	}
	if ok {
		return &api.APIError{StatusCode: apiErr.StatusCode, Body: context + ": " + apiErr.Body}
	}
	return &api.APIError{StatusCode: 0, Body: context + ": " + err.Error()}
}
