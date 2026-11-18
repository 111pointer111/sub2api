package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// SpeechRequest is the public HTTP text-to-speech request shared by providers.
type SpeechRequest struct {
	Model          string   `json:"model"`
	Input          string   `json:"input"`
	Voice          string   `json:"voice"`
	ResponseFormat string   `json:"response_format,omitempty"`
	Speed          *float64 `json:"speed,omitempty"`
	Lang           string   `json:"lang,omitempty"`
	SamplingRate   int      `json:"samplingRate,omitempty"`
}

func ParseSpeechRequest(body []byte) (*SpeechRequest, error) {
	var request SpeechRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("invalid speech request: %w", err)
	}
	request.Model = strings.TrimSpace(request.Model)
	request.Voice = strings.TrimSpace(request.Voice)
	if request.Model == "" || strings.TrimSpace(request.Input) == "" || request.Voice == "" {
		return nil, errors.New("model, input and voice are required")
	}
	if request.ResponseFormat == "" {
		request.ResponseFormat = "mp3"
	}
	if request.Speed != nil && (math.IsNaN(*request.Speed) || math.IsInf(*request.Speed, 0) || *request.Speed <= 0) {
		return nil, errors.New("speed must be a positive number")
	}
	return &request, nil
}

// ForwardSpeech shares audio transport and metering; providers only build their native request.
func (s *OpenAIGatewayService) ForwardSpeech(ctx context.Context, c *gin.Context, account *Account, speech *SpeechRequest, mappedModel string) (*OpenAIForwardResult, error) {
	started := time.Now()
	model := resolveOpenAIForwardModel(account, speech.Model, mappedModel)
	var request *http.Request
	var err error
	switch account.Platform {
	case PlatformFish:
		request, err = newFishSpeechRequest(ctx, account, speech, model, s.validateUpstreamBaseURL)
	case PlatformRime:
		request, err = newRimeSpeechRequest(ctx, account, speech, model, s.validateUpstreamBaseURL)
	default:
		err = fmt.Errorf("speech is not supported for platform %s", account.Platform)
	}
	if err != nil {
		writeSpeechError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	SetOpsUpstreamModel(c, model)
	SetActualOpenAIUpstreamEndpoint(c, request.URL.Path)
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	response, err := s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(started).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body := s.readUpstreamErrorBody(response)
		message := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(body))
		setOpsUpstreamError(c, response.StatusCode, message, "")
		if s.shouldFailoverOpenAIUpstreamResponse(account, response.StatusCode, message, body) {
			s.handleOpenAIAccountUpstreamError(ctx, account, response.StatusCode, response.Header, body, model)
			return nil, &UpstreamFailoverError{StatusCode: response.StatusCode, ResponseBody: body}
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		return s.handleCompatErrorResponse(response, c, account, writeSpeechError, model)
	}
	contentType := response.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "audio/") && contentType != "application/octet-stream" {
		writeSpeechError(c, http.StatusBadGateway, "upstream_error", "Speech upstream returned a non-audio response")
		return nil, errors.New("speech upstream returned a non-audio response")
	}
	buffer := make([]byte, 32*1024)
	n, readErr := io.ReadAtLeast(response.Body, buffer, 1)
	if n == 0 {
		writeSpeechError(c, http.StatusBadGateway, "upstream_error", "Speech upstream returned empty audio")
		return nil, errors.New("speech upstream returned empty audio")
	}
	result := &OpenAIForwardResult{
		RequestID:       StableGrokAudioBillingRequestID(response.Header.Get("x-request-id")),
		UpstreamHeaders: response.Header,
		Model:           speech.Model,
		BillingModel:    model,
		UpstreamModel:   model,
		Stream:          true,
		AudioUsage:      &AudioUsage{Mode: "tts", DurationOrUnits: float64(utf8.RuneCountInString(speech.Input)) / 1e6},
	}
	defer func() { result.Duration = time.Since(started) }()
	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), response.Header, s.responseHeaderFilter)
	c.Header("Content-Type", contentType)
	c.Header("X-Accel-Buffering", "no")
	MarkResponseCommitted(c)
	for {
		if _, err := c.Writer.Write(buffer[:n]); err != nil {
			return result, err
		}
		c.Writer.Flush()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return result, nil
			}
			return result, readErr
		}
		n, readErr = response.Body.Read(buffer)
	}
}

func writeSpeechError(c *gin.Context, status int, errorType, message string) {
	c.JSON(status, gin.H{"error": gin.H{"type": errorType, "message": message}})
}
