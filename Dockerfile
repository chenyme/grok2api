# 构建阶段 - 使用完整镜像编译依赖
FROM python:3.11-slim AS builder

WORKDIR /build

# 安装编译工具
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    gcc \
    g++ \
    libffi-dev \
    && rm -rf /var/lib/apt/lists/*

# 安装依赖到独立目录
COPY requirements.txt .
RUN pip install --no-cache-dir --prefix=/install --compile -r requirements.txt

# 清理 Python 包中的冗余文件
RUN find /install -type d -name "__pycache__" -exec rm -rf {} + 2>/dev/null || true && \
    find /install -type d -name "tests" -exec rm -rf {} + 2>/dev/null || true && \
    find /install -type d -name "test" -exec rm -rf {} + 2>/dev/null || true && \
    find /install -type d -name "*.dist-info" -exec sh -c 'rm -f "$1"/RECORD "$1"/INSTALLER' _ {} \; && \
    find /install -type f -name "*.pyc" -delete && \
    find /install -type f -name "*.pyo" -delete && \
    find /install -type f -name "*.c" -delete && \
    find /install -type f -name "*.h" -delete && \
    find /install -type f -name "*.txt" -path "*/pip/*" -delete 2>/dev/null || true

# 运行阶段 - 使用最小镜像
FROM python:3.11-slim

WORKDIR /app

# 只安装必要的运行时库
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    libffi8 \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && rm -rf /tmp/* /var/tmp/* \
    && rm -rf /usr/share/doc/* \
    && rm -rf /usr/share/man/* \
    && rm -rf /var/cache/apt/*

# 从构建阶段复制已安装的包
COPY --from=builder /install /usr/local

# 创建必要的目录和文件
RUN mkdir -p /app/logs /app/data/temp/image /app/data/temp/video && \
    echo '{"ssoNormal": {}, "ssoSuper": {}}' > /app/data/token.json

# 复制应用代码
COPY app/ ./app/
COPY main.py .
COPY data/setting.toml ./data/

# 删除 Python 字节码和缓存
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

EXPOSE 8001

CMD ["python", "-m", "uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8001"]