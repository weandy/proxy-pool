#!/bin/sh
set -e

# 数据目录：将 DB 和 config 放在持久化卷
DATA_DIR="/app/proxy-pool/data"
mkdir -p "$DATA_DIR"

# 如果数据目录没有 config.json，则创建默认
if [ ! -f "$DATA_DIR/config.json" ]; then
  cat > "$DATA_DIR/config.json" << 'EOF'
{
  "listen_addr": "127.0.0.1:18080",
  "urls_file": "../URLS.JSON",
  "concurrency": 200,
  "timeout_sec": 10,
  "verify_method": "apple",
  "geoip_path": "GeoLite2-Country.mmdb",
  "refresh_interval_min": 60,
  "honeypot_threshold": 50,
  "internal_key": "changeme",
  "db_path": "data/proxies.db",
  "max_latency_ms": 5000,
  "score_decay_alpha": 0.3,
  "blacklist_fail_threshold": 5,
  "blacklist_revive_rounds": 3
}
EOF
  echo "[deploy] 已创建默认 config.json"
fi

# 更新 config.json 的 internal_key（与环境变量同步）
if command -v python3 > /dev/null; then
  python3 -c "
import json, os
p = '$DATA_DIR/config.json'
c = json.load(open(p))
c['internal_key'] = os.getenv('INTERNAL_KEY', 'changeme')
c['db_path'] = 'data/proxies.db'
json.dump(c, open(p, 'w'), indent=2)
"
fi

echo "================================"
echo "  Proxy Pool Docker Container"
echo "  Admin: http://0.0.0.0:${ADMIN_PORT}"
echo "  User:  ${ADMIN_USER}"
echo "================================"

# 启动 Go 引擎（后台）
cd /app/proxy-pool
./proxy-pool &
ENGINE_PID=$!

# 等待引擎就绪
sleep 3

# 启动 Python admin（前台）
cd /app/proxy-admin
exec python3 -c "
import uvicorn
from main import app
uvicorn.run(app, host='0.0.0.0', port=int('${ADMIN_PORT}'), log_level='info')
"
