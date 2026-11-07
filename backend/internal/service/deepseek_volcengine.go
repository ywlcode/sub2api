package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/pkg/volcengine"
)

// DeepSeekVolcengineCompatibilityCredential is explicit because DeepSeek accounts
// may target Ark directly or through another relay. Never infer it from a model alias.
const DeepSeekVolcengineCompatibilityCredential = "deepseek_volcengine_compatibility"

func (a *Account) IsDeepSeekVolcengineCompatibilityEnabled() bool {
	if a == nil || a.Platform != PlatformDeepseek || a.Type != AccountTypeAPIKey {
		return false
	}
	enabled, _ := a.Credentials[DeepSeekVolcengineCompatibilityCredential].(bool)
	return enabled
}

// normalizeVolcengineRequestBody keeps the compatibility opt-in account-scoped.
func normalizeVolcengineRequestBody(account *Account, body []byte, protocol string) ([]byte, error) {
	if !account.IsDeepSeekVolcengineCompatibilityEnabled() {
		return body, nil
	}
	return volcengine.NormalizeRequest(body, protocol)
}

// Apply the final wire adaptation before gzip, for every HTTP ingress bridge
// and admin probe. Account scope also works when the next hop is another relay.
func accountVolcengineHTTP(upstream HTTPUpstream, account *Account) HTTPUpstream {
	if !account.IsDeepSeekVolcengineCompatibilityEnabled() {
		return upstream
	}
	return &volcengineHTTP{HTTPUpstream: upstream, account: account}
}

type volcengineHTTP struct {
	HTTPUpstream
	account *Account
}

func (u *volcengineHTTP) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	return u.send(req, func(prepared *http.Request) (*http.Response, error) {
		return u.HTTPUpstream.Do(prepared, proxy, accountID, concurrency)
	})
}

func (u *volcengineHTTP) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.send(req, func(prepared *http.Request) (*http.Response, error) {
		return u.HTTPUpstream.DoWithTLS(prepared, proxy, accountID, concurrency, profile)
	})
}

func (u *volcengineHTTP) send(req *http.Request, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	if req == nil || req.URL == nil || req.Method != http.MethodPost || req.Body == nil || req.GetBody == nil {
		return send(req)
	}
	var protocol string
	switch path := strings.TrimRight(req.URL.Path, "/"); {
	case strings.HasSuffix(path, "/responses"):
		protocol = APIProtocolResponses
	case strings.HasSuffix(path, "/chat/completions"):
		protocol = APIProtocolChatCompletions
	case strings.HasSuffix(path, "/messages"):
		protocol = APIProtocolAnthropic
	default:
		return send(req)
	}
	defer func() { _ = req.Body.Close() }()
	if err := context.Cause(req.Context()); err != nil {
		return nil, err
	}
	reader, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, fmt.Errorf("normalize Volcengine request: GetBody returned no body")
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return nil, err
	}
	body, err = normalizeVolcengineRequestBody(u.account, body, protocol)
	if err != nil {
		return nil, err
	}
	prepared := req.Clone(req.Context())
	prepared.Body = io.NopCloser(bytes.NewReader(body))
	prepared.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	prepared.ContentLength = int64(len(body))
	prepared.TransferEncoding = nil
	prepared.Header.Del("Content-Length")
	return send(prepared)
}
