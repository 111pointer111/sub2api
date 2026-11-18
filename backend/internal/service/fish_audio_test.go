package service

import (
	"bytes"
	"context"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFishPrivateVoiceRouting(t *testing.T) {
	const voice = "0123456789abcdef0123456789abcdef"
	a := &Account{Platform: PlatformFish, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture", "fish_voice_ids": voice}}
	b := &Account{Platform: PlatformFish, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "other"}}
	require.True(t, a.SupportsOpenAIEndpointCapability(FishVoiceCapability(voice)))
	require.False(t, b.SupportsOpenAIEndpointCapability(FishVoiceCapability(voice)))
	require.True(t, b.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityTranscription))
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions))
	speech := &SpeechRequest{Model: "s2.1-pro", Input: "今天吃什么？", Voice: voice, ResponseFormat: "wav", SamplingRate: 24000}
	build := func(a *Account) (*http.Request, error) {
		return newFishSpeechRequest(context.Background(), a, speech, speech.Model, func(s string) (string, error) { return s, nil })
	}
	_, err := build(b)
	require.Error(t, err)
	req, err := build(a)
	require.NoError(t, err)
	require.Equal(t, "https://api.fish.audio/v1/tts", req.URL.String())
	require.Equal(t, speech.Model, req.Header.Get("model"))
	raw, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.JSONEq(t, `{"text":"今天吃什么？","reference_id":"0123456789abcdef0123456789abcdef","format":"wav","sample_rate":24000}`, string(raw))
}
func TestTranscriptionRoutingAndAudioUsage(t *testing.T) {
	var input bytes.Buffer
	w := multipart.NewWriter(&input)
	require.NoError(t, w.WriteField("model", "transcribe-1-pro"))
	file, err := w.CreateFormFile("file", "message.wav")
	require.NoError(t, err)
	audio := minimalSilentWAV()
	_, err = file.Write(audio)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	task, err := ParseTranscriptionRequest(input.Bytes(), w.FormDataContentType())
	require.NoError(t, err)
	for _, platform := range []string{PlatformFish, PlatformOpenAI} {
		t.Run(platform, func(t *testing.T) {
			model, field, path := "transcribe-1-pro", "audio", "/v1/asr"
			if platform == PlatformOpenAI {
				model, field, path = "gpt-4o-mini-transcribe", "file", "/v1/audio/transcriptions"
			}
			account := &Account{Platform: platform, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture"}}
			upstream := &systemOneHTTPUpstream{do: func(req *http.Request) (*http.Response, error) {
				require.Equal(t, path, req.URL.Path)
				require.Equal(t, "Bearer fixture", req.Header.Get("Authorization"))
				require.NoError(t, req.ParseMultipartForm(1<<20))
				if platform == PlatformFish {
					require.Equal(t, model, req.Header.Get("model"))
				} else {
					require.Equal(t, model, req.FormValue("model"))
				}
				part, _, err := req.FormFile(field)
				require.NoError(t, err)
				defer part.Close()
				received, err := io.ReadAll(part)
				require.NoError(t, err)
				require.Equal(t, audio, received)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"text":"早上好","duration":1.6}`))}, nil
			}}
			s := &OpenAIGatewayService{httpUpstream: upstream}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/audio/transcriptions", nil)
			task.Model = model
			result, err := s.ForwardTranscription(context.Background(), c, account, task, model)
			require.NoError(t, err)
			require.Equal(t, "早上好", gjson.Get(rec.Body.String(), "text").String())
			require.InDelta(t, 1.6/3600, result.AudioUsage.DurationOrUnits, 1e-10)
		})
	}
	var duplicate bytes.Buffer
	d := multipart.NewWriter(&duplicate)
	require.NoError(t, d.WriteField("model", "one"))
	require.NoError(t, d.WriteField("model", "two"))
	require.NoError(t, d.Close())
	_, err = ParseTranscriptionRequest(duplicate.Bytes(), d.FormDataContentType())
	require.ErrorContains(t, err, "duplicate")
}
