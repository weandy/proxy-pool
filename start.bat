@echo off
REM ============================================
REM  Proxy Pool 本地启动脚本（Windows）
REM  放置于项目根目录，一键启动整个系统
REM ============================================

echo ================================
echo   Proxy Pool 启动中...
echo ================================

REM 检查 .env 文件
if not exist "%~dp0.env" (
    echo [ERROR] 未找到 .env 配置文件！
    echo         请先复制 .env.example 为 .env 并修改配置：
    echo         copy .env.example .env
    pause
    exit /b 1
)

REM 检查 Go 引擎是否已编译
if not exist "%~dp0proxy-pool\proxy-pool.exe" (
    echo [INFO] Go 引擎未编译，正在编译...
    cd /d "%~dp0proxy-pool"
    go build -o proxy-pool.exe .
    if errorlevel 1 (
        echo [ERROR] Go 编译失败！
        pause
        exit /b 1
    )
    echo [OK] Go 引擎编译完成
    cd /d "%~dp0"
)

REM 检查 Python 依赖
echo [INFO] 检查 Python 依赖...
pip show fastapi >nul 2>&1
if errorlevel 1 (
    echo [INFO] 安装 Python 依赖...
    pip install -r "%~dp0proxy-admin\requirements.txt" -q
)

REM 启动 Python 网关（它会自动拉起 Go 引擎）
echo [INFO] 启动 Proxy Pool...
echo.
cd /d "%~dp0proxy-admin"
python main.py
