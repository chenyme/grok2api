#!/usr/bin/env python3
"""
Grok2API 并发性能测试脚本

测试不同并发级别下的API性能表现
"""

import asyncio
import aiohttp
import time
import statistics
import argparse
from datetime import datetime
from typing import List, Dict, Any
import json


class ConcurrencyTester:
    """并发测试器"""
    
    def __init__(self, base_url: str, api_key: str = None):
        self.base_url = base_url.rstrip('/')
        self.api_key = api_key
        self.results: List[Dict[str, Any]] = []
    
    async def test_request(self, session: aiohttp.ClientSession, request_id: int) -> Dict[str, Any]:
        """发送单个测试请求"""
        url = f"{self.base_url}/v1/chat/completions"
        
        headers = {
            "Content-Type": "application/json"
        }
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        
        payload = {
            "model": "grok-3-fast",
            "messages": [
                {"role": "user", "content": f"测试请求 #{request_id}，请简短回复OK"}
            ],
            "stream": False,
            "max_tokens": 10
        }
        
        start_time = time.time()
        
        try:
            async with session.post(url, json=payload, headers=headers, timeout=30) as response:
                status = response.status
                
                if status == 200:
                    data = await response.json()
                    elapsed = time.time() - start_time
                    
                    return {
                        "id": request_id,
                        "status": "success",
                        "http_status": status,
                        "elapsed": elapsed,
                        "response_length": len(json.dumps(data))
                    }
                else:
                    elapsed = time.time() - start_time
                    error_text = await response.text()
                    
                    return {
                        "id": request_id,
                        "status": "error",
                        "http_status": status,
                        "elapsed": elapsed,
                        "error": error_text[:200]
                    }
        
        except asyncio.TimeoutError:
            elapsed = time.time() - start_time
            return {
                "id": request_id,
                "status": "timeout",
                "elapsed": elapsed,
                "error": "Request timeout"
            }
        
        except Exception as e:
            elapsed = time.time() - start_time
            return {
                "id": request_id,
                "status": "exception",
                "elapsed": elapsed,
                "error": str(e)
            }
    
    async def run_concurrent_test(self, concurrency: int, total_requests: int):
        """运行并发测试"""
        print(f"\n{'='*60}")
        print(f"📊 测试配置：并发数 {concurrency}, 总请求数 {total_requests}")
        print(f"{'='*60}")
        
        connector = aiohttp.TCPConnector(limit=concurrency, limit_per_host=concurrency)
        timeout = aiohttp.ClientTimeout(total=60)
        
        async with aiohttp.ClientSession(connector=connector, timeout=timeout) as session:
            # 预热
            print("🔥 预热中...")
            await self.test_request(session, 0)
            
            # 开始测试
            print(f"🚀 开始并发测试...")
            start_time = time.time()
            
            # 创建任务
            tasks = []
            for i in range(1, total_requests + 1):
                task = asyncio.create_task(self.test_request(session, i))
                tasks.append(task)
                
                # 控制并发数
                if len(tasks) >= concurrency:
                    results = await asyncio.gather(*tasks)
                    self.results.extend(results)
                    tasks = []
                    
                    # 显示进度
                    print(f"  进度: {i}/{total_requests} ({i/total_requests*100:.1f}%)", end='\r')
            
            # 处理剩余任务
            if tasks:
                results = await asyncio.gather(*tasks)
                self.results.extend(results)
            
            total_time = time.time() - start_time
            
            # 统计和输出
            self.print_statistics(concurrency, total_requests, total_time)
    
    def print_statistics(self, concurrency: int, total_requests: int, total_time: float):
        """打印统计信息"""
        success_results = [r for r in self.results if r["status"] == "success"]
        error_results = [r for r in self.results if r["status"] != "success"]
        
        success_count = len(success_results)
        error_count = len(error_results)
        
        if success_results:
            latencies = [r["elapsed"] for r in success_results]
            avg_latency = statistics.mean(latencies)
            min_latency = min(latencies)
            max_latency = max(latencies)
            p50_latency = statistics.median(latencies)
            p95_latency = sorted(latencies)[int(len(latencies) * 0.95)] if len(latencies) > 1 else latencies[0]
            p99_latency = sorted(latencies)[int(len(latencies) * 0.99)] if len(latencies) > 1 else latencies[0]
        else:
            avg_latency = min_latency = max_latency = p50_latency = p95_latency = p99_latency = 0
        
        throughput = total_requests / total_time if total_time > 0 else 0
        
        print(f"\n\n{'='*60}")
        print(f"📈 测试结果统计")
        print(f"{'='*60}")
        print(f"  测试时间: {total_time:.2f}s")
        print(f"  总请求数: {total_requests}")
        print(f"  并发数: {concurrency}")
        print(f"")
        print(f"  成功请求: {success_count} ({success_count/total_requests*100:.1f}%)")
        print(f"  失败请求: {error_count} ({error_count/total_requests*100:.1f}%)")
        print(f"")
        print(f"  吞吐量: {throughput:.2f} req/s")
        print(f"")
        print(f"  延迟统计:")
        print(f"    最小: {min_latency*1000:.0f}ms")
        print(f"    平均: {avg_latency*1000:.0f}ms")
        print(f"    最大: {max_latency*1000:.0f}ms")
        print(f"    P50:  {p50_latency*1000:.0f}ms")
        print(f"    P95:  {p95_latency*1000:.0f}ms")
        print(f"    P99:  {p99_latency*1000:.0f}ms")
        
        # 错误详情
        if error_results:
            print(f"\n  ⚠️  错误详情:")
            error_types = {}
            for r in error_results:
                error_type = r.get("status", "unknown")
                error_types[error_type] = error_types.get(error_type, 0) + 1
            
            for error_type, count in error_types.items():
                print(f"    {error_type}: {count}")
        
        print(f"{'='*60}\n")
        
        # 性能评级
        self.print_performance_rating(throughput, avg_latency)
    
    def print_performance_rating(self, throughput: float, avg_latency: float):
        """打印性能评级"""
        print(f"🎯 性能评级:")
        
        # 吞吐量评级
        if throughput >= 100:
            rating = "⭐⭐⭐⭐⭐ 优秀"
        elif throughput >= 60:
            rating = "⭐⭐⭐⭐ 良好"
        elif throughput >= 30:
            rating = "⭐⭐⭐ 中等"
        elif throughput >= 10:
            rating = "⭐⭐ 较低"
        else:
            rating = "⭐ 需优化"
        
        print(f"  吞吐量 ({throughput:.1f} req/s): {rating}")
        
        # 延迟评级
        if avg_latency < 0.5:
            rating = "⭐⭐⭐⭐⭐ 优秀"
        elif avg_latency < 1.0:
            rating = "⭐⭐⭐⭐ 良好"
        elif avg_latency < 2.0:
            rating = "⭐⭐⭐ 中等"
        elif avg_latency < 5.0:
            rating = "⭐⭐ 较高"
        else:
            rating = "⭐ 需优化"
        
        print(f"  平均延迟 ({avg_latency*1000:.0f}ms): {rating}")
        print()


