package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentbox/internal/config"
	"agentbox/internal/store"
)

func TestParseProxyLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want config.Proxy
	}{
		{
			name: "带协议与凭证的完整 URL",
			line: "socks5://user:pa55@1.2.3.4:1080",
			want: config.Proxy{Scheme: "socks5", Host: "1.2.3.4", Port: 1080, Username: "user", Password: "pa55", Name: "1.2.3.4"},
		},
		{
			name: "名称后缀",
			line: "http://1.2.3.4:8080#香港节点",
			want: config.Proxy{Scheme: "http", Host: "1.2.3.4", Port: 8080, Name: "香港节点"},
		},
		{
			// 供应商给的清单大多是这个形状，且默认就是 socks5。
			name: "host:port:user:pass 默认按 socks5",
			line: "5.6.7.8:1080:bob:secret",
			want: config.Proxy{Scheme: "socks5", Host: "5.6.7.8", Port: 1080, Username: "bob", Password: "secret", Name: "5.6.7.8"},
		},
		{
			name: "只有主机端口",
			line: "  5.6.7.8:1080  ",
			want: config.Proxy{Scheme: "socks5", Host: "5.6.7.8", Port: 1080, Name: "5.6.7.8"},
		},
		{
			// socks5h 与我们的行为等价（域名在代理侧解析），归一成 socks5，
			// 否则会因为「协议不支持」被整行丢掉。
			name: "socks5h 归一为 socks5",
			line: "socks5h://1.2.3.4:1080",
			want: config.Proxy{Scheme: "socks5", Host: "1.2.3.4", Port: 1080, Name: "1.2.3.4"},
		},
		{
			name: "域名主机",
			line: "socks5://gate.example.com:9000",
			want: config.Proxy{Scheme: "socks5", Host: "gate.example.com", Port: 9000, Name: "gate.example.com"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseProxyLine(c.line)
			if err != nil {
				t.Fatalf("parseProxyLine(%q): %v", c.line, err)
			}
			got.ID = "" // 随机生成，不参与比较
			if got != c.want {
				t.Errorf("parseProxyLine(%q)\n got %+v\nwant %+v", c.line, got, c.want)
			}
		})
	}
}

func TestParseProxyLineRejects(t *testing.T) {
	for _, line := range []string{
		"",
		"1.2.3.4",               // 缺端口
		"1.2.3.4:abc",           // 端口不是数字
		"1.2.3.4:99999",         // 端口越界
		"1.2.3.4:1080:onlyuser", // 三段，既不是 2 也不是 4
		"ftp://1.2.3.4:21",      // 协议不支持
	} {
		if p, err := parseProxyLine(line); err == nil {
			t.Errorf("parseProxyLine(%q) 应该报错，却得到 %+v", line, p)
		}
	}
}

// newProxyServer 造一个只带代理池与账号的 Server：桥接与 env 注入都只读配置，
// 不需要 docker 或 store 的写路径。
func newProxyServer(accountProxy string, proxies []config.Proxy) *Server {
	return &Server{
		cfg: &config.Config{
			AuthToken: "a-long-enough-token",
			Accounts:  []config.Account{{ID: "claude-1", Type: "claude", Label: "A", ProxyID: accountProxy}},
			Proxies:   proxies,
			ProxyBridge: config.ProxyBridgeConfig{
				Bind: "172.17.0.1:1081", Host: "172.17.0.1",
			},
		},
	}
}

var testProxies = []config.Proxy{
	{ID: "px1", Name: "香港", Scheme: "socks5", Host: "1.2.3.4", Port: 1080, Username: "u", Password: "p"},
}

func TestProxyEnvListInjectsBothCases(t *testing.T) {
	s := newProxyServer("px1", testProxies)
	sess := store.Session{ID: "s1", User: "alice", AccountID: "claude-1"}

	env := map[string]string{}
	for _, kv := range s.proxyEnvList(sess) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	want := "http://claude-1:" + s.proxySecret("claude-1") + "@172.17.0.1:1081"
	// 大小写两套都得给：curl 只认小写 http_proxy，Node/Go 读大写，漏一半的
	// 后果是「有些请求悄悄走了服务器自己的 IP」，而不是报错。
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if !strings.Contains(env["NO_PROXY"], "127.0.0.1") || !strings.Contains(env["NO_PROXY"], "172.17.0.1") {
		t.Errorf("NO_PROXY = %q，本机与网桥网关必须排除", env["NO_PROXY"])
	}
	// proxyEnvNames 是终端 tmux 清理时的依据，漏一个就会留下过期的代理变量。
	for k := range env {
		found := false
		for _, n := range proxyEnvNames {
			if n == k {
				found = true
			}
		}
		if !found {
			t.Errorf("注入了 %q 但它不在 proxyEnvNames 里", k)
		}
	}
}

func TestProxyEnvListEmptyWithoutBinding(t *testing.T) {
	s := newProxyServer("", testProxies)
	sess := store.Session{ID: "s1", User: "alice", AccountID: "claude-1"}
	if env := s.proxyEnvList(sess); len(env) != 0 {
		t.Errorf("没绑定代理的账号不该被注入代理变量: %v", env)
	}
}

