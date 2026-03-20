#!/bin/bash
set -e

echo "=== Proxy Pool 一键部署 ==="

# 创建用户
if ! id proxypool &>/dev/null; then
    useradd -r -s /bin/false proxypool
    echo "[OK] 创建用户 proxypool"
fi

# Go 引擎部署
echo "[1/4] 部署 Go 引擎..."
mkdir -p /opt/proxy-pool
cp proxy-pool/proxy-pool /opt/proxy-pool/
cp proxy-pool/config.json /opt/proxy-pool/ 2>/dev/null || true
cp proxy-pool/URLS.JSON /opt/proxy-pool/ 2>/dev/null || echo "[WARN] 未找到 URLS.JSON，请手动放置"
cp proxy-pool/GeoLite2-Country.mmdb /opt/proxy-pool/ 2>/dev/null || echo "[INFO] GeoIP 数据库将自动下载"
chown -R proxypool:proxypool /opt/proxy-pool

# Python 管理后台部署
echo "[2/4] 部署 Python 管理后台..."
mkdir -p /opt/proxy-admin
cp -r proxy-admin/* /opt/proxy-admin/
cd /opt/proxy-admin
python3 -m venv venv
./venv/bin/pip install -r requirements.txt -q
chown -R proxypool:proxypool /opt/proxy-admin
cd -

# 安装 systemd 服务
echo "[3/4] 安装 systemd 服务..."
cp deploy/proxy-pool.service /etc/systemd/system/
cp deploy/proxy-admin.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable proxy-pool proxy-admin

# 启动服务
echo "[4/4] 启动服务..."
systemctl start proxy-pool
sleep 2
systemctl start proxy-admin

echo ""
echo "=== 部署完成 ==="
echo "  Go 引擎:    systemctl status proxy-pool"
echo "  Python 网关: systemctl status proxy-admin"
echo "  管理后台:    http://$(hostname -I | awk '{print $1}'):9090"
echo "  对外 API:    http://$(hostname -I | awk '{print $1}'):9090/api/*"
echo ""
echo "⚠ 请修改以下安全配置:"
echo "  1. /opt/proxy-pool/config.json → internal_key"
echo "  2. /etc/systemd/system/proxy-admin.service → INTERNAL_KEY, ADMIN_PASS, JWT_SECRET"
echo "  3. systemctl restart proxy-pool proxy-admin"
