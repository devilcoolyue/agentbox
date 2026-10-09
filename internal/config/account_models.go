package config

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxAccountModels bounds one account's list; relays can report thousands of
// IDs, but a picker with more than this many entries is not a usable choice.
const MaxAccountModels = 500

// normalizeModels validates the account's own model list in place. An empty
// list keeps the account on the global model list and must not name a default.
func (a *Account) normalizeModels() error {
	if len(a.Models) == 0 {
		a.Models = nil
		if a.DefaultModel != "" {
			return fmt.Errorf("未配置可用模型时不能指定默认模型")
		}
		return nil
	}
	if len(a.Models) > MaxAccountModels {
		return fmt.Errorf("可用模型最多 %d 个", MaxAccountModels)
	}
	seen := map[string]bool{}
	for i := range a.Models {
		m := &a.Models[i]
		m.ID = strings.TrimSpace(m.ID)
		m.Label = strings.TrimSpace(m.Label)
		if !modelIDRe.MatchString(m.ID) {
			return fmt.Errorf("模型 ID %q 无效", m.ID)
		}
		if seen[m.ID] {
			return fmt.Errorf("模型 %q 重复", m.ID)
		}
		seen[m.ID] = true
		if m.Label == "" {
			m.Label = m.ID
		}
		if utf8.RuneCountInString(m.Label) > 80 {
			return fmt.Errorf("模型 %s 的显示名称过长", m.ID)
		}
		if err := ValidateReasoning(a.Type, m.Reasoning); err != nil {
			return fmt.Errorf("模型 %s: %w", m.ID, err)
		}
	}
	a.DefaultModel = strings.TrimSpace(a.DefaultModel)
	if a.DefaultModel == "" {
		for _, m := range a.Models {
			if !m.Hidden {
				a.DefaultModel = m.ID
				break
			}
		}
		if a.DefaultModel == "" {
			return fmt.Errorf("至少保留一个在对话中显示的模型")
		}
	}
	if !seen[a.DefaultModel] {
		return fmt.Errorf("默认模型 %s 不在可用模型中", a.DefaultModel)
	}
	// New workspaces start on the default, so it is always offered.
	if !a.ShowsModel(a.DefaultModel) {
		return fmt.Errorf("默认模型 %s 不能隐藏", a.DefaultModel)
	}
	return nil
}

// ShowsModel reports whether the chat picker offers the model: listed and not
// hidden. Hidden models remain allowed (AllowsModel).
func (a Account) ShowsModel(id string) bool {
	for _, m := range a.Models {
		if m.ID == id {
			return !m.Hidden
		}
	}
	return false
}

// ValidModelID is the model ID rule shared by every place a model reaches a CLI.
func ValidModelID(id string) bool { return modelIDRe.MatchString(id) }

// RestrictsModels reports whether web chat is limited to the account's list.
func (a Account) RestrictsModels() bool { return len(a.Models) > 0 }

// AllowsModel is true for every model when the account has no list of its own.
func (a Account) AllowsModel(id string) bool {
	if !a.RestrictsModels() {
		return true
	}
	for _, m := range a.Models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// ResolveModel keeps a workspace's model when the account allows it and falls
// back to the account default otherwise (the list changed after the workspace
// was created, or the workspace moved to this account).
func (a Account) ResolveModel(model string) string {
	if a.AllowsModel(model) {
		return model
	}
	return a.DefaultModel
}

// NewWorkspaceModel is the model a workspace created on this account starts with.
func (c *Config) NewWorkspaceModel(a Account) string {
	if a.RestrictsModels() {
		return a.DefaultModel
	}
	return c.GetDefaultModel(a.Type)
}
