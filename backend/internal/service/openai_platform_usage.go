package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

const (
	openAIPlatformUsageTTL        = 5 * time.Minute
	openAIPlatformUsageMinRefresh = 30 * time.Second
	openAIPlatformUsageMaxBody    = 2 * 1024 * 1024
)

// OpenAIPlatformUsage reports official calendar-month totals in UTC. Costs cover
// all billed products. Token/request counts cover the completions usage endpoint.
type OpenAIPlatformUsage struct {
	ProjectID         string    `json:"project_id,omitempty"`
	PeriodStart       time.Time `json:"period_start"`
	PeriodEnd         time.Time `json:"period_end"`
	FetchedAt         time.Time `json:"fetched_at"`
	MonthlyCost       string    `json:"monthly_cost"`
	TodayCost         string    `json:"today_cost"`
	Currency          string    `json:"currency"`
	InputTokens       int64     `json:"input_tokens"`
	OutputTokens      int64     `json:"output_tokens"`
	CachedInputTokens int64     `json:"cached_input_tokens"`
	Requests          int64     `json:"requests"`
}

type openAIPlatformUsageCacheEntry struct {
	identity [32]byte
	result   *OpenAIPlatformUsage
	err      error
	at       time.Time
}

type openAIPlatformUsageService struct {
	httpUpstream HTTPUpstream
	proxyRepo    ProxyRepository
	cache        sync.Map
	flight       singleflight.Group
}

// IsOpenAIPlatformAPIKey limits administrative queries to official Platform accounts.
func IsOpenAIPlatformAPIKey(account *Account) bool {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey {
		return false
	}
	baseURL := strings.TrimSpace(account.GetCredential("base_url"))
	if baseURL == "" {
		return true
	}
	u, err := url.Parse(baseURL)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "api.openai.com") &&
		(u.Port() == "" || u.Port() == "443") && u.User == nil && u.RawQuery == "" && u.Fragment == "" &&
		(strings.TrimRight(u.Path, "/") == "" || strings.TrimRight(u.Path, "/") == "/v1")
}

