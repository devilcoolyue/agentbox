#!/bin/sh
# 为 agentbox 账号池做一份【独立】的 Claude 登录。
#
# 每次登录都是一条独立的 OAuth 授权链（各自有各自的刷新令牌），
# 因此这份凭证专供 agentbox 使用，与宿主机日常 /root/.claude 的登录
# 互不干扰、互不作废。
#
# 用法：bash scripts/claude-pool-login.sh
#   1. 会用独立 HOME 打开 Claude Code，按提示完成订阅登录
#      （无浏览器环境会给出 URL，本地浏览器打开后把授权码粘回来）
#   2. 登录成功后退出 Claude Code（/exit 或 Ctrl+C）
#   3. 脚本自动把凭证发布到 accounts/claude-1/，容器下次拉起即生效
#
# 注意：之后不要再用这个 LOGIN_HOME 跑 claude，令牌链由账号池接管。
set -e
DIR="$(cd "$(dirname "$0")/.." && pwd)"
LOGIN_HOME="$DIR/accounts/.claude-login-home"
POOL="$DIR/accounts/claude-1"

mkdir -p "$LOGIN_HOME"
echo ">>> 打开 Claude Code 完成登录（登录完成后退出即可）..."
HOME="$LOGIN_HOME" claude || true

CRED="$LOGIN_HOME/.claude/.credentials.json"
if [ ! -f "$CRED" ]; then
  echo "!!! 未找到 $CRED，登录似乎没有完成，请重跑脚本" >&2
  exit 1
fi
cp -f "$CRED" "$POOL/.credentials.json"
rm -f "$CRED"   # 池子接管令牌链，登录 HOME 里不留副本
echo ">>> 已发布到 $POOL/.credentials.json"
echo ">>> 重启会话容器（或直接发一轮对话）即可用新凭证"
