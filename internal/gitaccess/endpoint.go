package gitaccess

import (
	"errors"
	"net"
	"net/url"
	"path"
	"strings"
	"unicode"
)

// BaseURL accepts HTTPS origins and optional GitLab installation subpaths.
// Credentials, fragments, queries and ambiguous encoded path separators are
// rejected, not silently normalized into a different authorization scope.
func BaseURL(raw string) (string, error) { return baseURL(raw, false) }

// ConnectionBaseURL additionally accepts plain HTTP for connections routed
// through the owner's intranet tunnel: many self-hosted GitLab instances on
// company networks only serve HTTP. The route is immutable once saved, so such
// a connection can never send its token in clear text from the server itself.
func ConnectionBaseURL(raw string, network NetworkPolicy) (string, error) {
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Scheme == "http" {
		if network.Route != "tunnel" {
			return "", errors.New("HTTP 服务地址只能通过「我的内网隧道」连接；服务端直连必须使用 HTTPS")
		}
		if network.CAPEM != "" {
			return "", errors.New("HTTP 服务地址不使用 CA 证书，请清空公司 CA 证书")
		}
		return baseURL(raw, true)
	}
	return baseURL(raw, false)
}

func baseURL(raw string, allowHTTP bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !(u.Scheme == "https" || allowHTTP && u.Scheme == "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		if allowHTTP {
			return "", errors.New("服务地址必须是 HTTP 或 HTTPS，且不能包含用户名、密码、查询参数或片段")
		}
		return "", errors.New("服务地址必须是 HTTPS，且不能包含用户名、密码、查询参数或片段")
	}
	if strings.ContainsAny(u.Host, "\\%") || strings.ContainsAny(u.Path, "\\\x00") || u.RawPath != "" {
		return "", errors.New("服务地址格式无效")
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", errors.New("服务地址不能包含空格或控制字符")
		}
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("服务主机不能为空")
	}
	if ip := net.ParseIP(host); ip == nil {
		for _, c := range host {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
				return "", errors.New("服务域名格式无效，请使用 ASCII 域名")
			}
		}
	}
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		u.Host = host
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		}
	}
	clean := path.Clean("/" + strings.TrimPrefix(u.Path, "/"))
	if strings.TrimRight(u.Path, "/") != "" && clean != strings.TrimRight(u.Path, "/") {
		return "", errors.New("服务地址不能包含路径跳转")
	}
	u.Path = strings.TrimRight(clean, "/")
	return u.String(), nil
}

// RepositoryURL requires the scheme of its connection: an HTTP connection only
// exists behind the intranet tunnel, and an HTTPS one never downgrades.
func RepositoryURL(raw, base string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", errors.New("连接服务地址无效")
	}
	canonical, err := baseURL(raw, b.Scheme == "http")
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(canonical)
	if u.Scheme != b.Scheme || u.Host != b.Host || !(b.Path == "" || strings.HasPrefix(u.Path, b.Path+"/")) || u.Path == "" {
		return "", errors.New("仓库地址必须位于所选连接的服务地址下")
	}
	return canonical, nil
}
