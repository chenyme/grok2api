FROM python:3.11-slim AS base

WORKDIR /app

# 安装依赖阶段
FROM base AS dependencies

# 安装运行时需要的系统依赖
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    gcc \
    && rm -rf /var/lib/apt/lists/*

# 复制并安装 Python 依赖
COPY requirements.txt .
RUN pip install --no-cache-dir --upgrade pip && \
    pip install --no-cache-dir -r requirements.txt

# 最终运行阶段
FROM python:3.11-slim AS runtime

WORKDIR /app

# 复制必要的 Python 依赖
COPY --from=dependencies /usr/local/lib/python3.11/site-packages /usr/local/lib/python3.11/site-packages
COPY --from=dependencies /usr/local/bin /usr/local/bin

# 复制应用代码
COPY . .

# 创建必要的目录和文件
RUN mkdir -p /app/logs /app/data/temp && \
    echo '{"ssoNormal": {}, "ssoSuper": {}}' > /app/data/token.json

# 复制应用代码
COPY app/ ./app/
COPY main.py .
COPY data/setting.toml ./data/

# 删除 Python 字节码和缓存
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

EXPOSE 8000

CMD ["python", "-m", "uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]