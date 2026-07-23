# 10 块域名 + 99 块 VPS，自建 Grok 中转站

整套下来大概一百出头一年。  
**你自己只做三件事**：买域名、买海外 VPS、把 Cloud Mail 邮箱搭好。  
剩下的——装网关、跑注册机、域名反代——**丢给 AI 一句提示词就行**。

用到的开源项目：

- 网关：[chenyme/grok2api](https://github.com/chenyme/grok2api)
- 注册机：[yunfanxing6/grok-register](https://github.com/yunfanxing6/grok-register)
- 邮箱：[maillab/cloud-mail](https://github.com/maillab/cloud-mail)

最终效果：注册机自动开号 → 灌进 grok2api → 域名 HTTPS 当 OpenAI 兼容中转，Claude / Cursor 改个 Base URL 就能用。

---

## 你自己做的（就这些）

### 1. 买域名（约 10 元/年）

[Spaceship](https://www.spaceship.com/) 搜个便宜的，`.xyz` 很常见。  
域名 **Nameserver 指到 Cloudflare**，后面邮箱和 DNS 都在 CF 里配。

### 2. 买海外 VPS（约 99 元/年）

腾讯云轻量 → **海外**（香港 / 新加坡）→ Ubuntu 22/24。  
记下公网 IP，安全组放行 **22、80、443**。  
能 `ssh root@你的IP` 就行。

### 3. 搭 Cloud Mail（收注册验证码）

项目：[maillab/cloud-mail](https://github.com/maillab/cloud-mail)  
文档：[doc.skymail.ink](https://doc.skymail.ink)

在 Cloudflare 上按文档部署 Workers，配好 MX。  
完成后你手里要有：

- API 地址（带 `/api`）
- 管理员邮箱 / 密码
- 邮箱域名（比如 `edu.你的域名`）

邮箱这一步涉及 CF 控制台点选，AI 不好代劳，自己跟着文档走最稳。

### 4. DNS 一条 A 记录

| 类型 | 名字 | 值 |
|------|------|-----|
| A | `grok2api` | 你的 VPS IP |

Cloudflare 建议 **灰云**（DNS only），方便 Caddy 签证书。

---

## 剩下全丢给 AI

SSH 能连上 VPS、邮箱信息齐了之后，把下面整段复制给 Claude / Cursor / 你用的 AI：

```text
我的 Ubuntu 海外 VPS：ssh root@<VPS_IP>
域名：grok2api.<你的域名>（A 记录已指向这台机，灰云）

请按顺序帮我做完，并在最后用表格给出所有地址和密码：

1. 部署 https://github.com/chenyme/grok2api
   - Docker 启动，只监听 127.0.0.1
   - 用 Caddy 把 https://grok2api.<你的域名> 反代过去，自动 HTTPS

2. 部署 https://github.com/yunfanxing6/grok-register
   - 按仓库 README 装依赖、编译、清障栈（WARP/FlareSolverr）
   - 邮箱用 Cloud Mail：
     EMAIL_API=<你的 Cloud Mail API，带 /api>
     EMAIL_DOMAIN=<你的邮箱域名>
     CLOUDMAIL_ADMIN_EMAIL=<管理员邮箱>
     CLOUDMAIL_ADMIN_PASSWORD=<管理员密码>
   - 注册成功自动上传到本机 grok2api（127.0.0.1）

3. 跑通 1 个号：grok start -t 1 --thread 1
   确认管理后台账号池有号，并告诉我：
   - 管理后台地址
   - 管理员账号密码
   - API Base（…/v1）
   - 怎么创建客户端 Key
```

把尖括号换成你的真实信息。  
AI 跑完，后台有号、域名能打开，就成了。

号不够继续让 AI 执行，或自己：

```bash
grok start -t 20 --thread 2
```

---

## 接到客户端

后台建一个 API Key，然后：

- Base URL：`https://grok2api.你的域名/v1`
- API Key：你建的那个

Claude Code / Cursor / 任意 OpenAI 兼容客户端，改这两项即可。

```bash
curl -sS "https://grok2api.你的域名/v1/chat/completions" \
  -H "Authorization: Bearer 你的key" \
  -H "Content-Type: application/json" \
  -d '{"model":"grok-4","messages":[{"role":"user","content":"hi"}],"max_tokens":32}'
```

---

## 注意

- 密码、Key、`config.env` 别推公开仓库  
- grok2api 只绑本机，外网只走 443  
- 遵守 xAI 和服务条款，本文仅供学习  

**总结**：人买域名 + VPS + 邮箱；AI 装网关、注册机、反代、灌号。一句提示词，自己的 Grok 中转站。
