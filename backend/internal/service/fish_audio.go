package service

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
)

var FishModelIDs = []string{"s2-pro", "s2.1-pro", "s2.1-pro-free", "drama-3", "transcribe-1-pro"}

const fishVoiceCapabilityPrefix = "fish-voice/"
const OpenAIEndpointCapabilityTranscription OpenAIEndpointCapability = "transcription"

func IsFishModel(model string) bool { return slices.Contains(FishModelIDs, model) }

func FishVoiceCapability(voice string) OpenAIEndpointCapability {
	return OpenAIEndpointCapability(fishVoiceCapabilityPrefix + voice)
}

// Restrict private voices before the scheduler consumes an account slot.
func (a *Account) HasFishVoice(voice string) bool {
	if len(voice) != 32 {
		return false
	}
	if _, err := hex.DecodeString(voice); err != nil {
		return false
	}
	return slices.Contains(strings.Fields(a.GetCredential("fish_voice_ids")), voice)
}

func fishRequest(ctx context.Context, account *Account, path, model, contentType string, body io.Reader, validate func(string) (string, error)) (*http.Request, error) {
	key := strings.TrimSpace(account.GetCredential("api_key"))
	if account.Type != AccountTypeAPIKey || key == "" {
		return nil, errors.New("Fish Audio requires API key credentials")
	}
	base := strings.TrimRight(strings.TrimSpace(account.GetCredential("base_url")), "/")
	if base == "" {
		base = "https://api.fish.audio"
	}
	target, err := validate(strings.TrimSuffix(base, "/v1") + path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("model", model)
	return req, nil
}

func newFishSpeechRequest(ctx context.Context, account *Account, speech *SpeechRequest, model string, validate func(string) (string, error)) (*http.Request, error) {
	if !IsFishModel(model) || model == "transcribe-1-pro" {
		return nil, errors.New("unsupported Fish speech model")
	}
	if !account.HasFishVoice(speech.Voice) {
		return nil, errors.New("voice is not registered for this Fish account")
	}
	if !slices.Contains([]string{"wav", "mp3", "pcm", "opus"}, speech.ResponseFormat) {
		return nil, errors.New("unsupported Fish audio format")
	}
	payload := map[string]any{"text": speech.Input, "reference_id": speech.Voice, "format": speech.ResponseFormat}
	if speech.Speed != nil {
		payload["prosody"] = map[string]float64{"speed": *speech.Speed}
	}
	if speech.Lang != "" {
		payload["language"] = speech.Lang
	}
	if speech.SamplingRate < 0 {
		return nil, errors.New("invalid sampling rate")
	}
	if speech.SamplingRate > 0 {
		payload["sample_rate"] = speech.SamplingRate
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return fishRequest(ctx, account, "/v1/tts", model, "application/json", bytes.NewReader(raw), validate)
}
