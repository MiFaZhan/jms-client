package config

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestEnvVarName(t *testing.T) {
	tests := []struct {
		name  string
		alias string
		kind  string
		want  string
	}{
		{name: "password hyphen", alias: "bastion-wan", kind: CredPassword, want: "JMS_PASSWORD_BASTION_WAN"},
		{name: "otp hyphen", alias: "bastion-wan", kind: CredOTP, want: "JMS_OTP_BASTION_WAN"},
		{name: "password plain", alias: "bastion", kind: CredPassword, want: "JMS_PASSWORD_BASTION"},
		{name: "otp plain", alias: "bastion", kind: CredOTP, want: "JMS_OTP_BASTION"},
		{name: "already uppercase", alias: "BASTION", kind: CredPassword, want: "JMS_PASSWORD_BASTION"},
		{name: "mixed case hyphen", alias: "bastion-wan", kind: CredOTP, want: "JMS_OTP_BASTION_WAN"},
		{name: "multiple hyphens", alias: "a-b-c", kind: CredPassword, want: "JMS_PASSWORD_A_B_C"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EnvVarName(tt.alias, tt.kind)
			if err != nil {
				t.Fatalf("EnvVarName(%q, %q) = %v", tt.alias, tt.kind, err)
			}
			if got != tt.want {
				t.Fatalf("EnvVarName(%q, %q) = %q, want %q", tt.alias, tt.kind, got, tt.want)
			}
		})
	}
}

func TestEnvVarNameRejectsUnknownKind(t *testing.T) {
	// The keyring path rejects an unknown kind; the env path must agree,
	// otherwise a mistyped kind silently reads the password variable.
	for _, kind := range []string{"totp", "otp_secret", "", "PASSWORD"} {
		if _, err := EnvVarName("bastion", kind); err == nil {
			t.Errorf("EnvVarName(bastion, %q) = nil error, want one", kind)
		}
	}
	// The known kinds must still work.
	for _, kind := range []string{CredPassword, CredOTP} {
		if _, err := EnvVarName("bastion", kind); err != nil {
			t.Errorf("EnvVarName(bastion, %q) = %v, want nil", kind, err)
		}
	}
}

func TestEnvStoreGet(t *testing.T) {
	t.Run("password", func(t *testing.T) {
		t.Setenv("JMS_PASSWORD_BASTION_WAN", "s3cret")
		got, err := NewEnvStore().Get("bastion-wan", CredPassword)
		if err != nil || got != "s3cret" {
			t.Fatalf("Get() = (%q, %v), want (s3cret, nil)", got, err)
		}
	})
	t.Run("otp", func(t *testing.T) {
		t.Setenv("JMS_OTP_BASTION", "ABCDEF")
		got, err := NewEnvStore().Get("bastion", CredOTP)
		if err != nil || got != "ABCDEF" {
			t.Fatalf("Get() = (%q, %v), want (ABCDEF, nil)", got, err)
		}
	})
	t.Run("missing variable", func(t *testing.T) {
		t.Setenv("JMS_PASSWORD_ABSENT", "")
		_, err := NewEnvStore().Get("absent", CredPassword)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get() = %v, want ErrNotFound", err)
		}
	})
	t.Run("empty variable counts as absent", func(t *testing.T) {
		t.Setenv("JMS_PASSWORD_BLANK", "")
		_, err := NewEnvStore().Get("blank", CredPassword)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get() = %v, want ErrNotFound", err)
		}
	})
}

func TestEnvStoreIsReadOnly(t *testing.T) {
	err := NewEnvStore().Set("bastion", CredPassword, "value")
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Set() = %v, want ErrReadOnly", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "JMS_PASSWORD_BASTION") {
		t.Errorf("Set() error %q does not name the variable the user should set", msg)
	}
}

func TestEnvStoreDeleteIsNoop(t *testing.T) {
	if err := NewEnvStore().Delete("bastion"); err != nil {
		t.Fatalf("Delete() = %v, want nil", err)
	}
}

func TestMemoryStoreRoundTrip(t *testing.T) {
	store := NewMemoryStore()
	if _, err := store.Get("bastion", CredPassword); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() on an empty store = %v, want ErrNotFound", err)
	}
	if err := store.Set("bastion", CredPassword, "pw"); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if err := store.Set("bastion", CredOTP, "otp"); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if got, err := store.Get("bastion", CredPassword); err != nil || got != "pw" {
		t.Fatalf("Get(password) = (%q, %v), want (pw, nil)", got, err)
	}
	if got, err := store.Get("bastion", CredOTP); err != nil || got != "otp" {
		t.Fatalf("Get(otp) = (%q, %v), want (otp, nil)", got, err)
	}
}

