package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Speech handles the shared text-to-speech HTTP endpoint.
func (h *OpenAIGatewayHandler) Speech(c *gin.Context) {
	h.audio(c, false)
}
func (h *OpenAIGatewayHandler) Transcribe(c *gin.Context) {
	h.audio(c, true)
}
func (h *OpenAIGatewayHandler) audio(c *gin.Context, transcribe bool) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey.Group == nil {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(c, "handler.speech", zap.Int64("api_key_id", apiKey.ID))
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}
	body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
		} else {
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		}
		return
	}
	var speech *service.SpeechRequest
	var transcript *service.TranscriptionRequest
	var model string
	var estimate service.InflightEstimateRequest
	endpoint := "audio/speech"
	capability := service.OpenAIEndpointCapabilitySpeech
	if transcribe {
		transcript, err = service.ParseTranscriptionRequest(body, c.GetHeader("Content-Type"))
		if err == nil {
			model = transcript.Model
			estimate = grokVoiceInflightEstimate("stt", transcript.Audio)
		}
		capability = service.OpenAIEndpointCapabilityTranscription
		endpoint = "audio/transcriptions"
	} else {
		speech, err = service.ParseSpeechRequest(body)
		if err == nil {
			model = speech.Model
			estimate = service.InflightEstimateRequest{Kind: service.InflightEstimateAudio, AudioMode: "tts", AudioUnits: float64(utf8.RuneCountInString(speech.Input)) / 1e6}
		}
	}
	if err != nil {
		h.errorResponse(c, 400, "invalid_request_error", err.Error())
		return
	}
	ensureCompositeTargetPlatform(c, apiKey, model)
	platform := effectiveAPIKeyPlatform(c, apiKey)
	if (transcribe && platform != service.PlatformFish && platform != service.PlatformOpenAI) || (!transcribe && platform != service.PlatformFish && platform != service.PlatformRime) {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Audio endpoint is not supported for this platform")
		return
	}
	setOpsRequestContext(c, model, !transcribe)
	if !transcribe {
		setOpsEndpointContext(c, "", int16(service.RequestTypeStream))
		if platform == service.PlatformFish {
			capability = service.FishVoiceCapability(speech.Voice)
		}
		auditBody, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": speech.Input}}})
		if decision := h.checkSecurityAudit(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIChat, model, auditBody); decision != nil && !decision.AllowNextStage {
			h.openAISecurityAuditError(c, decision)
			return
		}
	}
	streamStarted := false
	userRelease, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userRelease != nil {
		defer userRelease()
	}
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, status, code, message)
		return
	}
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, model)
	forwardModel := openAIChannelForwardModel(channelMapping, model)
	estimate.Model = forwardModel
	inflightDone, err := reserveInflightBalance(c, h.billingCacheService, h.gatewayService, apiKey, subscription, estimate)
	if err != nil {
		status, code, message, _ := billingErrorDetails(err)
		h.errorResponse(c, status, code, message)
		return
	}
	defer inflightDone()
	failed := map[int64]struct{}{}
	var last *service.UpstreamFailoverError
	maxSwitches := h.maxAccountSwitches
	if maxSwitches <= 0 {
		maxSwitches = 3
	}
	for attempt := 0; attempt <= maxSwitches; attempt++ {
		selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			c.Request.Context(), apiKey.GroupID, "", "", forwardModel, failed,
			service.OpenAIUpstreamTransportHTTPSSE, capability,
			false, false, false, platform,
		)
		if err != nil || selection == nil || selection.Account == nil {
			break
		}
		account := selection.Account
		release, status := h.acquireResponsesAccountSlot(c, apiKey.GroupID, "", selection, false, &streamStarted, reqLog)
		if status != openAISlotAcquireOK {
			if status == openAISlotAcquireFailed {
				return
			}
			failed[account.ID] = struct{}{}
			continue
		}
		result, forwardErr := func() (*service.OpenAIForwardResult, error) {
			defer release()
			if transcribe {
				return h.gatewayService.ForwardTranscription(c.Request.Context(), c, account, transcript, forwardModel)
			}
			return h.gatewayService.ForwardSpeech(c.Request.Context(), c, account, speech, forwardModel)
		}()
		if result != nil {
			// An interrupted audio stream still consumed synthesis work upstream.
			h.recordAudioUsage(c, apiKey, account, subscription, endpoint, body, result)
			if forwardErr != nil {
				reqLog.Info("speech.stream_interrupted", zap.Error(forwardErr))
			}
			return
		}
		var failover *service.UpstreamFailoverError
		if errors.As(forwardErr, &failover) && failover.ShouldRetryNextAccount() && !c.Writer.Written() {
			failed[account.ID] = struct{}{}
			last = failover
			continue
		}
		return
	}
	if last != nil {
		h.handleFailoverExhausted(c, last, false)
	} else {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "No available accounts for this audio model and voice")
	}
}
