package auth

import (
	"errors"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// nowFn is indirected so tests can pin the TOTP timestamp.
var nowFn = time.Now

// GenerateCode returns the current TOTP code for a base32 secret.
//
// It is exported so the CLI can validate a secret at entry time
// (`jms config add`) and so tests can assert against a known code.
func GenerateCode(secret string) (string, error) {
	return totp.GenerateCode(secret, nowFn())
}

// ValidateSecret reports whether secret is a usable base32 TOTP secret.
//
// An empty or whitespace-only secret is rejected here rather than handed to
// pquerna/otp, which happily derives a code from the empty string: that
// would let a missing secret silently bypass the interactive prompt and
// send a bogus challenge (DESIGN.md 3.4 / 7.3).
func ValidateSecret(secret string) error {
	if strings.TrimSpace(secret) == "" {
		return errors.New("TOTP secret is empty")
	}
	_, err := totp.GenerateCode(secret, nowFn())
	return err
}

// KeyOptions returns the TOTP parameters used when provisioning a secret.
// Retained for the `jms config add --provision-otp` flow (post-M1).
func KeyOptions() totp.ValidateOpts {
	return totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	}
}