func TestMemoryStoreDeleteRemovesBothKinds(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Set("bastion", CredPassword, "pw"); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if err := store.Set("bastion", CredOTP, "otp"); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if err := store.Delete("bastion"); err != nil {
		t.Fatalf("Delete() = %v, want nil", err)
	}
	if _, err := store.Get("bastion", CredPassword); !errors.Is(err, ErrNotFound) {
		t.Errorf("password survived Delete: %v", err)
	}
	if _, err := store.Get("bastion", CredOTP); !errors.Is(err, ErrNotFound) {
		t.Errorf("otp survived Delete: %v", err)
	}
}

func TestMemoryStoreDeleteAbsentAliasIsNotAnError(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Delete("ghost"); err != nil {
		t.Fatalf("Delete(ghost) = %v, want nil", err)
	}
}

// Each goroutine owns a distinct alias: a credential store guarantees that
// individual operations are synchronized, not that a caller's Set->Get sequence
// is isolated from another goroutine's Delete. Sharing one alias would race
// against that (non-)guarantee and fail intermittently.
func TestMemoryStoreIsConcurrencySafe(t *testing.T) {
	store := NewMemoryStore()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		alias := fmt.Sprintf("alias-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Set(alias, CredPassword, "pw"); err != nil {
				t.Errorf("Set() = %v", err)
			}
			if got, err := store.Get(alias, CredPassword); err != nil || got != "pw" {
				t.Errorf("Get() = (%q, %v), want (%q, nil)", got, err, "pw")
			}
			if _, err := store.Get(alias, CredOTP); err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("Get() = %v", err)
			}
			if err := store.Delete(alias); err != nil {
				t.Errorf("Delete() = %v", err)
			}
		}()
	}
	wg.Wait()
	// Every alias was deleted exactly once.
	for i := 0; i < 32; i++ {
		if _, err := store.Get(fmt.Sprintf("alias-%d", i), CredPassword); !errors.Is(err, ErrNotFound) {
			t.Errorf("alias-%d survived Delete: %v", i, err)
		}
	}
}

func TestChainPrefersEarlierStore(t *testing.T) {
	t.Run("env wins over memory", func(t *testing.T) {
		t.Setenv("JMS_PASSWORD_BASTION", "from-env")
		mem := NewMemoryStore()
		if err := mem.Set("bastion", CredPassword, "from-memory"); err != nil {
			t.Fatalf("Set() = %v", err)
		}
		got, err := Chain(NewEnvStore(), mem).Get("bastion", CredPassword)
		if err != nil || got != "from-env" {
			t.Fatalf("Get() = (%q, %v), want (from-env, nil)", got, err)
		}
	})
	t.Run("memory is used when the env var is absent", func(t *testing.T) {
		t.Setenv("JMS_PASSWORD_BASTION", "")
		mem := NewMemoryStore()
		if err := mem.Set("bastion", CredPassword, "from-memory"); err != nil {
			t.Fatalf("Set() = %v", err)
		}
		got, err := Chain(NewEnvStore(), mem).Get("bastion", CredPassword)
		if err != nil || got != "from-memory" {
			t.Fatalf("Get() = (%q, %v), want (from-memory, nil)", got, err)
		}
	})
	t.Run("neither store has the credential", func(t *testing.T) {
		t.Setenv("JMS_PASSWORD_BASTION", "")
		_, err := Chain(NewEnvStore(), NewMemoryStore()).Get("bastion", CredPassword)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get() = %v, want ErrNotFound", err)
		}
	})
}

func TestChainSetFallsThroughReadOnlyStore(t *testing.T) {
	mem := NewMemoryStore()
	chain := Chain(NewEnvStore(), mem)
	if err := chain.Set("bastion", CredPassword, "pw"); err != nil {
		t.Fatalf("Set() = %v, want the write to reach the memory store", err)
	}
	got, err := mem.Get("bastion", CredPassword)
	if err != nil || got != "pw" {
		t.Fatalf("memory store Get() = (%q, %v), want (pw, nil)", got, err)
	}
}

