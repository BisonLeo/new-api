package openaicompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

func ResponsesResponseToChatCompletionsResponse(resp *dto.OpenAIResponsesResponse, id string) (*dto.OpenAITextResponse, *dto.Usage, error) {
	if resp == nil {
		return nil, nil, errors.New("response is nil")
	}

	text := ExtractOutputTextFromResponses(resp)

	usage := &dto.Usage{}
	if resp.Usage != nil {
		if resp.Usage.InputTokens != 0 {
			usage.PromptTokens = resp.Usage.InputTokens
			usage.InputTokens = resp.Usage.InputTokens
		}
		if resp.Usage.OutputTokens != 0 {
			usage.CompletionTokens = resp.Usage.OutputTokens
			usage.OutputTokens = resp.Usage.OutputTokens
		}
		if resp.Usage.TotalTokens != 0 {
			usage.TotalTokens = resp.Usage.TotalTokens
		} else {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
		if resp.Usage.InputTokensDetails != nil {
			usage.PromptTokensDetails.CachedTokens = resp.Usage.InputTokensDetails.CachedTokens
			usage.PromptTokensDetails.ImageTokens = resp.Usage.InputTokensDetails.ImageTokens
			usage.PromptTokensDetails.AudioTokens = resp.Usage.InputTokensDetails.AudioTokens
		}
		if resp.Usage.CompletionTokenDetails.ReasoningTokens != 0 {
			usage.CompletionTokenDetails.ReasoningTokens = resp.Usage.CompletionTokenDetails.ReasoningTokens
		}
	}

	created := resp.CreatedAt

	var toolCalls []dto.ToolCallResponse
	if len(resp.Output) > 0 {
		for _, out := range resp.Output {
			if out.Type != "function_call" {
				continue
			}
			name := strings.TrimSpace(out.Name)
			if name == "" {
				continue
			}
			callId := strings.TrimSpace(out.CallId)
			if callId == "" {
				callId = strings.TrimSpace(out.ID)
			}
			toolCalls = append(toolCalls, dto.ToolCallResponse{
				ID:   callId,
				Type: "function",
				Function: dto.FunctionResponse{
					Name:      name,
					Arguments: out.Arguments,
				},
			})
		}
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	msg := dto.Message{
		Role:    "assistant",
		Content: text,
	}
	if len(toolCalls) > 0 {
		msg.SetToolCalls(toolCalls)
	}

	out := &dto.OpenAITextResponse{
		Id:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   resp.Model,
		Choices: []dto.OpenAITextResponseChoice{
			{
				Index:        0,
				Message:      msg,
				FinishReason: finishReason,
			},
		},
		Usage: *usage,
	}

	return out, usage, nil
}

// ResponsesRequestToChatCompletionsRequest converts a Responses API request
// to a Chat Completions API request.
//
// Limitations: server-side stateful fields (previous_response_id, conversation,
// context_management) cannot be represented in chat completions. If
// previous_response_id is set and input is empty the call is rejected because
// the client depends on server-side conversation history that we cannot
// reconstruct. When input IS present the stateful fields are silently ignored
// (best-effort conversion).
func ResponsesRequestToChatCompletionsRequest(req *dto.OpenAIResponsesRequest) (*dto.GeneralOpenAIRequest, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	if req.Model == "" {
		return nil, errors.New("model is required")
	}

	// Reject requests that rely entirely on server-side state.
	if req.PreviousResponseID != "" && len(req.Input) == 0 {
		return nil, errors.New(
			"previous_response_id without input is not supported: " +
				"chat completions conversion cannot retrieve server-side conversation history")
	}

	var messages []dto.Message

	// Instructions → system message
	if len(req.Instructions) > 0 {
		var instructions string
		if err := common.Unmarshal(req.Instructions, &instructions); err == nil && strings.TrimSpace(instructions) != "" {
			messages = append(messages, dto.Message{
				Role:    "system",
				Content: instructions,
			})
		}
	}

	// Input → messages
	if len(req.Input) > 0 {
		switch common.GetJsonType(req.Input) {
		case "string":
			var inputStr string
			if err := common.Unmarshal(req.Input, &inputStr); err == nil {
				messages = append(messages, dto.Message{
					Role:    "user",
					Content: inputStr,
				})
			}
		case "array":
			var inputItems []map[string]any
			if err := common.Unmarshal(req.Input, &inputItems); err != nil {
				return nil, fmt.Errorf("failed to parse input: %w", err)
			}

			var pendingToolCalls []dto.ToolCallResponse

			flushToolCalls := func() {
				if len(pendingToolCalls) == 0 {
					return
				}
				msg := dto.Message{
					Role:    "assistant",
					Content: "",
				}
				msg.SetToolCalls(pendingToolCalls)
				messages = append(messages, msg)
				pendingToolCalls = nil
			}

			for _, item := range inputItems {
				itemType, _ := item["type"].(string)
				role, _ := item["role"].(string)

				switch {
				case itemType == "function_call":
					callID, _ := item["call_id"].(string)
					name, _ := item["name"].(string)
					arguments, _ := item["arguments"].(string)
					pendingToolCalls = append(pendingToolCalls, dto.ToolCallResponse{
						ID:   callID,
						Type: "function",
						Function: dto.FunctionResponse{
							Name:      name,
							Arguments: arguments,
						},
					})

				case itemType == "function_call_output":
					flushToolCalls()
					callID, _ := item["call_id"].(string)
					output := interfaceToJSONString(item["output"])
					messages = append(messages, dto.Message{
						Role:       "tool",
						Content:    output,
						ToolCallId: callID,
					})

				case role == "user" || role == "assistant" || role == "system" || role == "developer":
					flushToolCalls()
					msgRole := role
					if msgRole == "developer" {
						msgRole = "system"
					}
					msg := dto.Message{Role: msgRole}
					if content, ok := item["content"]; ok {
						msg.Content = convertResponsesContentToChat(content)
					}
					messages = append(messages, msg)

				default:
					flushToolCalls()
					if content, ok := item["content"]; ok {
						msg := dto.Message{Role: "user"}
						msg.Content = convertResponsesContentToChat(content)
						messages = append(messages, msg)
					}
				}
			}
			flushToolCalls()
		}
	}

	if len(messages) == 0 {
		return nil, errors.New("no messages could be derived from input")
	}

	out := &dto.GeneralOpenAIRequest{
		Model:                req.Model,
		Messages:             messages,
		Stream:               req.Stream,
		User:                 req.User,
		Store:                req.Store,
		Metadata:             req.Metadata,
		PromptCacheRetention: req.PromptCacheRetention,
	}

	if len(req.PromptCacheKey) > 0 {
		var key string
		if err := common.Unmarshal(req.PromptCacheKey, &key); err == nil {
			out.PromptCacheKey = key
		}
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		out.MaxCompletionTokens = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out.Temperature = req.Temperature
	}
	if req.TopP != nil {
		out.TopP = req.TopP
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" && req.Reasoning.Effort != "none" {
		out.ReasoningEffort = req.Reasoning.Effort
	}

	// Stream options
	if req.Stream != nil && *req.Stream {
		out.StreamOptions = &dto.StreamOptions{IncludeUsage: true}
	}

	// ParallelToolCalls
	if len(req.ParallelToolCalls) > 0 {
		var ptc bool
		if err := common.Unmarshal(req.ParallelToolCalls, &ptc); err == nil {
			out.ParallelTooCalls = &ptc
		}
	}

	// Tools
	if len(req.Tools) > 0 {
		var tools []map[string]any
		if err := common.Unmarshal(req.Tools, &tools); err == nil {
			for _, tool := range tools {
				toolType, _ := tool["type"].(string)
				if toolType == "" {
					continue
				}
				if toolType == "function" {
					name, _ := tool["name"].(string)
					description, _ := tool["description"].(string)
					parameters := tool["parameters"]
					out.Tools = append(out.Tools, dto.ToolCallRequest{
						Type: "function",
						Function: dto.FunctionRequest{
							Name:        name,
							Description: description,
							Parameters:  parameters,
						},
					})
					continue
				}
				if b, err := common.Marshal(tool); err == nil {
					out.Tools = append(out.Tools, dto.ToolCallRequest{
						Type:   toolType,
						Custom: b,
					})
				}
			}
		}
	}

	// ToolChoice
	if len(req.ToolChoice) > 0 {
		var tcStr string
		if err := common.Unmarshal(req.ToolChoice, &tcStr); err == nil {
			out.ToolChoice = tcStr
		} else {
			var tcMap map[string]any
			if err := common.Unmarshal(req.ToolChoice, &tcMap); err == nil {
				tcType, _ := tcMap["type"].(string)
				if tcType == "function" {
					name, _ := tcMap["name"].(string)
					if name != "" {
						out.ToolChoice = map[string]any{
							"type":     "function",
							"function": map[string]any{"name": name},
						}
					} else {
						out.ToolChoice = tcMap
					}
				} else {
					out.ToolChoice = tcMap
				}
			}
		}
	}

	// Text (response format)
	if len(req.Text) > 0 {
		out.ResponseFormat = convertResponsesTextToResponseFormat(req.Text)
	}

	return out, nil
}

// interfaceToJSONString converts an arbitrary value to a string suitable for
// the Chat Completions tool message content field. Strings are returned as-is;
// maps and slices are JSON-marshaled so they round-trip correctly instead of
// being rendered with Go's fmt.Sprintf formatting.
func interfaceToJSONString(v any) string {
	if v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	default:
		b, err := common.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// convertResponsesContentToChat converts Responses API content to Chat API content.
func convertResponsesContentToChat(content any) any {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var chatParts []dto.MediaContent
		for _, part := range v {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			partType, _ := partMap["type"].(string)
			switch partType {
			case "input_text":
				text, _ := partMap["text"].(string)
				chatParts = append(chatParts, dto.MediaContent{
					Type: dto.ContentTypeText,
					Text: text,
				})
			case "input_image":
				imageURL := partMap["image_url"]
				switch iu := imageURL.(type) {
				case string:
					chatParts = append(chatParts, dto.MediaContent{
						Type:     dto.ContentTypeImageURL,
						ImageUrl: &dto.MessageImageUrl{Url: iu},
					})
				default:
					chatParts = append(chatParts, dto.MediaContent{
						Type:     dto.ContentTypeImageURL,
						ImageUrl: imageURL,
					})
				}
			case "input_audio":
				chatParts = append(chatParts, dto.MediaContent{
					Type:       dto.ContentTypeInputAudio,
					InputAudio: partMap["input_audio"],
				})
			case "input_file":
				chatParts = append(chatParts, dto.MediaContent{
					Type: dto.ContentTypeFile,
					File: partMap["file"],
				})
			case "input_video":
				chatParts = append(chatParts, dto.MediaContent{
					Type:     dto.ContentTypeVideoUrl,
					VideoUrl: partMap["video_url"],
				})
			default:
				chatParts = append(chatParts, dto.MediaContent{
					Type: partType,
				})
			}
		}
		if len(chatParts) > 0 {
			return chatParts
		}
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// convertResponsesTextToResponseFormat converts Responses API text field to Chat API response_format.
func convertResponsesTextToResponseFormat(textRaw json.RawMessage) *dto.ResponseFormat {
	if len(textRaw) == 0 {
		return nil
	}
	var textObj map[string]any
	if err := common.Unmarshal(textRaw, &textObj); err != nil {
		return nil
	}
	formatRaw, ok := textObj["format"]
	if !ok {
		return nil
	}
	formatMap, ok := formatRaw.(map[string]any)
	if !ok {
		return nil
	}
	formatType, _ := formatMap["type"].(string)
	if formatType == "" {
		return nil
	}

	rf := &dto.ResponseFormat{Type: formatType}
	if formatType == "json_schema" {
		schemaCopy := make(map[string]any)
		for k, v := range formatMap {
			if k == "type" {
				continue
			}
			schemaCopy[k] = v
		}
		if len(schemaCopy) > 0 {
			rf.JsonSchema, _ = common.Marshal(schemaCopy)
		}
	}
	return rf
}

// ChatCompletionsResponseToResponsesResponse converts a Chat Completions response
// to a Responses API response.
func ChatCompletionsResponseToResponsesResponse(resp *dto.OpenAITextResponse, model string) (*dto.OpenAIResponsesResponse, error) {
	if resp == nil {
		return nil, errors.New("response is nil")
	}

	respID := "resp_" + common.GetUUID()
	now := int(time.Now().Unix())
	if model == "" {
		model = resp.Model
	}

	var outputs []dto.ResponsesOutput
	var usage *dto.Usage

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]

		// Text content — handle both string content and array content
		var textContent string
		if choice.Message.IsStringContent() {
			textContent = choice.Message.StringContent()
		} else {
			// Extract text from array/structured content (multimodal responses)
			parts := choice.Message.ParseContent()
			var sb strings.Builder
			for _, p := range parts {
				if p.Type == dto.ContentTypeText && p.Text != "" {
					sb.WriteString(p.Text)
				}
			}
			textContent = sb.String()
		}
		if textContent != "" {
			outputs = append(outputs, dto.ResponsesOutput{
				Type:   "message",
				ID:     "msg_" + common.GetUUID(),
				Status: "completed",
				Role:   "assistant",
				Content: []dto.ResponsesOutputContent{
					{
						Type:        "output_text",
						Text:        textContent,
						Annotations: []interface{}{},
					},
				},
			})
		}

		// Tool calls
		for _, tc := range choice.Message.ParseToolCalls() {
			if tc.Type != "" && tc.Type != "function" {
				continue
			}
			callID := strings.TrimSpace(tc.ID)
			if callID == "" {
				continue
			}
			outputs = append(outputs, dto.ResponsesOutput{
				Type:      "function_call",
				ID:        "fc_" + common.GetUUID(),
				Status:    "completed",
				CallId:    callID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}

	// Usage conversion
	usage = &dto.Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
		InputTokens:      resp.Usage.PromptTokens,
		OutputTokens:     resp.Usage.CompletionTokens,
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	usage.PromptTokensDetails = resp.Usage.PromptTokensDetails
	usage.CompletionTokenDetails = resp.Usage.CompletionTokenDetails
	if resp.Usage.PromptTokensDetails.CachedTokens > 0 ||
		resp.Usage.PromptTokensDetails.ImageTokens > 0 ||
		resp.Usage.PromptTokensDetails.AudioTokens > 0 {
		usage.InputTokensDetails = &dto.InputTokenDetails{
			CachedTokens: resp.Usage.PromptTokensDetails.CachedTokens,
			ImageTokens:  resp.Usage.PromptTokensDetails.ImageTokens,
			AudioTokens:  resp.Usage.PromptTokensDetails.AudioTokens,
		}
	}

	out := &dto.OpenAIResponsesResponse{
		ID:        respID,
		Object:    "response",
		CreatedAt: now,
		Status:    json.RawMessage(`"completed"`),
		Model:     model,
		Output:    outputs,
		Usage:     usage,
	}

	return out, nil
}

func ExtractOutputTextFromResponses(resp *dto.OpenAIResponsesResponse) string {
	if resp == nil || len(resp.Output) == 0 {
		return ""
	}

	var sb strings.Builder

	// Prefer assistant message outputs.
	for _, out := range resp.Output {
		if out.Type != "message" {
			continue
		}
		if out.Role != "" && out.Role != "assistant" {
			continue
		}
		for _, c := range out.Content {
			if c.Type == "output_text" && c.Text != "" {
				sb.WriteString(c.Text)
			}
		}
	}
	if sb.Len() > 0 {
		return sb.String()
	}
	for _, out := range resp.Output {
		for _, c := range out.Content {
			if c.Text != "" {
				sb.WriteString(c.Text)
			}
		}
	}
	return sb.String()
}
