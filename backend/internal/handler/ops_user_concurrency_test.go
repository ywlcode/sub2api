//go:build unit

package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func runOpsConcurrencyResponses(t *testing.T, handle gin.HandlerFunc) (*httptest.ResponseRecorder, *service.OpsInsertErrorLogInput) {
	t.Helper()
	setupOpsErrorLogTestQueue(t, 2)
	r := gin.New()
	r.Use(OpsErrorLoggerMiddleware(service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)))
	r.POST("/v1/responses", handle)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	require.Len(t, opsErrorLogQueue, 1)
	row := (<-opsErrorLogQueue).entry
	t.Logf("wire=%d ops=%d type=%s phase=%s owner=%s source=%s business=%t reason=%q", w.Code, row.StatusCode, row.ErrorType, row.ErrorPhase, row.ErrorOwner, row.ErrorSource, row.IsBusinessLimited, row.ErrorMessage)
	return w, row
}

func TestOpsUserConcurrencyResponses(t *testing.T) {
	for _, handler := range []struct {
		name    string
		respond func(*gin.Context, error, string, bool)
	}{{"openai", (&OpenAIGatewayHandler{}).handleConcurrencyError}, {"gateway", (&GatewayHandler{}).handleConcurrencyError}} {
		for _, started := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/headers_sent=%t", handler.name, started), func(t *testing.T) {
				w, row := runOpsConcurrencyResponses(t, func(c *gin.Context) {
					setOpsRequestContext(c, "gpt-5.1", true)
					if started {
						c.Header("Content-Type", "text/event-stream")
						_, _ = c.Writer.WriteString(": ping\n\n")
						c.Writer.Flush()
					}
					handler.respond(c, &ConcurrencyError{SlotType: "user", IsTimeout: true}, "user", started)
				})
				const wireMessage = "Concurrency limit exceeded for user, please retry later"
				if started {
					require.Equal(t, http.StatusOK, w.Code)
					require.True(t, w.Flushed)
					require.True(t, strings.HasPrefix(w.Body.String(), ": ping\n\n"))
					_, wireErr := parseResponsesFailedSSE(t, strings.TrimPrefix(w.Body.String(), ": ping\n\n"))
					require.Equal(t, "gateway_concurrency_limit", wireErr["code"])
					require.Equal(t, wireMessage, wireErr["message"])
				} else {
					require.Equal(t, http.StatusTooManyRequests, w.Code)
					require.JSONEq(t, `{"code":"gateway_concurrency_limit","type":"rate_limit_error","message":"Concurrency limit exceeded for user, please retry later"}`, gjson.Get(w.Body.String(), "error").Raw)
				}
				require.Equal(t, http.StatusTooManyRequests, row.StatusCode)
				require.Equal(t, "rate_limit_error", row.ErrorType)
				require.Equal(t, "用户并发超限", row.ErrorMessage)
				require.Equal(t, "request", row.ErrorPhase)
				require.Equal(t, "client_request", row.ErrorSource)
				require.Equal(t, "client", row.ErrorOwner)
				require.True(t, row.IsBusinessLimited)
				require.Nil(t, row.UpstreamStatusCode)
				require.Nil(t, row.UpstreamErrorMessage)
				require.Nil(t, row.AccountID)
			})
		}
	}
}

func TestOpsUserConcurrencyUpstreamContrast(t *testing.T) {
	for _, streamFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("upstream_stream=%t", streamFailure), func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if streamFailure {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"partial"}

data: {"type":"response.failed","response":{"id":"resp_remote","status":"failed","error":{"code":"gateway_concurrency_limit","message":"Concurrency limit exceeded for user, please retry later"}}}

`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"type":"upstream_error","code":"gateway_concurrency_limit","message":"Concurrency limit exceeded for user, please retry later"}}`)
			}))
			defer provider.Close()
			base, err := url.Parse(provider.URL)
			require.NoError(t, err)
			account := issue715AccountsOfType(service.AccountTypeAPIKey)[0]
			h := newOpenAIResponsesFailoverTestHandler(t, &peerResetHTTPUpstream{client: provider.Client(), base: base}, account)
			h.maxAccountSwitches = 0
			w, row := runOpsConcurrencyResponses(t, func(c *gin.Context) {
				seed, _ := newOpenAIResponsesFailoverTestContext(t, c.Request.Context())
				for k, v := range seed.Keys {
					c.Set(k, v)
				}
				h.Responses(c)
			})
			require.Positive(t, calls.Load())
			if streamFailure {
				require.Equal(t, http.StatusOK, w.Code)
				require.Contains(t, w.Body.String(), "response.failed")
				require.Contains(t, w.Body.String(), "gateway_concurrency_limit")
			} else {
				require.Equal(t, http.StatusBadGateway, w.Code)
			}
			require.Equal(t, http.StatusBadGateway, row.StatusCode)
			require.Equal(t, "upstream", row.ErrorPhase)
			require.Equal(t, "provider", row.ErrorOwner)
			require.Equal(t, "upstream_http", row.ErrorSource)
			require.False(t, row.IsBusinessLimited)
			require.Equal(t, new(http.StatusBadGateway), row.UpstreamStatusCode)
			require.Equal(t, &account.ID, row.AccountID)
		})
	}
}
