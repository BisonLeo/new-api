package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
)

type AwsClaudeRequest struct {
	// AnthropicVersion should be "bedrock-2023-05-31"
	AnthropicVersion string              `json:"anthropic_version"`
	AnthropicBeta    json.RawMessage     `json:"anthropic_beta,omitempty"`
	System           any                 `json:"system,omitempty"`
	Messages         []dto.ClaudeMessage `json:"messages"`
	MaxTokens        uint                `json:"max_tokens,omitempty"`
	Temperature      *float64            `json:"temperature,omitempty"`
	TopP             float64             `json:"top_p,omitempty"`
	TopK             int                 `json:"top_k,omitempty"`
	StopSequences    []string            `json:"stop_sequences,omitempty"`
	Tools            any                 `json:"tools,omitempty"`
	ToolChoice       any                 `json:"tool_choice,omitempty"`
	Thinking         *dto.Thinking       `json:"thinking,omitempty"`
	OutputConfig     json.RawMessage     `json:"output_config,omitempty"`
	//Metadata         json.RawMessage     `json:"metadata,omitempty"`
}

// redactForLog returns a JSON summary of jsonData with verbose content stripped,
// keeping only metadata fields useful for debugging (betas, model params, etc.).
func redactForLog(jsonData []byte) string {
	var m map[string]interface{}
	if err := common.Unmarshal(jsonData, &m); err != nil {
		return "<unmarshal error>"
	}
	if msgs, ok := m["messages"]; ok {
		if arr, ok := msgs.([]interface{}); ok {
			roles := make([]string, 0, len(arr))
			for _, msg := range arr {
				if obj, ok := msg.(map[string]interface{}); ok {
					if role, ok := obj["role"].(string); ok {
						roles = append(roles, role)
					}
				}
			}
			m["messages"] = fmt.Sprintf("[%d messages: %s]", len(arr), strings.Join(roles, ","))
		}
	}
	if _, ok := m["system"]; ok {
		m["system"] = "[redacted]"
	}
	if tools, ok := m["tools"]; ok {
		if arr, ok := tools.([]interface{}); ok {
			names := make([]string, 0, len(arr))
			for _, t := range arr {
				if obj, ok := t.(map[string]interface{}); ok {
					if name, ok := obj["name"].(string); ok {
						names = append(names, name)
					}
				}
			}
			m["tools"] = fmt.Sprintf("[%d tools: %s]", len(arr), strings.Join(names, ","))
		}
	}
	b, _ := common.Marshal(m)
	return string(b)
}

// bedrockCacheControlAllowedFields is the set of cache_control sub-fields that
// AWS Bedrock accepts on Anthropic Claude models. "scope" is Anthropic-API-only
// and must be stripped. "ttl" is honored natively by Bedrock on supported
// models (Sonnet 4.5, Haiku 4.5, Opus 4.5+) — no anthropic_beta flag is
// required, only the cache_control field itself. See AWS docs:
// https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html
var bedrockCacheControlAllowedFields = map[string]struct{}{
	"type": {},
	"ttl":  {},
}

// stripBedrockUnsupportedCacheFields recursively removes fields from cache_control
// that Bedrock doesn't support (e.g. "scope").
func stripBedrockUnsupportedCacheFields(v any) any {
	switch val := v.(type) {
	case map[string]interface{}:
		if cc, ok := val["cache_control"]; ok {
			if ccMap, ok := cc.(map[string]interface{}); ok {
				cleaned := map[string]interface{}{}
				for k, v := range ccMap {
					if _, allowed := bedrockCacheControlAllowedFields[k]; allowed {
						cleaned[k] = v
					}
				}
				val["cache_control"] = cleaned
			}
		}
		for k, child := range val {
			val[k] = stripBedrockUnsupportedCacheFields(child)
		}
		return val
	case []interface{}:
		for i, child := range val {
			val[i] = stripBedrockUnsupportedCacheFields(child)
		}
		return val
	default:
		return v
	}
}


