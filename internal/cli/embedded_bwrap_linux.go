//go:build linux && amd64

package cli

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

const embeddedBubblewrapSHA256 = "5724ad6485dc04210a5c8c8b74e20862eece00fab510a0ca91ea44a11e6ed167"

// Bubblewrap 0.12.0, built from containers/bubblewrap commit
// 2f55bae38468d0c50cf5df87b1e481e882b63acb with generic x86-64 code
// generation. See internal/licenses/THIRD_PARTY_NOTICES.txt.
//
//go:embed assets/bwrap-linux-amd64
var embeddedBubblewrap []byte

func installEmbeddedBubblewrap(privateTemp string) (string, error) {
	digest := sha256.Sum256(embeddedBubblewrap)
	if hex.EncodeToString(digest[:]) != embeddedBubblewrapSHA256 {
		return "", fmt.Errorf("embedded Bubblewrap digest mismatch")
	}
	path := filepath.Join(privateTemp, fmt.Sprintf("bwrap-%d", os.Getpid()))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return "", fmt.Errorf("create embedded Bubblewrap: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}
	if _, err := file.Write(embeddedBubblewrap); err != nil {
		cleanup()
		return "", fmt.Errorf("write embedded Bubblewrap: %w", err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("sync embedded Bubblewrap: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close embedded Bubblewrap: %w", err)
	}
	return path, nil
}
