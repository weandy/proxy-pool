#!/bin/bash
# ============================================
#  Proxy Pool 本地启动脚本（Linux/Mac）
#  放置于项目根目录，一键启动整个系统
# ============================================
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "================================"
echo "  Proxy Pool 启动中..."
echo "================================"

# 检查 .env 文件
if [ ! -f "$SCRIPT_DIR/.env" ]; then
    echo "[ERROR] 未找到 .env 配置文件！"
    echo "        请先复制 .env.example 为 .env 并修改配置："
    echo "        cp .env.example .env"
    exit 1
fi

# 加载 .env 为环境变量（Go 引擎通过 os.Getenv 读取）
set -a
source <(grep -v '^#' "$SCRIPT_DIR/.env" | grep -v '^\s*$')
set +a

# 检查 Go 引擎
if [ ! -f "$SCRIPT_DIR/proxy-pool/proxy-pool" ]; then
    # 优先使用预编译的二进制文件
    if [ -f "$SCRIPT_DIR/proxy-pool/proxy-pool-linux" ]; then
        echo "[INFO] 使用预编译的 Go 引擎..."
        mv "$SCRIPT_DIR/proxy-pool/proxy-pool-linux" "$SCRIPT_DIR/proxy-pool/proxy-pool"
        chmod +x "$SCRIPT_DIR/proxy-pool/proxy-pool"
        echo "[OK] Go 引擎已就绪"
    else
        echo "[INFO] Go 引擎未编译，正在编译..."
        cd "$SCRIPT_DIR/proxy-pool"
        if ! go build -o proxy-pool .; then
            echo "[ERROR] Go 引擎编译失败，请检查 Go 环境或模块依赖！"
            exit 1
        fi
        echo "[OK] Go 引擎编译完成"
        cd "$SCRIPT_DIR"
    fi
fi

# 检查 Python 依赖
echo "[INFO] 检查 Python 依赖..."
if ! python3 -c "import fastapi" 2>/dev/null; then
    echo "[INFO] 安装 Python 依赖..."
    pip3 install -r "$SCRIPT_DIR/proxy-admin/requirements.txt" -q
fi

# 启动 Python 网关（它会自动拉起 Go 引擎）
echo "[INFO] 启动 Proxy Pool..."
echo ""
cd "$SCRIPT_DIR/proxy-admin"
exec python3 main.py
