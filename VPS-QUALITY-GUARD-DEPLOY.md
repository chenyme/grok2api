# VPS 部署：Grok2API + 出口质量守护

本文部署的是当前仓库里的增强版 Grok2API，以及
tools/egress-quality-guard 质量守护 sidecar。

## 1. 部署结构

~~~text
浏览器 / API 客户端
        |
    Nginx + HTTPS
        |
  127.0.0.1:8000
        |
  grok2api 容器 <---- quality_guard_state 卷 ----> egress-quality-guard 容器
        |
    Grok Build 出口节点
~~~

质量守护页面在 Grok2API 管理端里，sidecar 不监听端口，也不应该直接暴露到公网。
两个容器必须使用同一个 quality_guard_state 卷。

## 2. 前置条件

以下命令以 Ubuntu/Debian VPS 为例，需要一个有 sudo 权限的用户：

~~~bash
sudo apt update
sudo apt install -y ca-certificates curl git openssl jq nginx certbot python3-certbot-nginx
curl -fsSL https://get.docker.com | sudo sh
sudo systemctl enable --now docker
sudo docker compose version
~~~

VPS 需要能够访问 Grok 上游和你配置的 HTTP/SOCKS 代理。域名的 DNS 记录先指向 VPS 的公网 IP。

## 3. 把增强版源码放到 VPS

当前质量守护代码位于 egress-enhancements 分支。基础仓库的 main 或官方镜像不一定包含本功能。

推荐把当前分支推送到你自己的 GitHub 私有仓库，然后在 VPS 执行：

~~~bash
# 在本机项目目录执行；remote-name 换成你自己的远程仓库名
git push remote-name egress-enhancements

# 在 VPS 执行
sudo mkdir -p /opt/grok2api
sudo chown "$USER":"$USER" /opt/grok2api
git clone -b egress-enhancements https://github.com/你的用户名/你的仓库.git /opt/grok2api
cd /opt/grok2api
~~~

如果分支还没有推送，也可以从本机上传当前工作树。不要上传本机的 config.yaml、data/、.env 或生产日志：

~~~bash
rsync -az \
  --exclude config.yaml \
  --exclude '.env' \
  --exclude 'data/' \
  --exclude 'frontend/node_modules/' \
  --exclude 'frontend/dist/' \
  --exclude 'tools/egress-quality-guard/.venv/' \
  /本机项目目录/ root@你的VPS_IP:/opt/grok2api/
~~~

## 4. 准备主服务配置

~~~bash
cd /opt/grok2api
cp config.example.yaml config.yaml
chmod 600 config.yaml
openssl rand -hex 32
openssl rand -base64 32
sudo nano config.yaml
~~~

在 config.yaml 中至少修改：

~~~yaml
auth:
  secureCookies: false # 完成 HTTPS 后改为 true

secrets:
  jwtSecret: "上面生成的 Hex 密钥"
  credentialEncryptionKey: "上面生成的 Base64 密钥"

bootstrapAdmin:
  username: "admin"
  password: "一个强密码"
~~~

不要更换已经写入账号后的 credentialEncryptionKey。首次登录并修改管理员密码后，删除整个
bootstrapAdmin 段。

## 5. 配置 Compose

在仓库根目录创建或编辑 .env：

~~~dotenv
GROK2API_PORT=127.0.0.1:8000
QUALITY_GUARD_ENV_FILE=/etc/grok2api-egress-quality-guard.env
TZ=Asia/Shanghai
~~~

127.0.0.1:8000 只允许 VPS 本机的 Nginx 访问，不会把管理端口直接暴露给公网。

复制质量守护配置到仓库外，并限制权限：

~~~bash
sudo install -m 0600 \
  tools/egress-quality-guard/egress-quality-guard.env.example \
  /etc/grok2api-egress-quality-guard.env
sudo nano /etc/grok2api-egress-quality-guard.env
~~~

Compose 部署时，必须把示例里的地址改成容器服务名：

~~~dotenv
GROK2API_BASE_URL=http://grok2api:8000
GROK2API_ADMIN_USERNAME=admin
GROK2API_ADMIN_PASSWORD=先填 bootstrapAdmin 的密码
QUALITY_GUARD_CLIENT_KEY_ID=1
QUALITY_GUARD_MODEL=grok-4.5

QUALITY_GUARD_MODE=hybrid
QUALITY_GUARD_ACTIVE_INTERVAL_SECONDS=1800
QUALITY_GUARD_PASSIVE_POLL_SECONDS=5
QUALITY_GUARD_SOFT_TPS=500
QUALITY_GUARD_HARD_TPS=1000
QUALITY_GUARD_CONSECUTIVE_SOFT=2
QUALITY_GUARD_CONSECUTIVE_ERRORS=2
QUALITY_GUARD_QUARANTINE_SECONDS=300
QUALITY_GUARD_MIN_HEALTHY_NODES=3

