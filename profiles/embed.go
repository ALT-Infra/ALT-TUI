// Package profiles contains the Team Profiles shipped with ALT.
package profiles

import _ "embed"

// Engineering is the bundled free engineering Team Profile.
//
//go:embed engineering.yaml
var Engineering []byte

// Free is the bundled general-purpose free Team Profile.
//
//go:embed free.yaml
var Free []byte
