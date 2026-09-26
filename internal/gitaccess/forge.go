package gitaccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Forge is a narrow API client. Paths are derived from the validated repository,
// never arbitrary browser input or next-page URLs returned by the provider.
type Forge struct {
	Provider, BaseURL, Project, Token, AuthType string
	Client                                      *http.Client
}
type ForgeBranch struct {
	Name      string `json:"name"`
	SHA       string `json:"sha"`
	Protected bool   `json:"protected"`
}
type Review struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Source string `json:"source"`
	Target string `json:"target"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
}
type ReviewInput struct {
	Title, Body, Source, Target string
	Draft                       bool
}
type ForgeError struct{ Status int }

func (e *ForgeError) Error() string {
	switch e.Status {
	case 401, 403:
		return "平台 API 权限不足：请检查 Token 或 OAuth 范围、组织 SSO 与仓库访问权限"
	case 404:
		return "平台未找到该仓库、分支或合并请求，或连接无权访问"
	case 409, 422:
		return "平台拒绝创建：可能已存在合并请求、没有可合并差异或分支已变化，请刷新核实"
	case 429:
		return "平台 API 请求过多，请稍后重试"
	}
	return fmt.Sprintf("平台 API 请求失败（HTTP %d）", e.Status)
}
func NewForge(provider, base, repository, token, authType string, client *http.Client) (*Forge, error) {
	if provider != "github" && provider != "gitlab" {
		return nil, errors.New("此连接不支持 PR/MR，请选择 GitHub 或 GitLab API 连接")
	}
	canonical, err := RepositoryURL(repository, base)
	if err != nil {
		return nil, err
	}
	b, _ := url.Parse(base)
	r, _ := url.Parse(canonical)
	project := strings.TrimSuffix(strings.TrimPrefix(r.Path, strings.TrimRight(b.Path, "/")+"/"), ".git")
	segments := strings.Split(project, "/")
	if len(segments) < 2 || provider == "github" && len(segments) != 2 {
		return nil, errors.New("无法从远程地址识别托管平台的仓库路径")
	}
	for _, part := range segments {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("仓库路径无效")
		}
	}
	return &Forge{Provider: provider, BaseURL: base, Project: project, Token: token, AuthType: authType, Client: client}, nil
}
func (f *Forge) root() string {
	if f.Provider == "gitlab" {
		return f.BaseURL + "/api/v4/projects/" + url.PathEscape(f.Project)
	}
	base := f.BaseURL + "/api/v3"
	if f.BaseURL == "https://github.com" {
		base = "https://api.github.com"
	}
	parts := strings.Split(f.Project, "/")
	return base + "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
}
func (f *Forge) call(ctx context.Context, method, suffix string, input, output any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, f.root()+suffix, body)
	if err != nil {
		return errors.New("平台 API 地址无效")
	}
	if f.Provider == "gitlab" && f.AuthType == "pat" {
		req.Header.Set("PRIVATE-TOKEN", f.Token)
	} else {
		req.Header.Set("Authorization", "Bearer "+f.Token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "agentbox-git")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *f.Client
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return errors.New("平台 API 网络请求失败；写操作结果可能未知，请刷新列表核实")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &ForgeError{Status: response.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
	if err != nil || len(raw) > 2<<20 {
		return errors.New("平台 API 响应读取失败或过大")
	}
	if output != nil && json.Unmarshal(raw, output) != nil {
		return errors.New("平台 API 返回格式无效")
	}
	return nil
}
func (f *Forge) DefaultBranch(ctx context.Context) (string, error) {
	var metadata struct {
		DefaultBranch string `json:"default_branch"`
	}
	err := f.call(ctx, "GET", "", nil, &metadata)
	return metadata.DefaultBranch, err
}
func (f *Forge) Branch(ctx context.Context, name string) (ForgeBranch, error) {
	var branch ForgeBranch
	if f.Provider == "github" {
		var data struct {
			Name      string `json:"name"`
			Protected bool   `json:"protected"`
			Commit    struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		err := f.call(ctx, "GET", "/branches/"+url.PathEscape(name), nil, &data)
		branch = ForgeBranch{Name: data.Name, SHA: data.Commit.SHA, Protected: data.Protected}
		if err != nil {
			return branch, err
		}
	} else {
		var data struct {
			Name      string `json:"name"`
			Protected bool   `json:"protected"`
			Commit    struct {
				ID string `json:"id"`
			} `json:"commit"`
		}
		err := f.call(ctx, "GET", "/repository/branches/"+url.PathEscape(name), nil, &data)
		branch = ForgeBranch{Name: data.Name, SHA: data.Commit.ID, Protected: data.Protected}
		if err != nil {
			return branch, err
		}
	}
	if branch.Name != name || !validOID(branch.SHA) {
		return branch, errors.New("平台返回的分支身份无效")
	}
	return branch, nil
}

// Review URLs must point to this platform/repository; malicious API responses
// cannot turn UI links into javascript: URLs or cross-origin credential traps.
func (f *Forge) reviewURL(raw string) string {
	b, err := url.Parse(f.BaseURL)
	if err != nil {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != b.Scheme || u.Host != b.Host || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return ""
	}
	prefix := strings.TrimRight(b.Path, "/") + "/" + f.Project + "/"
	if !strings.HasPrefix(u.Path, prefix) {
		return ""
	}
	return u.String()
}

type githubReview struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Head   struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (f *Forge) githubRow(p githubReview) Review {
	return Review{Number: p.Number, Title: p.Title, URL: f.reviewURL(p.URL), Source: p.Head.Ref, Target: p.Base.Ref, State: p.State, Draft: p.Draft}
}

type gitlabReview struct {
	IID           int64  `json:"iid"`
	Title         string `json:"title"`
	URL           string `json:"web_url"`
	State         string `json:"state"`
	Draft         bool   `json:"draft"`
	Source        string `json:"source_branch"`
	Target        string `json:"target_branch"`
	SourceProject int64  `json:"source_project_id"`
	TargetProject int64  `json:"target_project_id"`
}

func (f *Forge) gitlabRow(p gitlabReview) Review {
	return Review{Number: p.IID, Title: p.Title, URL: f.reviewURL(p.URL), Source: p.Source, Target: p.Target, State: p.State, Draft: p.Draft}
}
func (f *Forge) Reviews(ctx context.Context, source, target string, page int) ([]Review, bool, error) {
	if page < 1 || page > 1000 {
		return nil, false, errors.New("无效页码")
	}
	q := url.Values{"per_page": {"50"}, "page": {strconv.Itoa(page)}}
	rows := []Review{}
	rawCount := 0
	if f.Provider == "github" {
		q.Set("state", "open")
		q.Set("sort", "updated")
		q.Set("direction", "desc")
		if source != "" {
			q.Set("head", strings.Split(f.Project, "/")[0]+":"+source)
		}
		if target != "" {
			q.Set("base", target)
		}
		var data []githubReview
		if err := f.call(ctx, "GET", "/pulls?"+q.Encode(), nil, &data); err != nil {
			return nil, false, err
		}
		rawCount = len(data)
		for _, p := range data {
			if source != "" && p.Head.Repo.FullName != f.Project {
				continue
			}
			rows = append(rows, f.githubRow(p))
		}
	} else {
		q.Set("state", "opened")
		q.Set("order_by", "updated_at")
		q.Set("sort", "desc")
		q.Set("scope", "all")
		if source != "" {
			q.Set("source_branch", source)
		}
		if target != "" {
			q.Set("target_branch", target)
		}
		var data []gitlabReview
		if err := f.call(ctx, "GET", "/merge_requests?"+q.Encode(), nil, &data); err != nil {
			return nil, false, err
		}
		rawCount = len(data)
		for _, p := range data {
			if source != "" && p.SourceProject != p.TargetProject {
				continue
			}
			rows = append(rows, f.gitlabRow(p))
		}
	}
	return rows, rawCount == 50, nil
}
func (f *Forge) CreateReview(ctx context.Context, input ReviewInput) (Review, error) {
	if f.Provider == "github" {
		var data githubReview
		err := f.call(ctx, "POST", "/pulls", map[string]any{"title": input.Title, "body": input.Body, "head": input.Source, "base": input.Target, "draft": input.Draft}, &data)
		if err != nil {
			return Review{}, err
		}
		return f.githubRow(data), nil
	}
	title := input.Title
	if input.Draft && !strings.HasPrefix(strings.ToLower(title), "draft:") {
		title = "Draft: " + title
	}
	var data gitlabReview
	err := f.call(ctx, "POST", "/merge_requests", map[string]any{"title": title, "description": input.Body, "source_branch": input.Source, "target_branch": input.Target, "remove_source_branch": false}, &data)
	if err != nil {
		return Review{}, err
	}
	return f.gitlabRow(data), nil
}
