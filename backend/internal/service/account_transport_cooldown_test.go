//go:build unit

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestUpstreamTransportCooldownConfig(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		want        time.Duration
		invalid     bool
	}{
		{"absent", "", 10 * time.Minute, false},
		{"null", `null`, 10 * time.Minute, false},
		{"empty", `{}`, 10 * time.Minute, false},
		{"disabled", `{"enabled":false}`, 0, false},
		{"disabled null duration", `{"enabled":false,"duration_minutes":null}`, 0, false},
		{"null switch", `{"enabled":null,"duration_minutes":3}`, 3 * time.Minute, false},
		{"minimum", `{"duration_minutes":1}`, time.Minute, false},
		{"maximum", `{"enabled":true,"duration_minutes":1440}`, 24 * time.Hour, false},
		{"future field", `{"duration_minutes":3,"future_option":true}`, 3 * time.Minute, false},
		{"boolean", `false`, 10 * time.Minute, true},
		{"array", `[]`, 10 * time.Minute, true},
		{"string switch", `{"enabled":"false"}`, 10 * time.Minute, true},
		{"string duration", `{"duration_minutes":"5"}`, 10 * time.Minute, true},
		{"fraction", `{"duration_minutes":1.5}`, 10 * time.Minute, true},
		{"zero", `{"duration_minutes":0}`, 10 * time.Minute, true},
		{"negative", `{"duration_minutes":-1}`, 10 * time.Minute, true},
		{"too large", `{"duration_minutes":1441}`, 10 * time.Minute, true},
		{"disabled invalid duration", `{"enabled":false,"duration_minutes":0}`, 10 * time.Minute, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			credentials := map[string]any{}
			if tt.value != "" {
				var stored any
				require.NoError(t, json.Unmarshal([]byte(tt.value), &stored))
				credentials[UpstreamTransportCooldownCredentialKey] = stored
			}
			err := ValidateUpstreamTransportCooldownConfig(credentials)
			if tt.invalid {
				require.Equal(t, "INVALID_UPSTREAM_TRANSPORT_COOLDOWN", infraerrors.Reason(err))
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.want, (&Account{Credentials: credentials}).UpstreamTransportCooldown())
		})
	}
	require.Equal(t, 10*time.Minute, (*Account)(nil).UpstreamTransportCooldown())
}

func TestTransportCooldownOpenAIFailurePolicy(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformDeepseek} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%v", platform, enabled), func(t *testing.T) {
				repo := &openaiTransportAccountRepoStub{}
				svc := &OpenAIGatewayService{accountRepo: repo}
				account := &Account{ID: 91, Platform: platform, Type: AccountTypeAPIKey, Credentials: map[string]any{
					UpstreamTransportCooldownCredentialKey: UpstreamTransportCooldownConfig{Enabled: enabled, DurationMinutes: 3},
				}}
				c, rec := newOpenAITransportErrTestContext()
				before := time.Now()
				err := svc.handleOpenAIUpstreamTransportError(t.Context(), c, account, errors.New("dial tcp: connection refused"), false)
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.True(t, failover.ShouldRetryNextAccount())
				require.Equal(t, http.StatusBadGateway, failover.StatusCode)
				require.Zero(t, rec.Body.Len())
				events, exists := c.Get(OpsUpstreamErrorsKey)
				require.True(t, exists, "disabling the pause must not hide the failure")
				require.Len(t, events, 1)
				require.Equal(t, enabled && platform == PlatformOpenAI, svc.isOpenAIAccountRuntimeBlocked(account))
				if enabled {
					require.Len(t, repo.tempUnschedCalls, 1)
					require.WithinDuration(t, before.Add(3*time.Minute), repo.tempUnschedCalls[0].until, time.Second)
				} else {
					require.Empty(t, repo.tempUnschedCalls)
				}
			})
		}
	}
}

func TestTransportCooldownGatewayAndGeminiFailurePolicy(t *testing.T) {
	for _, path := range []string{"anthropic", "bedrock", "gemini"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%v", path, enabled), func(t *testing.T) {
				repo := &transportTempUnschedRepoStub{}
				credentials := map[string]any{UpstreamTransportCooldownCredentialKey: UpstreamTransportCooldownConfig{Enabled: enabled, DurationMinutes: 25}}
				account := &Account{ID: 92, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: credentials}
				if path == "bedrock" {
					account.Type = AccountTypeBedrock
				}
				c := newTransportErrorTestGin(t)
				fault := errors.New("dial tcp: connection refused")
				before := time.Now()
				var err error
				if path == "gemini" {
					svc, upstream, geminiRepo := newGeminiTransportErrorService(fault)
					repo = geminiRepo
					account = geminiPoolModeAPIKeyAccount()
					account.Credentials[UpstreamTransportCooldownCredentialKey] = credentials[UpstreamTransportCooldownCredentialKey]
					_, err = svc.ForwardNative(t.Context(), c, account, "gemini-2.5-flash", "streamGenerateContent", true, []byte(geminiTransportTestNativeBody))
					require.Equal(t, 1, upstream.calls)
				} else {
					svc := &GatewayService{accountRepo: repo}
					err = svc.handleUpstreamTransportError(t.Context(), c, account, fault, OpsUpstreamErrorEvent{})
				}
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.True(t, failover.ShouldRetryNextAccount())
				require.False(t, c.Writer.Written())
				events, exists := c.Get(OpsUpstreamErrorsKey)
				require.True(t, exists)
				require.Len(t, events, 1)
				if enabled {
					require.Equal(t, 1, repo.calls)
					require.Equal(t, account.ID, repo.lastID)
					require.WithinDuration(t, before.Add(25*time.Minute), repo.lastUntil, time.Second)
				} else {
					require.Zero(t, repo.calls)
				}
			})
		}
	}
}

