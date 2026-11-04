package service

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type gzipTestHTTP struct {
	send func(*http.Request) (*http.Response, error)
}

func (u gzipTestHTTP) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.send(r)
}
func (u gzipTestHTTP) DoWithTLS(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.send(r)
}

func gzipTestRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "http://relay.example/v1/chat/completions", bytes.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.Header.Set("Authorization", "Bearer fixture-key")
	return r
}

func gzipTestAccount(platform string, enabled bool) *Account {
	return &Account{ID: 7, Platform: platform, Type: AccountTypeAPIKey,
		Extra: map[string]any{AccountExtraRequestGzipEnabled: enabled}}
}

func TestRequestGzipGatewayWireAndReplay(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformDeepseek} {
		t.Run(platform, func(t *testing.T) {
			// Exercise the 20 MiB relay use case without retaining another raw copy.
			body := []byte(`{"model":"deepseek-test","input":"` + strings.Repeat("a", 20<<20) + `"}`)
			upstream := gzipTestHTTP{send: func(r *http.Request) (*http.Response, error) {
				require.Equal(t, "gzip", r.Header.Get("Content-Encoding"))
				require.Equal(t, "Bearer fixture-key", r.Header.Get("Authorization"))
				wire, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.NoError(t, r.Body.Close())
				require.Equal(t, int64(len(wire)), r.ContentLength)
				require.Empty(t, r.Header.Get("Content-Length"))
				require.Empty(t, r.TransferEncoding)
				require.Less(t, len(wire), len(body))
				for i := 0; i < 2; i++ {
					replay, err := r.GetBody()
					require.NoError(t, err)
					gz, err := gzip.NewReader(replay)
					require.NoError(t, err)
					decoded, err := io.ReadAll(gz)
					require.NoError(t, err)
					require.Equal(t, body, decoded)
					require.NoError(t, gz.Close())
					require.NoError(t, replay.Close())
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
			}}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			req := gzipTestRequest(t, body)
			req.Header.Set("Content-Length", "stale-value")
			resp, err := svc.doOpenAIUpstream(req, "", gzipTestAccount(platform, true))
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Empty(t, req.Header.Get("Content-Encoding"))
			require.Equal(t, "stale-value", req.Header.Get("Content-Length"))
			require.Equal(t, int64(len(body)), req.ContentLength)
			replay, err := req.GetBody()
			require.NoError(t, err)
			original, err := io.ReadAll(replay)
			require.NoError(t, err)
			require.Equal(t, body, original)
			require.NoError(t, replay.Close())
		})
	}
}

func TestRequestGzipAccountIsolationAndTLS(t *testing.T) {
	body := []byte(strings.Repeat("json body ", 1024))
	var encodings []string
	upstream := gzipTestHTTP{send: func(r *http.Request) (*http.Response, error) {
		encodings = append(encodings, r.Header.Get("Content-Encoding"))
		_, err := io.Copy(io.Discard, r.Body)
		require.NoError(t, err)
		require.NoError(t, r.Body.Close())
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
	}}
	for _, enabled := range []bool{true, false, true, false} {
		account := gzipTestAccount(PlatformAnthropic, enabled)
		resp, err := gatewayHTTPUpstream(upstream, nil, account, nil).DoWithTLS(gzipTestRequest(t, body), "", 7, 1, nil)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	require.Equal(t, []string{"gzip", "", "gzip", ""}, encodings)
}

func TestRequestGzipSkipsOtherWireFormats(t *testing.T) {
	for name, change := range map[string]func(*http.Request){
		"small":          func(r *http.Request) { r.ContentLength = 100 },
		"get":            func(r *http.Request) { r.Method = http.MethodGet },
		"websocket":      func(r *http.Request) { r.Header.Set("Upgrade", "websocket") },
		"multipart":      func(r *http.Request) { r.Header.Set("Content-Type", "multipart/form-data; boundary=test") },
		"encoded":        func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") },
		"no_replay":      func(r *http.Request) { r.GetBody = nil },
		"unknown_length": func(r *http.Request) { r.ContentLength = -1 },
		"trailer":        func(r *http.Request) { r.Trailer = http.Header{"Digest": {"test"}} },
	} {
		t.Run(name, func(t *testing.T) {
			r := gzipTestRequest(t, bytes.Repeat([]byte("a"), 4096))
			change(r)
			prepared, err := gzipUpstreamRequest(r)
			require.NoError(t, err)
			require.Same(t, r, prepared)
		})
	}
	random := make([]byte, 8192)
	_, err := rand.Read(random)
	require.NoError(t, err)
	r := gzipTestRequest(t, random)
	prepared, err := gzipUpstreamRequest(r)
	require.NoError(t, err)
	require.Same(t, r, prepared)
	untouched, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	require.Equal(t, random, untouched)
}

type gzipCancelReader struct {
	io.Reader
	cancel context.CancelFunc
}

func (r gzipCancelReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.cancel()
	return n, err
}

func TestRequestGzipCancellationDoesNotSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := gzipTestRequest(t, bytes.Repeat([]byte("a"), 1<<20)).WithContext(ctx)
	originalGetBody := req.GetBody
	req.GetBody = func() (io.ReadCloser, error) {
		body, err := originalGetBody()
		return io.NopCloser(gzipCancelReader{Reader: body, cancel: cancel}), err
	}
	upstream := gzipTestHTTP{send: func(*http.Request) (*http.Response, error) {
		t.Fatal("canceled compression must not start a physical attempt")
		return nil, nil
	}}
	_, err := accountRequestGzipHTTP(upstream, gzipTestAccount(PlatformDeepseek, true)).Do(req, "", 7, 1)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRequestGzipKeepsStreamingResponses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "expected gzip", 400)
			return
		}
		_, _ = io.Copy(io.Discard, gz)
		_ = gz.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	defer cancel()
	req := gzipTestRequest(t, bytes.Repeat([]byte("a"), 4096)).WithContext(ctx)
	req.URL, _ = req.URL.Parse(server.URL)
	upstream := gzipTestHTTP{send: server.Client().Do}
	resp, err := gatewayHTTPUpstream(upstream, nil, gzipTestAccount(PlatformDeepseek, true), nil).Do(req, "", 7, 1)
	require.NoError(t, err)
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "data: first\n", line)
	cancel()
	require.NoError(t, resp.Body.Close())
}

