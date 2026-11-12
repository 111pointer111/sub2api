package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

var RimeSpeechModelIDs = []string{"coda", "mistv3"}

func newRimeSpeechRequest(ctx context.Context, account *Account, speech *SpeechRequest, model string, validateURL func(string) (string, error)) (*http.Request, error) {
	if account.Type != AccountTypeAPIKey || strings.TrimSpace(account.GetCredential("api_key")) == "" {
		return nil, errors.New("Rime requires API key credentials")
	}
	if model != "coda" && model != "mistv3" {
		return nil, errors.New("Rime model must be coda or mistv3")
	}
	if utf8.RuneCountInString(speech.Input) > 1000 {
		return nil, errors.New("Rime input must not exceed 1000 characters")
	}
	formats := map[string]string{
		"mp3": "audio/mpeg", "wav": "audio/wav", "pcm": "audio/L16",
		"opus": "audio/ogg;codecs=opus", "webm": "audio/webm;codecs=opus", "mulaw": "audio/PCMU",
	}
	accept, ok := formats[speech.ResponseFormat]
	if !ok {
		return nil, fmt.Errorf("unsupported Rime response_format %q", speech.ResponseFormat)
	}
	payload := map[string]any{"text": speech.Input, "speaker": speech.Voice, "modelId": model}
	if speech.Lang != "" {
		payload["lang"] = speech.Lang
	}
	if speech.SamplingRate < 0 {
		return nil, errors.New("samplingRate must be positive")
	}
	if speech.SamplingRate > 0 {
		payload["samplingRate"] = speech.SamplingRate
	}
	if speech.Speed != nil {
		if *speech.Speed < 0.4 || *speech.Speed > 2.5 {
			return nil, errors.New("Rime speed must be between 0.4 and 2.5")
		}
		// Rime timeScaleFactor expresses duration, so speed uses its reciprocal.
		payload["timeScaleFactor"] = 1 / *speech.Speed
	}
	baseURL := strings.TrimSpace(account.GetCredential("base_url"))
	if baseURL == "" || strings.TrimRight(baseURL, "/") == "https://coda.api.rime.ai" || strings.TrimRight(baseURL, "/") == "https://mist.api.rime.ai" {
		baseURL = "https://coda.api.rime.ai/"
		if model == "mistv3" {
			baseURL = "https://mist.api.rime.ai/"
		}
	}
	target, err := validateURL(baseURL)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if request.URL.Path == "" {
		request.URL.Path = "/"
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(account.GetCredential("api_key")))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", accept)
	return request, nil
}
