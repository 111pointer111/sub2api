package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *AccountTestService) testSpeechConnection(c *gin.Context, account *Account, model, prompt string) error {
	if model == "" {
		model = "coda"
		if account.Platform == PlatformFish {
			model = "s2.1-pro"
		}
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "Hello from Sub2API."
	}
	speech := &SpeechRequest{Model: model, Input: prompt, Voice: "astra", ResponseFormat: "mp3"}
	mapped := resolveOpenAIForwardModel(account, model, "")
	var request *http.Request
	var err error
	if account.Platform == PlatformFish {
		voices := strings.Fields(account.GetCredential("fish_voice_ids"))
		if mapped == "transcribe-1-pro" {
			t := &TranscriptionRequest{Model: model, Filename: "test.wav", Audio: minimalSilentWAV()}
			request, err = t.upstream(c.Request.Context(), account, mapped, s.validateUpstreamBaseURL)
		} else {
			if len(voices) == 0 {
				return s.sendErrorAndEnd(c, "Register a Fish voice ID before testing synthesis")
			}
			speech.Voice = voices[0]
			request, err = newFishSpeechRequest(c.Request.Context(), account, speech, mapped, s.validateUpstreamBaseURL)
		}
	} else {
		request, err = newRimeSpeechRequest(c.Request.Context(), account, speech, mapped, s.validateUpstreamBaseURL)
	}
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
		return s.sendErrorAndEnd(c, fmt.Sprintf("%s returned HTTP %d", account.Platform, response.StatusCode))
	}
	contentType := response.Header.Get("Content-Type")
	if mapped == "transcribe-1-pro" {
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
		var result struct {
			Text *string `json:"text"`
		}
		if readErr != nil || len(raw) > 2<<20 || json.Unmarshal(raw, &result) != nil || result.Text == nil {
			return s.sendErrorAndEnd(c, "Invalid transcription response")
		}
		s.sendEvent(c, TestEvent{Type: "content", Text: *result.Text})
		s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
		return nil
	}
	if contentType == "application/octet-stream" {
		contentType = "audio/mpeg"
	}
	if !strings.HasPrefix(contentType, "audio/") {
		return s.sendErrorAndEnd(c, "Speech upstream returned a non-audio response")
	}
	audio, err := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	if err != nil || len(audio) == 0 || len(audio) > 8<<20 {
		return s.sendErrorAndEnd(c, "Speech upstream returned empty, incomplete or oversized audio")
	}
	s.sendEvent(c, TestEvent{Type: "content", AudioURL: "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(audio), MimeType: contentType})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}
