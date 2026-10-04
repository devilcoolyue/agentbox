package syncproto

import (
	"net"
	"net/url"
	"path"
	"strings"
	"unicode"
)

// NormalizeServer is shared by durable bindings and HTTP transport. It never
// accepts credentials, query strings, fragments or escaped path components.
func NormalizeServer(address string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || strings.ContainsAny(u.EscapedPath(), "%\\") {
		return "", ErrInvalid
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "443" && u.Scheme == "https" || port == "80" && u.Scheme == "http" {
		port = ""
	}
	u.Host = host
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	u.Path = strings.TrimRight(path.Clean("/"+u.Path), "/") + "/"
	return u.String(), nil
}

func (b Binding) Validate() error {
	server, err := NormalizeServer(b.Server)
	if err != nil || server != b.Server || b.Version != Version || !ValidHash(b.ServerID) || !ValidHash(b.LocalID) || b.User == "" || len(b.User) > 256 || strings.IndexFunc(b.User, unicode.IsControl) >= 0 || !validIdentity(b.Workspace) || !validIdentity(b.Project) || b.ProjectPath != "." && !ValidPath(b.ProjectPath) {
		return ErrInvalid
	}
	return nil
}

type ServerIdentity struct {
	Version  int            `json:"protocol_version"`
	ServerID string         `json:"server_id"`
	User     string         `json:"user"`
	Features map[string]int `json:"features"`
}

func (s ServerIdentity) Validate() error {
	if s.Version != Version || !ValidHash(s.ServerID) || s.User == "" || len(s.User) > 256 || strings.IndexFunc(s.User, unicode.IsControl) >= 0 || s.Features == nil {
		return ErrInvalid
	}
	return nil
}
