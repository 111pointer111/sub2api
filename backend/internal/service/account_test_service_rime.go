package service

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *AccountTestService) testRimeSpeechConnection(c *gin.Context, account *Account, model, prompt string) error {
	if model == "" {
		model = "coda"
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "Hello from Sub2API."
	}
	speech := &SpeechRequest{Model: model, Input: prompt, Voice: "astra", ResponseFormat: "mp3"}
	request, err := newRimeSpeechRequest(c.Request.Context(), account, speech, resolveOpenAIForwardModel(account, model, ""), s.validateUpstreamBaseURL)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.Flush()
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	response, err := s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, sanitizeUpstreamErrorMessage(err.Error()))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Rime returned HTTP %d", response.StatusCode))
	}
	contentType := response.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "audio/") {
		return s.sendErrorAndEnd(c, "Rime returned a non-audio response")
	}
	audio, err := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	if err != nil || len(audio) == 0 || len(audio) > 8<<20 {
		return s.sendErrorAndEnd(c, "Rime returned empty, incomplete or oversized audio")
	}
	s.sendEvent(c, TestEvent{Type: "content", AudioURL: "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(audio), MimeType: contentType})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}
