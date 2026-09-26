package gitaccess

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type SSHCredential struct {
	PrivateKey string `json:"private_key"`
	Passphrase string `json:"passphrase,omitempty"`
	HostKey    string `json:"host_key"`
}

func (c SSHCredential) Signer() (ssh.Signer, error) {
	if len(c.PrivateKey) > 32<<10 || len(c.Passphrase) > 4096 {
		return nil, errors.New("SSH 私钥或口令过长")
	}
	var signer ssh.Signer
	var err error
	if c.Passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(c.PrivateKey), []byte(c.Passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey([]byte(c.PrivateKey))
	}
	if err != nil {
		return nil, errors.New("SSH 私钥或解密口令无效")
	}
	return signer, nil
}
func (c SSHCredential) Fingerprint() (string, error) {
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(c.HostKey))
	if err != nil || strings.TrimSpace(string(rest)) != "" {
		return "", errors.New("请填写管理员确认的单个 SSH 主机公钥（算法与 base64 内容），不能自动接受未知主机")
	}
	if _, ok := key.(*ssh.Certificate); ok {
		return "", errors.New("当前需要普通 SSH 主机公钥，不支持主机证书")
	}
	return ssh.FingerprintSHA256(key), nil
}
func (c SSHCredential) PublicKey() (string, error) {
	s, err := c.Signer()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))), nil
}

var sshUser = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

func SSHBaseURL(raw, username string) (string, error) {
	if !sshUser.MatchString(username) {
		return "", errors.New("SSH 用户名无效")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "ssh" || u.User != nil && u.User.Username() != username {
		return "", errors.New("SSH 地址应为 ssh://主机[:端口]，用户名单独填写")
	}
	if u.User != nil {
		if _, set := u.User.Password(); set {
			return "", errors.New("SSH 地址不能包含密码")
		}
		u.User = nil
	}
	u.Scheme = "https"
	port := u.Port()
	canonical, err := BaseURL(u.String())
	if err != nil {
		return "", err
	}
	parsed, _ := url.Parse(canonical)
	parsed.Scheme = "ssh"
	// HTTPS normalization removes :443; retain it for SSH if explicitly selected.
	if port == "443" {
		parsed.Host = net.JoinHostPort(parsed.Hostname(), "443")
	}
	if parsed.Port() == "22" {
		h := parsed.Hostname()
		if strings.Contains(h, ":") {
			h = "[" + h + "]"
		}
		parsed.Host = h
	}
	return parsed.String(), nil
}
func SSHRepositoryURL(raw, base, username string) (string, error) {
	if !strings.Contains(raw, "://") {
		left, right, ok := strings.Cut(raw, ":")
		if !ok || strings.Contains(left, "/") {
			return "", errors.New("SSH 仓库地址无效")
		}
		raw = "ssh://" + left + "/" + right
	}
	canonical, err := SSHBaseURL(raw, username)
	if err != nil {
		return "", err
	}
	parsed, _ := url.Parse(canonical)
	b, err := url.Parse(base)
	if err != nil {
		return "", errors.New("SSH 连接地址无效")
	}
	if parsed.Host != b.Host || parsed.Path == "" || !(b.Path == "" || strings.HasPrefix(parsed.Path, b.Path+"/")) {
		return "", errors.New("SSH 仓库必须位于连接的主机及路径下")
	}
	// Git hosts accept a repository path relative to their service root. Refuse
	// shell metacharacters and dot segments; no arbitrary remote shell commands.
	for _, part := range strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, "-") {
			return "", errors.New("SSH 仓库路径无效")
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c)) {
				return "", errors.New("SSH 仓库路径仅支持字母、数字、点、下划线和连字符")
			}
		}
	}
	if path.Clean(parsed.Path) != parsed.Path {
		return "", errors.New("SSH 仓库路径无效")
	}
	parsed.User = url.User(username)
	return parsed.String(), nil
}

type SSHStream struct {
	io.Reader
	Writer  io.WriteCloser
	session *ssh.Session
	client  *ssh.Client
	stop    func() bool
}

func (s *SSHStream) Close()      { s.stop(); _ = s.session.Close(); _ = s.client.Close() }
func (s *SSHStream) Wait() error { return s.session.Wait() }

