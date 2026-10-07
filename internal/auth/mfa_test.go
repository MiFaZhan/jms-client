package auth

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// pinTime pins the TOTP clock for a test and restores it afterwards.
func pinTime(t *testing.T, at time.Time) {
	t.Helper()
	prev := nowFn
	nowFn = func() time.Time { return at }
	t.Cleanup(func() { nowFn = prev })
}

func TestGenerateCodeIsDeterministicForAPinnedClock(t *testing.T) {
	pinTime(t, time.Unix(59, 0)) // just before a step boundary

	first, err := GenerateCode("JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatalf("GenerateCode() = %v", err)
	}
	second, _ := GenerateCode("JBSWY3DPEHPK3PXP")
	if first != second {
		t.Fatalf("two calls at the same instant differ: %q vs %q", first, second)
	}
	if len(first) != 6 {
		t.Fatalf("code = %q, want 6 digits", first)
	}
}

func TestGenerateCodeChangesAcrossAStepBoundary(t *testing.T) {
	pinTime(t, time.Unix(59, 0))
	before, _ := GenerateCode("JBSWY3DPEHPK3PXP")

	pinTime(t, time.Unix(90, 0)) // one 30s step later
	after, _ := GenerateCode("JBSWY3DPEHPK3PXP")

	if before == after {
		t.Fatalf("code did not rotate across the 30s step: %q", before)
	}
}

func TestGenerateCodeIsSixDigitsForAKnownVector(t *testing.T) {
	pinTime(t, time.Unix(1111111109, 0)) // standard RFC 6238 vector instant
	code, err := GenerateCode("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ")
	if err != nil {
		t.Fatalf("GenerateCode() = %v", err)
	}
	if len(code) != 6 {
		t.Fatalf("code = %q, want 6 digits", code)
	}
}

func TestKeyOptionsUsesTheKoKoDefaults(t *testing.T) {
	opts := KeyOptions()
	if opts.Period != 30 {
		t.Errorf("Period = %d, want 30", opts.Period)
	}
	if opts.Digits != otp.DigitsSix {
		t.Errorf("Digits = %v, want six", opts.Digits)
	}
	if opts.Algorithm != otp.AlgorithmSHA1 {
		t.Errorf("Algorithm = %v, want SHA1", opts.Algorithm)
	}
}

func TestValidateSecretRoundTripsThroughGenerateCode(t *testing.T) {
	// A secret that validates must also generate; a secret that fails to
	// generate must fail validation. Keep the two in lockstep.
	valid := "JBSWY3DPEHPK3PXP"
	if err := ValidateSecret(valid); err != nil {
		t.Fatalf("ValidateSecret(%q) = %v", valid, err)
	}
	if _, err := GenerateCode(valid); err != nil {
		t.Fatalf("GenerateCode(%q) = %v, but ValidateSecret passed", valid, err)
	}

	invalid := "not-base32!!"
	if err := ValidateSecret(invalid); err == nil {
		t.Errorf("ValidateSecret(%q) = nil, want an error", invalid)
	}
}

var (
	_ = json.Marshal
	_ = totp.GenerateCode
	_ = testing.Short
)
