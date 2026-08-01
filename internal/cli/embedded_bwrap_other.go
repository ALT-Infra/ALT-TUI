//go:build !linux || !amd64

package cli

import "fmt"

func installEmbeddedBubblewrap(_ string) (string, error) {
	return "", fmt.Errorf("embedded Bubblewrap is available only in the Linux amd64 build")
}
