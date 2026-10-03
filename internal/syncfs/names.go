package syncfs

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"

	"agentbox/internal/syncproto"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type NamePolicy struct {
	Windows                bool `json:"windows"`
	CaseSensitive          bool `json:"case_sensitive"`
	NormalizationSensitive bool `json:"normalization_sensitive"`
	MaxPathUnits           int  `json:"max_path_units"`
}
type NameIssue struct {
	Path   string `json:"path"`
	Other  string `json:"other,omitempty"`
	Reason string `json:"reason"`
}

// CheckNames validates the entire prospective destination tree before writes.
// It preserves original names; renaming to fit Windows would corrupt the remote
// namespace and conceal conflicts when syncing back to Linux.
func CheckNames(m syncproto.Manifest, policy NamePolicy) ([]NameIssue, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	issues := []NameIssue{}
	keys := map[string]string{}
	fold := cases.Fold()
	for _, name := range Names(m) {
		if policy.Windows {
			for _, component := range strings.Split(name, "/") {
				if reason := windowsNameIssue(component); reason != "" {
					issues = append(issues, NameIssue{Path: name, Reason: reason})
					break
				}
			}
		}
		if policy.MaxPathUnits > 0 && len(utf16.Encode([]rune(name))) > policy.MaxPathUnits {
			issues = append(issues, NameIssue{Path: name, Reason: "path_too_long"})
		}
		key := name
		if !policy.NormalizationSensitive {
			key = norm.NFC.String(key)
		}
		if !policy.CaseSensitive {
			key = fold.String(key)
		}
		if other, ok := keys[key]; ok && other != name {
			issues = append(issues, NameIssue{Path: name, Other: other, Reason: "name_collision"})
		} else {
			keys[key] = name
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Path != issues[j].Path {
			return issues[i].Path < issues[j].Path
		}
		return issues[i].Reason < issues[j].Reason
	})
	return issues, nil
}
func windowsNameIssue(name string) string {
	if strings.ContainsAny(name, `<>:"\|?*`) || strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) }) {
		return "windows_invalid_character"
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return "windows_trailing_dot_or_space"
	}
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CONIN$" || base == "CONOUT$" {
		return "windows_reserved_name"
	}
	for _, prefix := range []string{"COM", "LPT"} {
		for _, digit := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
			if base == prefix+digit {
				return "windows_reserved_name"
			}
		}
	}
	if len(utf16.Encode([]rune(name))) > 255 {
		return "component_too_long"
	}
	return ""
}

func RequireNames(m syncproto.Manifest, policy NamePolicy) error {
	issues, err := CheckNames(m, policy)
	if err != nil {
		return err
	}
	if len(issues) > 0 {
		return fmt.Errorf("%w: %s (%s)", ErrUnsafe, issues[0].Path, issues[0].Reason)
	}
	return nil
}
