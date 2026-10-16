package extension

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"unicode/utf8"
)

var propRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// ConfigSchema is the supported JSON Schema subset: a flat object whose
// properties are booleans, integers, numbers, strings or string enums. The host
// renders it as a form; extensions never ship their own UI.
type ConfigSchema struct {
	Type       string                `json:"type"`
	Properties map[string]SchemaProp `json:"properties,omitempty"`
}

type SchemaProp struct {
	Type        string          `json:"type"` // boolean | integer | number | string | enum
	Title       Text            `json:"title"`
	Description Text            `json:"description,omitempty"`
	Minimum     *float64        `json:"minimum,omitempty"`
	Maximum     *float64        `json:"maximum,omitempty"`
	MaxLength   int             `json:"maxLength,omitempty"`
	Enum        []string        `json:"enum,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
}

func (s ConfigSchema) validate() error {
	if s.Type != "object" {
		return fmt.Errorf(`type must be "object"`)
	}
	if len(s.Properties) > 64 {
		return fmt.Errorf("at most 64 properties")
	}
	for name, p := range s.Properties {
		if !propRe.MatchString(name) {
			return fmt.Errorf("invalid property name %q", name)
		}
		if err := p.Title.validate(name+".title", 64, false); err != nil {
			return err
		}
		if p.Description != nil {
			if err := p.Description.validate(name+".description", 300, false); err != nil {
				return err
			}
		}
		switch p.Type {
		case "boolean", "integer", "number":
			if p.MaxLength != 0 || p.Enum != nil {
				return fmt.Errorf("%s: maxLength/enum only apply to strings", name)
			}
		case "string":
			if p.MaxLength < 0 || p.MaxLength > 1024 || p.Enum != nil {
				return fmt.Errorf("%s: maxLength must be 0..1024", name)
			}
		case "enum":
			if len(p.Enum) == 0 || len(p.Enum) > 32 {
				return fmt.Errorf("%s: enum needs 1..32 values", name)
			}
			for _, v := range p.Enum {
				if err := plainText(v, 64, false); err != nil {
					return fmt.Errorf("%s: enum value: %w", name, err)
				}
			}
		default:
			return fmt.Errorf("%s: unsupported type %q", name, p.Type)
		}
		if (p.Minimum != nil || p.Maximum != nil) && p.Type != "integer" && p.Type != "number" {
			return fmt.Errorf("%s: minimum/maximum only apply to numbers", name)
		}
		if p.Minimum != nil && p.Maximum != nil && *p.Minimum > *p.Maximum {
			return fmt.Errorf("%s: minimum exceeds maximum", name)
		}
		if p.Default != nil {
			if _, err := p.check(p.Default); err != nil {
				return fmt.Errorf("%s: default: %w", name, err)
			}
		}
	}
	return nil
}

// check validates one value and returns its canonical form.
func (p SchemaProp) check(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid JSON")
	}
	switch p.Type {
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("must be a boolean")
		}
		return b, nil
	case "integer", "number":
		n, ok := v.(json.Number)
		if !ok {
			return nil, fmt.Errorf("must be a number")
		}
		f, err := n.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("must be a finite number")
		}
		if p.Type == "integer" {
			i, err := n.Int64()
			if err != nil || math.Abs(f) > 1<<53 {
				return nil, fmt.Errorf("must be an integer")
			}
			f = float64(i)
		}
		if p.Minimum != nil && f < *p.Minimum {
			return nil, fmt.Errorf("must be >= %v", *p.Minimum)
		}
		if p.Maximum != nil && f > *p.Maximum {
			return nil, fmt.Errorf("must be <= %v", *p.Maximum)
		}
		if p.Type == "integer" {
			return int64(f), nil
		}
		return f, nil
	case "string", "enum":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		if !utf8.ValidString(s) {
			return nil, fmt.Errorf("must be UTF-8")
		}
		if p.Type == "enum" {
			if !slices.Contains(p.Enum, s) {
				return nil, fmt.Errorf("must be one of the listed values")
			}
			return s, nil
		}
		limit := p.MaxLength
		if limit == 0 {
			limit = 1024
		}
		if utf8.RuneCountInString(s) > limit {
			return nil, fmt.Errorf("longer than %d characters", limit)
		}
		return s, nil
	}
	return nil, fmt.Errorf("unsupported type")
}

// Normalize validates an administrator-supplied config object and returns it
// with defaults filled in and keys sorted. An empty input means "all
// defaults". Unknown keys are rejected so a typo never silently does nothing.
func (s ConfigSchema) Normalize(raw json.RawMessage) (json.RawMessage, error) {
	values := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) != 0 && string(bytes.TrimSpace(raw)) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		if err := dec.Decode(&values); err != nil {
			return nil, fmt.Errorf("config must be a JSON object")
		}
	}
	out := map[string]any{}
	for key, value := range values {
		p, ok := s.Properties[key]
		if !ok {
			return nil, fmt.Errorf("config: unknown setting %q", key)
		}
		v, err := p.check(value)
		if err != nil {
			return nil, fmt.Errorf("config.%s: %w", key, err)
		}
		out[key] = v
	}
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := out[name]; ok || s.Properties[name].Default == nil {
			continue
		}
		v, err := s.Properties[name].check(s.Properties[name].Default)
		if err != nil {
			return nil, err
		}
		out[name] = v
	}
	return json.Marshal(out) // encoding/json sorts map keys
}