func TestTransportCooldownDoesNotDisableHealthBreaker(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("network pause=%v", enabled), func(t *testing.T) {
			limiter, account, cache, repo := newAPIKeyHealthFixture(t)
			account.Credentials[UpstreamTransportCooldownCredentialKey] = UpstreamTransportCooldownConfig{Enabled: enabled, DurationMinutes: 1}
			cache.tripAt = 1
			sends := 0
			upstream := apiKeyHealthTestHTTP{DoFunc: func(*http.Request) (*http.Response, error) {
				sends++
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
			}}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://upstream.example", nil)
			require.NoError(t, err)
			client := apiKeyHealthHTTP(upstream, limiter, account)
			resp, err := client.Do(req, "", account.ID, 10)
			require.NoError(t, err)
			_, err = io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, []apiKeyHealthSample{{false, http.StatusServiceUnavailable}}, cache.samples)
			require.Equal(t, 1, repo.setCalls)
			var state TempUnschedState
			require.NoError(t, json.Unmarshal([]byte(repo.reason), &state))
			require.Equal(t, APIKeyHealthBreakerReason, state.MatchedKeyword)
			require.EqualValues(t, 15*60, state.UntilUnix-state.TriggeredAtUnix, "network duration must not change health cooldown")
			_, err = client.Do(req, "", account.ID, 10)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Equal(t, 1, sends, "tripped health breaker must still block upstream calls")
		})
	}
}

func TestTransportCooldownDisablePreservesExistingPause(t *testing.T) {
	repo := &openaiTransportAccountRepoStub{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{ID: 93, Platform: PlatformOpenAI}
	svc.tempUnscheduleOpenAITransportError(t.Context(), account, "connection refused")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	account.Credentials = map[string]any{UpstreamTransportCooldownCredentialKey: map[string]any{"enabled": false}}
	svc.tempUnscheduleOpenAITransportError(t.Context(), account, "connection refused again")
	require.Len(t, repo.tempUnschedCalls, 1, "disabled setting prevents new pauses")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account), "disabling must not clear an existing block")
}

func TestTransportCooldownValidationBeforePersistence(t *testing.T) {
	invalid := map[string]any{UpstreamTransportCooldownCredentialKey: map[string]any{"duration_minutes": 0}}
	account := func() *Account {
		return &Account{ID: 94, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Credentials: map[string]any{"api_key": "test-key"}}
	}
	t.Run("admin create", func(t *testing.T) {
		_, err := buildAccountForCreate(&CreateAccountInput{Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: invalid}, nil)
		requireApplicationErrorReason(t, err, "INVALID_UPSTREAM_TRANSPORT_COOLDOWN")
	})
	t.Run("admin update", func(t *testing.T) {
		repo := &updateAccountCredsRepoStub{account: account()}
		_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(t.Context(), 94, &UpdateAccountInput{Credentials: invalid})
		requireApplicationErrorReason(t, err, "INVALID_UPSTREAM_TRANSPORT_COOLDOWN")
		require.Zero(t, repo.updateCalls)
	})
	t.Run("admin bulk", func(t *testing.T) {
		repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{account()}}
		_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(t.Context(), &BulkUpdateAccountsInput{AccountIDs: []int64{94}, Credentials: invalid})
		requireApplicationErrorReason(t, err, "INVALID_UPSTREAM_TRANSPORT_COOLDOWN")
		require.Zero(t, repo.bulkUpdateCalls)
	})
	t.Run("account create", func(t *testing.T) {
		_, err := (&AccountService{}).Create(t.Context(), CreateAccountRequest{Credentials: invalid})
		requireApplicationErrorReason(t, err, "INVALID_UPSTREAM_TRANSPORT_COOLDOWN")
	})
	t.Run("account update", func(t *testing.T) {
		repo := &updateAccountCredsRepoStub{account: account()}
		_, err := (&AccountService{accountRepo: repo}).Update(t.Context(), 94, UpdateAccountRequest{Credentials: &invalid})
		requireApplicationErrorReason(t, err, "INVALID_UPSTREAM_TRANSPORT_COOLDOWN")
		require.Zero(t, repo.updateCalls)
	})
}
