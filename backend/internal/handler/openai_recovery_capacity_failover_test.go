//go:build unit

package handler

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const fixtureError = `{"error":{"message":"Request capacity is exhausted; retry later"}}`
const fixtureIncident = `{"error":{"type":"invalid_request_error","code":"model_not_found","message":"Request capacity is exhausted; retry later"}}`
const fixtureCompletion = `{"id":"resp_local_b","status":"completed","error":null,"model":"gpt-5.1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fixture B completed"}]}],"usage":{"input_tokens":5,"output_tokens":3}}`

func TestOpenAIRecoveryCapacityFailover(t *testing.T) {
	for _, scenario := range []string{"incident", "missing_model", "auth", "message_only", "stream", "pinned", "committed", "no_alternative", "zero_budget", "changed_wire", "same_upstream", "both_rejected", "tool_result", "retry_after", "quota", "rate_limit", "reset", "ambiguous", "generic_429"} {
		t.Run(scenario, func(t *testing.T) {
			var mu sync.Mutex
			var bodies [2][]string
			const statusMessage = "Our servers are currently overloaded. Please try again later."
			accounts := issue715AccountsOfType(service.AccountTypeAPIKey)
			stream := scenario == "stream" || scenario == "committed"
			for i := range accounts {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if scenario == "pinned" && strings.Contains(string(body), "learn the owner") {
						_, _ = io.WriteString(w, strings.Replace(fixtureCompletion, gjson.Get(fixtureCompletion, "output").Raw, "["+issue715Agent+"]", 1))
						return
					}
					mu.Lock()
					bodies[i] = append(bodies[i], string(body))
					mu.Unlock()
					if i == 0 || scenario == "both_rejected" {
						w.Header().Set("X-Request-Id", fmt.Sprintf("fixture-%d", i+1))
						rejection := fixtureIncident
						switch scenario {
						case "message_only":
							rejection = fixtureError
						case "missing_model":
							rejection = strings.Replace(fixtureIncident, "Request capacity is exhausted; retry later", "Model not found", 1)
						case "auth":
							rejection = strings.Replace(fixtureIncident, "model_not_found", "invalid_api_key", 1)
						case "retry_after":
							w.Header().Set("Retry-After", "120")
						case "reset":
							w.Header().Set("X-RateLimit-Remaining-Requests", "0")
						case "quota":
							rejection = strings.Replace(rejection, "model_not_found", "insufficient_quota", 1)
						case "rate_limit":
							rejection = strings.Replace(rejection, "invalid_request_error", "rate_limit_error", 1)
						case "ambiguous":
							rejection = strings.Replace(rejection, `"message"`, `"headers":{"retry-after-ms":"500"},"message"`, 1)
						case "generic_429":
							rejection = `{"error":{"message":"Too many requests"}}`
						}
						w.WriteHeader(429)
						_, _ = io.WriteString(w, rejection)
						return
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\ndata: [DONE]\n\n", fixtureCompletion)
					} else {
						_, _ = io.WriteString(w, fixtureCompletion)
					}
				}))
				t.Cleanup(srv.Close)
				accounts[i].Credentials["base_url"] = srv.URL
			}
			if scenario == "same_upstream" {
				accounts[1].Credentials["base_url"] = accounts[0].Credentials["base_url"]
			}
			if scenario == "no_alternative" {
				accounts = accounts[:1]
			}
			if scenario == "changed_wire" {
				accounts[1].Credentials["model_mapping"] = map[string]any{"gpt-5.1": "fixture-other-model"}
			}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			settings := service.NewSettingService(&omit400SettingsRepo{enabled409: true}, cfg)
			h := newOpenAIResponsesFailoverTestHandlerWithSettings(t, recoveryCapacityLoopbackUpstream{testutil.NewRealHTTPUpstream(cfg)}, nil, settings, accounts...)
			h.cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			h.cfg.Gateway.OpenAIWS.LBTopK = 1
			if scenario == "zero_budget" {
				h.maxAccountSwitches = 0
			}
			status, msg := 502, statusMessage
			h.errorPassthroughService = service.NewErrorPassthroughService(&errorPassthroughRepoStub{rules: []*model.ErrorPassthroughRule{{ID: 8, Name: "fixture overload", Enabled: true, ErrorCodes: []int{429}, MatchMode: model.MatchModeAny, ResponseCode: &status, CustomMessage: &msg}}}, nil)
			if scenario == "pinned" {
				c, rec := newIssue715Context(t, `{"model":"gpt-5.1","input":"learn the owner"}`)
				c.Request.Header.Set("session_id", "fixture-capacity")
				h.Responses(c)
				require.Contains(t, rec.Body.String(), "fixture-task-reference-715")
			}
			c, rec := newIssue715Context(t, issue715Body(scenario == "tool_result", stream))
			c.Request.Header.Set("session_id", "fixture-capacity")
			if scenario == "committed" {
				c.Writer.Header().Set("Content-Type", "text/event-stream")
				_, err := c.Writer.WriteString(": fixture committed\n\n")
				require.NoError(t, err)
			}
			h.Responses(c)
			mu.Lock()
			defer mu.Unlock()
			t.Logf("A=%d B=%d HTTP=%d result=%s", len(bodies[0]), len(bodies[1]), rec.Code, rec.Body.String())
			require.Len(t, bodies[0], 1)
			if scenario == "pinned" {
				require.Contains(t, bodies[0][0], "fixture-task-reference-715")
			} else {
				require.NotContains(t, bodies[0][0], "fixture-task-reference-715")
			}
			require.Contains(t, bodies[0][0], "Keep ledger balance 11")
			if scenario == "incident" || scenario == "stream" || scenario == "both_rejected" {
				require.Equal(t, []string{bodies[0][0]}, bodies[1])
				events := c.MustGet(service.OpsUpstreamErrorsKey).([]*service.OpsUpstreamErrorEvent)
				require.Equal(t, "failover", events[0].Kind)
				require.Equal(t, int64(1), events[0].AccountID)
				require.Equal(t, "fixture-1", events[0].UpstreamRequestID)
				require.Equal(t, int64(2), c.GetInt64(opsAccountIDKey))
			} else {
				require.Empty(t, bodies[1])
			}
			switch scenario {
			case "incident", "stream":
				require.Equal(t, 200, rec.Code)
				completed := issue758CompletedResponse(t, rec.Body.String(), stream)
				require.Equal(t, "resp_local_b", completed.Get("id").String())
				require.Equal(t, 1, strings.Count(rec.Body.String(), "fixture B completed"))
			case "pinned":
				require.Equal(t, 409, rec.Code)
				require.Contains(t, rec.Body.String(), "encrypted_agent_recovery_owner_unavailable")
			case "committed":
				require.Equal(t, 200, rec.Code)
				require.Contains(t, rec.Body.String(), "event: response.failed")
				require.Contains(t, rec.Body.String(), msg)
			default:
				require.Equal(t, 502, rec.Code)
				require.Contains(t, rec.Body.String(), msg)
				if scenario == "retry_after" {
					require.Equal(t, "120", rec.Header().Get("Retry-After"))
				}
			}
		})
	}
}

