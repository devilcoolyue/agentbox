package dockerx

import "testing"

// 容器 env 只能加不能减：账号从中转站切回订阅登录时，摘掉的 ANTHROPIC_AUTH_TOKEN
// 若还烘在旧容器里，claude CLI 会拿它盖掉 OAuth 凭证。所以带这类残留的容器必须
// 被认成脏的、下次拉起时重建。
func TestHasBakedEnv(t *testing.T) {
	imageEnv := []string{"PATH=/usr/bin", "NODE_VERSION=22.23.2", "HOME=/home/agent", "LANG=C.UTF-8", "DISABLE_AUTOUPDATER=1"}

	cases := []struct {
		name         string
		containerEnv []string
		want         bool
	}{
		{
			name:         "干净容器：只有 base + 镜像自带",
			containerEnv: append(baseContainerEnv(), imageEnv...),
			want:         false,
		},
		{
			name:         "烘进了中转站令牌",
			containerEnv: append(baseContainerEnv(), "ANTHROPIC_AUTH_TOKEN=sk-ant-oat01-x", "PATH=/usr/bin"),
			want:         true,
		},
		{
			name:         "只烘了 base_url 也算脏",
			containerEnv: append(baseContainerEnv(), "ANTHROPIC_BASE_URL=https://relay.example.com"),
			want:         true,
		},
		{
			// 镜像升级会改 NODE_VERSION 的值，值变不是残留，别把好容器判死。
			name:         "镜像变量换了值不算脏",
			containerEnv: []string{"PATH=/usr/bin", "NODE_VERSION=20.0.0", "HOME=/home/agent", "TERM=xterm-256color"},
			want:         false,
		},
		{
			name:         "没有等号的畸形项按整串当键",
			containerEnv: []string{"WEIRD"},
			want:         true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasBakedEnv(c.containerEnv, imageEnv); got != c.want {
				t.Errorf("hasBakedEnv(%v) = %v, 想要 %v", c.containerEnv, got, c.want)
			}
		})
	}
}
