# ===== 阶段1: 编译 Go 引擎 =====
FROM golang:1.22-alpine AS go-builder
WORKDIR /build
COPY proxy-pool/go.mod proxy-pool/go.sum ./
RUN go mod download
COPY proxy-pool/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /proxy-pool .

# ===== 阶段2: 运行环境 =====
FROM python:3.11-slim
WORKDIR /app

# 系统依赖
RUN apt-get update && apt-get install -y --no-install-recommends curl && \
    rm -rf /var/lib/apt/lists/*

# 复制 Go 引擎
COPY --from=go-builder /proxy-pool /app/proxy-pool/proxy-pool
RUN chmod +x /app/proxy-pool/proxy-pool

# 复制 GeoIP 和默认配置
COPY proxy-pool/GeoLite2-Country.mmdb /app/proxy-pool/
COPY proxy-pool/URLS.JSON /app/proxy-pool/

# Python 依赖
COPY proxy-admin/requirements.txt /app/proxy-admin/
RUN pip install --no-cache-dir -r /app/proxy-admin/requirements.txt

# 复制 Python 代码
COPY proxy-admin/ /app/proxy-admin/

# 数据卷（持久化数据库和配置）
VOLUME ["/app/proxy-pool/data"]

# 环境变量（可通过 docker-compose 或 -e 覆盖）
ENV ENGINE_URL=http://127.0.0.1:18080 \
    INTERNAL_KEY=changeme \
    ADMIN_PORT=9090 \
    ADMIN_HOST=0.0.0.0 \
    ADMIN_USER=admin \
    ADMIN_PASS=admin123 \
    JWT_SECRET=proxy-admin-secret-key-change-me

EXPOSE 9090

# 启动脚本
COPY deploy/start.sh /app/start.sh
RUN chmod +x /app/start.sh

CMD ["/app/start.sh"]
