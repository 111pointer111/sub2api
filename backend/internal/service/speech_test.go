package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestParseSpeechRequest(t *testing.T) {
	request, err := ParseSpeechRequest([]byte(`{"model":"coda","input":" Hello ","voice":"astra"}`))
	require.NoError(t, err)
	require.Equal(t, "mp3", request.ResponseFormat)
	require.Equal(t, " Hello ", request.Input)
	for _, body := range []string{`null`, `{}`, `[]`, `{"model":"coda","input":" ","voice":"astra"}`, `{"model":"coda","input":"Hello","voice":4}`, `{"model":"coda","input":"Hello","voice":"astra","speed":0}`} {
		_, err := ParseSpeechRequest([]byte(body))
		require.Error(t, err, body)
	}
}

func TestRimeAccountConnection(t *testing.T) {
	account := &Account{ID: 22, Platform: PlatformRime, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}}
	repo := &systemOnePolicyAccountRepo{account: account}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader("test-audio"))}}
	service := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/accounts/22/test", nil)
	require.NoError(t, service.TestAccountConnection(c, account.ID, "mistv3", "", AccountTestModeDefault))
	require.Equal(t, "mist.api.rime.ai", upstream.lastReq.URL.Host)
	require.Contains(t, recorder.Body.String(), `"audio_url":"data:audio/mpeg;base64,`)
	require.Contains(t, recorder.Body.String(), `"success":true`)
}

func TestRimeSpeechLive(t *testing.T) {
	key := os.Getenv("RIME_API_KEY")
	if key == "" {
		t.Skip("set RIME_API_KEY to run authorized live synthesis")
	}
	client := &http.Client{Timeout: 45 * time.Second}
	upstream := &systemOneHTTPUpstream{do: client.Do}
	service := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 1, Platform: PlatformRime, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": key}}
	for _, model := range RimeSpeechModelIDs {
		t.Run(model, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
			speech, err := ParseSpeechRequest([]byte(`{"model":"` + model + `","input":"Hello from Sub2API.","voice":"astra","response_format":"mp3"}`))
			require.NoError(t, err)
			result, err := service.ForwardSpeech(context.Background(), c, account, speech, "")
			require.NoErrorf(t, err, "upstream status %d", recorder.Code)
			require.NotNil(t, result)
			require.Greater(t, recorder.Body.Len(), 512)
			require.Contains(t, recorder.Header().Get("Content-Type"), "audio/")
			filename := filepath.Join(t.TempDir(), model+".mp3")
			require.NoError(t, os.WriteFile(filename, recorder.Body.Bytes(), 0o600))
			if ffprobe, err := exec.LookPath("ffprobe"); err == nil {
				output, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name,sample_rate", "-of", "json", filename).CombinedOutput()
				require.NoError(t, err, string(output))
				require.Equal(t, "mp3", gjson.GetBytes(output, "streams.0.codec_name").String())
				t.Logf("%s returned %d bytes of MP3 audio", model, recorder.Body.Len())
			}
		})
	}
}

func TestRimeSpeechRequest(t *testing.T) {
	account := &Account{Platform: PlatformRime, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-rime-key", "base_url": "https://coda.api.rime.ai"}}
	speech, err := ParseSpeechRequest([]byte(`{"model":"mistv3","input":" Hello ","voice":"astra","speed":2,"lang":"en","samplingRate":24000,"response_format":"wav"}`))
	require.NoError(t, err)
	service := &OpenAIGatewayService{}
	request, err := newRimeSpeechRequest(context.Background(), account, speech, speech.Model, service.validateUpstreamBaseURL)
	require.NoError(t, err)
	require.Equal(t, "https://mist.api.rime.ai/", request.URL.String())
	require.Equal(t, "Bearer test-rime-key", request.Header.Get("Authorization"))
	require.Equal(t, "audio/wav", request.Header.Get("Accept"))
	body, err := io.ReadAll(request.Body)
	require.NoError(t, err)
	require.Equal(t, " Hello ", gjson.GetBytes(body, "text").String())
	require.Equal(t, "astra", gjson.GetBytes(body, "speaker").String())
	require.Equal(t, "mistv3", gjson.GetBytes(body, "modelId").String())
	require.Equal(t, 0.5, gjson.GetBytes(body, "timeScaleFactor").Float())
	require.Equal(t, int64(24000), gjson.GetBytes(body, "samplingRate").Int())

	account.Credentials["base_url"] = "https://relay.example.com/native/tts"
	request, err = newRimeSpeechRequest(context.Background(), account, speech, "coda", service.validateUpstreamBaseURL)
	require.NoError(t, err)
	require.Equal(t, "https://relay.example.com/native/tts", request.URL.String())
}

func TestRimeSpeechValidation(t *testing.T) {
	for _, test := range []struct {
		name, model, input, format, baseURL string
	}{
		{"unknown model", "unknown", "Hello", "mp3", ""},
		{"too long", "coda", strings.Repeat("你", 1001), "mp3", ""},
		{"unsupported format", "coda", "Hello", "flac", ""},
		{"insecure URL", "coda", "Hello", "mp3", "http://example.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := &Account{Platform: PlatformRime, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "key", "base_url": test.baseURL}}
			speech := &SpeechRequest{Model: test.model, Input: test.input, Voice: "astra", ResponseFormat: test.format}
			service := &OpenAIGatewayService{}
			_, err := newRimeSpeechRequest(context.Background(), account, speech, test.model, service.validateUpstreamBaseURL)
			require.Error(t, err)
		})
	}
	account := &Account{Platform: PlatformRime, Type: AccountTypeAPIKey}
	require.True(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySpeech))
	require.False(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions))
	require.False(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses))
	account.Type = AccountTypeOAuth
	require.False(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySpeech))
	service := &OpenAIGatewayService{}
	_, err := newRimeSpeechRequest(context.Background(), account, &SpeechRequest{}, "coda", service.validateUpstreamBaseURL)
	require.Error(t, err)
}

