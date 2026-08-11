package credential

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	keyring "github.com/zalando/go-keyring"
)

const service = "alt-v1"

var (
	ErrNotFound = errors.New("credential not found")
	ErrInvalid  = errors.New("credential is invalid")
)

type Source string

const (
	SourceEnvironment Source = "environment"
	SourceKeyring     Source = "OS keyring"
	SourcePrivateFile Source = "private credential file"
)

type keyringBackend interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type systemKeyring struct{}

func (systemKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}

func (systemKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}

func (systemKeyring) Delete(service, user string) error {
	return keyring.Delete(service, user)
}

type Store struct {
	dataDir string
	keyring keyringBackend
}

func NewStore(dataDir string) Store {
	return Store{dataDir: dataDir, keyring: systemKeyring{}}
}

func (s Store) Resolve(provider, environmentVariable string) (string, error) {
	value, _, err := s.Lookup(provider, environmentVariable)
	return value, err
}

func (s Store) Lookup(provider, environmentVariable string) (string, Source, error) {
	if environmentVariable != "" {
		if value := strings.TrimSpace(os.Getenv(environmentVariable)); value != "" {
			value, err := validCredential(provider, value)
			if err != nil {
				return "", "", err
			}
			return value, SourceEnvironment, nil
		}
	}

	// A fallback file represents a newer write made while the desktop keyring
	// was unavailable, so it must take precedence over a potentially stale
	// keyring value. A successful future Set removes the fallback.
	value, err := s.readPrivateFile(provider)
	if err == nil {
		value, err = validCredential(provider, value)
		if err != nil {
			return "", "", err
		}
		return value, SourcePrivateFile, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}

	value, err = s.backend().Get(service, account(provider))
	if err == nil && strings.TrimSpace(value) != "" {
		value, err = validCredential(provider, value)
		if err != nil {
			return "", "", err
		}
		return value, SourceKeyring, nil
	}
	if errors.Is(err, keyring.ErrNotFound) || err != nil || strings.TrimSpace(value) == "" {
		return "", "", fmt.Errorf("%w: set %s or run `alt auth set`", ErrNotFound, environmentVariable)
	}
	return "", "", fmt.Errorf("read %s credential", provider)
}

func (s Store) Set(provider, secret string) (Source, error) {
	var err error
	secret, err = validCredential(provider, secret)
	if err != nil {
		return "", err
	}

	if err := s.backend().Set(service, account(provider), secret); err == nil {
		if err := s.removePrivateFile(provider); err != nil {
			return "", err
		}
		return SourceKeyring, nil
	}

	if err := s.writePrivateFile(provider, secret); err != nil {
		return "", fmt.Errorf("OS keyring unavailable and private credential fallback failed: %w", err)
	}
	return SourcePrivateFile, nil
}

func validCredential(provider, secret string) (string, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", fmt.Errorf("credential cannot be empty")
	}
	for _, character := range secret {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("%w: stored %s credential contains control characters; run `alt auth set %s` to replace it", ErrInvalid, provider, provider)
		}
	}
	return secret, nil
}

func (s Store) Delete(provider string) error {
	// Delete both because a keyring can become available again after a fallback
	// credential was written.
	_ = s.backend().Delete(service, account(provider))
	return s.removePrivateFile(provider)
}

func (s Store) backend() keyringBackend {
	if s.keyring == nil {
		return systemKeyring{}
	}
	return s.keyring
}

func (s Store) privateFilePath(provider string) (string, error) {
	if strings.TrimSpace(s.dataDir) == "" {
		return "", fmt.Errorf("credential data directory is not configured")
	}
	for _, character := range provider {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return "", fmt.Errorf("invalid provider name")
	}
	if provider == "" || provider == "." || provider == ".." {
		return "", fmt.Errorf("invalid provider name")
	}
	return filepath.Join(s.dataDir, "credentials", provider), nil
}

func (s Store) readPrivateFile(provider string) (string, error) {
	path, err := s.privateFilePath(provider)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("credential file permissions are too broad; run `chmod 600 %s`", path)
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read private credential file: %w", err)
	}
	if strings.TrimSpace(string(value)) == "" {
		return "", os.ErrNotExist
	}
	return strings.TrimSpace(string(value)), nil
}

func (s Store) writePrivateFile(provider, secret string) error {
	path, err := s.privateFilePath(provider)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("protect credential directory: %w", err)
	}

	file, err := os.CreateTemp(directory, ".credential-*")
	if err != nil {
		return fmt.Errorf("create temporary credential file: %w", err)
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)

	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("protect temporary credential file: %w", err)
	}
	if _, err := file.WriteString(secret); err != nil {
		file.Close()
		return fmt.Errorf("write temporary credential file: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync temporary credential file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary credential file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install private credential file: %w", err)
	}
	return nil
}

func (s Store) removePrivateFile(provider string) error {
	path, err := s.privateFilePath(provider)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove private credential file: %w", err)
	}
	return nil
}

func account(provider string) string {
	return "provider:" + provider
}
