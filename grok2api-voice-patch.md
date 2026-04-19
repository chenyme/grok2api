# grok2api ChatKit Voice 自定义提示词补丁

在 chenyme/grok2api 中为 ChatKit Voice 添加自定义 system prompt 支持。

## 修改清单

需要修改 4 个文件（按顺序）：

1. `app/dataplane/reverse/protocol/xai_livekit.py` — payload 构建层
2. `app/dataplane/reverse/transport/livekit.py` — transport 层透传
3. `app/products/web/webui/voice.py` — API 端点接收参数
4. `app/statics/webui/chatkit.html` — 前端 UI 加输入框
5. `app/statics/js/webui/chatkit.js` — 前端发送参数

下面逐个给出 patch（基于 main 分支最新代码）。

---

## Patch 1: xai_livekit.py

文件路径: `app/dataplane/reverse/protocol/xai_livekit.py`

### 原始代码（第 34-48 行）
```python
def build_token_request_payload(
    voice:       str   = "ara",
    personality: str   = "assistant",
    speed:       float = 1.0,
) -> bytes:
    """Return the JSON body for POST /rest/livekit/tokens."""
    session_payload = orjson.dumps({
        "voice":           voice,
        "personality":     personality,
        "playback_speed":  speed,
        "enable_vision":   False,
        "turn_detection":  {"type": "server_vad"},
    }).decode()

    return orjson.dumps({
        "sessionPayload":       session_payload,
        "requestAgentDispatch": False,
        "livekitUrl":           LIVEKIT_WS_BASE,
        "params":               {"enable_markdown_transcript": "true"},
    })
```

### 替换为
```python
def build_token_request_payload(
    voice:              str   = "ara",
    personality:        str   = "assistant",
    speed:              float = 1.0,
    custom_instruction: str   = "",
) -> bytes:
    """Return the JSON body for POST /rest/livekit/tokens."""
    payload_dict = {
        "voice":           voice,
        "personality":     personality,
        "playback_speed":  speed,
        "enable_vision":   False,
        "turn_detection":  {"type": "server_vad"},
    }
    if custom_instruction:
        payload_dict["customPersonality"] = custom_instruction

    session_payload = orjson.dumps(payload_dict).decode()

    return orjson.dumps({
        "sessionPayload":       session_payload,
        "requestAgentDispatch": False,
        "livekitUrl":           LIVEKIT_WS_BASE,
        "params":               {"enable_markdown_transcript": "true"},
    })
```

---

## Patch 2: livekit.py (transport)

文件路径: `app/dataplane/reverse/transport/livekit.py`

### 原始代码（第 28-32 行）
```python
async def fetch_livekit_token(
    token:       str,
    *,
    voice:       str   = "ara",
    personality: str   = "assistant",
    speed:       float = 1.0,
) -> Dict[str, Any]:
```

### 替换为
```python
async def fetch_livekit_token(
    token:       str,
    *,
    voice:              str   = "ara",
    personality:        str   = "assistant",
    speed:              float = 1.0,
    custom_instruction: str   = "",
) -> Dict[str, Any]:
```

### 原始代码（第 43-46 行）
```python
    payload = build_token_request_payload(
        voice       = voice,
        personality = personality,
        speed       = speed,
    )
```

### 替换为
```python
    payload = build_token_request_payload(
        voice              = voice,
        personality        = personality,
        speed              = speed,
        custom_instruction = custom_instruction,
    )
```

---

## Patch 3: voice.py (API)

文件路径: `app/products/web/webui/voice.py`

### 原始代码（第 40-44 行）
```python
@router.get("/voice/token", response_model=VoiceTokenResponse)
async def voice_token(
    voice: str = "ara",
    personality: str = "assistant",
    speed: float = 1.0,
):
```

### 替换为
```python
@router.get("/voice/token", response_model=VoiceTokenResponse)
async def voice_token(
    voice: str = "ara",
    personality: str = "assistant",
    speed: float = 1.0,
    instruction: str = "",
):
```

### 原始代码（第 55 行）
```python
        data = await fetch_livekit_token(token, voice=voice, personality=personality, speed=speed)
```

### 替换为
```python
        data = await fetch_livekit_token(token, voice=voice, personality=personality, speed=speed, custom_instruction=instruction)
```

---

## Patch 4: chatkit.html

文件路径: `app/statics/webui/chatkit.html`

### 在 personality 下拉框之后、speed 下拉框之前，插入自定义提示输入框

找到这段：
```html
            <label class="webui-chatkit-pill">
              <span class="webui-chatkit-pill-label" data-i18n="webui.chatkit.speedLabel">语速</span>
```

在它前面插入：
```html
            <label class="webui-chatkit-pill">
              <span class="webui-chatkit-pill-label" data-i18n="webui.chatkit.instructionLabel">提示词</span>
              <input id="instructionInput" type="text" class="input webui-chatkit-pill-input"
                     placeholder="自定义 system prompt（可选）" />
            </label>
```

完整上下文（替换后）：
```html
            <label class="webui-chatkit-pill">
              <span class="webui-chatkit-pill-label" data-i18n="webui.chatkit.personalityLabel">个性</span>
              <select id="personalitySelect" class="input webui-chatkit-pill-input">
                <option value="assistant" selected>Assistant</option>
                <option value="custom">Custom</option>
                <option value="therapist">Therapist</option>
                <option value="storyteller">Storyteller</option>
                <option value="kids_story_time">Kids Story Time</option>
                <option value="meditation">Meditation</option>
                <option value="unhinged">Unhinged 18+</option>
                <option value="sexy">Sexy 18+</option>
              </select>
            </label>
            <label class="webui-chatkit-pill">
              <span class="webui-chatkit-pill-label" data-i18n="webui.chatkit.instructionLabel">提示词</span>
              <input id="instructionInput" type="text" class="input webui-chatkit-pill-input"
                     placeholder="自定义 system prompt（可选）" />
            </label>
            <label class="webui-chatkit-pill">
              <span class="webui-chatkit-pill-label" data-i18n="webui.chatkit.speedLabel">语速</span>
```

---

## Patch 5: chatkit.js

文件路径: `app/statics/js/webui/chatkit.js`

### 在顶部变量声明区添加 instructionInput 引用

找到：
```javascript
  const speedSelect = document.getElementById('speedSelect');
```

在后面添加：
```javascript
  const instructionInput = document.getElementById('instructionInput');
```

### 修改 params 构建

找到（约第 380 行）：
```javascript
      const params = new URLSearchParams({
        voice: voiceSelect?.value || 'ara',
        personality: personalitySelect?.value || 'assistant',
        speed: speedSelect?.value || '1.0',
      });
```

替换为：
```javascript
      const params = new URLSearchParams({
        voice: voiceSelect?.value || 'ara',
        personality: personalitySelect?.value || 'assistant',
        speed: speedSelect?.value || '1.0',
        instruction: instructionInput?.value?.trim() || '',
      });
```

---

## 使用方式

修改完成后，在 ChatKit 页面 `/webui/chatkit` 的工具栏会多出一个「提示词」输入框。

- 留空 = 只用 personality 预设，行为和之前完全一致
- 填入文本 = 作为 `customPersonality` 发给 Grok LiveKit 端点
- 建议配合 personality 设为 `custom` 使用

## 注意事项

1. `customPersonality` 是 Grok 网页端已有的字段，理论上 Voice 端也会接受，但需要实测验证
2. 如果 Grok 上游不认这个字段，会被静默忽略，不影响正常使用
3. GET 请求的 URL 长度有限制（~2000 字符），提示词不宜过长
4. 修改的是服务端文件，需要重启 grok2api 生效
