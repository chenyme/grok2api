#!/bin/bash

# Grok2API 并发测试脚本（Shell版本）
# 使用 curl 和 GNU parallel 进行并发测试

set -e

# 配置
BASE_URL="${BASE_URL:-http://localhost:8001}"
API_KEY="${API_KEY:-}"
CONCURRENCY="${CONCURRENCY:-10}"
TOTAL_REQUESTS="${TOTAL_REQUESTS:-50}"

# 颜色
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}╔══════════════════════════════════════════════════════════╗${NC}"
echo -e "${BLUE}║          Grok2API 并发性能测试工具 (Shell版)             ║${NC}"
echo -e "${BLUE}╚══════════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "${GREEN}🔗 测试目标:${NC} $BASE_URL"
echo -e "${GREEN}🔑 API Key:${NC} ${API_KEY:-(未设置)}"
echo -e "${GREEN}📊 并发数:${NC} $CONCURRENCY"
echo -e "${GREEN}📈 总请求数:${NC} $TOTAL_REQUESTS"
echo ""

# 检查依赖
if ! command -v curl &> /dev/null; then
    echo -e "${RED}❌ 错误: 需要安装 curl${NC}"
    exit 1
fi

# 创建临时目录
TMP_DIR=$(mktemp -d)
trap "rm -rf $TMP_DIR" EXIT

# 单个请求函数
test_request() {
    local request_id=$1
    local start_time=$(date +%s.%N)
    
    # 构建请求
    local headers="Content-Type: application/json"
    if [ -n "$API_KEY" ]; then
        headers="${headers}\nAuthorization: Bearer ${API_KEY}"
    fi
    
    local response=$(curl -s -w "\n%{http_code}\n%{time_total}" \
        -X POST "${BASE_URL}/v1/chat/completions" \
        -H "Content-Type: application/json" \
        ${API_KEY:+-H "Authorization: Bearer $API_KEY"} \
        -d "{
            \"model\": \"grok-3-fast\",
            \"messages\": [{\"role\": \"user\", \"content\": \"测试请求 #${request_id}，请简短回复OK\"}],
            \"stream\": false,
            \"max_tokens\": 10
        }" 2>&1)
    
    local http_code=$(echo "$response" | tail -n 2 | head -n 1)
    local time_total=$(echo "$response" | tail -n 1)
    
    # 记录结果
    echo "${request_id},${http_code},${time_total}" >> "$TMP_DIR/results.csv"
    
    # 显示进度
    echo -ne "\r  进度: ${request_id}/${TOTAL_REQUESTS}"
}

# 导出函数供 parallel 使用
export -f test_request
export BASE_URL API_KEY TMP_DIR

# 清空结果文件
echo "id,status,time" > "$TMP_DIR/results.csv"

echo -e "${YELLOW}🚀 开始并发测试...${NC}"
START_TIME=$(date +%s.%N)

# 使用 GNU parallel（如果可用），否则使用简单循环
if command -v parallel &> /dev/null; then
    seq 1 $TOTAL_REQUESTS | parallel -j $CONCURRENCY test_request {}
else
    # 简单的后台任务并发
    for i in $(seq 1 $TOTAL_REQUESTS); do
        test_request $i &
        
        # 控制并发数
        if (( i % CONCURRENCY == 0 )); then
            wait
        fi
    done
    wait
fi

END_TIME=$(date +%s.%N)
TOTAL_TIME=$(echo "$END_TIME - $START_TIME" | bc)

echo -e "\n"

# 统计结果
echo -e "${BLUE}═══════════════════════════════════════════════════════════${NC}"
echo -e "${BLUE}📈 测试结果统计${NC}"
echo -e "${BLUE}═══════════════════════════════════════════════════════════${NC}"

# 统计成功/失败
SUCCESS_COUNT=$(awk -F',' '$2 == 200 {count++} END {print count+0}' "$TMP_DIR/results.csv")
ERROR_COUNT=$((TOTAL_REQUESTS - SUCCESS_COUNT))

echo -e "  测试时间: ${TOTAL_TIME}s"
echo -e "  总请求数: ${TOTAL_REQUESTS}"
echo -e "  并发数: ${CONCURRENCY}"
echo ""
echo -e "  成功请求: ${GREEN}${SUCCESS_COUNT}${NC} ($(echo "scale=1; $SUCCESS_COUNT * 100 / $TOTAL_REQUESTS" | bc)%)"
echo -e "  失败请求: ${RED}${ERROR_COUNT}${NC} ($(echo "scale=1; $ERROR_COUNT * 100 / $TOTAL_REQUESTS" | bc)%)"
echo ""

# 计算吞吐量
THROUGHPUT=$(echo "scale=2; $TOTAL_REQUESTS / $TOTAL_TIME" | bc)
echo -e "  吞吐量: ${GREEN}${THROUGHPUT}${NC} req/s"
echo ""

# 延迟统计（只统计成功的请求）
if [ $SUCCESS_COUNT -gt 0 ]; then
    echo -e "  延迟统计:"
    
    # 提取成功请求的延迟时间
    awk -F',' '$2 == 200 {print $3}' "$TMP_DIR/results.csv" | sort -n > "$TMP_DIR/latencies.txt"
    
    MIN=$(head -n 1 "$TMP_DIR/latencies.txt" | awk '{printf "%.0f", $1*1000}')
    MAX=$(tail -n 1 "$TMP_DIR/latencies.txt" | awk '{printf "%.0f", $1*1000}')
    AVG=$(awk '{sum+=$1; count++} END {printf "%.0f", sum/count*1000}' "$TMP_DIR/latencies.txt")
    
    # P50
    P50_LINE=$((SUCCESS_COUNT / 2))
    P50=$(sed -n "${P50_LINE}p" "$TMP_DIR/latencies.txt" | awk '{printf "%.0f", $1*1000}')
    
    # P95
    P95_LINE=$(echo "scale=0; $SUCCESS_COUNT * 0.95 / 1" | bc)
    P95=$(sed -n "${P95_LINE}p" "$TMP_DIR/latencies.txt" | awk '{printf "%.0f", $1*1000}')
    
    # P99
    P99_LINE=$(echo "scale=0; $SUCCESS_COUNT * 0.99 / 1" | bc)
    P99=$(sed -n "${P99_LINE}p" "$TMP_DIR/latencies.txt" | awk '{printf "%.0f", $1*1000}')
    
    echo -e "    最小: ${MIN}ms"
    echo -e "    平均: ${AVG}ms"
    echo -e "    最大: ${MAX}ms"
    echo -e "    P50:  ${P50}ms"
    echo -e "    P95:  ${P95}ms"
    echo -e "    P99:  ${P99}ms"
fi

echo -e "${BLUE}═══════════════════════════════════════════════════════════${NC}"

# 性能评级
echo -e "${YELLOW}🎯 性能评级:${NC}"

if (( $(echo "$THROUGHPUT >= 100" | bc -l) )); then
    RATING="⭐⭐⭐⭐⭐ 优秀"
elif (( $(echo "$THROUGHPUT >= 60" | bc -l) )); then
    RATING="⭐⭐⭐⭐ 良好"
elif (( $(echo "$THROUGHPUT >= 30" | bc -l) )); then
    RATING="⭐⭐⭐ 中等"
elif (( $(echo "$THROUGHPUT >= 10" | bc -l) )); then
    RATING="⭐⭐ 较低"
else
    RATING="⭐ 需优化"
fi

echo -e "  吞吐量 (${THROUGHPUT} req/s): ${RATING}"

echo ""
echo -e "${GREEN}✅ 测试完成！${NC}"
