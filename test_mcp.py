"""
MCP-style Agent loop test using only stdlib (no openai SDK needed)
"""
import json, math, urllib.request, urllib.error

BASE_URL = "http://localhost:8000/v1/chat/completions"

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "calculator",
            "description": "Evaluate a math expression.",
            "parameters": {
                "type": "object",
                "properties": {
                    "expression": {"type": "string", "description": "e.g. '2**10' or '144**0.5'"}
                },
                "required": ["expression"]
            }
        }
    },
    {
        "type": "function",
        "function": {
            "name": "get_weather",
            "description": "Get weather for a city.",
            "parameters": {
                "type": "object",
                "properties": {"city": {"type": "string"}},
                "required": ["city"]
            }
        }
    },
    {
        "type": "function",
        "function": {
            "name": "string_reverse",
            "description": "Reverse a string.",
            "parameters": {
                "type": "object",
                "properties": {"text": {"type": "string"}},
                "required": ["text"]
            }
        }
    }
]

def call_api(messages, tools=None, tool_choice="auto", model="grok-4"):
    payload = {
        "model": model,
        "messages": messages,
        "stream": False,
        "tools": tools or TOOLS,
        "tool_choice": tool_choice,
    }
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        BASE_URL,
        data=data,
        headers={"Content-Type": "application/json"},
        method="POST"
    )
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read())

def execute_tool(name, args):
    if name == "calculator":
        try:
            result = eval(args["expression"], {"__builtins__": {}}, {"math": math})
            return json.dumps({"result": result})
        except Exception as e:
            return json.dumps({"error": str(e)})
    elif name == "get_weather":
        db = {
            "tokyo":    {"temp": 12, "condition": "Cloudy",  "humidity": 65},
            "beijing":  {"temp":  3, "condition": "Sunny",   "humidity": 30},
            "new york": {"temp":  8, "condition": "Rainy",   "humidity": 80},
            "london":   {"temp":  6, "condition": "Foggy",   "humidity": 90},
        }
        return json.dumps(db.get(args["city"].lower(), {"temp": 20, "condition": "Unknown"}))
    elif name == "string_reverse":
        return json.dumps({"result": args["text"][::-1]})
    return json.dumps({"error": f"unknown tool {name}"})

def run_agent(user_msg, model="grok-4"):
    print(f"\n{'='*60}")
    print(f"[USER] {user_msg}")
    print("="*60)
    messages = [{"role": "user", "content": user_msg}]
    step = 0

    while True:
        step += 1
        print(f"\n--- Step {step}: calling {model} ---")
        resp = call_api(messages, model=model)
        choice = resp["choices"][0]
        msg = choice["message"]
        finish = choice["finish_reason"]
        print(f"finish_reason: {finish}")

        messages.append(msg)

        if finish == "tool_calls":
            for tc in msg["tool_calls"]:
                fn   = tc["function"]["name"]
                args = json.loads(tc["function"]["arguments"])
                print(f"  >> tool_call: {fn}({args})")
                result = execute_tool(fn, args)
                print(f"  << tool_result: {result}")
                messages.append({
                    "role": "tool",
                    "tool_call_id": tc["id"],
                    "name": fn,
                    "content": result,
                })
        else:
            print(f"\n[FINAL] {msg.get('content', '')}")
            return msg.get("content", "")

if __name__ == "__main__":
    tests = [
        "What is 2 to the power of 15? Use the calculator tool.",
        "What is the weather in Tokyo and London? Check both.",
        "Reverse the string 'Hello MCP World!' using the string_reverse tool.",
    ]
    passed = 0
    for q in tests:
        try:
            run_agent(q)
            passed += 1
        except Exception as e:
            print(f"ERROR: {e}")

    print(f"\n\n{'='*60}")
    print(f"Result: {passed}/{len(tests)} tests passed")
    print("="*60)
