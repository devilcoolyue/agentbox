// Package extension is the Agentbox extension host: it validates extension
// manifests, runs one instance per enabled extension, and dispatches hooks to
// them over the SPI in pkg/extspi. instance.go is the hook-independent
// lifecycle; each hook surface (model.go) adds its own dispatch. The design is
// documented in docs/architecture/extensions-spi.md.
package extension

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"agentbox/pkg/extspi"
)

// Locales every user-facing manifest string may carry.
var Locales = []string{"zh-CN", "zh-TW", "en"}

var (
	idRe      = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)
	codeRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{2,47}$`)
	semverRe  = regexp.MustCompile(`^(0|[1-9]\d{0,5})\.(0|[1-9]\d{0,5})\.(0|[1-9]\d{0,5})(-[0-9A-Za-z.-]{1,32})?$`)
	spiRe     = regexp.MustCompile(`^(0|[1-9]\d{0,3})\.(0|[1-9]\d{0,3})$`)
	digestRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]{0,200}@sha256:[a-f0-9]{64}$`)
	metricRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	officialP = "agentbox."
)

// Text is a localized string keyed by locale. Display falls back to en, then
// to any present locale.
type Text map[string]string

// Pick returns the best text for locale.
func (t Text) Pick(locale string) string {
	if v := t[locale]; v != "" {
		return v
	}
	if v := t["en"]; v != "" {
		return v
	}
	for _, l := range Locales {
		if v := t[l]; v != "" {
			return v
		}
	}
	return ""
}

func (t Text) validate(field string, maxRunes int, multiline bool) error {
	if len(t) == 0 {
		return fmt.Errorf("%s is required", field)
	}
	for locale, v := range t {
		if !slices.Contains(Locales, locale) {
			return fmt.Errorf("%s: unsupported locale %q", field, locale)
		}
		if err := plainText(v, maxRunes, multiline); err != nil {
			return fmt.Errorf("%s[%s]: %w", field, locale, err)
		}
	}
	return nil
}

// plainText accepts printable text only, bounded in runes.
func plainText(s string, maxRunes int, multiline bool) error {
	if !utf8.ValidString(s) || strings.TrimSpace(s) == "" {
		return fmt.Errorf("must be non-empty UTF-8 text")
	}
	if utf8.RuneCountInString(s) > maxRunes {
		return fmt.Errorf("longer than %d characters", maxRunes)
	}
	for _, r := range s {
		if r == '\n' && multiline {
			continue
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return fmt.Errorf("contains control characters")
		}
	}
	return nil
}

type Runtime struct {
	Type  string `json:"type"` // builtin | container
	Image string `json:"image,omitempty"`
}

type ErrorDef struct {
	Code    string `json:"code"`
	Message Text   `json:"message"`
}

type MetricDef struct {
	Name  string `json:"name"`
	Title Text   `json:"title"`
}

type StatusDefs struct {
	Metrics []MetricDef `json:"metrics,omitempty"`
}

// Manifest is the parsed, validated extension.json.
type Manifest struct {
	ID            string       `json:"id"`
	Version       string       `json:"version"`
	SPI           string       `json:"spi"`
	Publisher     string       `json:"publisher"`
	Name          Text         `json:"name"`
	Description   Text         `json:"description"`
	License       string       `json:"license,omitempty"`
	Homepage      string       `json:"homepage,omitempty"`
	Runtime       Runtime      `json:"runtime"`
	Hooks         []string     `json:"hooks"`
	Permissions   []string     `json:"permissions"`
	Bindings      []string     `json:"bindings"`
	FailurePolicy string       `json:"failure_policy"`
	ConfigSchema  ConfigSchema `json:"config_schema"`
	Errors        []ErrorDef   `json:"errors"`
	Status        StatusDefs   `json:"status"`
}

// FailurePolicies are the allowed failure_policy values.
const (
	FailOpen   = "fail_open"
	FailClosed = "fail_closed"
)

// ParseManifest decodes and validates an untrusted manifest. Unknown fields
// are rejected; new fields arrive with a new SPI minor version.
func ParseManifest(raw []byte, builtin bool) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if err := m.validate(builtin); err != nil {
		return Manifest{}, fmt.Errorf("manifest %s: %w", m.ID, err)
	}
	return m, nil
}

