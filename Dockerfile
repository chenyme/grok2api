# 构建阶段
FROM python:3.11-slim AS builder

WORKDIR /build

# 安装依赖到独立目录
COPY requirements.txt .
RUN pip install --no-cache-dir --only-binary=:all: --prefix=/install -r requirements.txt && \
  find /install -type d -name "__pycache__" -exec rm -rf {} + 2>/dev/null || true && \
  find /install -type d -name "tests" -exec rm -rf {} + 2>/dev/null || true && \
  find /install -type d -name "test" -exec rm -rf {} + 2>/dev/null || true && \
  find /install -type d -name "*.dist-info" -exec sh -c 'rm -f "$1"/RECORD "$1"/INSTALLER' _ {} \; && \
  find /install -type f -name "*.pyc" -delete && \
  find /install -type f -name "*.pyo" -delete && \
  find /install -name "*.so" -exec strip --strip-unneeded {} \; 2>/dev/null || true

# 运行阶段 - 使用最小镜像
FROM python:3.11-slim

WORKDIR /app

# 清理基础镜像中的冗余文件
RUN rm -rf /usr/share/doc/* \
  /usr/share/man/* \
  /usr/share/locale/* \
  /var/cache/apt/* \
  /var/lib/apt/lists/* \
  /tmp/* \
  /var/tmp/*

# 从构建阶段复制已安装的包
COPY --from=builder /install /usr/local

# 创建必要的目录（包括用于挂载的data目录）
RUN mkdir -p /app/logs /app/data/temp/image /app/data/temp/video

# 复制应用代码
COPY app/ ./app/
COPY main.py .

# 复制并设置 entrypoint 脚本
COPY docker-entrypoint.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# 删除 Python 字节码和缓存
ENV PYTHONDONTWRITEBYTECODE=1 \
  PYTHONUNBUFFERED=1

EXPOSE 8000

# 使用 entrypoint 脚本初始化配置
ENTRYPOINT ["docker-entrypoint.sh"]

# 默认启动命令
CMD ["python", "-m", "uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]
