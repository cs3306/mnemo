// Package assets embeds the example persona and profile so the binary is self-contained.
package assets

import _ "embed"

//go:embed persona.example.md
var PersonaExample string

//go:embed profile.example.md
var ProfileExample string
