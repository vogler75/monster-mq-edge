package scripting

import (
	_ "embed"
	"strings"
)

//go:embed docs/starlark.md
var starlarkDocs string

//go:embed docs/starlark_skill.md
var starlarkSkill string

// GetDocumentation returns the full Markdown API documentation for the specified scripting language.
// If language is empty or unrecognized, returns Starlark documentation.
func GetDocumentation(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "starlark", "star", "python", "py", "":
		return starlarkDocs
	default:
		return starlarkDocs
	}
}

// GetSkill returns the AI Skill markdown instructions for the specified scripting language.
// If language is empty or unrecognized, returns Starlark skill.
func GetSkill(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "starlark", "star", "python", "py", "":
		return starlarkSkill
	default:
		return starlarkSkill
	}
}
