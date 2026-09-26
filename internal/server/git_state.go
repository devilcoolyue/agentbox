package server

import (
	"net/url"
	"strconv"
	"strings"
)

type gitRemote struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Push bool   `json:"push"`
}

type gitBranchState struct {
	Branch        string `json:"branch"`
	Head          string `json:"head"`
	Detached      bool   `json:"detached"`
	Unborn        bool   `json:"unborn"`
	Upstream      string `json:"upstream"`
	Ahead         int    `json:"ahead"`
	Behind        int    `json:"behind"`
	TrackingKnown bool   `json:"tracking_known"`
}

// Porcelain v2 -z keeps spaces, quotes, newlines and rename separators in
// filenames intact. Rename records have a second NUL-delimited original path.
func parseGitState(out string) (gitBranchState, []gitFile) {
	st := gitBranchState{}
	files := []gitFile{}
	records := strings.Split(out, "\x00")
	for i := 0; i < len(records); i++ {
		line := records[i]
		switch {
		case strings.HasPrefix(line, "# branch.oid "):
			st.Head = strings.TrimPrefix(line, "# branch.oid ")
			st.Unborn = st.Head == "(initial)"
			if st.Unborn {
				st.Head = ""
			}
		case strings.HasPrefix(line, "# branch.head "):
			st.Branch = strings.TrimPrefix(line, "# branch.head ")
			st.Detached = st.Branch == "(detached)"
			if st.Detached {
				st.Branch = ""
			}
		case strings.HasPrefix(line, "# branch.upstream "):
			st.Upstream = strings.TrimPrefix(line, "# branch.upstream ")
		case strings.HasPrefix(line, "# branch.ab "):
			parts := strings.Fields(strings.TrimPrefix(line, "# branch.ab "))
			if len(parts) == 2 {
				a, ae := strconv.Atoi(strings.TrimPrefix(parts[0], "+"))
				b, be := strconv.Atoi(strings.TrimPrefix(parts[1], "-"))
				if ae == nil && be == nil {
					st.Ahead, st.Behind, st.TrackingKnown = a, b, true
				}
			}
		case strings.HasPrefix(line, "? "):
			files = append(files, gitFile{Path: line[2:], Status: "??", Untracked: true})
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "), strings.HasPrefix(line, "u "):
			n := 9
			if line[0] == '2' {
				n = 10
			}
			if line[0] == 'u' {
				n = 11
			}
			parts := strings.SplitN(line, " ", n)
			if len(parts) == n {
				files = append(files, gitFile{Path: parts[n-1], Status: strings.ReplaceAll(parts[1], ".", " ")})
			}
			if line[0] == '2' {
				i++
			}
		}
	}
	return st, files
}

func displayGitURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http" || u.Scheme == "ssh" || u.Scheme == "git") {
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		return u.String()
	}
	// scp-like SSH syntax. Other transports (including local paths and helpers)
	// are not sent to the browser; they may embed secrets in arbitrary syntax.
	if !strings.Contains(raw, "://") && !strings.Contains(raw, "::") && !strings.ContainsAny(raw, "\n\r\t ?#") {
		if at := strings.LastIndexByte(raw, '@'); at >= 0 {
			raw = raw[at+1:]
		}
		if host, path, ok := strings.Cut(raw, ":"); ok && host != "" && path != "" && !strings.ContainsAny(host, "/\\") {
			return host + ":" + path
		}
	}
	return "（本地路径或未识别的远程地址）"
}

func parseGitRemotes(out string) []gitRemote {
	remotes := []gitRemote{}
	for _, record := range strings.Split(out, "\x00") {
		key, value, ok := strings.Cut(record, "\n")
		if !ok || !strings.HasPrefix(key, "remote.") {
			continue
		}
		push := strings.HasSuffix(key, ".pushurl")
		suffix := ".url"
		if push {
			suffix = ".pushurl"
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "remote."), suffix)
		remotes = append(remotes, gitRemote{Name: name, URL: displayGitURL(value), Push: push})
	}
	return remotes
}
