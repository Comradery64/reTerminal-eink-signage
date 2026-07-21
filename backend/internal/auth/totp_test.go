package auth

import (
	"strings"
	"testing"
	"time"
)

func TestTOTPCodeIsSixDigitsAndDeterministic(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1_700_000_000, 0)

	code, err := TOTPCode(secret, now)
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	if len(code) != 6 {
		t.Fatalf("TOTPCode returned %q, want 6 digits", code)
	}
	again, err := TOTPCode(secret, now)
	if err != nil || again != code {
		t.Fatalf("TOTPCode must be deterministic for the same secret/time, got %q then %q", code, again)
	}
}

func TestVerifyTOTPAcceptsCurrentCode(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	code, err := TOTPCode(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyTOTP(secret, code, now) {
		t.Fatal("the current code must verify")
	}
}

func TestVerifyTOTPToleratesOneStepClockDrift(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	prevStepCode, err := TOTPCode(secret, now.Add(-totpStep))
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyTOTP(secret, prevStepCode, now) {
		t.Fatal("a code from one step ago must still verify (clock drift tolerance)")
	}

	tooOld, err := TOTPCode(secret, now.Add(-3*totpStep))
	if err != nil {
		t.Fatal(err)
	}
	if VerifyTOTP(secret, tooOld, now) {
		t.Fatal("a code from three steps ago must not verify")
	}
}

func TestVerifyTOTPRejectsAnotherAccountsSecret(t *testing.T) {
	secretA, secretB := NewTOTPSecret(), NewTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	codeA, err := TOTPCode(secretA, now)
	if err != nil {
		t.Fatal(err)
	}
	if VerifyTOTP(secretB, codeA, now) {
		t.Fatal("a code generated for one secret must not verify against a different secret")
	}
}

func TestTOTPURIContainsSecretAndAccount(t *testing.T) {
	uri := TOTPURI("MeetingDisplayFleet", "alice", "ABCD1234")
	if !strings.Contains(uri, "secret=ABCD1234") || !strings.Contains(uri, "alice") || !strings.Contains(uri, "MeetingDisplayFleet") {
		t.Fatalf("otpauth URI missing expected parts: %s", uri)
	}
}
