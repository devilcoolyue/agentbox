package gitaccess

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Grant permits one repository and one operation. Only the scoped short-lived
// grant reaches the container; the provider credential stays in this handler.
// ReadOnly connections never get receive-pack access. Push grants additionally
// enforce the exact old/new object IDs and target ref on the Git wire protocol.
type Grant struct {
	Progress func(received, sent int64)

	Ticket     string
	Repository string
	Write      bool
	Ref        string
	Old        string
	New        string
	Authorize  func(context.Context) (username, secret string, err error)
	Client     *http.Client
}

func (g Grant) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	prefix := "/" + g.Ticket + "/"
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) != 2 || subtle.ConstantTimeCompare([]byte(parts[0]), []byte(g.Ticket)) != 1 {
		http.NotFound(w, r)
		return
	}
	suffix := parts[1]
	service := "git-upload-pack"
	if g.Write {
		service = "git-receive-pack"
	}
	allowed := r.Method == "GET" && suffix == "info/refs" && r.URL.RawQuery == "service="+service || r.Method == "POST" && suffix == service && r.URL.RawQuery == ""
	if !allowed || !strings.HasPrefix(r.URL.Path, prefix) || r.URL.RawPath != "" {
		http.Error(w, "Git transport request denied", 403)
		return
	}
	username, secret, err := g.Authorize(r.Context())
	if err != nil {
		http.Error(w, "Git connection is unavailable or revoked", 403)
		return
	}
	body := io.Reader(r.Body)
	if r.Method == "POST" && g.Write {
		first, err := checkPush(r.Body, g.Old, g.New, g.Ref)
		if err != nil {
			http.Error(w, "Push target changed; refresh the push preview", 409)
			return
		}
		body = io.MultiReader(bytes.NewReader(first), r.Body)
	}
	u, err := url.Parse(g.Repository)
	if err != nil {
		http.Error(w, "Invalid repository", 500)
		return
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + suffix
	u.RawQuery = r.URL.RawQuery
	if g.Progress != nil && r.Method == "POST" {
		body = progressReader{r: body, progress: func(n int64) { g.Progress(0, n) }}
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), body)
	if err != nil {
		http.Error(w, "Git transport failed", 502)
		return
	}
	req.SetBasicAuth(username, secret)
	req.ContentLength = r.ContentLength
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" {
		if g.Write || encoding != "gzip" {
			http.Error(w, "Unsupported Git content encoding", 400)
			return
		}
		req.Header.Set("Content-Encoding", encoding)
	}
	// Forward only Git protocol headers; never browser cookies or caller auth.
	if r.Method == "POST" {
		req.Header.Set("Content-Type", "application/x-"+service+"-request")
	}
	if v := r.Header.Get("Git-Protocol"); v == "version=2" {
		req.Header.Set("Git-Protocol", v)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "agentbox-git")
	client := *g.Client
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		http.Error(w, "Git upstream connection failed", 502)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		http.Error(w, "Git upstream refused request (HTTP "+strconv.Itoa(response.StatusCode)+")", response.StatusCode)
		return
	}
	expectedType := "application/x-" + service + "-result"
	if r.Method == "GET" {
		expectedType = "application/x-" + service + "-advertisement"
	}
	if strings.Split(response.Header.Get("Content-Type"), ";")[0] != expectedType {
		http.Error(w, "Upstream does not provide Git smart HTTP", 502)
		return
	}
	w.Header().Set("Content-Type", expectedType)
	w.Header().Set("Cache-Control", "no-store")
	var content io.Reader = response.Body
	if g.Progress != nil {
		content = progressReader{r: content, progress: func(n int64) { g.Progress(n, 0) }}
	}
	_, _ = io.Copy(w, content)
}

func checkPush(r io.Reader, old, new, ref string) ([]byte, error) {
	if !validOID(old) || !validOID(new) || strings.Trim(new, "0") == "" {
		return nil, errors.New("invalid expected object")
	}
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n, err := strconv.ParseUint(string(header[:]), 16, 16)
	if err != nil || n < 4 || n > 65520 {
		return nil, errors.New("invalid packet")
	}
	data := make([]byte, int(n)-4)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	command, _, _ := strings.Cut(string(data), "\x00")
	command = strings.TrimSuffix(command, "\n")
	if command != old+" "+new+" "+ref {
		return nil, errors.New("push differs from grant")
	}
	var flush [4]byte
	if _, err := io.ReadFull(r, flush[:]); err != nil {
		return nil, err
	}
	if string(flush[:]) != "0000" {
		return nil, errors.New("multiple ref updates denied")
	}
	out := append(header[:], data...)
	out = append(out, flush[:]...)
	return out, nil
}
func validOID(v string) bool {
	if len(v) != 40 && len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

type progressReader struct {
	r        io.Reader
	progress func(int64)
}

func (r progressReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.progress(int64(n))
	}
	return n, err
}
