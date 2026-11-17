//go:build unit

package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Real HTTP supplier and handler, real isolated Redis and independent gateway
// instances: learn ownership, reject replay via HTTP 200 SSE, then recover.
func TestOpenAI200OmitHistoryHTTP(t *testing.T) {
	for _, scenario := range []string{"enabled", "off", "phrase", "still_rejected", "output_started", "retry_400", "retry_503", "retry_disconnect"} {
		t.Run(scenario, func(t *testing.T) {
			clients := issue805Redis(t)
			followup := issue715Body(true, true)
			var err error
			followup, err = sjson.SetRaw(followup, "input.5", gjson.Get(followup, "input.4").Raw)
			require.NoError(t, err)
			followup, err = sjson.SetRaw(followup, "input.4", `{"type":"reasoning","encrypted_content":"fixture-200-reasoning","summary":[{"type":"summary_text","text":"visible summary survives"}]}`)
			require.NoError(t, err)
			var mu sync.Mutex
			var bodies [][]byte
			var identities []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					t.Error(readErr)
					return
				}
				mu.Lock()
				bodies = append(bodies, body)
				identities = append(identities, r.Header.Get("Authorization"))
				n := len(bodies)
				mu.Unlock()
				w.Header().Set("X-Request-Id", fmt.Sprintf("attempt-%d", n))
				if n >= 3 && (scenario == "retry_400" || scenario == "retry_503") {
					w.Header().Set("Content-Type", "application/json")
					if scenario == "retry_400" {
						w.WriteHeader(400)
					} else {
						w.WriteHeader(503)
					}
					_, _ = io.WriteString(w, `{"error":{"code":"invalid_encrypted_content","message":"fixture rejection"}}`)
					return
				}
				if n >= 3 && scenario == "retry_disconnect" {
					conn, _, hijackErr := w.(http.Hijacker).Hijack()
					if hijackErr != nil {
						t.Error(hijackErr)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if n > 1 && (strings.Contains(string(body), "fixture-200-reasoning") || scenario == "still_rejected") {
					_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_rejected\",\"status\":\"in_progress\"}}\n\n")
					w.(http.Flusher).Flush()
					if scenario == "output_started" {
						_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"already delivered\"}\n\n")
						w.(http.Flusher).Flush()
					}
					message, code := "Encrypted reasoning rejected", "invalid_encrypted_content"
					if scenario == "phrase" {
						message = "Encrypted function output content could not be decrypted or decoded."
						code = "server_error"
					}
					_, _ = fmt.Fprintf(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_rejected\",\"status\":\"failed\",\"error\":{\"code\":%q,\"message\":%q}}}\n\n", code, message)
					return
				}
				output := "[" + issue715Agent + "]"
				if n > 1 {
					for _, path := range []string{"input.1", "input.2", "input.3", "input.5"} {
						assert.JSONEq(t, gjson.Get(followup, path).Raw, gjson.GetBytes(body, path).Raw, path)
					}
					if !strings.Contains(string(body), "visible summary survives") {
						t.Error("summary lost")
					}
					if strings.Contains(string(body), "fixture-task-reference-715") {
						t.Error("opaque agent history survived")
					}
					output = `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"continued from readable history"}]}]`
				}
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_200_%d\",\"status\":\"completed\",\"error\":null,\"model\":\"gpt-5.1\",\"output\":%s,\"usage\":{\"input_tokens\":5,\"output_tokens\":3}}}\n\n", n, output)
			}))
			defer srv.Close()
			accounts := issue715AccountsOfType(service.AccountTypeAPIKey)
			for i := range accounts {
				accounts[i].Credentials["base_url"] = srv.URL
			}
			newNode := func(enabled bool) *OpenAIGatewayHandler {
				cfg := &config.Config{RunMode: config.RunModeSimple}
				cfg.Security.URLAllowlist.AllowInsecureHTTP = true
				settings := service.NewSettingService(&omit400SettingsRepo{enabled200: enabled, enabled400: true}, cfg)
				h := newOpenAIResponsesFailoverTestHandlerWithSettings(t, testutil.NewRealHTTPUpstream(cfg), repository.NewGatewayCache(clients()), settings, accounts...)
				h.cfg.Security.URLAllowlist.AllowInsecureHTTP = true
				return h
			}
			first := issue758Request(t, newNode(false), strings.Replace(issue715Body(false, true), issue715Agent+",", "", 1))
			require.Equal(t, http.StatusOK, first.Code, first.Body.String())
			issue758CompletedResponse(t, first.Body.String(), true)
			owners := issue805Owners(t, clients())
			require.Len(t, owners, 1)
			answer := issue758Request(t, newNode(scenario != "off"), followup)
			mu.Lock()
			defer mu.Unlock()
			if scenario == "off" || scenario == "output_started" {
				require.Len(t, bodies, 2)
				require.Contains(t, answer.Body.String(), "response.failed")
				require.NotContains(t, answer.Body.String(), "response.completed")
			} else {
				require.Len(t, bodies, 3, "producer + rejection + one same-account retry; %s", answer.Body.String())
				require.Equal(t, identities[1], identities[2])
				if scenario == "enabled" || scenario == "phrase" {
					require.Equal(t, http.StatusOK, answer.Code, answer.Body.String())
					issue758CompletedResponse(t, answer.Body.String(), true)
					require.Contains(t, answer.Body.String(), "continued from readable history")
					require.NotContains(t, answer.Body.String(), "resp_rejected")
					require.NotContains(t, answer.Body.String(), "response.failed")
					require.Equal(t, "attempt-3", answer.Header().Get("X-Request-Id"))
				} else {
					require.NotContains(t, answer.Body.String(), "response.completed")
					require.True(t, answer.Code >= 400 || strings.Contains(answer.Body.String(), "response.failed"), answer.Body.String())
				}
			}
			require.Equal(t, owners, issue805Owners(t, clients()), "never republish original inputs as the new producer")
		})
	}
}

func TestOpenAI200RecoveredTelemetryKeepsActualHTTPStatus(t *testing.T) {
	setupOpsErrorLogTestQueue(t, 2)
	repo := &ingressRejectOpsRepo{}
	ops := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Set(service.OpsUpstreamErrorsKey, []*service.OpsUpstreamErrorEvent{{
			UpstreamStatusCode: http.StatusOK,
			Kind:               "retry", Stage: "native_sse", Reason: "encrypted_history_200_omitted",
			Message:             "Explicit encrypted history rejection before output; recovered",
			EncryptedHistory200: &service.OpenAI400HistoryDiagnostic{Applied: true, Decision: "same_account_retry_scheduled"},
		}})
		c.JSON(http.StatusOK, gin.H{"status": "completed"})
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, int64(1), OpsErrorLogQueueLength())
	job := <-opsErrorLogQueue
	require.Equal(t, http.StatusOK, job.entry.StatusCode, "recovered request stays outside failure SLA")
	require.Equal(t, http.StatusOK, *job.entry.UpstreamStatusCode, "do not fabricate upstream HTTP 400")
	require.Contains(t, job.entry.ErrorMessage, "Recovered")
	events, err := service.ParseOpsUpstreamErrors(*job.entry.UpstreamErrorsJSON)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.True(t, events[0].EncryptedHistory200.Applied)
	require.Equal(t, "encrypted_history_200_omitted", events[0].Reason)
}
