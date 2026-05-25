# test_real2.py — phenomenon-based 1h vs 5m test, no cost math required
import json, sys, time, urllib.request, urllib.error

import os
KEY = os.environ.get("OPENROUTER_API_KEY", "sk-or-v1-<YOUR_OPENROUTER_KEY>")
URL = "https://openrouter.ai/api/v1/messages"
PRESET = os.environ.get("OPENROUTER_PRESET", "@preset/<your-preset-slug>")

# ~5000 tokens of cacheable system prefix
PREFIX = ("You are a meticulous coding assistant who reasons step by step and explains "
          "technical details with care. Always cite specific lines and files. ") * 250

def body(tag):
    return {
        "preset": PRESET,
        "model": "anthropic/claude-sonnet-4-6",
        "max_tokens": 24,
        "stream": False,                 # non-streaming -> single JSON, robust parse
        "system": [
            {"type": "text", "text": PREFIX.strip(),
             "cache_control": {"type": "ephemeral", "ttl": "1h"}}
        ],
        # Different tail per call so OR/Bedrock can't dedupe to an empty response
        "messages": [{"role": "user", "content": f"Reply with just the token: {tag}"}],
    }

def call(label, tag):
    req = urllib.request.Request(URL,
        data=json.dumps(body(tag)).encode(),
        headers={
            "Authorization": f"Bearer {KEY}",
            "Content-Type": "application/json",
            "anthropic-version": "2023-06-01",
        })
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            raw = r.read().decode("utf-8", errors="replace")
    except urllib.error.HTTPError as e:
        raw = e.read().decode(errors="replace")
        print(f"=== {label} === HTTP {e.code}\n{raw[:800]}\n", flush=True)
        with open(f"raw_{label}.txt", "w", encoding="utf-8") as f: f.write(raw)
        return

    # Always dump raw for inspection
    with open(f"raw_{label}.json", "w", encoding="utf-8") as f: f.write(raw)

    try:
        d = json.loads(raw)
    except json.JSONDecodeError:
        print(f"=== {label} === non-JSON body (first 600 chars):\n{raw[:600]}\n", flush=True)
        return

    u  = d.get("usage", {}) or {}
    cw = u.get("cache_creation_input_tokens") or 0
    cr = u.get("cache_read_input_tokens") or 0
    inp = u.get("input_tokens") or 0
    out = u.get("output_tokens") or 0
    phenom = "READ-HIT" if cr > 0 else ("WRITE" if cw > 0 else "NEITHER")

    print(f"=== {label} ===", flush=True)
    print(f"  provider: {d.get('provider')}   is_byok: {u.get('is_byok')}   model: {d.get('model')}", flush=True)
    print(f"  input={inp}  output={out}  cache_write={cw}  cache_read={cr}", flush=True)
    print(f"  phenomenon: {phenom}", flush=True)
    print(f"  raw saved to: raw_{label}.json", flush=True)
    print(flush=True)

def wait_heartbeat(secs, interval=15):
    start = time.time(); elapsed = 0
    print(f">>> waiting {secs}s (heartbeat every {interval}s)...", flush=True)
    while elapsed < secs:
        time.sleep(min(interval, secs - elapsed))
        elapsed = int(time.time() - start)
        print(f"  [alive] t+{elapsed}s  remaining {secs-elapsed}s", flush=True)

call("R1-write",    tag="alpha")
time.sleep(3)
call("R2-read-3s",  tag="beta")        # should hit cache regardless of TTL
wait_heartbeat(360, interval=15)
call("R3-read-6m",  tag="gamma")       # the decisive call