func (s *AccountUsageService) GetOpenAIPlatformUsage(ctx context.Context, accountID int64, force bool) (*OpenAIPlatformUsage, error) {
	if s.openAIPlatformUsage == nil {
		return nil, infraerrors.ServiceUnavailable("OPENAI_PLATFORM_USAGE_UNAVAILABLE", "OpenAI Platform usage is unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return s.openAIPlatformUsage.query(ctx, account, force)
}

func (s *openAIPlatformUsageService) query(ctx context.Context, account *Account, force bool) (*OpenAIPlatformUsage, error) {
	if !IsOpenAIPlatformAPIKey(account) {
		return nil, infraerrors.BadRequest("OPENAI_PLATFORM_USAGE_ACCOUNT_INVALID", "an official OpenAI Platform API key account is required")
	}
	adminKey := strings.TrimSpace(account.GetCredential("openai_admin_api_key"))
	if adminKey == "" {
		return nil, infraerrors.BadRequest("OPENAI_PLATFORM_USAGE_KEY_REQUIRED", "configure an OpenAI Admin API key first")
	}
	proxyURL := ""
	if account.ProxyID != nil {
		proxy := account.Proxy
		if proxy == nil && s.proxyRepo != nil {
			var err error
			proxy, err = s.proxyRepo.GetByID(ctx, *account.ProxyID)
			if err != nil {
				return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_PLATFORM_USAGE_PROXY_FAILED", "failed to load the account proxy")
			}
		}
		if proxy == nil {
			return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_PLATFORM_USAGE_PROXY_FAILED", "account proxy is unavailable")
		}
		proxyURL = proxy.URL()
	}
	projectID := strings.TrimSpace(account.GetCredential("openai_usage_project_id"))
	now := time.Now().UTC()
	identityJSON, _ := json.Marshal([]string{adminKey, projectID, proxyURL, now.Format("2006-01-02")})
	identity := sha256.Sum256(identityJSON)
	loadCached := func() (*openAIPlatformUsageCacheEntry, bool) {
		value, ok := s.cache.Load(account.ID)
		if !ok {
			return nil, false
		}
		entry := value.(*openAIPlatformUsageCacheEntry)
		ttl := openAIPlatformUsageTTL
		if force || entry.err != nil {
			ttl = openAIPlatformUsageMinRefresh
		}
		return entry, entry.identity == identity && time.Since(entry.at) < ttl
	}
	if entry, ok := loadCached(); ok {
		return entry.result, entry.err
	}
	value, err, _ := s.flight.Do(fmt.Sprintf("%d:%x", account.ID, identity), func() (any, error) {
		if entry, ok := loadCached(); ok {
			return entry.result, entry.err
		}
		result, err := s.fetch(ctx, account, adminKey, projectID, proxyURL, now)
		s.cache.Store(account.ID, &openAIPlatformUsageCacheEntry{identity: identity, result: result, err: err, at: time.Now()})
		return result, err
	})
	if err != nil {
		return nil, err
	}
	return value.(*OpenAIPlatformUsage), nil
}

type openAIPlatformUsageBucket struct {
	StartTime int64             `json:"start_time"`
	Results   []json.RawMessage `json:"results"`
}

func (s *openAIPlatformUsageService) fetch(ctx context.Context, account *Account, adminKey, projectID, proxyURL string, now time.Time) (*OpenAIPlatformUsage, error) {
	ctx = WithHTTPUpstreamRedirectsDisabled(ctx)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	result := &OpenAIPlatformUsage{ProjectID: projectID, PeriodStart: start, PeriodEnd: now, Currency: "USD"}
	params := url.Values{
		"start_time":   {strconv.FormatInt(start.Unix(), 10)},
		"end_time":     {strconv.FormatInt(now.Unix(), 10)},
		"bucket_width": {"1d"},
		"limit":        {"31"},
	}
	if projectID != "" {
		params.Set("project_ids", projectID)
	}
	var costs, usage []openAIPlatformUsageBucket
	var group errgroup.Group
	group.Go(func() (err error) {
		costs, err = s.fetchPages(ctx, account, adminKey, proxyURL, "/organization/costs", params)
		return err
	})
	group.Go(func() (err error) {
		usage, err = s.fetchPages(ctx, account, adminKey, proxyURL, "/organization/usage/completions", params)
		return err
	})
	if err := group.Wait(); err != nil {
		return nil, err
	}
	monthly, today := decimal.Zero, decimal.Zero
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
	for _, bucket := range costs {
		for _, raw := range bucket.Results {
			var item struct {
				Object string `json:"object"`
				Amount *struct {
					Value    *decimal.Decimal `json:"value"`
					Currency string           `json:"currency"`
				} `json:"amount"`
			}
			if json.Unmarshal(raw, &item) != nil || item.Object != "organization.costs.result" ||
				item.Amount == nil || item.Amount.Value == nil || !strings.EqualFold(item.Amount.Currency, "usd") {
				return nil, openAIPlatformUsageInvalidResponse()
			}
			monthly = monthly.Add(*item.Amount.Value)
			if bucket.StartTime >= todayStart {
				today = today.Add(*item.Amount.Value)
			}
		}
	}
	for _, bucket := range usage {
		for _, raw := range bucket.Results {
			var item struct {
				Object            string `json:"object"`
				InputTokens       *int64 `json:"input_tokens"`
				OutputTokens      *int64 `json:"output_tokens"`
				CachedInputTokens int64  `json:"input_cached_tokens"`
				Requests          *int64 `json:"num_model_requests"`
			}
			if json.Unmarshal(raw, &item) != nil || item.Object != "organization.usage.completions.result" ||
				item.InputTokens == nil || item.OutputTokens == nil || item.Requests == nil ||
				*item.InputTokens < 0 || *item.OutputTokens < 0 || *item.Requests < 0 || item.CachedInputTokens < 0 {
				return nil, openAIPlatformUsageInvalidResponse()
			}
			result.InputTokens += *item.InputTokens
			result.OutputTokens += *item.OutputTokens
			result.CachedInputTokens += item.CachedInputTokens
			result.Requests += *item.Requests
		}
	}
	result.MonthlyCost, result.TodayCost = monthly.String(), today.String()
	result.FetchedAt = time.Now().UTC()
	return result, nil
}

func (s *openAIPlatformUsageService) fetchPages(ctx context.Context, account *Account, adminKey, proxyURL, path string, params url.Values) ([]openAIPlatformUsageBucket, error) {
	query := params.Encode()
	start, _ := strconv.ParseInt(params.Get("start_time"), 10, 64)
	end, _ := strconv.ParseInt(params.Get("end_time"), 10, 64)
	page := ""
	seen := make(map[string]bool)
	var buckets []openAIPlatformUsageBucket
	for range 32 {
		// The Admin key always goes to the fixed official host. Account header
		// overrides and model-forwarding base URLs must not affect this request.
		target := "https://api.openai.com/v1" + path + "?" + query
		if page != "" {
			target += "&page=" + url.QueryEscape(page)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, openAIPlatformUsageInvalidResponse()
		}
		req.Header.Set("Authorization", "Bearer "+adminKey)
		req.Header.Set("Accept", "application/json")
		resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
		if err != nil {
			return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_PLATFORM_USAGE_REQUEST_FAILED", "failed to query OpenAI Platform usage")
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, openAIPlatformUsageMaxBody+1))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			// Upstream 401 must not be interpreted as an expired Sub2API session.
			return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_PLATFORM_USAGE_AUTH_FAILED", "check the Admin API key and its organization usage permissions")
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, infraerrors.TooManyRequests("OPENAI_PLATFORM_USAGE_RATE_LIMITED", "OpenAI usage queries are rate limited; try again later")
		}
		if resp.StatusCode != http.StatusOK {
			return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_PLATFORM_USAGE_UPSTREAM_ERROR", "OpenAI Platform usage is temporarily unavailable")
		}
		var payload struct {
			Object   string                      `json:"object"`
			Data     []openAIPlatformUsageBucket `json:"data"`
			HasMore  *bool                       `json:"has_more"`
			NextPage string                      `json:"next_page"`
		}
		if readErr != nil || len(body) > openAIPlatformUsageMaxBody || json.Unmarshal(body, &payload) != nil ||
			payload.Object != "page" || payload.Data == nil || payload.HasMore == nil {
			return nil, openAIPlatformUsageInvalidResponse()
		}
		for _, bucket := range payload.Data {
			if bucket.Results == nil || bucket.StartTime < start || bucket.StartTime >= end {
				return nil, openAIPlatformUsageInvalidResponse()
			}
		}
		buckets = append(buckets, payload.Data...)
		if !*payload.HasMore {
			return buckets, nil
		}
		if payload.NextPage == "" || seen[payload.NextPage] {
			return nil, openAIPlatformUsageInvalidResponse()
		}
		page = payload.NextPage
		seen[page] = true
	}
	return nil, openAIPlatformUsageInvalidResponse()
}

func openAIPlatformUsageInvalidResponse() error {
	return infraerrors.New(http.StatusBadGateway, "OPENAI_PLATFORM_USAGE_INVALID_RESPONSE", "OpenAI returned incomplete or invalid usage data")
}
