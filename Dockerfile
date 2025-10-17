FROM python:3.11-slim AS base

WORKDIR /app

# 安装依赖阶段
FROM base AS dependencies

# 安装编译所需的系统依赖
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    gcc \
    g++ \
    libffi-dev \
    && rm -rf /var/lib/apt/lists/*

# 复制并安装 Python 依赖
COPY requirements.txt .
RUN pip install --no-cache-dir --upgrade pip && \
    pip install --no-cache-dir --compile -r requirements.txt && \
    find /usr/local/lib/python3.11/site-packages -type d -name "__pycache__" -exec rm -rf {} + 2>/dev/null || true && \
    find /usr/local/lib/python3.11/site-packages -type d -name "tests" -exec rm -rf {} + 2>/dev/null || true && \
    find /usr/local/lib/python3.11/site-packages -type d -name "test" -exec rm -rf {} + 2>/dev/null || true && \
    find /usr/local/lib/python3.11/site-packages -type f -name "*.pyc" -delete && \
    find /usr/local/lib/python3.11/site-packages -type f -name "*.pyo" -delete

# 最终运行阶段
FROM python:3.11-slim AS runtime

WORKDIR /app

# 只安装运行时必需的系统依赖
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    libffi8 \
    && rm -rf /var/lib/apt/lists/* \
    && rm -rf /tmp/* /var/tmp/*

# 复制必要的 Python 依赖
COPY --from=dependencies /usr/local/lib/python3.11/site-packages /usr/local/lib/python3.11/site-packages
COPY --from=dependencies /usr/local/bin /usr/local/bin

# 创建必要的目录和文件
RUN mkdir -p /app/logs /app/data/temp/image /app/data/temp/video && \
    echo '{"ssoNormal": {}, "ssoSuper": {}}' > /app/data/token.json

# 复制应用代码
COPY app/ ./app/
COPY main.py .
COPY data/setting.toml ./data/

EXPOSE 8000

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]