// 绑定了但代理停用/不存在时仍然注入：这时候请求应该失败，而不是退回直连把
// 服务器真实 IP 交出去——那正是绑代理要防的事。
func TestProxyEnvListKeepsInjectingWhenProxyBroken(t *testing.T) {
	s := newProxyServer("px1", []config.Proxy{
		{ID: "px1", Name: "香港", Scheme: "socks5", Host: "1.2.3.4", Port: 1080, Disabled: true},
	})
	sess := store.Session{ID: "s1", User: "alice", AccountID: "claude-1"}
	if env := s.proxyEnvList(sess); len(env) == 0 {
		t.Error("代理停用时也必须注入，绝不能静默改走直连")
	}
}

func TestProxySecretIsStableAndPerAccount(t *testing.T) {
	s := newProxyServer("px1", testProxies)
	a := s.proxySecret("claude-1")
	if a != s.proxySecret("claude-1") {
		t.Error("同一账号两次取到的密钥不一致")
	}
	if a == s.proxySecret("codex-1") {
		t.Error("不同账号必须得到不同密钥，否则容器之间能互相冒用出口 IP")
	}
	if len(a) != 32 {
		t.Errorf("密钥长度 = %d, want 32", len(a))
	}
}

func TestBridgeAuthRejectsBadCredentials(t *testing.T) {
	s := newProxyServer("px1", testProxies)

	cases := []struct {
		name string
		user string
		pass string
		want int
	}{
		{"无凭证", "", "", http.StatusProxyAuthRequired},
		{"密钥错误", "claude-1", "wrong", http.StatusProxyAuthRequired},
		{"账号不存在", "nobody", s.proxySecret("nobody"), http.StatusProxyAuthRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodConnect, "https://api.anthropic.com:443", nil)
			if c.user != "" {
				r.SetBasicAuth(c.user, c.pass)
				r.Header.Set("Proxy-Authorization", r.Header.Get("Authorization"))
				r.Header.Del("Authorization")
			}
			w := httptest.NewRecorder()
			if _, ok := s.bridgeAuth(w, r); ok {
				t.Fatal("不该通过认证")
			}
			if w.Code != c.want {
				t.Errorf("status = %d, want %d", w.Code, c.want)
			}
		})
	}
}

func TestBridgeAuthResolvesBoundProxy(t *testing.T) {
	s := newProxyServer("px1", testProxies)
	r := httptest.NewRequest(http.MethodConnect, "https://api.anthropic.com:443", nil)
	r.SetBasicAuth("claude-1", s.proxySecret("claude-1"))
	r.Header.Set("Proxy-Authorization", r.Header.Get("Authorization"))
	r.Header.Del("Authorization")

	w := httptest.NewRecorder()
	p, ok := s.bridgeAuth(w, r)
	if !ok {
		t.Fatalf("认证应通过, status=%d body=%s", w.Code, w.Body.String())
	}
	if p.ID != "px1" {
		t.Errorf("解析到的代理 = %q, want px1", p.ID)
	}
}

// 停用的代理不能被桥接放行，否则「停用」在容器这条路径上形同虚设。
func TestBridgeAuthRefusesDisabledProxy(t *testing.T) {
	s := newProxyServer("px1", []config.Proxy{
		{ID: "px1", Scheme: "socks5", Host: "1.2.3.4", Port: 1080, Disabled: true},
	})
	r := httptest.NewRequest(http.MethodConnect, "https://api.anthropic.com:443", nil)
	r.SetBasicAuth("claude-1", s.proxySecret("claude-1"))
	r.Header.Set("Proxy-Authorization", r.Header.Get("Authorization"))
	r.Header.Del("Authorization")

	w := httptest.NewRecorder()
	if _, ok := s.bridgeAuth(w, r); ok {
		t.Fatal("停用的代理不该放行")
	}
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

// 账号解绑后，终端里 tmux 的全局环境必须把代理变量清掉；只写不清的话，标签页
// 一直开着的终端会永远用着已经取消的出口。
func TestTmuxEnvSyncUnsetsProxyVars(t *testing.T) {
	script := tmuxEnvSync([]string{"ANTHROPIC_BASE_URL=https://x"})
	for _, k := range proxyEnvNames {
		if !strings.Contains(script, "tmux set-environment -gu "+k+";") {
			t.Errorf("缺少对 %s 的清除指令: %s", k, script)
		}
	}

	// 有值时应该是 set 而不是 unset。
	withProxy := tmuxEnvSync([]string{"HTTPS_PROXY=http://u:p@172.17.0.1:1081"})
	if strings.Contains(withProxy, "-gu HTTPS_PROXY;") {
		t.Errorf("HTTPS_PROXY 有值却被清除: %s", withProxy)
	}
	if !strings.Contains(withProxy, "tmux set-environment -g HTTPS_PROXY 'http://u:p@172.17.0.1:1081'") {
		t.Errorf("HTTPS_PROXY 没被正确写入: %s", withProxy)
	}
}
