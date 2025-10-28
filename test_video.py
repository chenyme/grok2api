#!/usr/bin/env python3
"""测试视频生成功能"""

import asyncio
import sys
from pathlib import Path

# 添加项目根目录到Python路径
project_root = Path(__file__).parent
sys.path.insert(0, str(project_root))

from app.services.grok.video import video_generator


async def test_video_generation():
    """测试视频生成功能"""
    print("开始测试视频生成功能...")
    
    # 测试图片路径（请替换为实际存在的图片路径）
    test_image_path = "C:\\Users\\21449\\Pictures\\Saved Pictures\\R.gif"  # 请确保这个路径存在

    if not Path(test_image_path).exists():
        print(f"测试图片不存在: {test_image_path}")
        return
    
    try:
        # 调用视频生成服务
        result = await video_generator.generate_video_from_image(
            image_path=test_image_path,
            file_name="test_video",
            mode="normal",
            model_name="grok-3"
        )
        
        if result.get("success"):
            print("✅ 视频生成成功!")
            print(f"视频URL: {result.get('conversation_result', {}).get('data', {}).get('final_video_url', 'N/A')}")
            print(f"视频ID: {result.get('conversation_result', {}).get('data', {}).get('video_id', 'N/A')}")
            print(f"进度: {result.get('conversation_result', {}).get('data', {}).get('progress', 'N/A')}%")
        else:
            print("❌ 视频生成失败!")
            print(f"错误: {result.get('error', '未知错误')}")
            print(f"详细信息: {result}")
            
    except Exception as e:
        print(f"❌ 测试异常: {str(e)}")


if __name__ == "__main__":
    asyncio.run(test_video_generation())