QUALITY_GUARD_STATE_FILE=/var/lib/grok2api-quality-guard/state.json
QUALITY_GUARD_LOCK_FILE=/var/lib/grok2api-quality-guard/guard.lock
QUALITY_GUARD_RUNTIME_CONFIG_FILE=/var/lib/grok2api-quality-guard/runtime-config.json
QUALITY_GUARD_INSECURE_TLS=false
~~~

GROK2API_BASE_URL=http://127.0.0.1:8000 只适用于 systemd 或宿主机直接运行，不能用于这里的
sidecar 容器；容器里的 127.0.0.1 指向 sidecar 自己。

## 6. 先只启动主服务

不要立刻启动质量守护，因为此时 Client Key ID 和模型还没有确认：

~~~bash
cd /opt/grok2api
docker compose \
  -f docker-compose.yml \
  -f tools/egress-quality-guard/compose.override.example.yml \
  config --quiet

docker compose \
  -f docker-compose.yml \
  -f tools/egress-quality-guard/compose.override.example.yml \
  up -d --build grok2api

curl -fsS http://127.0.0.1:8000/healthz
~~~

第一次构建会在 VPS 上编译 Go 后端和 React 前端，耗时可能较长。这里使用的是当前工作树构建的
grok2api-egress-enhanced:local，不要用只有基础 Compose 文件的 docker compose pull 替代它。

## 7. 首次登录和 HTTPS

如果还没有域名反向代理，可以用 SSH 隧道临时访问：

~~~bash
ssh -N -L 8000:127.0.0.1:8000 root@你的VPS_IP
~~~

然后在本机浏览器打开 http://127.0.0.1:8000。有域名时建议直接配置 Nginx，示例配置如下：

~~~nginx
server {
    listen 80;
    server_name api.example.com;

    client_max_body_size 32m;

    location / {
        proxy_pass http://127.0.0.1:8000;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 2h;
        proxy_send_timeout 2h;
    }
}
~~~

保存到 /etc/nginx/sites-available/grok2api 后启用：

~~~bash
sudo ln -s /etc/nginx/sites-available/grok2api /etc/nginx/sites-enabled/grok2api
sudo nginx -t
sudo systemctl reload nginx
sudo certbot --nginx -d api.example.com
~~~

HTTPS 正常后，回到 config.yaml 把 auth.secureCookies 改为 true，删除 bootstrapAdmin，然后重启主服务：

~~~bash
docker compose \
  -f docker-compose.yml \
  -f tools/egress-quality-guard/compose.override.example.yml \
  up -d grok2api
~~~

首次登录后在管理端修改管理员密码。修改密码后，必须同步修改
/etc/grok2api-egress-quality-guard.env 中的 GROK2API_ADMIN_PASSWORD，再启动 sidecar。

## 8. 创建专用探测 Client Key

在管理端进入“客户端密钥”，新建一个只给质量守护使用的 Key，例如 quality-guard-probe：

- 渠道范围只选 Grok Build；
- 模型范围选择实际要探测的 Build 模型，或暂时允许全部模型；
- 保持 Key 启用且不过期；
- 本地计费限制设为无限制；
- RPM 和最大并发设置为足够值，不要复用普通用户 Key。

质量守护使用的是这个 Key 的数字 ID，不使用它的 g2a_... secret。管理端表格不直接显示数字 ID，
可以用管理员 API 查询。先在 VPS 安装 jq，再执行：

~~~bash
read -r -p "管理员用户名: " ADMIN_USERNAME
read -r -s -p "管理员密码: " ADMIN_PASSWORD
printf '\n'

ADMIN_TOKEN="$(
  jq -n --arg u "$ADMIN_USERNAME" --arg p "$ADMIN_PASSWORD" \
    '{username:$u,password:$p}' |
  curl -fsS http://127.0.0.1:8000/api/admin/v1/auth/login \
    -H 'Content-Type: application/json' --data-binary @- |
  jq -r '.data.tokens.accessToken'
)"

curl -fsS \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  'http://127.0.0.1:8000/api/admin/v1/client-keys?page=1&pageSize=200' |
  jq -r '.data.items[] | select(.name == "quality-guard-probe") | .id'

unset ADMIN_USERNAME ADMIN_PASSWORD ADMIN_TOKEN
~~~

把输出的数字写入 QUALITY_GUARD_CLIENT_KEY_ID。如果你使用域名，也可以把两个
http://127.0.0.1:8000 替换为 https://api.example.com。

QUALITY_GUARD_MODEL 必须是管理端“模型”页面里已启用、并且这些 Build 账号都能调用的公开模型 ID。

## 9. 添加出口并绑定账号

在“设置 -> 出口代理”或“质量守护”页面：

1. 新建至少 3 个 Grok Build 出口节点；
2. 每个节点填写独立的 HTTP/SOCKS 代理地址并保持启用；
3. 在账号页面把可使用目标模型的 Build 账号明确绑定到对应节点；
4. 先手动测试节点，确认出口代理可连通且账号绑定正确。

