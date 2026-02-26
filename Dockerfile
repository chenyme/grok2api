FROM python:3.13-alpine AS builder

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    TZ=Asia/Shanghai \
    # 把 uv 包安装到系统 Python 环境
    UV_PROJECT_ENVIRONMENT=/opt/venv

# 确保 uv 的 bin 目录
ENV PATH="$UV_PROJECT_ENVIRONMENT/bin:$PATH"

RUN apk add --no-cache \
    tzdata \
    ca-certificates \
    build-base \
    linux-headers \
    libffi-dev \
    openssl-dev \
    curl-dev \
    cargo \
    rust

WORKDIR /app

# 安装 uv
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /bin/

COPY pyproject.toml uv.lock ./

RUN uv sync --frozen --no-dev --no-install-project \
    && find /opt/venv -type d -name "__pycache__" -prune -exec rm -rf {} + \
    && find /opt/venv -type f -name "*.pyc" -delete \
    && find /opt/venv -type d -name "tests" -prune -exec rm -rf {} + \
    && find /opt/venv -type d -name "test" -prune -exec rm -rf {} + \
    && find /opt/venv -type d -name "testing" -prune -exec rm -rf {} + \
    && find /opt/venv -type f -name "*.so" -exec strip --strip-unneeded {} + || true \
    && rm -rf /root/.cache /tmp/uv-cache

FROM python:3.13-alpine

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    TZ=Asia/Shanghai \
    VIRTUAL_ENV=/opt/venv

ENV PATH="$VIRTUAL_ENV/bin:$PATH"

RUN apk add --no-cache \
    tzdata \
    ca-certificates \
    libffi \
    openssl \
    libgcc \
    libstdc++ \
    libcurl

WORKDIR /app

COPY --from=builder /opt/venv /opt/venv

COPY config.defaults.toml ./
COPY app ./app
COPY main.py ./
COPY scripts ./scripts

RUN mkdir -p /app/data /app/logs \
    && chmod +x /app/scripts/entrypoint.sh

EXPOSE 8000

ENTRYPOINT ["/app/scripts/entrypoint.sh"]

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]
