package research

import (
	"fmt"
	"strings"
)

type Provider string

const (
	ProviderExa    Provider = "exa"
	ProviderLinkup Provider = "linkup"
)

type Connection struct {
	ID                    Provider
	Name                  string
	CredentialEnvironment string
}

var connections = []Connection{
	{ID: ProviderExa, Name: "Exa", CredentialEnvironment: "EXA_API_KEY"},
	{ID: ProviderLinkup, Name: "Linkup", CredentialEnvironment: "LINKUP_API_KEY"},
}

func Connections() []Connection {
	return append([]Connection(nil), connections...)
}

func ParseProvider(raw string) (Provider, error) {
	value := Provider(strings.ToLower(strings.TrimSpace(raw)))
	for _, connection := range connections {
		if connection.ID == value {
			return value, nil
		}
	}
	return "", fmt.Errorf("unsupported research provider %q", strings.TrimSpace(raw))
}

func Lookup(value Provider) (Connection, bool) {
	for _, connection := range connections {
		if connection.ID == value {
			return connection, true
		}
	}
	return Connection{}, false
}