func formatRequest(requestBody io.Reader, requestHeader http.Header) (*AwsClaudeRequest, error) {
	// Read body bytes so we can log them and still decode
	bodyBytes, err := io.ReadAll(requestBody)
	if err != nil {
		return nil, err
	}
	logger.LogInfo(context.Background(), fmt.Sprintf("aws bedrock request body (pre-format): %s", redactForLog(bodyBytes)))

	var awsClaudeRequest AwsClaudeRequest
	err = common.DecodeJson(bytes.NewReader(bodyBytes), &awsClaudeRequest)
	if err != nil {
		return nil, err
	}
	logger.LogInfo(context.Background(), fmt.Sprintf("aws bedrock anthropic_beta from body: %s", string(awsClaudeRequest.AnthropicBeta)))

	awsClaudeRequest.AnthropicVersion = "bedrock-2023-05-31"

	// Strip Anthropic-API-only sub-fields from cache_control (e.g. "scope")
	// that Bedrock rejects. The "ttl" sub-field IS preserved — Bedrock honors
	// `cache_control:{type:"ephemeral", ttl:"1h"}` directly on supported
	// models (Claude Sonnet 4.5, Haiku 4.5, Opus 4.5+), no beta flag needed.
	awsClaudeRequest.System = stripBedrockUnsupportedCacheFields(awsClaudeRequest.System)
	for i := range awsClaudeRequest.Messages {
		awsClaudeRequest.Messages[i].Content = stripBedrockUnsupportedCacheFields(awsClaudeRequest.Messages[i].Content)
	}
	awsClaudeRequest.Tools = stripBedrockUnsupportedCacheFields(awsClaudeRequest.Tools)

	// anthropic-beta HTTP header is intentionally NOT forwarded to the Bedrock request body.
	// Client betas (e.g. from Claude Code CLI: claude-code-*, advisor-tool-*,
	// interleaved-thinking-*, prompt-caching-scope-*, etc.) are Anthropic-API-specific
	// and are rejected by Bedrock with "ValidationException: invalid beta flag".
	// Bedrock's accepted set is small and undocumented; trying to allowlist client
	// betas tends to ship 400s as Anthropic adds new ones. To pass a known
	// Bedrock-supported beta (e.g. computer-use-2024-10-22), set anthropic_beta
	// in the channel param override so it arrives via the request body, not the header.
	anthropicBetaValues := requestHeader.Get("anthropic-beta")
	logger.LogInfo(context.Background(), fmt.Sprintf("aws bedrock anthropic-beta header (ignored for Bedrock): %q", anthropicBetaValues))

	finalJson, _ := common.Marshal(awsClaudeRequest)
	logger.LogInfo(context.Background(), fmt.Sprintf("aws bedrock final request body: %s", redactForLog(finalJson)))
	return &awsClaudeRequest, nil
}

// NovaMessage Nova模型使用messages-v1格式
type NovaMessage struct {
	Role    string        `json:"role"`
	Content []NovaContent `json:"content"`
}

type NovaContent struct {
	Text string `json:"text"`
}

type NovaRequest struct {
	SchemaVersion   string               `json:"schemaVersion"`             // 请求版本，例如 "1.0"
	Messages        []NovaMessage        `json:"messages"`                  // 对话消息列表
	InferenceConfig *NovaInferenceConfig `json:"inferenceConfig,omitempty"` // 推理配置，可选
}

type NovaInferenceConfig struct {
	MaxTokens     int      `json:"maxTokens,omitempty"`     // 最大生成的 token 数
	Temperature   float64  `json:"temperature,omitempty"`   // 随机性 (默认 0.7, 范围 0-1)
	TopP          float64  `json:"topP,omitempty"`          // nucleus sampling (默认 0.9, 范围 0-1)
	TopK          int      `json:"topK,omitempty"`          // 限制候选 token 数 (默认 50, 范围 0-128)
	StopSequences []string `json:"stopSequences,omitempty"` // 停止生成的序列
}

// 转换OpenAI请求为Nova格式
func convertToNovaRequest(req *dto.GeneralOpenAIRequest) *NovaRequest {
	novaMessages := make([]NovaMessage, len(req.Messages))
	for i, msg := range req.Messages {
		novaMessages[i] = NovaMessage{
			Role:    msg.Role,
			Content: []NovaContent{{Text: msg.StringContent()}},
		}
	}

	novaReq := &NovaRequest{
		SchemaVersion: "messages-v1",
		Messages:      novaMessages,
	}

	// 设置推理配置
	if (req.MaxTokens != nil && *req.MaxTokens != 0) || (req.Temperature != nil && *req.Temperature != 0) || (req.TopP != nil && *req.TopP != 0) || (req.TopK != nil && *req.TopK != 0) || req.Stop != nil {
		novaReq.InferenceConfig = &NovaInferenceConfig{}
		if req.MaxTokens != nil && *req.MaxTokens != 0 {
			novaReq.InferenceConfig.MaxTokens = int(*req.MaxTokens)
		}
		if req.Temperature != nil && *req.Temperature != 0 {
			novaReq.InferenceConfig.Temperature = *req.Temperature
		}
		if req.TopP != nil && *req.TopP != 0 {
			novaReq.InferenceConfig.TopP = *req.TopP
		}
		if req.TopK != nil && *req.TopK != 0 {
			novaReq.InferenceConfig.TopK = *req.TopK
		}
		if req.Stop != nil {
			if stopSequences := parseStopSequences(req.Stop); len(stopSequences) > 0 {
				novaReq.InferenceConfig.StopSequences = stopSequences
			}
		}
	}

	return novaReq
}

// parseStopSequences 解析停止序列，支持字符串或字符串数组
func parseStopSequences(stop any) []string {
	if stop == nil {
		return nil
	}

	switch v := stop.(type) {
	case string:
		if v != "" {
			return []string{v}
		}
	case []string:
		return v
	case []interface{}:
		var sequences []string
		for _, item := range v {
			if str, ok := item.(string); ok && str != "" {
				sequences = append(sequences, str)
			}
		}
		return sequences
	}
	return nil
}
