"""管理后台配置"""
import os
import sys
from pathlib import Path
from dotenv import load_dotenv

# 自动向上寻找并加载 .env 文件（支持 docker 和 直接运行）
_admin_dir = Path(__file__).parent.resolve()
load_dotenv(dotenv_path=_admin_dir.parent / ".env")

# Go 引擎地址
ENGINE_URL = os.getenv("ENGINE_URL", "http://127.0.0.1:18080")
INTERNAL_KEY = os.getenv("INTERNAL_KEY", "changeme")

# Go 引擎可执行文件（聚合启动用）
_admin_dir = Path(__file__).parent.resolve()
ENGINE_DIR = os.getenv("ENGINE_DIR", str(_admin_dir.parent / "proxy-pool"))
ENGINE_BIN = os.getenv("ENGINE_BIN", "proxy-pool.exe" if sys.platform == "win32" else "proxy-pool")

# 管理后台
ADMIN_PORT = int(os.getenv("ADMIN_PORT", "9090"))
ADMIN_HOST = os.getenv("ADMIN_HOST", "0.0.0.0")

# 登录凭据
ADMIN_USER = os.getenv("ADMIN_USER", "admin")
ADMIN_PASS = os.getenv("ADMIN_PASS", "admin123")

# JWT
JWT_SECRET = os.getenv("JWT_SECRET", "proxy-admin-secret-key-change-me")
JWT_ALGORITHM = "HS256"
JWT_EXPIRE_HOURS = 24

