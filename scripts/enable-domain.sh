#!/usr/bin/env bash
# HTTPS 域名启用一键脚本。
# 前提：DNS 已解析（在 DNS 服务商为 PROD_DOMAIN 加 A 记录指向本机公网 IP）。
# 做四件事：签证书 -> 部署 HTTPS 反代 -> agentbox 回绑 127.0.0.1 -> 关闭公网端口。
#
# 参数来自 deploy/production.env（见 deploy/production.env.example），也可直接用
# 环境变量覆盖：PROD_DOMAIN（必填）、PROD_LISTEN（默认 127.0.0.1:8180）。
set -euo pipefail
cd "$(dirname "$0")/.."
APP_DIR=$(pwd -P)

# 载入生产参数（存在才加载；也允许纯环境变量方式）。
if [[ -f deploy/production.env ]]; then
    # shellcheck disable=SC1091
    source deploy/production.env
fi

DOMAIN=${PROD_DOMAIN:?请在 deploy/production.env 设置 PROD_DOMAIN 或通过环境变量传入}
LISTEN=${PROD_LISTEN:-127.0.0.1:8180}
PORT=${LISTEN##*:}
CONF=/etc/nginx/conf.d/$DOMAIN.conf

if [[ ! -f /etc/systemd/system/agentbox.service ]]; then
    echo "错误：agentbox systemd 单元未安装，请先跑 ./deploy/install.sh" >&2
    exit 1
fi

echo "==> 1/4 检查 DNS ($DOMAIN)"
if ! getent hosts "$DOMAIN" >/dev/null; then
    echo "错误：$DOMAIN 尚未解析，请先在 DNS 服务商添加指向本机公网 IP 的 A 记录" >&2
    exit 1
fi

echo "==> 2/4 签发证书 (certbot webroot)"
if [ ! -e "/etc/letsencrypt/live/$DOMAIN/fullchain.pem" ]; then
    certbot certonly --webroot -w /var/www/certbot -d "$DOMAIN" \
        --non-interactive --agree-tos --keep-until-expiring
fi

echo "==> 3/4 部署完整 nginx 配置"
# heredoc 不加引号以展开 $DOMAIN/$PORT；nginx 自身的变量用 \$ 转义保留。
cat > "$CONF" <<NGINX
server {
    listen 80;
    listen [::]:80;
    server_name $DOMAIN;

    location ^~ /.well-known/acme-challenge/ {
        root /var/www/certbot;
        default_type text/plain;
    }

    location / {
        return 301 https://\$host\$request_uri;
    }
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name $DOMAIN;

    ssl_certificate /etc/letsencrypt/live/$DOMAIN/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/$DOMAIN/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_cache shared:SSL:10m;
    ssl_session_timeout 1d;
    ssl_session_tickets off;

    location / {
        proxy_pass http://$LISTEN;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        # WebSocket（对话流 + 终端 PTY 两条通道都依赖）
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }

    underscores_in_headers on;
    # 代码包上传上限（服务端 max_upload_mb=512，留余量）
    client_max_body_size 600m;
}
NGINX
nginx -t && systemctl reload nginx

echo "==> 4/4 收紧直连入口：回绑 $LISTEN，关闭防火墙 $PORT"
python3 - "$APP_DIR/config.json" "$LISTEN" <<'EOF'
import json, sys
path, listen = sys.argv[1], sys.argv[2]
cfg = json.load(open(path))
cfg['listen'] = listen
json.dump(cfg, open(path, 'w'), ensure_ascii=False, indent=2)
EOF
# 交给 systemd 重启，绝不 pkill + nohup 裸起：那会与 Restart=always 的实例抢
# data/agentbox.lock，触发 StartLimitBurst 崩溃循环（见 install.sh 注释）。
systemctl restart agentbox
firewall-cmd --remove-port="$PORT/tcp" --permanent >/dev/null && firewall-cmd --reload >/dev/null

echo
echo "完成。访问地址：https://$DOMAIN （密码不变，见 config.json 的 auth_token）"
sleep 1.5
curl -s -o /dev/null -w "本地回环检查: %{http_code}\n" "http://$LISTEN/"