func (m *Manifest) validate(builtin bool) error {
	if len(m.ID) > 64 || !idRe.MatchString(m.ID) {
		return fmt.Errorf("invalid id")
	}
	if strings.HasPrefix(m.ID, officialP) != builtin {
		return fmt.Errorf("the agentbox. prefix is reserved for, and required of, official builtin extensions")
	}
	if !semverRe.MatchString(m.Version) {
		return fmt.Errorf("version must be semver")
	}
	if !spiRe.MatchString(m.SPI) || !SupportsSPI(m.SPI) {
		return fmt.Errorf("unsupported spi version %q (host speaks %s)", m.SPI, extspi.Version)
	}
	if err := plainText(m.Publisher, 64, false); err != nil {
		return fmt.Errorf("publisher: %w", err)
	}
	if err := m.Name.validate("name", 64, false); err != nil {
		return err
	}
	if err := m.Description.validate("description", 1000, true); err != nil {
		return err
	}
	if len(m.License) > 64 || (m.License != "" && plainText(m.License, 64, false) != nil) {
		return fmt.Errorf("invalid license")
	}
	if m.Homepage != "" && (len(m.Homepage) > 512 || !strings.HasPrefix(m.Homepage, "https://") || strings.ContainsAny(m.Homepage, " \t\r\n\"'<>")) {
		return fmt.Errorf("homepage must be an https URL")
	}
	switch m.Runtime.Type {
	case "builtin":
		if !builtin || m.Runtime.Image != "" {
			return fmt.Errorf("builtin runtime is reserved for official extensions")
		}
	case "container":
		if !digestRe.MatchString(m.Runtime.Image) {
			return fmt.Errorf("container runtime requires an image pinned by sha256 digest")
		}
		return fmt.Errorf("container runtime is not available yet")
	default:
		return fmt.Errorf("unknown runtime %q", m.Runtime.Type)
	}
	if len(m.Hooks) == 0 {
		return fmt.Errorf("at least one hook is required")
	}
	if err := unique("hooks", m.Hooks, func(h string) bool { _, ok := extspi.LookupHook(h); return ok }); err != nil {
		return err
	}
	if err := unique("permissions", m.Permissions, extspi.KnownPermission); err != nil {
		return err
	}
	for _, hook := range m.Hooks {
		if spec, _ := extspi.LookupHook(hook); !slices.Contains(m.Permissions, spec.Permission) {
			return fmt.Errorf("hook %s requires permission %s", hook, spec.Permission)
		}
	}
	if len(m.Bindings) != 1 || m.Bindings[0] != "account" {
		return fmt.Errorf(`bindings must be ["account"]`)
	}
	if m.FailurePolicy != FailOpen && m.FailurePolicy != FailClosed {
		return fmt.Errorf("failure_policy must be fail_open or fail_closed")
	}
	if err := m.ConfigSchema.validate(); err != nil {
		return fmt.Errorf("config_schema: %w", err)
	}
	// Only hooks that can reject or interrupt need error codes.
	if len(m.Errors) > 32 {
		return fmt.Errorf("errors must declare at most 32 codes")
	}
	seen := map[string]bool{}
	for _, e := range m.Errors {
		if !codeRe.MatchString(e.Code) || seen[e.Code] || strings.HasPrefix(e.Code, "extension_") {
			return fmt.Errorf("invalid or duplicate error code %q", e.Code)
		}
		seen[e.Code] = true
		if err := e.Message.validate("errors."+e.Code, 300, false); err != nil {
			return err
		}
	}
	if len(m.Status.Metrics) > 32 {
		return fmt.Errorf("at most 32 metrics")
	}
	seen = map[string]bool{}
	for _, metric := range m.Status.Metrics {
		if !metricRe.MatchString(metric.Name) || seen[metric.Name] {
			return fmt.Errorf("invalid or duplicate metric %q", metric.Name)
		}
		seen[metric.Name] = true
		if err := metric.Title.validate("metrics."+metric.Name, 64, false); err != nil {
			return err
		}
	}
	return nil
}

func unique(field string, values []string, known func(string) bool) error {
	seen := map[string]bool{}
	for _, v := range values {
		if !known(v) || seen[v] {
			return fmt.Errorf("%s: unknown or duplicate %q", field, v)
		}
		seen[v] = true
	}
	return nil
}

// SupportsSPI reports whether the host speaks the requested major.minor.
func SupportsSPI(v string) bool {
	wantMajor, wantMinor, ok := splitVersion(v)
	haveMajor, haveMinor, _ := splitVersion(extspi.Version)
	return ok && wantMajor == haveMajor && wantMinor <= haveMinor
}

func splitVersion(v string) (int, int, bool) {
	major, minor, ok := strings.Cut(v, ".")
	if !ok {
		return 0, 0, false
	}
	a, err1 := strconv.Atoi(major)
	b, err2 := strconv.Atoi(minor)
	return a, b, err1 == nil && err2 == nil
}

// HasHook reports whether the manifest declares hook.
func (m Manifest) HasHook(hook string) bool { return slices.Contains(m.Hooks, hook) }

// ErrorDef returns the declared error for code.
func (m Manifest) ErrorDef(code string) (ErrorDef, bool) {
	for _, e := range m.Errors {
		if e.Code == code {
			return e, true
		}
	}
	return ErrorDef{}, false
}

// HasMetric reports whether name is a declared status metric.
func (m Manifest) HasMetric(name string) bool {
	for _, metric := range m.Status.Metrics {
		if metric.Name == name {
			return true
		}
	}
	return false
}
