package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type openAIPlatformUsageTestUpstream struct {
	HTTPUpstream
	do func(*http.Request) (*http.Response, error)
}

func (u *openAIPlatformUsageTestUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.do(req)
}

type openAIPlatformUsageTestRepo struct {
	AccountRepository
	account *Account
}

func (r *openAIPlatformUsageTestRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func openAIPlatformTestAccount() *Account {
	return &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "model-key", "openai_admin_api_key": "admin-key", "base_url": "https://api.openai.com/v1"}}
}

func openAIPlatformTestResponse(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func openAIPlatformTestPage(start int64, results string) string {
	return fmt.Sprintf(`{"object":"page","data":[{"start_time":%d,"results":%s}],"has_more":false}`, start, results)
}

func TestOpenAIPlatformUsage_NormalEntryPaginationAndCache(t *testing.T) {
	account := openAIPlatformTestAccount()
	account.Credentials["openai_usage_project_id"] = "proj_example"
	account.Credentials["header_override_enabled"] = true
	account.Credentials["header_overrides"] = map[string]any{"Authorization": "Bearer model-key"}
	var mu sync.Mutex
	var calls int
	upstream := &openAIPlatformUsageTestUpstream{do: func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		require.Equal(t, "api.openai.com", req.URL.Host)
		require.Equal(t, "Bearer admin-key", req.Header.Get("Authorization"))
		require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
		require.Equal(t, "proj_example", req.URL.Query().Get("project_ids"))
		require.Equal(t, "31", req.URL.Query().Get("limit"))
		now := time.Now().UTC()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
		if strings.HasSuffix(req.URL.Path, "/costs") {
			if req.URL.Query().Get("page") == "" {
				return openAIPlatformTestResponse(fmt.Sprintf(`{"object":"page","data":[{"start_time":%d,"results":[{"object":"organization.costs.result","amount":{"value":0.1,"currency":"usd"}}]}],"has_more":true,"next_page":"next+/="}`, today), 200), nil
			}
			require.Equal(t, "next+/=", req.URL.Query().Get("page"))
			return openAIPlatformTestResponse(openAIPlatformTestPage(today, `[{"object":"organization.costs.result","amount":{"value":0.2,"currency":"usd"}}]`), 200), nil
		}
		require.Equal(t, "/v1/organization/usage/completions", req.URL.Path)
		return openAIPlatformTestResponse(openAIPlatformTestPage(today, `[{"object":"organization.usage.completions.result","input_tokens":100,"output_tokens":50,"input_cached_tokens":40,"num_model_requests":3}]`), 200), nil
	}}
	svc := &AccountUsageService{accountRepo: &openAIPlatformUsageTestRepo{account: account}, openAIPlatformUsage: &openAIPlatformUsageService{httpUpstream: upstream}}
	for _, force := range []bool{false, false, true} {
		got, err := svc.GetOpenAIPlatformUsage(context.Background(), account.ID, force)
		require.NoError(t, err)
		require.Equal(t, "0.3", got.MonthlyCost)
		require.Equal(t, "0.3", got.TodayCost)
		require.Equal(t, int64(100), got.InputTokens)
		require.Equal(t, int64(50), got.OutputTokens)
		require.Equal(t, int64(40), got.CachedInputTokens)
		require.Equal(t, int64(3), got.Requests)
		require.Equal(t, time.UTC, got.PeriodStart.Location())
		require.Equal(t, 1, got.PeriodStart.Day())
	}
	require.Equal(t, 3, calls)
	account.Credentials["openai_usage_project_id"] = "proj_changed"
	upstream.do = func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "proj_changed", req.URL.Query().Get("project_ids"))
		return openAIPlatformTestResponse(`{"object":"page","data":[],"has_more":false}`, 200), nil
	}
	got, err := svc.GetOpenAIPlatformUsage(context.Background(), account.ID, false)
	require.NoError(t, err)
	require.Equal(t, "proj_changed", got.ProjectID)
	require.Equal(t, "0", got.MonthlyCost)
}

func TestOpenAIPlatformUsage_RejectsIncompleteAndUnauthorizedResponses(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
	}{
		{"unauthorized", `admin-key`, 401},
		{"forbidden", `admin-key`, 403},
		{"rate limited", `{}`, 429},
		{"upstream failure", `admin-key`, 500},
		{"malformed", `{`, 200},
		{"missing data", `{"object":"page","has_more":false}`, 200},
		{"missing page status", `{"object":"page","data":[]}`, 200},
		{"missing cursor", `{"object":"page","data":[],"has_more":true}`, 200},
		{"oversized", strings.Repeat("x", openAIPlatformUsageMaxBody+1), 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &openAIPlatformUsageTestUpstream{do: func(*http.Request) (*http.Response, error) {
				return openAIPlatformTestResponse(tt.body, tt.status), nil
			}}
			svc := &openAIPlatformUsageService{httpUpstream: upstream}
			got, err := svc.query(context.Background(), openAIPlatformTestAccount(), false)
			require.Error(t, err)
			require.Nil(t, got)
			require.NotContains(t, err.Error(), "admin-key")
			if tt.status == 401 || tt.status == 403 {
				require.Equal(t, 502, infraerrors.Code(err))
			}
		})
	}
}