// OpenSSH runs only the remote Git service command; no host Git subprocess or
// private key file. The caller controls routing and cancellation of the stream.
func OpenSSH(ctx context.Context, repository, username string, credential SSHCredential, write bool, version2 bool, dial func(context.Context, string, string) (net.Conn, error)) (*SSHStream, error) {
	signer, err := credential.Signer()
	if err != nil {
		return nil, err
	}
	if _, err = credential.Fingerprint(); err != nil {
		return nil, err
	}
	hostKey, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(credential.HostKey))
	u, err := url.Parse(repository)
	if err != nil {
		return nil, err
	}
	base := *u
	base.Path = ""
	base.User = nil
	if _, err = SSHRepositoryURL(repository, base.String(), username); err != nil {
		return nil, err
	}
	address := u.Host
	if u.Port() == "" {
		address = net.JoinHostPort(u.Hostname(), "22")
	}
	conn, err := dial(ctx, "tcp", address)
	if err != nil {
		return nil, errors.New("SSH 目标不可达，请检查网络路由与端口")
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	config := &ssh.ClientConfig{User: username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		want := sha256.Sum256(hostKey.Marshal())
		got := sha256.Sum256(key.Marshal())
		if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			return errors.New("SSH 主机密钥不匹配")
		}
		return nil
	}}
	cc, ch, reqs, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		conn.Close()
		return nil, errors.New("SSH 认证或主机密钥验证失败")
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(cc, ch, reqs)
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, errors.New("无法启动 SSH Git 会话")
	}
	fail := func() (*SSHStream, error) {
		session.Close()
		client.Close()
		return nil, errors.New("无法执行远程 Git 协议")
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return fail()
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return fail()
	}
	session.Stderr = io.Discard
	if version2 {
		_ = session.Setenv("GIT_PROTOCOL", "version=2")
	}
	service := "git-upload-pack"
	if write {
		service = "git-receive-pack"
	}
	command := service + " '" + strings.TrimPrefix(u.Path, "/") + "'"
	if err = session.Start(command); err != nil {
		return fail()
	}
	cancelStream := context.AfterFunc(ctx, func() { _ = client.Close() })
	return &SSHStream{Reader: stdout, Writer: stdin, session: session, client: client, stop: cancelStream}, nil
}
func ParseSSHCredential(raw []byte) (SSHCredential, error) {
	var c SSHCredential
	if json.Unmarshal(raw, &c) != nil {
		return c, errors.New("SSH 凭证格式无效")
	}
	return c, nil
}

// Native Git protocol's first packet selects the service. Its path is a random
// scoped ticket; it never controls the actual SSH host/repository.
func ReadGitRequest(r io.Reader, ticket string, write bool) (bool, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return false, err
	}
	n, err := strconv.ParseUint(string(header[:]), 16, 16)
	if err != nil || n < 4 || n > 4096 {
		return false, errors.New("invalid Git request")
	}
	data := make([]byte, int(n)-4)
	if _, err = io.ReadFull(r, data); err != nil {
		return false, err
	}
	parts := strings.Split(string(data), "\x00")
	service := "git-upload-pack"
	if write {
		service = "git-receive-pack"
	}
	expected := service + " /" + ticket
	if subtle.ConstantTimeCompare([]byte(parts[0]), []byte(expected)) != 1 {
		return false, errors.New("Git request denied")
	}
	v2 := false
	for _, part := range parts[1:] {
		if part == "version=2" {
			v2 = true
		} else if part != "" && !strings.HasPrefix(part, "host=") {
			return false, errors.New("unsupported Git request")
		}
	}
	return v2, nil
}

// ForwardPush validates the only permitted ref update before delivering any
// command or pack data to the upstream SSH receive-pack process.
func ForwardPush(dst io.Writer, src io.Reader, old, next, ref string) (int64, error) {
	first, err := checkPush(src, old, next, ref)
	if err != nil {
		return 0, err
	}
	n, err := dst.Write(first)
	if err != nil {
		return int64(n), err
	}
	rest, err := io.Copy(dst, src)
	return int64(n) + rest, err
}
