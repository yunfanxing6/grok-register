# 手把手：自建 Grok 中转站（域名 + VPS + Cloud Mail + grok2api + 注册机）

只需要你自己买域名、买海外 VPS、把域名解析到 VPS。  
邮箱、网关、注册机、反代，全部可以丢给 AI 按下面提示词部署。

---

## 开源项目

| 用途 | 仓库 |
|------|------|
| 中转网关 | [chenyme/grok2api](https://github.com/chenyme/grok2api) |
| 注册机（Go） | [yunfanxing6/grok-register](https://github.com/yunfanxing6/grok-register) |
| 自建邮箱 | [maillab/cloud-mail](https://github.com/maillab/cloud-mail) |

---

## 大概成本

| 项目 | 费用 | 说明 |
|------|------|------|
| 域名 | 约 10 元/年 | Spaceship 等，`.xyz` 很便宜 |
| 海外 VPS | 约 99 元/年 | 腾讯云轻量海外（香港/新加坡等） |
| Cloud Mail | 几乎 0 | 部署在 Cloudflare Workers |
| SSL / 反代 | 0 | Caddy 自动签 Let's Encrypt |

整套跑通后：注册机自动灌号 → grok2api 账号池 → 域名 HTTPS 对外当 OpenAI 兼容中转。

---

## 架构一览

```text
你 / Claude Code / Cursor
        │  OpenAI 兼容 API
        ▼
  https://grok2api.你的域名   ← Caddy 反代
        │
        ▼
  grok2api (Docker :8000/8001)  ← 账号池 + 管理后台
        ▲
        │  注册成功后自动上传 SSO / Build OAuth
        │
  grok-register (同机)  ──收验证码──►  Cloud Mail (Workers)
        │
        └── 清障：WARP + Privoxy + FlareSolverr
```

下面分步：前几步你自己点几下；后面整段复制给 AI。

---

## 一、你要做的（只有这些）

### 1. Spaceship 买域名（约 10 元）

1. 打开 [Spaceship](https://www.spaceship.com/) 注册/登录  
2. 搜索一个便宜域名（推荐 `.xyz` / `.top` 等）  
3. 结账，记下域名，例如：`example.xyz`  
4. 进入域名 DNS 管理（后面要加 A / MX 记录）

> 也可以用 Cloudflare 注册域名，后续邮箱和反代更顺。

### 2. 腾讯云买 99 元/年海外轻量 VPS

1. 腾讯云轻量应用服务器 → 选 **海外**（香港 / 新加坡等）  
2. 套餐约 99 元/年，系统选 **Ubuntu 22.04 / 24.04**  
3. 记下：
   - 公网 IP（下文用 `<VPS_IP>`，示例环境是 `47.x.x.x` 这类）
   - root 密码 或 SSH 密钥  
4. 防火墙 / 安全组放行：**22、80、443**（不要把 8000/8001 裸奔公网）

### 3. DNS：把子域名指到 VPS

在域名 DNS 里加（把 `example.xyz` 换成你的）：

| 类型 | 主机记录 | 值 | 用途 |
|------|----------|-----|------|
| A | `grok2api` | `<VPS_IP>` | 中转站入口 |
| A | `@` 或 `mail` 等 | 按 Cloud Mail 文档 | 邮箱（见下一步） |

Cloudflare 用户：

- **中转站子域**建议 **DNS only（灰云）**，让 Caddy 自己签证书最省事  
- 邮箱相关记录按 [Cloud Mail 文档](https://doc.skymail.ink) 配置

准备好这三样就可以让 AI 干活：

- SSH：`ssh root@<VPS_IP>`
- 域名：`grok2api.example.xyz` 已解析
- 邮箱：Cloud Mail 的 API / 管理员 / 域名（下一步）

---

## 二、部署 Cloud Mail（自建临时邮箱）

项目：[maillab/cloud-mail](https://github.com/maillab/cloud-mail)  
官方文档：[doc.skymail.ink](https://doc.skymail.ink)

作用：用你自己的域名批量生成邮箱，给 Grok 注册收验证码，比公共临时邮箱稳。

### 你需要准备

1. 域名已接入 **Cloudflare**（Nameserver 指到 CF）  
2. 一个 Cloudflare 账号（Workers / D1 / KV 免费档通常够个人用）

### 提示词：让 AI 帮你部署 Cloud Mail

```text
请帮我部署 Cloud Mail 自建邮箱。

项目：https://github.com/maillab/cloud-mail
文档：https://doc.skymail.ink

我的信息：
- 域名：<example.xyz>
- 邮箱域名（用于注册收信）：<mail.example.xyz 或 edu.example.xyz>
- Cloudflare 账号已登录（Workers 可用）

要求：
1. 按官方文档部署到 Cloudflare Workers（界面部署或 wrangler 均可）
2. 配置收信所需的 DNS（MX / SPF 等，按文档）
3. 创建管理员账号，开启可用的开放 API
4. 最后用表格给我：
   - 管理后台地址
   - API 根地址（必须带 /api，例如 https://xxx.workers.dev/api）
   - 管理员邮箱
   - 管理员密码（让我自己填的就标「你自设」）
   - 邮箱域名 EMAIL_DOMAIN
5. 用 API 测一次：登录拿 JWT、创建一个测试邮箱用户、确认能列邮件

注意：
- 注册机认证头是裸 JWT：Authorization: <token>（不要加 Bearer）
- 不要把真实密码写进 git
```

部署成功后你手里应有：

```text
EMAIL_API=https://cloud-mail.xxx.workers.dev/api
EMAIL_DOMAIN=mail.example.xyz
CLOUDMAIL_ADMIN_EMAIL=admin@...
CLOUDMAIL_ADMIN_PASSWORD=...
```

---

## 三、VPS 上部署 grok2api + 域名反代

项目：[chenyme/grok2api](https://github.com/chenyme/grok2api)

作用：多账号 Grok 网关，对外 OpenAI 兼容（`/v1/chat/completions` 等），自带管理后台和账号池。

### 提示词 1：部署 grok2api + Caddy HTTPS

```text
我有一台 Ubuntu 海外 VPS，请帮我部署 chenyme/grok2api，并用域名 HTTPS 反代。

SSH：ssh root@<VPS_IP>
域名：https://grok2api.<example.xyz>（DNS A 记录已指向该 IP，建议灰云）
项目：https://github.com/chenyme/grok2api

要求：
1. 安装 Docker + Docker Compose（如未装）
2. clone 项目到 /opt/grok2api（或 /opt/grok2api-chenyme）
3. 按官方方式生成/编写 config.yaml：
   - 设置 bootstrapAdmin 用户名密码（随机强密码并告诉我）
   - 数据卷持久化
4. docker compose 启动，监听 127.0.0.1:8001→容器 8000（或 8000，二选一，不要裸奔 0.0.0.0 公网）
5. 安装 Caddy，配置：
   grok2api.<example.xyz> {
     reverse_proxy 127.0.0.1:8001
   }
   自动 HTTPS；只开放 22/80/443
6. 若出口 IP 不干净，按项目文档接 WARP/代理（socks5），保证能访问 x.ai
7. 最后用表格给我：
   - 管理后台 URL
   - 管理员账号密码
   - API Base（https://grok2api.xxx/v1）
   - 创建客户端 API Key 的方式
   - 健康检查与验证 curl

验证：
curl -sS https://grok2api.<example.xyz>/healthz
能打开管理后台即可进入下一步。
```

成功后你手里应有：

- 管理后台：`https://grok2api.你的域名`
- 管理员账号密码
- 客户端 API Key（后台创建，形如 `g2a_...`）
- 本地端口（示例：`127.0.0.1:8001`）

---

## 四、同机部署注册机，自动灌号进 grok2api

项目：[yunfanxing6/grok-register](https://github.com/yunfanxing6/grok-register)

作用（一条命令）：

1. Cloud Mail 开邮箱  
2. 过 Turnstile / CF，注册 Grok  
3. Device OAuth / SSO  
4. **自动上传到 grok2api 账号池**（也可上传 CPA）

### 提示词 2：VPS 上装注册机并配置自动上传

```text
请在同一台 VPS 上部署 Grok 注册机，注册成功后自动上传到本机 grok2api。

仓库：https://github.com/yunfanxing6/grok-register
SSH：ssh root@<VPS_IP>
系统：Ubuntu

依赖与安装按仓库 README「完整部署」：
- 系统库 + Go（仅编译）+ Docker
- make build && make install → /usr/local/bin/grok
- Playwright + CloakBrowser venv（Turnstile 必做）
- clearance 清障栈：cd clearance && docker compose up -d
  （WARP + Privoxy + FlareSolverr，本机 40080 / 8191）

邮箱用 Cloud Mail：
  EMAIL_MODE=cloudmail
  EMAIL_API=<https://xxx.workers.dev/api>
  EMAIL_DOMAIN=<mail.example.xyz>
  CLOUDMAIL_ADMIN_EMAIL=<admin@...>
  CLOUDMAIL_ADMIN_PASSWORD=<密码>
  # 若 Workers 在国内/特定网络访问不稳，可设 CLOUDMAIL_PROXY

grok2api 自动上传（与网关同机，走 127.0.0.1，不要走 WARP）：
  G2A_CHENYME_ENABLED=1
  G2A_CHENYME_BASE=http://127.0.0.1:8001
  G2A_CHENYME_USER=<管理后台用户名>
  G2A_CHENYME_PASSWORD=<管理后台密码>
  G2A_CHENYME_UPLOAD_SSO=1
  G2A_CHENYME_UPLOAD_BUILD=1

清障相关建议：
  CLEARANCE_ENABLED=1
  REGISTER_PROXY=http://127.0.0.1:40080
  FLARESOLVERR_URL=http://127.0.0.1:8191
  HTTPS_PROXY=http://127.0.0.1:40080
  HTTP_PROXY=http://127.0.0.1:40080
  NO_PROXY=127.0.0.1,localhost,<你的 cloud-mail host>

配置写到 /root/.grok/config.env（可用 grok config）。

验证顺序：
1. docker compose ps 清障栈 healthy
2. Turnstile 冒烟（README 里的 mint 命令）
3. grok start -t 1 --thread 1
4. grok logs -f 看到注册成功 + [g2a/chenyme] uploaded
5. 打开 grok2api 管理后台，确认账号池多了号

先跑 1 个成功，再 grok start -t 20 --thread 2（或 3）。
不要把 config.env、SSO、密码提交到 git。
```

### 日常用法

```bash
grok start -t 10 --thread 2   # 目标 10 个成功号，2 并发
grok status
grok logs -f
grok stop
```

产物目录：`~/.grok/outputs/<时间戳>/{SSO,CPA}/`  
开启 `G2A_CHENYME_*` 后一般不用手导，后台池子会自动涨。

---

## 五、接到 Claude Code / Cursor / 任意 OpenAI 客户端

### 提示词 3：写进本机工具

```text
我的 grok2api 已可用：
Base URL: https://grok2api.<example.xyz>/v1
API Key: <g2a_xxx>
模型：按后台模型列表选（如 grok-4 / grok-3 等，以 /v1/models 为准）

请探测本机已装的 Claude Code、Codex、Cursor、OpenCode 等，
把 Base URL + API Key 配进去；改之前备份配置；
每个工具给一条最短验证命令；不要把 key 写进 git。
```

### 自己 curl 抽查

```bash
curl -sS "https://grok2api.你的域名/v1/chat/completions" \
  -H "Authorization: Bearer 你的g2a_key" \
  -H "Content-Type: application/json" \
  -d '{"model":"grok-4","messages":[{"role":"user","content":"hi"}],"max_tokens":32}'
```

有正常 JSON 回复就通了。

---

## 六、推荐落地顺序（检查清单）

1. [ ] Spaceship 域名已买  
2. [ ] 腾讯云海外 VPS 已开，22/80/443 放行  
3. [ ] `grok2api.域名` A 记录 → VPS IP  
4. [ ] Cloud Mail 部署完成，API + 管理员 + 邮箱域名可用  
5. [ ] VPS：Docker + grok2api 健康  
6. [ ] Caddy 反代 HTTPS 可打开管理后台  
7. [ ] 注册机 + CloakBrowser + clearance 栈  
8. [ ] `grok start -t 1` 成功且后台出现新账号  
9. [ ] 客户端 Base URL + Key 调通  

---

## 七、常见坑

| 现象 | 处理 |
|------|------|
| Turnstile 一直 timeout | 查 CloakBrowser 是否 install；清障栈是否 healthy；`REGISTER_PROXY` 是否 40080 |
| 注册成功但 g2a 上传失败 | 确认 `G2A_CHENYME_BASE=http://127.0.0.1:端口`，且该地址在 `NO_PROXY` 里 |
| Cloud Mail 登录/收信失败 | API 必须带 `/api`；JWT **不要**加 `Bearer`；查 MX/SPF |
| Caddy 签证书失败 | 域名是否灰云指向正确 IP；80/443 是否放行；是否被橙云劫持 |
| 管理后台能开、API 401 | 用后台创建的 **客户端 Key**（`g2a_...`），不是管理员密码 |
| 号很快 401/403 | 出口 IP 质量问题，给 grok2api 接 WARP/干净代理；降低并发 |

---

## 八、安全提醒

- 管理员密码、Cloud Mail 密码、`g2a_` Key、Management Key **当密码保管**  
- 不要把 `config.env`、SSO token、CPA JSON 提交 GitHub  
- 生产环境建议：grok2api **只绑 127.0.0.1**，外网只走 Caddy 443  
- 本教程仅供技术学习；请遵守 xAI / 当地法律法规与服务条款  

---

## 九、一句话总结

> **10 元域名 + 99 元海外 VPS + Cloud Mail + grok2api + grok-register**  
> 域名反代出去，就是你自己的 Grok OpenAI 兼容中转站；  
> 号不够就 `grok start -t N`，注册机会自动往池子里灌。

仓库传送门：

- 注册机：https://github.com/yunfanxing6/grok-register  
- 网关：https://github.com/chenyme/grok2api  
- 邮箱：https://github.com/maillab/cloud-mail  
- 邮箱文档：https://doc.skymail.ink  