func TestOpenAIPlatformUsage_InvalidAmountsAndTokensDoNotBecomeZero(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	today := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC).Unix()
	for _, tt := range []struct{ path, results string }{
		{"/v1/organization/costs", `[{"object":"organization.costs.result","amount":{"currency":"usd"}}]`},
		{"/v1/organization/costs", `[{"object":"organization.costs.result","amount":{"value":"oops","currency":"usd"}}]`},
		{"/v1/organization/costs", `[{"object":"organization.costs.result","amount":{"value":1,"currency":"eur"}}]`},
		{"/v1/organization/usage/completions", `[{"object":"organization.usage.completions.result","input_tokens":0,"output_tokens":0}]`},
	} {
		upstream := &openAIPlatformUsageTestUpstream{do: func(req *http.Request) (*http.Response, error) {
			results := "[]"
			if req.URL.Path == tt.path {
				results = tt.results
			}
			return openAIPlatformTestResponse(openAIPlatformTestPage(today, results), 200), nil
		}}
		svc := &openAIPlatformUsageService{httpUpstream: upstream}
		got, err := svc.fetch(context.Background(), openAIPlatformTestAccount(), "admin-key", "", "", now)
		require.Error(t, err)
		require.Nil(t, got)
	}
}

func TestOpenAIPlatformUsage_UTCDailyAndMonthlyCosts(t *testing.T) {
	now := time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)
	month := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Unix()
	today := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC).Unix()
	upstream := &openAIPlatformUsageTestUpstream{do: func(req *http.Request) (*http.Response, error) {
		require.Equal(t, fmt.Sprint(month), req.URL.Query().Get("start_time"))
		require.Equal(t, fmt.Sprint(now.Unix()), req.URL.Query().Get("end_time"))
		require.Empty(t, req.URL.Query().Get("project_ids"))
		if strings.HasSuffix(req.URL.Path, "/costs") {
			return openAIPlatformTestResponse(fmt.Sprintf(`{"object":"page","data":[{"start_time":%d,"results":[{"object":"organization.costs.result","amount":{"value":1.25,"currency":"usd"}}]},{"start_time":%d,"results":[{"object":"organization.costs.result","amount":{"value":0.75,"currency":"usd"}}]}],"has_more":false}`, month, today), 200), nil
		}
		return openAIPlatformTestResponse(`{"object":"page","data":[],"has_more":false}`, 200), nil
	}}
	got, err := (&openAIPlatformUsageService{httpUpstream: upstream}).fetch(context.Background(), openAIPlatformTestAccount(), "admin-key", "", "", now)
	require.NoError(t, err)
	require.Equal(t, "2", got.MonthlyCost)
	require.Equal(t, "0.75", got.TodayCost)
}

func TestOpenAIPlatformUsage_OfficialAccountsAndCredentialLifecycle(t *testing.T) {
	for _, baseURL := range []string{"", "https://api.openai.com", "https://api.openai.com/v1/", "https://api.openai.com:443/v1"} {
		account := openAIPlatformTestAccount()
		account.Credentials["base_url"] = baseURL
		require.True(t, IsOpenAIPlatformAPIKey(account), baseURL)
	}
	for _, baseURL := range []string{"https://api.openai.com.evil.test", "http://api.openai.com", "https://user@api.openai.com", "https://api.openai.com/v1/other", "https://relay.test/v1"} {
		account := openAIPlatformTestAccount()
		account.Credentials["base_url"] = baseURL
		_, err := (&openAIPlatformUsageService{}).query(context.Background(), account, false)
		require.Error(t, err)
	}
	account := openAIPlatformTestAccount()
	account.Credentials = MergePreservingSensitiveCreds(account.Credentials, map[string]any{"base_url": "https://api.openai.com"})
	require.Equal(t, "admin-key", account.GetCredential("openai_admin_api_key"))
	account.Credentials = MergePreservingSensitiveCreds(account.Credentials, map[string]any{"openai_admin_api_key": ""})
	_, err := (&openAIPlatformUsageService{}).query(context.Background(), account, false)
	require.Error(t, err)
	encoded, err := json.Marshal(account.Credentials)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "admin-key")
}
