"""JWT 认证"""
import hmac
from typing import Optional
from datetime import datetime, timedelta, timezone
from jose import jwt, JWTError
from fastapi import Request, HTTPException
from config import JWT_SECRET, JWT_ALGORITHM, JWT_EXPIRE_HOURS, ADMIN_USER, ADMIN_PASS


def create_token(username: str) -> str:
    expire = datetime.now(timezone.utc) + timedelta(hours=JWT_EXPIRE_HOURS)
    return jwt.encode({"sub": username, "exp": expire}, JWT_SECRET, algorithm=JWT_ALGORITHM)


def verify_token(token: str) -> Optional[str]:
    try:
        payload = jwt.decode(token, JWT_SECRET, algorithms=[JWT_ALGORITHM])
        return payload.get("sub")
    except JWTError:
        return None


def check_login(username: str, password: str) -> Optional[str]:
    """验证登录，返回 token 或 None（时间安全比较）"""
    if hmac.compare_digest(username, ADMIN_USER) and hmac.compare_digest(password, ADMIN_PASS):
        return create_token(username)
    return None


def get_current_user(request: Request) -> Optional[str]:
    """从 cookie 中获取当前用户"""
    token = request.cookies.get("token")
    if not token:
        return None
    return verify_token(token)


def require_auth(request: Request) -> str:
    """要求认证，未登录则抛异常"""
    user = get_current_user(request)
    if not user:
        raise HTTPException(status_code=302, headers={"Location": "/login"})
    return user
