# 渠道而外设置说明

该配置用于设置一些额外的渠道参数，可以通过 JSON 对象进行配置。主要包含以下两个设置项：

1. force_format
    - 用于标识是否对数据进行强制格式化为 OpenAI 格式
    - 类型为布尔值，设置为 true 时启用强制格式化

2. proxy
    - 用于配置网络代理
    - 类型为字符串，填写代理地址（例如 socks5 协议的代理地址）

3. thinking_to_content
   - 用于标识是否将思考内容`reasoning_content`转换为`<think>`标签拼接到内容中返回
   - 类型为布尔值，设置为 true 时启用思考内容转换

--------------------------------------------------------------

## JSON 格式示例

以下是一个示例配置，启用强制格式化并设置了代理地址：

```json
{
    "force_format": true,
   "thinking_to_content": true,
    "proxy": "socks5://xxxxxxx"
}
```

--------------------------------------------------------------

通过调整上述 JSON 配置中的值，可以灵活控制渠道的额外行为，比如是否进行格式化以及使用特定的网络代理。

--------------------------------------------------------------

## AWS Bedrock Claude Opus 4.7 测试

Claude Opus 4.7 在 Bedrock 上不能直接使用基础模型 ID `anthropic.claude-opus-4-7` 以 on-demand 方式调用，需要使用 inference profile ID 或 ARN。

已验证可用的 profile ID 示例：

- `global.anthropic.claude-opus-4-7`
- `us.anthropic.claude-opus-4-7`

可先列出当前账号可用的 inference profile：

```bash
aws bedrock list-inference-profiles --region us-east-1
```

PowerShell 测试示例：

```powershell
$messagesPath = Join-Path $env:TEMP "bedrock-messages.json"
$inferencePath = Join-Path $env:TEMP "bedrock-inference.json"
$additionalPath = Join-Path $env:TEMP "bedrock-additional.json"
$performancePath = Join-Path $env:TEMP "bedrock-performance.json"

[System.IO.File]::WriteAllText($messagesPath, '[{"role":"user","content":[{"text":"ping"}]}]', [System.Text.UTF8Encoding]::new($false))
[System.IO.File]::WriteAllText($inferencePath, '{"maxTokens":32,"stopSequences":[]}', [System.Text.UTF8Encoding]::new($false))
[System.IO.File]::WriteAllText($additionalPath, '{}', [System.Text.UTF8Encoding]::new($false))
[System.IO.File]::WriteAllText($performancePath, '{"latency":"standard"}', [System.Text.UTF8Encoding]::new($false))

$env:PYTHONIOENCODING = "utf-8"
$env:PYTHONUTF8 = "1"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding = [System.Text.Encoding]::UTF8

aws bedrock-runtime converse `
  --model-id global.anthropic.claude-opus-4-7 `
  --messages file://$messagesPath `
  --inference-config file://$inferencePath `
  --additional-model-request-fields file://$additionalPath `
  --performance-config file://$performancePath `
  --region us-east-1
```

如果直接使用 `anthropic.claude-opus-4-7` 返回 “Invocation of model ID ... with on-demand throughput isn’t supported”，说明需要改用 inference profile ID 或 ARN，这属于 Bedrock 的模型调用要求，不是请求体格式错误。