func TestRimeAccountsRequireAPIKeysAtCreateAndUpdate(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken, AccountTypeUpstream} {
		_, err := buildAccountForCreate(&CreateAccountInput{Platform: PlatformRime, Type: accountType}, map[string]any{})
		require.ErrorContains(t, err, "rime accounts only support apikey")
		accountService := &AccountService{}
		_, err = accountService.Create(context.Background(), CreateAccountRequest{Platform: PlatformRime, Type: accountType})
		require.ErrorContains(t, err, "rime accounts only support apikey")
		account := &Account{ID: 23, Platform: PlatformRime, Type: AccountTypeAPIKey}
		admin := &adminServiceImpl{accountRepo: &systemOnePolicyAccountRepo{account: account}}
		_, err = admin.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{Type: accountType})
		require.ErrorContains(t, err, "rime accounts only support apikey")
	}
}

func TestForwardSpeechStreamsAndMetersCharacters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
	reader, writer := io.Pipe()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: reader,
	}}
	service := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 21, Platform: PlatformRime, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "key", "model_mapping": map[string]any{"speech-alias": "coda"}}}
	speech := &SpeechRequest{Model: "speech-alias", Input: "Hi你😀", Voice: "astra", ResponseFormat: "mp3"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = writer.Write([]byte("first"))
		_, _ = writer.Write([]byte("second"))
		_ = writer.CloseWithError(errors.New("audio interrupted"))
	}()
	result, err := service.ForwardSpeech(context.Background(), c, account, speech, "")
	<-done
	require.ErrorContains(t, err, "audio interrupted")
	require.NotNil(t, result)
	require.Equal(t, "firstsecond", recorder.Body.String())
	require.True(t, recorder.Flushed)
	require.Equal(t, "speech-alias", result.Model)
	require.Equal(t, "coda", result.UpstreamModel)
	require.Equal(t, 4.0/1e6, result.AudioUsage.DurationOrUnits)
	require.True(t, result.Stream)
	require.NotEmpty(t, result.RequestID)
	require.Equal(t, "/", GetActualOpenAIUpstreamEndpoint(c))
}

func TestForwardSpeechRejectsInvalidAudioAndHTTPFailures(t *testing.T) {
	for _, test := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"JSON success", "application/json", `{}`, http.StatusOK},
		{"empty audio", "audio/mpeg", "", http.StatusOK},
		{"invalid input", "application/json", `{"error":"bad voice"}`, http.StatusBadRequest},
		{"rate limited", "application/json", `{"error":"rate limited"}`, http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: test.status, Header: http.Header{"Content-Type": {test.contentType}}, Body: io.NopCloser(strings.NewReader(test.body))}}
			service := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{Platform: PlatformRime, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "key"}}
			speech := &SpeechRequest{Model: "coda", Input: "Hello", Voice: "astra", ResponseFormat: "mp3"}
			result, err := service.ForwardSpeech(context.Background(), c, account, speech, "")
			require.Error(t, err)
			require.Nil(t, result)
			if test.status == http.StatusTooManyRequests {
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.False(t, c.Writer.Written())
			} else {
				require.True(t, c.Writer.Written())
			}
		})
	}
}