async def main():
    """主函数"""
    parser = argparse.ArgumentParser(description='Grok2API 并发性能测试')
    parser.add_argument('--url', default='http://localhost:8001', help='API 基础URL')
    parser.add_argument('--key', default='', help='API Key（可选）')
    parser.add_argument('-c', '--concurrency', type=int, default=10, help='并发数')
    parser.add_argument('-n', '--requests', type=int, default=50, help='总请求数')
    parser.add_argument('--multi-test', action='store_true', help='运行多级并发测试')
    
    args = parser.parse_args()
    
    print(f"""
╔══════════════════════════════════════════════════════════╗
║          Grok2API 并发性能测试工具                        ║
╚══════════════════════════════════════════════════════════╝

🔗 测试目标: {args.url}
🔑 API Key: {'已设置' if args.key else '未设置'}
⏰ 开始时间: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}
""")
    
    tester = ConcurrencyTester(args.url, args.key)
    
    if args.multi_test:
        # 多级并发测试
        test_configs = [
            (5, 20),    # 5并发，20请求
            (10, 50),   # 10并发，50请求
            (20, 100),  # 20并发，100请求
            (50, 200),  # 50并发，200请求
        ]
        
        for concurrency, requests in test_configs:
            tester.results = []  # 清空结果
            await tester.run_concurrent_test(concurrency, requests)
            await asyncio.sleep(2)  # 间隔2秒
    else:
        # 单次测试
        await tester.run_concurrent_test(args.concurrency, args.requests)
    
    print(f"\n✅ 测试完成！")
    print(f"⏰ 结束时间: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}\n")


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        print("\n\n⚠️  测试被用户中断")
    except Exception as e:
        print(f"\n\n❌ 测试失败: {e}")
