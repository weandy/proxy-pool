# Proxy Pool — 商业化代理池系统

Go 引擎抓取验证 + Python 网关管理鉴权，一条命令启动。

## 目录结构

```
├── proxy-pool/          # Go 引擎（代理抓取/验证/评分）
│   ├── *.go             # 源码
│   ├── go.mod / go.sum  # 依赖
│   ├── config.json      # 引擎配置
│   ├── URLS.JSON        # 代理源 URL 列表
│   └── GeoLite2-Country.mmdb  # GeoIP 数据库
│
├── proxy-admin/         # Python 网关（管理后台 + 对外 API）
│   ├── main.py          # 入口（聚合启动）
│   ├── config.py        # 配置
│   ├── auth.py          # JWT 认证
│   ├── database.py      # API Key + 用量统计
│   ├── api_routes.py    # 对外 API（鉴权/限速）
│   ├── engine_client.py # Go 引擎通信
│   ├── requirements.txt # Python 依赖
│   └── templates/       # 管理后台 HTML
│
├── deploy/              # 部署脚本（Linux systemd）
│   ├── install.sh
│   ├── proxy-pool.service
│   └── proxy-admin.service
│
└── .gitignore
```

## 本地启动

### 1. 编译 Go 引擎（首次或代码改动后）

```bash
cd proxy-pool
go build -o proxy-pool.exe .     # Windows
go build -o proxy-pool .         # Linux/Mac
```

### 2. 安装 Python 依赖（首次）

```bash
cd proxy-admin
pip install -r requirements.txt
```

### 3. 一键启动

```bash
cd proxy-admin
python main.py
```

程序会**自动启动 Go 引擎**，等待就绪后启动网关。

### 4. 访问

| 地址 | 说明 |
|------|------|
| `http://localhost:9090` | 管理后台（admin / admin123） |
| `http://localhost:9090/api/proxies?api_key=YOUR_KEY` | 代理列表 API |
| `http://localhost:9090/api/proxy/random?api_key=YOUR_KEY` | 随机代理 API |
| `http://localhost:9090/api/stats?api_key=YOUR_KEY` | 统计信息 API |

### 5. 创建 API Key

登录管理后台 → 侧边栏「Key 管理」→「+ 创建 Key」

## API 参数速查

```
GET /api/proxies?api_key=KEY&number=100&format=txt&country=CN&protocol=https
GET /api/proxy/random?api_key=KEY&protocol=https&country=US
GET /api/stats?api_key=KEY
```

- `number`: 数量（数字 或 `all`）
- `format`: `json`(默认) / `txt`(纯文本)
- `country`: 国家代码（逗号分隔）
- `protocol`: `http` / `https`
- 鉴权: `?api_key=` 或 `Authorization: Bearer`
