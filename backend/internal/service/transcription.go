package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type TranscriptionRequest struct {
	Model, Language, Filename string
	Audio                     []byte
}

func ParseTranscriptionRequest(body []byte, contentType string) (*TranscriptionRequest, error) {
	typ, params, err := mime.ParseMediaType(contentType)
	if err != nil || typ != "multipart/form-data" || params["boundary"] == "" {
		return nil, errors.New("multipart form with model and file is required")
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	out := &TranscriptionRequest{}
	seen := map[string]bool{}
	for {
		p, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("invalid multipart body")
		}
		name := p.FormName()
		if seen[name] {
			return nil, errors.New("duplicate transcription field")
		}
		seen[name] = true
		if name == "file" {
			out.Filename = p.FileName()
			out.Audio, err = io.ReadAll(io.LimitReader(p, (25<<20)+1))
			if err != nil || out.Filename == "" || len(out.Audio) == 0 || len(out.Audio) > 25<<20 {
				return nil, errors.New("invalid or oversized transcription file")
			}
			continue
		}
		v, err := io.ReadAll(io.LimitReader(p, 257))
		if err != nil || len(v) > 256 {
			return nil, errors.New("invalid transcription field")
		}
		value := strings.TrimSpace(string(v))
		switch name {
		case "model":
			out.Model = value
		case "language":
			out.Language = value
		case "response_format":
			if value != "json" {
				return nil, errors.New("only JSON transcription output is supported")
			}
		default:
			return nil, fmt.Errorf("unsupported transcription field")
		}
	}
	if out.Model == "" || len(out.Audio) == 0 {
		return nil, errors.New("model and file are required")
	}
	return out, nil
}

func (t *TranscriptionRequest) upstream(ctx context.Context, a *Account, model string, validate func(string) (string, error)) (*http.Request, error) {
	var b bytes.Buffer
	writer := multipart.NewWriter(&b)
	field := "file"
	if a.Platform == PlatformFish {
		if model != "transcribe-1-pro" {
			return nil, errors.New("unsupported Fish transcription model")
		}
		field = "audio"
	} else if a.Platform == PlatformOpenAI && a.Type == AccountTypeAPIKey {
		if err := writer.WriteField("model", model); err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("unsupported transcription account")
	}
	if t.Language != "" {
		if err := writer.WriteField("language", t.Language); err != nil {
			return nil, err
		}
	}
	part, err := writer.CreateFormFile(field, t.Filename)
	if err != nil {
		return nil, err
	}
	if _, err = part.Write(t.Audio); err != nil {
		return nil, err
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	if a.Platform == PlatformFish {
		return fishRequest(ctx, a, "/v1/asr", model, writer.FormDataContentType(), &b, validate)
	}
	base := strings.TrimSuffix(strings.TrimRight(a.GetOpenAIBaseURL(), "/"), "/v1")
	target, err := validate(base + "/v1/audio/transcriptions")
	if err != nil {
		return nil, err
	}
	key := a.GetCredential("api_key")
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("transcription API key is missing")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, &b)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+key)
	return req, nil
}

func (s *OpenAIGatewayService) ForwardTranscription(ctx context.Context, c *gin.Context, a *Account, t *TranscriptionRequest, mapped string) (*OpenAIForwardResult, error) {
	start := time.Now()
	model := resolveOpenAIForwardModel(a, t.Model, mapped)
	req, err := t.upstream(ctx, a, model, s.validateUpstreamBaseURL)
	if err != nil {
		writeSpeechError(c, 400, "invalid_request_error", err.Error())
		return nil, err
	}
	SetOpsUpstreamModel(c, model)
	SetActualOpenAIUpstreamEndpoint(c, req.URL.Path)
	proxy := ""
	if a.ProxyID != nil && a.Proxy != nil {
		proxy = a.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxy, a.ID, a.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(start).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, a, err, false)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := s.readUpstreamErrorBody(resp)
		message := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(body))
		setOpsUpstreamError(c, resp.StatusCode, message, "")
		if s.shouldFailoverOpenAIUpstreamResponse(a, resp.StatusCode, message, body) {
			s.handleOpenAIAccountUpstreamError(ctx, a, resp.StatusCode, resp.Header, body, model)
			return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: body}
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return s.handleCompatErrorResponse(resp, c, a, writeSpeechError, model)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	var result struct {
		Text *string `json:"text"`
	}
	if err != nil || len(raw) > 2<<20 || json.Unmarshal(raw, &result) != nil || result.Text == nil {
		writeSpeechError(c, 502, "upstream_error", "Invalid transcription response")
		return nil, errors.New("invalid transcription response")
	}
	usage := estimateGrokVoiceAudioUsage("stt", t.Audio, "application/octet-stream", raw, time.Since(start))
	c.JSON(http.StatusOK, gin.H{"text": *result.Text})
	return &OpenAIForwardResult{RequestID: StableGrokAudioBillingRequestID(resp.Header.Get("x-request-id")), Model: t.Model, UpstreamModel: model, BillingModel: model, Duration: time.Since(start), AudioUsage: usage}, nil
}