func TestChainSetWithoutWritableStore(t *testing.T) {
	err := Chain(NewEnvStore()).Set("bastion", CredPassword, "pw")
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Set() = %v, want ErrReadOnly", err)
	}
}

func TestChainSetReportsRealFailure(t *testing.T) {
	boom := errors.New("keychain is locked")
	err := Chain(NewEnvStore(), &stubStore{setErr: boom}).Set("bastion", CredPassword, "pw")
	if !errors.Is(err, boom) {
		t.Fatalf("Set() = %v, want the backing-store failure to surface", err)
	}
}

func TestChainGetReportsRealFailure(t *testing.T) {
	boom := errors.New("keychain is locked")
	_, err := Chain(&stubStore{getErr: boom}, NewMemoryStore()).Get("bastion", CredPassword)
	if !errors.Is(err, boom) {
		t.Fatalf("Get() = %v, want the backing-store failure to surface", err)
	}
}

func TestChainToleratesNilStores(t *testing.T) {
	mem := NewMemoryStore()
	if err := mem.Set("bastion", CredPassword, "pw"); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	got, err := Chain(nil, NewEnvStore(), nil, mem).Get("bastion", CredPassword)
	if err != nil || got != "pw" {
		t.Fatalf("Get() = (%q, %v), want (pw, nil)", got, err)
	}
}

func TestChainDeleteToleratesReadOnlyStore(t *testing.T) {
	mem := NewMemoryStore()
	if err := mem.Set("bastion", CredPassword, "pw"); err != nil {
		t.Fatalf("Set() = %v", err)
	}
	if err := Chain(NewEnvStore(), mem).Delete("bastion"); err != nil {
		t.Fatalf("Delete() = %v, want nil", err)
	}
	if _, err := mem.Get("bastion", CredPassword); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete() did not reach the memory store: %v", err)
	}
}

func TestKeyringStoreRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping keyring test in short mode")
	}
	// keyring.MockInit replaces the provider with an in-memory fake, so the
	// test never touches the real OS credential store.
	keyring.MockInit()

	store := NewKeyringStore()
	alias := "config-test-keyring"

	if _, err := store.Get(alias, CredPassword); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() on an empty store = %v, want ErrNotFound", err)
	}
	if err := store.Set(alias, CredPassword, "pw"); err != nil {
		t.Fatalf("Set(password) = %v", err)
	}
	if err := store.Set(alias, CredOTP, "otp"); err != nil {
		t.Fatalf("Set(otp) = %v", err)
	}
	if got, err := store.Get(alias, CredPassword); err != nil || got != "pw" {
		t.Fatalf("Get(password) = (%q, %v), want (pw, nil)", got, err)
	}
	if got, err := store.Get(alias, CredOTP); err != nil || got != "otp" {
		t.Fatalf("Get(otp) = (%q, %v), want (otp, nil)", got, err)
	}
	if err := store.Delete(alias); err != nil {
		t.Fatalf("Delete() = %v, want nil", err)
	}
	if _, err := store.Get(alias, CredPassword); !errors.Is(err, ErrNotFound) {
		t.Errorf("password survived Delete: %v", err)
	}
	if _, err := store.Get(alias, CredOTP); !errors.Is(err, ErrNotFound) {
		t.Errorf("otp survived Delete: %v", err)
	}
}

func TestKeyringDeleteToleratesAbsentEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping keyring test in short mode")
	}
	keyring.MockInit()
	if err := NewKeyringStore().Delete("config-test-never-written"); err != nil {
		t.Fatalf("Delete() = %v, want nil for an absent alias", err)
	}
}

func TestKeyringStoreRejectsUnknownKind(t *testing.T) {
	// An unknown kind must not silently alias onto the password entry.
	if _, err := NewKeyringStore().Get("bastion", "totp"); err == nil {
		t.Fatal("Get() with an unknown kind = nil, want an error")
	}
}

// stubStore is a CredentialStore whose failures can be injected, so the
// chain's error propagation is exercised without a real backend.
type stubStore struct {
	getErr    error
	setErr    error
	deleteErr error
}

func (s *stubStore) Get(alias, kind string) (string, error) {
	if s.getErr != nil {
		return "", s.getErr
	}
	return "", ErrNotFound
}

func (s *stubStore) Set(alias, kind, value string) error {
	return s.setErr
}

func (s *stubStore) Delete(alias string) error {
	return s.deleteErr
}
