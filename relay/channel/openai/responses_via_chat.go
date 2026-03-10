package openai

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	openaicompat "github.com/QuantumNous/new-api/service/openaicompat"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// OaiChatToResponsesHandler converts a non-streaming Chat Completions response
// into a Responses API response. This is the inverse of OaiResponsesToChatHandler.
func OaiChatToResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	var chatResp dto.OpenAITextResponse
	if err := common.Unmarshal(body, &chatResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	if oaiError := chatResp.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	// Estimate usage if missing
	usage := &chatResp.Usage
	if usage.TotalTokens == 0 {
		text := ""
		if len(chatResp.Choices) > 0 {
			text = chatResp.Choices[0].Message.StringContent()
		}
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
		chatResp.Usage = *usage
	}

	responsesResp, convErr := service.ChatCompletionsResponseToResponsesResponse(&chatResp, info.UpstreamModelName)
	if convErr != nil {
		return nil, types.NewOpenAIError(convErr, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	responseBody, err := common.Marshal(responsesResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

// OaiChatToResponsesStreamHandler converts a streaming Chat Completions response
// into a Responses API SSE stream. This is the inverse of OaiResponsesToChatStreamHandler.
func OaiChatToResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	responseId := helper.GetResponseID(c)
	createAt := time.Now().Unix()
	model := info.UpstreamModelName

	state := openaicompat.NewChatToResponsesStreamState(responseId, createAt, model)

	var (
		usage     = &dto.Usage{}
		streamErr *types.NewAPIError
	)

	emitEvents := func(events []dto.ResponsesStreamResponse) bool {
		for _, event := range events {
			jsonData, marshalErr := common.Marshal(event)
			if marshalErr != nil {
				logger.LogError(c, "failed to marshal responses stream event: "+marshalErr.Error())
				continue
			}
			helper.ResponseChunkData(c, event, string(jsonData))
		}
		return true
	}

	helper.StreamScannerHandler(c, resp, info, func(data string) bool {
		if streamErr != nil {
			return false
		}

		var chunk dto.ChatCompletionsStreamResponse
		if err := common.UnmarshalJsonStr(data, &chunk); err != nil {
			logger.LogError(c, "failed to unmarshal chat stream chunk: "+err.Error())
			return true
		}

		// Update model from upstream
		if chunk.Model != "" {
			model = chunk.Model
		}

		// Handle usage-only chunk (stream_options.include_usage)
		if len(chunk.Choices) == 0 && chunk.Usage != nil {
			if usageFromChunk := state.HandleUsageChunk(&chunk); usageFromChunk != nil {
				usage = usageFromChunk
			}
			return true
		}

		// Extract usage from final chunk if present
		if chunk.Usage != nil && service.ValidUsage(chunk.Usage) {
			usage = chunk.Usage
		}

		events := state.HandleChatChunk(&chunk)
		if len(events) > 0 {
			emitEvents(events)
		}

		return true
	})

	if streamErr != nil {
		return nil, streamErr
	}

	// Fallback usage estimation
	if usage.TotalTokens == 0 {
		text := state.OutputText.String()
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
	}

	// Emit final events
	finalEvents := state.FinalEvents(usage)
	emitEvents(finalEvents)

	helper.Done(c)
	return usage, nil
}