type recoveryCapacityLoopbackUpstream struct{ service.HTTPUpstream }

func (u recoveryCapacityLoopbackUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	if ip := net.ParseIP(req.URL.Hostname()); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("non-loopback test upstream")
	}
	return u.HTTPUpstream.Do(req, proxy, accountID, concurrency)
}

func TestOpenAIRecoveryCapacitySkipsIneligibleCandidate(t *testing.T) {
	for _, reason := range []string{"same_authority", "different_payload", "same_credential", "oauth_projection"} {
		t.Run(reason, func(t *testing.T) {
			accounts := issue715AccountsOfType(service.AccountTypeAPIKey)
			third := accounts[1]
			third.ID, third.Priority, third.Credentials = 3, 2, map[string]any{"api_key": "fixture-key-3"}
			accounts = append(accounts, third)
			var mu sync.Mutex
			var attempts []string
			var bodies []string
			for i := range accounts {
				if i == 1 && reason == "same_authority" {
					accounts[i].Credentials["base_url"] = accounts[0].Credentials["base_url"]
					continue
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					attempts = append(attempts, r.Header.Get("Authorization"))
					bodies = append(bodies, string(b))
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					if i == 0 {
						w.WriteHeader(429)
						_, _ = io.WriteString(w, fixtureIncident)
						return
					}
					_, _ = io.WriteString(w, fixtureCompletion)
				}))
				t.Cleanup(srv.Close)
				accounts[i].Credentials["base_url"] = srv.URL
			}
			if reason == "same_credential" {
				accounts[1].Credentials["api_key"] = accounts[0].Credentials["api_key"]
			}
			if reason == "oauth_projection" {
				accounts[1].Type = service.AccountTypeOAuth
				accounts[1].Credentials = map[string]any{"access_token": "fixture-oauth"}
			}
			if reason == "different_payload" {
				accounts[1].Credentials["model_mapping"] = map[string]any{"gpt-5.1": "other-model"}
			}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			settings := service.NewSettingService(&omit400SettingsRepo{enabled409: true}, cfg)
			h := newOpenAIResponsesFailoverTestHandlerWithSettings(t, recoveryCapacityLoopbackUpstream{testutil.NewRealHTTPUpstream(cfg)}, nil, settings, accounts...)
			h.cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			h.cfg.Gateway.OpenAIWS.LBTopK = 1
			h.maxAccountSwitches = 1
			c, rec := newIssue715Context(t, issue715Body(false, false))
			c.Request.Header.Set("session_id", "fixture-capacity")
			h.Responses(c)
			mu.Lock()
			defer mu.Unlock()
			t.Logf("switch budget=1, wire identities=%v, HTTP=%d", attempts, rec.Code)
			require.Equal(t, []string{"Bearer fixture-key-1", "Bearer fixture-key-3"}, attempts)
			require.Equal(t, bodies[0], bodies[1])
			require.NotContains(t, bodies[1], "fixture-task-reference-715")
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, "resp_local_b", issue758CompletedResponse(t, rec.Body.String(), false).Get("id").String())
		})
	}
}
