//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestVolcengineCompactionHTTPFailuresUseStandardGatewayPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		status   int
		code     string
		calls    int
		failover bool
	}{
		{400, "context_length_exceeded", 1, false},
		{500, "context_length_exceeded", 1, false},
		{401, "invalid_api_key", 1, true},
		{403, "permission_denied", 1, true},
		{429, "insufficient_quota", 1, true},
		{500, "server_error", 2, true},
		{503, "server_error", 2, true},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.code), func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				body := volcengineCompactionTestBody(t, stream)
				calls := 0
				upstream := gzipTestHTTP{send: func(req *http.Request) (*http.Response, error) {
					calls++
					data, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					require.Equal(t, "high", gjson.GetBytes(data, "reasoning.effort").String(), "HTTP failure must not disable thinking")
					return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"30"}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, tc.code, tc.code)))}, nil
				}}
				repo := &rateLimitAccountRepoStub{}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, rateLimitService: NewRateLimitService(repo, nil, &config.Config{}, nil, nil)}
				c, rec := volcengineUserContext(1, "/v1/responses", body)
				result, err := svc.Forward(context.Background(), c, volcengineTestAccount(), body)
				require.Error(t, err)
				require.Equal(t, tc.calls, calls)
				if tc.failover {
					var failure *UpstreamFailoverError
					require.ErrorAs(t, err, &failure)
					require.Equal(t, tc.status, failure.StatusCode)
					require.Nil(t, result)
					require.Empty(t, rec.Body.String())
				} else {
					require.Contains(t, rec.Body.String(), "context_length_exceeded")
					require.Zero(t, repo.setErrorCalls+repo.rateLimitedCalls+repo.tempCalls)
				}
				if tc.status == 401 {
					require.Positive(t, repo.setErrorCalls)
				}
				if tc.status == 429 {
					require.Positive(t, repo.rateLimitedCalls)
				}
			}
		})
	}
}

func TestVolcengineCompactionErrorAfterConsumedUsageIsBilledWithoutFailover(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			status int
			code   string
		}{{429, "insufficient_quota"}, {400, "context_length_exceeded"}, {401, "invalid_api_key"}, {503, "server_error"}} {
			t.Run(fmt.Sprintf("stream=%t/%d", stream, tc.status), func(t *testing.T) {
				calls := 0
				upstream := gzipTestHTTP{send: func(req *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return volcengineCompactionTestResponse(t, "", "completed", stream), nil
					}
					return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, tc.code, tc.code)))}, nil
				}}
				repo := &rateLimitAccountRepoStub{}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, rateLimitService: NewRateLimitService(repo, nil, &config.Config{}, nil, nil)}
				body := volcengineCompactionTestBody(t, stream)
				c, rec := volcengineUserContext(1, "/v1/responses", body)
				result, err := svc.Forward(context.Background(), c, volcengineTestAccount(), body)
				require.Error(t, err)
				var failure *UpstreamFailoverError
				require.False(t, errors.As(err, &failure), "already consumed tokens must remain attached to the billed account")
				require.NotNil(t, result)
				require.True(t, result.ObservedUsageOnError)
				require.Equal(t, 7, result.Usage.OutputTokens)
				require.Equal(t, 2, calls)
				require.NotContains(t, rec.Body.String(), "THOUGHT_MUST_NOT_BECOME_SUMMARY")
				if stream {
					require.Contains(t, rec.Body.String(), "response.failed")
				} else {
					require.GreaterOrEqual(t, rec.Code, 400)
				}
				if tc.status == 400 {
					require.Contains(t, rec.Body.String(), "context_length_exceeded")
				}
				if tc.status == 401 {
					require.Positive(t, repo.setErrorCalls)
				}
				if tc.status == 429 {
					require.Positive(t, repo.rateLimitedCalls)
				}
			})
		}
	}
}

func TestVolcengineCompactionBareSSEErrorKeepsContextAndQuotaPolicy(t *testing.T) {
	for _, code := range []string{"context_length_exceeded", "insufficient_quota"} {
		t.Run(code, func(t *testing.T) {
			calls := 0
			upstream := gzipTestHTTP{send: func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":%q,\"message\":%q}}\n\n", code, code)))}, nil
			}}
			repo := &rateLimitAccountRepoStub{}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, rateLimitService: NewRateLimitService(repo, nil, &config.Config{}, nil, nil)}
			body := volcengineCompactionTestBody(t, true)
			c, rec := volcengineUserContext(1, "/v1/responses", body)
			_, err := svc.Forward(context.Background(), c, volcengineTestAccount(), body)
			require.Error(t, err)
			require.Equal(t, 1, calls)
			if code == "context_length_exceeded" {
				require.Contains(t, rec.Body.String(), code)
			} else {
				var failure *UpstreamFailoverError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, 429, failure.StatusCode)
				require.Positive(t, repo.rateLimitedCalls)
			}
		})
	}
}
