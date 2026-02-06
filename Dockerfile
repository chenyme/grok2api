FROM python:3.13-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    TZ=Asia/Shanghai \
    # 修改：让 uv 直接把包安装到系统 Python 环境，或者指定明确的虚拟环境
    UV_PROJECT_ENVIRONMENT=/opt/venv

# 确保虚拟环境的 bin 目录在最前面
ENV PATH="$UV_PROJECT_ENVIRONMENT/bin:$PATH"

RUN apt-get update \
    && apt-get install -y --no-install-recommends tzdata ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# 安装 uv
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /bin/
# 或者保留你原有的 pip install uv

COPY pyproject.toml uv.lock ./

# 修改：使用 --frozen 且确保同步到指定的 /opt/venv
RUN uv sync --frozen --no-dev --no-install-project

COPY config.defaults.toml ./
COPY app ./app
COPY main.py ./
COPY scripts ./scripts

RUN mkdir -p /app/data /app/data/tmp /app/logs \
    && chmod +x /app/scripts/entrypoint.sh

EXPOSE 8000

ENTRYPOINT ["/app/scripts/entrypoint.sh"]

# 建议：直接调用 uvicorn，因为 /opt/venv/bin 已经在 PATH 中
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]