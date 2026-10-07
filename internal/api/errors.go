package api

import (
	"errors"
	"fmt"
)

// AuthError reports a rejected or expired credential (HTTP 401).
//
// The endpoint-selection policy treats AuthError as terminal: switching
// to another address cannot fix a credential problem (DESIGN.md §4.7).
//
// Body is a truncated copy of the response body and never contains
// request headers, so a bearer token cannot leak through an error.
type AuthError struct {
	StatusCode int
	Body       string
}

func (e *AuthError) Error() string {
	if e == nil {
		return "authentication error"
	}
	if e.Body == "" {
		return fmt.Sprintf("authentication error (HTTP %d)", e.StatusCode)
	}
	return fmt.Sprintf("authentication error (HTTP %d): %s", e.StatusCode, e.Body)
}

// APIError reports any other API failure.
//
// StatusCode is the HTTP status of the failed response, or 0 when the
// request never produced a response (transport failure, timeout,
// cancellation).
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	if e == nil {
		return "API error"
	}
	if e.StatusCode == 0 {
		return fmt.Sprintf("API transport failure: %s", e.Body)
	}
	return fmt.Sprintf("API error (HTTP %d): %s", e.StatusCode, e.Body)
}

// AsAuthError extracts an *AuthError from err, unwrapping as needed.
func AsAuthError(err error) (*AuthError, bool) {
	var target *AuthError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// AsAPIError extracts an *APIError from err, unwrapping as needed.
func AsAPIError(err error) (*APIError, bool) {
	var target *APIError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// IsTransportFailure reports whether err is an APIError with
// StatusCode == 0, i.e. no response was received.
func IsTransportFailure(err error) bool {
	apiErr, ok := AsAPIError(err)
	return ok && apiErr.StatusCode == 0
}