func TestRequestGzipPreservesLateEgressTimezoneRewrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") != "gzip" {
			http.Error(w, "expected compressed body", 400)
			return
		}
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		defer gz.Close()
		decoded, err := io.ReadAll(gz)
		want := r.URL.Query().Get("timezone")
		if err != nil || !bytes.Contains(decoded, []byte("<timezone>"+want+"</timezone>")) || r.Header.Get("X-Timezone") != want {
			http.Error(w, "body and headers must use the actual egress timezone", 400)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	probe := &egressTimezoneProber{probe: func(_ context.Context, proxy string) (*ProxyExitInfo, int64, error) {
		zone := "Asia/Tokyo"
		if proxy == "http://proxy-b:8080" {
			zone = "America/Los_Angeles"
		}
		return &ProxyExitInfo{IP: "203.0.113.10", Timezone: zone}, 0, nil
	}}
	svc := timezoneAutoService(true, true, probe)
	account := gzipTestAccount(PlatformOpenAI, true)
	ctx, cancel := context.WithTimeout(svc.withRequestTimezoneEgress(t.Context(), account), 5*time.Second)
	defer cancel()
	body := localeTestBody([]string{"<environment_context><timezone>Asia/Shanghai</timezone></environment_context>", strings.Repeat("context ", 1024)}, nil)
	original := gzipTestRequest(t, body).WithContext(ctx)
	original.Header.Set("X-Timezone", "Asia/Shanghai")
	for _, tc := range []struct{ proxy, timezone string }{
		{"http://proxy-a:8080", "Asia/Tokyo"},
		{"http://proxy-b:8080", "America/Los_Angeles"},
	} {
		retry, err := cloneUpstreamRequestForRetry(ctx, original)
		require.NoError(t, err)
		retry.URL, err = retry.URL.Parse(server.URL)
		require.NoError(t, err)
		query := retry.URL.Query()
		query.Set("timezone", tc.timezone)
		retry.URL.RawQuery = query.Encode()
		upstream := gzipTestHTTP{send: func(r *http.Request) (*http.Response, error) {
			// The repository binds the final egress after the account decorator.
			bound, err := BindOpenAIRequestTimezone(r, tc.proxy)
			if err != nil {
				return nil, err
			}
			return server.Client().Do(bound)
		}}
		resp, err := accountRequestGzipHTTP(upstream, account).Do(retry, tc.proxy, account.ID, 1)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
	require.Empty(t, original.Header.Get("Content-Encoding"))
	require.Equal(t, "Asia/Shanghai", original.Header.Get("X-Timezone"))
	replay, err := original.GetBody()
	require.NoError(t, err)
	defer replay.Close()
	unchanged, err := io.ReadAll(replay)
	require.NoError(t, err)
	require.Equal(t, body, unchanged)
}

func TestRequestGzipSettingsValidation(t *testing.T) {
	for _, value := range []any{nil, false, true} {
		extra := map[string]any{AccountExtraRequestGzipEnabled: value}
		require.NoError(t, ValidateRequestGzipExtra(extra))
		a, err := buildAccountForCreate(&CreateAccountInput{Type: AccountTypeAPIKey, Platform: PlatformDeepseek}, extra)
		require.NoError(t, err)
		require.Equal(t, value == true, a.RequestGzipEnabled())
		a.Type = AccountTypeOAuth
		require.False(t, a.RequestGzipEnabled())
	}
	var absent *Account
	require.False(t, absent.RequestGzipEnabled())
	require.False(t, (&Account{Type: AccountTypeAPIKey}).RequestGzipEnabled())
	for _, value := range []any{"true", 1, []any{true}} {
		extra := map[string]any{AccountExtraRequestGzipEnabled: value}
		require.Error(t, ValidateRequestGzipExtra(extra))
		s := &adminServiceImpl{}
		_, err := s.UpdateAccount(context.Background(), 7, &UpdateAccountInput{Extra: extra})
		require.Error(t, err)
		_, err = s.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{Extra: extra})
		require.Error(t, err)
		require.Error(t, s.UpdateAccountExtra(context.Background(), 7, extra))
	}
	getErr := errors.New("cannot rewind")
	req := gzipTestRequest(t, bytes.Repeat([]byte("a"), 4096))
	req.GetBody = func() (io.ReadCloser, error) { return nil, getErr }
	_, err := gzipUpstreamRequest(req)
	require.ErrorIs(t, err, getErr)
}
