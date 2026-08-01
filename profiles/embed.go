// Package profiles contains the Team Profiles shipped with ALT.
package profiles

import _ "embed"

// Engineering is the default general engineering Team Profile.
//
//go:embed engineering.yaml
var Engineering []byte
