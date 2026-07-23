# 10 元域名 + 99 元 VPS：自建 Grok 中转站全流程

> 适合发 X 长文 / 收藏自用。  
> 你只需要：买域名、买海外 VPS、把域名解析过去。  
> 邮箱、网关、注册机、HTTPS 反代——整段丢给 AI 做。

---

## 先说结论

一整套跑通后，你得到的是：

1. **自己的邮箱域名**收 Grok 注册验证码  
2. **注册机**自动批量开号  
3. 号自动灌进 **grok2api 账号池**  
4. 用域名 **HTTPS 反代** 出去，变成 OpenAI 兼容中转  
5. Claude Code / Cursor / 任意客户端，改 Base URL + Key 就能用  

大概成本：

| 项目 | 费用 | 说明 |
|------|------|------|
| 域名 | ≈ **10 元/年** | Spaceship 等，`.xyz` 很便宜 |
| 海外 VPS | ≈ **99 元/年** | 腾讯云轻量海外（香港/新加坡） |
| 邮箱 | ≈ 0 | [Cloud Mail](https://github.com/maillab/cloud-mail) 跑在 Cloudflare Workers |
| SSL / 反代 | 0 | Caddy 自动签 Let's Encrypt |
| 号 | 时间成本 | 注册机自动灌，不用手点 |

用到的开源项目：

| 用途 | 仓库 |
|------|------|
| 中转网关 | [chenyme/grok2api](https://github.com/chenyme/grok2api) |
| 注册机 | [yunfanxing6/grok-register](https://github.com/yunfanxing6/grok-register) |
| 自建邮箱 | [maillab/cloud-mail](https://github.com/maillab/cloud-mail) |

---

## 架构长什么样

```text
你的 Claude / Cursor / curl
        │  OpenAI 兼容：/v1/chat/completions
        ▼
https://grok2api.你的域名     ← Caddy HTTPS 反代（对外只开 443）
        │
        ▼
grok2api（Docker，本机 127.0.0.1）  ← 账号池 + 管理后台
        ▲
        │  注册成功自动上传 SSO / Build OAuth
        │
grok-register（同机 systemd）
        │
        ├── 收验证码 ──► Cloud Mail（Cloudflare Workers + 你的域名）
        └── 清障：WARP + Privoxy + FlareSolverr
```

一句话：

> **Cloud Mail 产邮箱 → 注册机开号 → 自动进 grok2api → 域名反代当中转。**

---

## 第 0 步：你自己要做的（就这些）

后面全是「复制提示词给 AI」。只有买东西和 DNS 需要你动手。

### 0.1 Spaceship 买 10 元域名

1. 打开 [Spaceship](https://www.spaceship.com/) 注册登录  
2. 搜一个便宜后缀：`.xyz` / `.top` 等，一年十几块人民币很常见  
3. 结账，记下域名，例如 `example.xyz`  

> 更省事的做法：域名 **Nameserver 指到 Cloudflare**。  
> 后面 Cloud Mail 和 DNS 都在 CF 里配，一条龙。

### 0.2 腾讯云 99 元/年海外 VPS

1. 腾讯云 → **轻量应用服务器** → 选 **海外**（香港 / 新加坡）  
2. 套餐约 **99 元/年**  
3. 系统：**Ubuntu 22.04 或 24.04**  
4. 记下：
   - 公网 IP（下文写 `<VPS_IP>`）
   - root 密码 或 SSH 密钥  
5. 防火墙 / 安全组只放行：**22、80、443**  
   - **不要**把 8000/8001 直接裸奔公网  

SSH 测一下：

```bash
ssh root@<VPS_IP>
```

### 0.3 DNS：先把中转子域名指过去

在 Cloudflare / Spaceship DNS 里加：

| 类型 | 主机记录 | 值 | 用途 |
|------|----------|-----|------|
| A | `grok2api` | `<VPS_IP>` | 中转站入口 |

Cloudflare 用户注意：

- `grok2api` 子域建议 **DNS only（灰云）**  
- 让 Caddy 自己签证书最省事，少踩橙云证书坑  

邮箱相关的 MX / SPF 等，下一步让 AI 按 Cloud Mail 文档加。

你现在应该有：

- 能 SSH 的海外 Ubuntu VPS  
- 域名 `example.xyz`  
- `grok2api.example.xyz` → VPS IP  

---

## 第 1 步：部署 Cloud Mail（自建收信邮箱）

项目：[maillab/cloud-mail](https://github.com/maillab/cloud-mail)  
文档：[doc.skymail.ink](https://doc.skymail.ink)

作用：用**你自己的域名**批量生成邮箱，给 Grok 注册收验证码。  
比公共临时邮箱稳，也更适合长期跑注册机。

### 你需要

- 域名已接到 **Cloudflare**（NS 指向 CF）  
- Cloudflare 账号（Workers / D1 免费档个人够用）

### 提示词：整段丢给 AI

```text
请帮我部署 Cloud Mail 自建邮箱。

项目：https://github.com/maillab/cloud-mail
文档：https://doc.skymail.ink

我的信息：
- 主域名：<example.xyz>
- 邮箱域名（用来注册收信）：<edu.example.xyz 或 mail.example.xyz>
- Cloudflare 账号已登录，Workers 可用

要求：
1. 按官方文档部署到 Cloudflare Workers（界面或 wrangler 均可）
2. 配置收信 DNS：MX / SPF 等，严格按文档
3. 创建管理员账号，打开可用的开放 API
4. 最后用表格给我：
   - 管理后台地址
   - API 根地址（必须带 /api，例如 https://xxx.workers.dev/api）
   - 管理员邮箱
   - 管理员密码（若让我自设就写「你自设」）
   - 邮箱域名 EMAIL_DOMAIN
5. 用 API 测一遍：登录拿 JWT → 创建一个测试邮箱 → 能列出邮件

注意：
- 注册机用的认证头是「裸 JWT」：Authorization: <token>
  （不要加 Bearer 前缀）
- 不要把真实密码写进 git / 公开仓库
```

部署成功后你手里应有：

```text
EMAIL_API=https://cloud-mail.xxx.workers.dev/api
EMAIL_DOMAIN=edu.example.xyz
CLOUDMAIL_ADMIN_EMAIL=admin@...
CLOUDMAIL_ADMIN_PASSWORD=...
```

---

## 第 2 步：VPS 上部署 grok2api + 域名反代

项目：[chenyme/grok2api](https://github.com/chenyme/grok2api)

作用：

- 多账号 Grok 网关  
- 对外 **OpenAI 兼容**（`/v1/chat/completions`、`/v1/models` …）  
- 自带管理后台和账号池  

### 提示词：部署网关 + Caddy HTTPS

```text
我有一台 Ubuntu 海外 VPS，请帮我部署 chenyme/grok2api，并用域名做 HTTPS 反代。

SSH：ssh root@<VPS_IP>
域名：https://grok2api.<example.xyz>
（DNS A 记录已指向该 IP，Cloudflare 建议灰云）
项目：https://github.com/chenyme/grok2api

要求：
1. 安装 Docker + Docker Compose（没有就装）
2. clone 到 /opt/grok2api-chenyme（或 /opt/grok2api）
3. 按官方方式写 config / compose：
   - 设置 bootstrapAdmin 用户名密码（随机强密码，结果用表格告诉我）
   - 数据卷持久化
4. 启动后只监听本机，例如：
   127.0.0.1:8001 → 容器 8000
   不要 0.0.0.0 裸奔公网
5. 安装 Caddy，配置：
   grok2api.<example.xyz> {
     reverse_proxy 127.0.0.1:8001
   }
   自动 HTTPS；系统防火墙只开 22/80/443
6. 若机器直连 x.ai 不稳，按项目文档接 WARP / 代理
7. 最后表格输出：
   - 管理后台 URL
   - 管理员账号密码
   - API Base（https://grok2api.xxx/v1）
   - 如何在后台创建客户端 API Key
   - 健康检查 curl

验证：
curl -sS https://grok2api.<example.xyz>/healthz
浏览器能打开管理后台即可。
```

成功后你手里应有：

- 管理后台：`https://grok2api.你的域名`  
- 管理员账号 / 密码  
- 客户端 Key（后台创建，形如 `g2a_...`）  
- 本机端口（示例：`127.0.0.1:8001`）  

这时中转壳子已经好了——只是池子里还没号。下一步用注册机自动灌。

---

## 第 3 步：同机部署注册机，自动灌号进 grok2api

项目：[yunfanxing6/grok-register](https://github.com/yunfanxing6/grok-register)

一条流水线干完：

1. Cloud Mail 开邮箱  
2. 过 Turnstile / CF，注册 Grok  
3. 拿 SSO + Device OAuth  
4. **自动上传到本机 grok2api 账号池**  

### 提示词：装注册机 + 自动上传

```text
请在同一台 VPS 上部署 Grok 注册机，注册成功后自动上传到本机 grok2api。

仓库：https://github.com/yunfanxing6/grok-register
SSH：ssh root@<VPS_IP>
系统：Ubuntu

按仓库 README「完整部署」安装：
- 系统依赖 + Go（仅编译用）+ Docker
- make build && make install → /usr/local/bin/grok
- Playwright + CloakBrowser venv（Turnstile 必做）
- 清障栈：cd clearance && docker compose up -d
  （WARP + Privoxy + FlareSolverr，本机常见端口 40080 / 8191）

邮箱用 Cloud Mail：
  EMAIL_MODE=cloudmail
  EMAIL_API=<https://xxx.workers.dev/api>
  EMAIL_DOMAIN=<edu.example.xyz>
  CLOUDMAIL_ADMIN_EMAIL=<admin@...>
  CLOUDMAIL_ADMIN_PASSWORD=<密码>
  # Workers 访问不稳时可加 CLOUDMAIL_PROXY

自动上传到本机 grok2api（走 127.0.0.1，不要走 WARP）：
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

可选：用 systemd 挂成长期服务，目标无限，单线程稳一点：
  ExecStart=/usr/local/bin/grok --worker --target 0 --threads 1

验证顺序：
1. 清障栈 docker compose ps 为 healthy
2. Turnstile 能 mint 出 token
3. grok start -t 1 --thread 1
4. grok logs -f 看到「注册成功」+ [g2a/chenyme] uploaded / imported
5. 打开 grok2api 管理后台，账号池多了号

先跑通 1 个，再 grok start -t 20 --thread 2。
不要把 config.env、SSO、密码提交到 git。
```

### 日常命令

```bash
grok start -t 10 --thread 2   # 目标 10 个成功号，2 并发
grok status
grok logs -f
grok stop
```

产物一般在：`~/.grok/outputs/<时间戳>/{SSO,CPA}/`  
开了 `G2A_CHENYME_*` 后，多数时候不用手导——后台池子自己涨。

### 小彩蛋（稳定性）

xAI 前端发版时，注册页的 Next.js **Server Action ID** 会变，  
旧进程可能连续 `signup http=404 Server action not found`。  

新版注册机会在检测到这类错误时 **自动重抓 Action ID**（约 90 秒冷却），  
一般不用你再 `systemctl restart`。

---

## 第 4 步：接到 Claude Code / Cursor / 任意客户端

### 提示词：写进本机工具

```text
我的 grok2api 已可用：
Base URL: https://grok2api.<example.xyz>/v1
API Key: <g2a_xxx>
模型：以后台 /v1/models 列表为准（如 grok-4 等）

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

有正常 JSON 回复，就通了。

---

## 推荐落地顺序（打勾清单）

1. [ ] Spaceship 域名已买（约 10 元）  
2. [ ] 腾讯云海外 VPS 已开（约 99 元/年），22/80/443 放行  
3. [ ] `grok2api.域名` A 记录 → VPS IP（灰云更省事）  
4. [ ] Cloud Mail 部署完成，API + 管理员 + 邮箱域名可用  
5. [ ] VPS：Docker + grok2api 健康，只绑 127.0.0.1  
6. [ ] Caddy 反代 HTTPS，管理后台能打开  
7. [ ] 注册机 + CloakBrowser + clearance 栈就绪  
8. [ ] `grok start -t 1` 成功，且后台出现新账号  
9. [ ] 客户端 Base URL + Key 调通  

---

## 常见坑

| 现象 | 怎么处理 |
|------|----------|
| Turnstile 一直 timeout | CloakBrowser 装没装；清障栈是否 healthy；代理是不是 `127.0.0.1:40080` |
| 注册成功但上传 g2a 失败 | `G2A_CHENYME_BASE` 必须是 `http://127.0.0.1:端口`，且写进 `NO_PROXY` |
| Cloud Mail 登录/收信失败 | API 要带 `/api`；JWT **不要**加 `Bearer`；查 MX / SPF |
| Caddy 签证书失败 | 域名是否灰云、是否指对 IP；80/443 是否放行 |
| 管理后台能开、API 401 | 用后台创建的 **客户端 Key**（`g2a_...`），不是管理员密码 |
| 号很快 401 / 403 | 出口 IP 质量差 → 给网关接 WARP/干净代理；降并发 |
| 连续 `Server action not found` | 拉最新注册机（会自动刷新 Action ID），或重启服务重抓配置 |

---

## 安全提醒（必看）

- 管理员密码、Cloud Mail 密码、`g2a_` Key **当密码保管**  
- 不要把 `config.env`、SSO、CPA JSON 推到公开 GitHub  
- 生产建议：grok2api **只监听 127.0.0.1**，外网只走 Caddy 443  
- 本教程仅供技术学习；请遵守 xAI / 当地法律法规与服务条款  

---

## 一句话收尾

> **10 元域名 + 99 元海外 VPS**  
> + Cloud Mail 收信  
> + grok-register 自动灌号  
> + chenyme/grok2api 账号池  
> + Caddy 域名反代  

= 你自己的 Grok OpenAI 兼容中转站。

号不够就：

```bash
grok start -t 20 --thread 2
```

注册机会继续往池子里灌。

---

## 传送门

- 注册机：https://github.com/yunfanxing6/grok-register  
- 网关：https://github.com/chenyme/grok2api  
- 邮箱：https://github.com/maillab/cloud-mail  
- 邮箱文档：https://doc.skymail.ink  
- 更完整的仓库内教程：https://github.com/yunfanxing6/grok-register/blob/main/docs/TUTORIAL.zh-CN.md  

---

*发 X 时建议：开头用成本 + 架构图抓住注意力；中间每步只贴一条「提示词」；文末放三个 GitHub 链接。敏感信息一律用占位符，别晒真实 Key / 密码 / 服务器 IP。*