默认 QUALITY_GUARD_MIN_HEALTHY_NODES=3，少于 3 个可用节点时它会拒绝继续隔离，避免把整个账号池打空。
如果你确实只部署 1 或 2 个节点，应把这个值改成不超过实际受管节点数量，但生产上不建议这样做。

如果只想守护部分节点，可设置逗号分隔的 ID：

~~~dotenv
QUALITY_GUARD_NODE_IDS=1,2,3
~~~

留空则守护所有启用的、配置了代理的 grok_build 节点，以及此前由守护程序隔离的节点。

## 10. 启动质量守护

确认管理员密码、Client Key ID、模型和节点都已填写后：

~~~bash
cd /opt/grok2api

docker compose \
  -f docker-compose.yml \
  -f tools/egress-quality-guard/compose.override.example.yml \
  config --quiet

docker compose \
  -f docker-compose.yml \
  -f tools/egress-quality-guard/compose.override.example.yml \
  up -d --build grok2api egress-quality-guard

docker compose \
  -f docker-compose.yml \
  -f tools/egress-quality-guard/compose.override.example.yml \
  ps

docker compose \
  -f docker-compose.yml \
  -f tools/egress-quality-guard/compose.override.example.yml \
  logs --tail=100 egress-quality-guard
~~~

进入管理端左侧“质量守护”：

- “守护服务”应显示运行正常；
- 能看到受管节点和最近探测时间；
- 每个节点应显示正常、可疑或已隔离状态；
- 首次被动审计只建立基线，不会回放历史请求；
- hybrid 会读取真实请求审计，并按主动探测间隔逐节点检测。

## 11. 日常运维

定义一个方便重复使用的 Compose 函数：

~~~bash
dc() {
  docker compose \
    -f docker-compose.yml \
    -f tools/egress-quality-guard/compose.override.example.yml \
    "$@"
}
~~~

常用命令：

~~~bash
dc ps
dc logs --tail=200 grok2api
dc logs --tail=200 egress-quality-guard
dc restart egress-quality-guard
~~~

升级前备份 config.yaml 和 Compose 数据卷。更新源码后重新构建：

~~~bash
git pull
dc config --quiet
dc up -d --build grok2api egress-quality-guard
~~~

不要执行 docker compose down -v，它会删除数据库、媒体和质量守护状态卷。

## 12. 常见问题

### 页面显示“质量守护尚未连接”

检查主服务是否使用了增强版 Compose override，以及主服务环境变量和 sidecar 是否指向同一个状态卷：

~~~bash
dc config | sed -n '/grok2api:/,/^[^ ]/p'
dc ps
dc logs --tail=200 egress-quality-guard
~~~

主服务必须有：

~~~text
QUALITY_GUARD_STATE_FILE=/var/lib/grok2api-quality-guard/state.json
QUALITY_GUARD_RUNTIME_CONFIG_FILE=/var/lib/grok2api-quality-guard/runtime-config.json
~~~

### sidecar 一直重启

优先检查：

- GROK2API_BASE_URL 是否为 http://grok2api:8000；
- 管理员密码是否已经同步到 /etc/grok2api-egress-quality-guard.env；
- QUALITY_GUARD_CLIENT_KEY_ID 是否是数字、Key 是否启用且未过期；
- QUALITY_GUARD_MODEL 是否是可用的 Build 模型；
- 节点是否为 grok_build、已配置代理并绑定了账号。

### 探测失败或节点没有数据

质量探测只支持配置了代理的 Grok Build 节点，且每个节点需要明确绑定能够调用目标模型的账号。
先在管理端逐节点手动测试，再查看 sidecar 日志。

### 流式响应卡住或很慢

检查 Nginx 的 proxy_buffering off、proxy_read_timeout 2h，并确认公网防火墙只放行 80/443，
没有把 8000 端口暴露到互联网。

### 为什么不能只执行 docker compose pull

基础 Compose 文件使用官方镜像。质量守护接口、管理页面和状态卷接入需要当前增强源码构建的主镜像，
所以要使用本文的两个 Compose 文件并执行 up -d --build。

## 13. 安全边界

- 不要把 /etc/grok2api-egress-quality-guard.env、config.yaml、数据库、账号导出、Cookie 或日志提交到 Git；
- 质量守护 sidecar 不暴露端口；
- 管理端和 /api/admin/* 只通过 HTTPS 访问；
- auth.secureCookies 在 HTTPS 生产环境保持 true；
- 为质量守护单独创建 Client Key；
- 调整阈值后先观察日志，Token/s 是启发式信号，不是模型质量的绝对证明。

grok-register-panel 是独立的账号注册辅助项目，不是质量守护运行必需组件；只部署本文所述防降质面板时不需要启动它。
