package credential

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	keyring "github.com/zalando/go-keyring"
)

type unavailableKeyring struct{}

func (unavailableKeyring) Get(string, string) (string, error) {
	return "", errors.New("keyring unavailable")
}

func (unavailableKeyring) Set(string, string, string) error {
	return errors.New("keyring unavailable")
}

func (unavailableKeyring) Delete(string, string) error {
	return errors.New("keyring unavailable")
}

type memoryKeyring struct {
	value string
}

func (m *memoryKeyring) Get(string, string) (string, error) {
	if m.value == "" {
		return "", keyring.ErrNotFound
	}
	return m.value, nil
}

func (m *memoryKeyring) Set(_, _, value string) error {
	m.value = value
	return nil
}

func (m *memoryKeyring) Delete(string, string) error {
	m.value = ""
	return nil
}

func TestPrivateFileFallbackRoundTrip(t *testing.T) {
	store := Store{dataDir: t.TempDir(), keyring: unavailableKeyring{}}

	source, err := store.Set("opencode", "test-secret")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if source != SourcePrivateFile {
		t.Fatalf("Set source = %q, want %q", source, SourcePrivateFile)
	}

	path, err := store.privateFilePath("opencode")
	if err != nil {
		t.Fatalf("privateFilePath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("credential permissions = %o, want 600", permissions)
	}

	value, source, err := store.Lookup("opencode", "")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if value != "test-secret" || source != SourcePrivateFile {
		t.Fatalf("Lookup = (%q, %q), want private-file credential", value, source)
	}

	if err := store.Delete("opencode"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fallback file remains after Delete: %v", err)
	}
}

func TestSuccessfulKeyringWriteRemovesStaleFallback(t *testing.T) {
	dataDir := t.TempDir()
	failingStore := Store{dataDir: dataDir, keyring: unavailableKeyring{}}
	if _, err := failingStore.Set("opencode", "stale-secret"); err != nil {
		t.Fatalf("seed fallback: %v", err)
	}

	backend := &memoryKeyring{}
	store := Store{dataDir: dataDir, keyring: backend}
	source, err := store.Set("opencode", "current-secret")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if source != SourceKeyring {
		t.Fatalf("Set source = %q, want %q", source, SourceKeyring)
	}

	path := filepath.Join(dataDir, "credentials", "opencode")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale fallback remains after keyring write: %v", err)
	}
	value, source, err := store.Lookup("opencode", "")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if value != "current-secret" || source != SourceKeyring {
		t.Fatalf("Lookup = (%q, %q), want current keyring credential", value, source)
	}
}

func TestEnvironmentCredentialTakesPrecedence(t *testing.T) {
	t.Setenv("ALT_TEST_API_KEY", "environment-secret")
	store := Store{dataDir: t.TempDir(), keyring: unavailableKeyring{}}
	if _, err := store.Set("opencode", "file-secret"); err != nil {
		t.Fatalf("seed fallback: %v", err)
	}

	value, source, err := store.Lookup("opencode", "ALT_TEST_API_KEY")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if value != "environment-secret" || source != SourceEnvironment {
		t.Fatalf("Lookup = (%q, %q), want environment credential", value, source)
	}
}

func TestCredentialsRejectEmbeddedControlCharactersBeforeHTTPUse(t *testing.T) {
	store := Store{dataDir: t.TempDir(), keyring: unavailableKeyring{}}
	if _, err := store.Set("exa", "exa-key\x1b"); err == nil {
		t.Fatal("Set accepted a credential that cannot be placed in an HTTP header")
	}

	path, err := store.privateFilePath("exa")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Lookup("exa", ""); err == nil {
		t.Fatal("Lookup exposed an invalid stored credential to a provider client")
	} else if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Lookup error = %v, want ErrInvalid", err)
	}
}
