#!/usr/bin/env bash
# agentbox.example.com 域名启用一键脚本。
# 前提：DNS 已解析（Cloudflare 加 A 记录 agentbox -> 154.219.123.71）。
# 做四件事：签证书 -> 部署 HTTPS 反代 -> agentbox 回绑 127.0.0.1 -> 关闭 8180 公网端口。
set -euo pipefail

DOMAIN=agentbox.example.com
CONF=/etc/nginx/conf.d/$DOMAIN.conf
APP_DIR="$(cd "$(dirname "$0")/.." && pwd)"

echo "==> 1/4 检查 DNS"
if ! getent hosts $DOMAIN >/dev/null; then
    echo "错误：$DOMAIN 尚未解析，请先在 Cloudflare 添加 A 记录 agentbox -> 154.219.123.71"
    exit 1
fi

echo "==> 2/4 签发证书 (certbot webroot)"
if [ ! -e /etc/letsencrypt/live/$DOMAIN/fullchain.pem ]; then
    certbot certonly --webroot -w /var/www/certbot -d $DOMAIN \
        --non-interactive --agree-tos --keep-until-expiring
fi

echo "==> 3/4 部署完整 nginx 配置"
cat > $CONF <<'NGINX'
server {
    listen 80;
    listen [::]:80;
    server_name agentbox.example.com;

    location ^~ /.well-known/acme-challenge/ {
        root /var/www/certbot;
        default_type text/plain;
    }

    location / {
        return 301 https://$host$request_uri;
    }
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name agentbox.example.com;

    ssl_certificate /etc/letsencrypt/live/agentbox.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/agentbox.example.com/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_cache shared:SSL:10m;
    ssl_session_timeout 1d;
    ssl_session_tickets off;

    location / {
        proxy_pass http://127.0.0.1:8180;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        # WebSocket（对话流 + 终端 PTY 两条通道都依赖）
        proxy_set_header Upgrade $http_upgrade;
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

echo "==> 4/4 收紧直连入口：回绑 127.0.0.1，关闭防火墙 8180"
python3 - <<EOF
import json
p='$APP_DIR/config.json'
cfg=json.load(open(p)); cfg['listen']='127.0.0.1:8180'
json.dump(cfg, open(p,'w'), ensure_ascii=False, indent=2)
EOF
pkill -x agentbox || true; sleep 1
(cd "$APP_DIR" && setsid nohup ./agentbox -config config.json >/var/log/agentbox.log 2>&1 &)
sleep 1.5
firewall-cmd --remove-port=8180/tcp --permanent >/dev/null && firewall-cmd --reload >/dev/null

echo
echo "完成。访问地址：https://$DOMAIN （密码不变，见 config.json 的 auth_token）"
curl -s -o /dev/null -w "本地回环检查: %{http_code}\n" http://127.0.0.1:8180/
