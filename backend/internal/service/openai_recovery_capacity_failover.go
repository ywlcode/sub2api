package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type openAIRecoveryCapacityKey struct{}

var ErrOpenAIRecoveryCapacityCandidate = errors.New("readable capacity failover candidate cannot send the same request")

// OpenAIRecoveryCapacityFailover belongs to one HTTP Responses handler. Each
// account builds its own native projection, but both sends must agree on the
// complete sanitized wire body, including model and request controls.
type OpenAIRecoveryCapacityFailover struct {
	failure          *UpstreamFailoverError
	bodyDigest       [sha256.Size]byte
	credentialDigest [sha256.Size]byte
	authority        string
	accountID        int64
	sentAlternative  bool
}

func (f *OpenAIRecoveryCapacityFailover) WithContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, openAIRecoveryCapacityKey{}, f)
}

func (f *OpenAIRecoveryCapacityFailover) Matches(err *UpstreamFailoverError) bool {
	return err != nil && f.failure == err
}

func recoveryCapacityFromContext(ctx context.Context) *OpenAIRecoveryCapacityFailover {
	f, _ := ctx.Value(openAIRecoveryCapacityKey{}).(*OpenAIRecoveryCapacityFailover)
	return f
}

// Compare effective ports and canonical host spelling; a different path, API
// key or an explicit default port does not establish a second upstream.
func recoveryCapacityAuthority(u *url.URL) string {
	if u == nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return ""
		}
	} else {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return ""
		}
		port = strconv.FormatUint(n, 10)
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), port)
}

func (f *OpenAIRecoveryCapacityFailover) acceptsAccount(account *Account) bool {
	if f == nil || f.failure == nil {
		return true
	}
	// OAuth plugins, harvest tickets and safety prompts can replace the body
	// or identity downstream of the native recovery proof. They cannot
	// certify a byte-identical second send at this boundary.
	if account == nil || !account.IsOpenAIApiKey() || f.sentAlternative || account.ID == f.accountID {
		return false
	}
	target := account.GetOpenAIBaseURL()
	if account.UsesNativeCNResponses() && account.IsAdaptiveAPIProtocol() {
		target = account.GetCNProtocolBaseURL(APIProtocolResponses)
	}
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	authority := recoveryCapacityAuthority(u)
	return authority != "" && authority != f.authority
}

func (f *OpenAIRecoveryCapacityFailover) checkWire(account *Account, req *http.Request, body []byte) error {
	digest := sha256.Sum256(body)
	authority := recoveryCapacityAuthority(req.URL)
	credential := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	if f.failure == nil {
		f.bodyDigest, f.authority, f.accountID, f.credentialDigest = digest, authority, account.ID, credential
		return nil
	}
	if f.sentAlternative {
		return f.failure
	}
	if !account.IsOpenAIApiKey() || account.ID == f.accountID || authority == "" || authority == f.authority || credential == f.credentialDigest || f.bodyDigest != digest {
		return ErrOpenAIRecoveryCapacityCandidate
	}
	f.sentAlternative = true
	return nil
}

const recoveryCapacityMessage = "Request capacity is exhausted; retry later"

// Some MMC pools use model_not_found for a pre-generation capacity rejection.
// Only the observed envelope identifies that case. Other fields may carry a
// reset, throttle or conflicting reason even if the message is identical.
func isRecoveryCapacityRejection(headers http.Header, body []byte) bool {
	for name := range headers {
		name = strings.ToLower(name)
		if strings.Contains(name, "retry") || strings.Contains(name, "limit") || strings.Contains(name, "quota") || strings.Contains(name, "throttle") || strings.Contains(name, "reset") {
			return false
		}
	}
	if disposition, _ := classifyOpenAIOAuth429(headers, body); disposition != openAIOAuth429Transient {
		return false
	}
	if !openAIEncryptedJSONUnambiguous(body) {
		return false
	}
	root := gjson.ParseBytes(body).Map()
	fields := root["error"].Map()
	if msg, ok := root["message"]; ok {
		if msg.String() != recoveryCapacityMessage {
			return false
		}
		delete(root, "message")
	}
	return len(root) == 1 && len(fields) == 3 && fields["message"].String() == recoveryCapacityMessage &&
		fields["type"].String() == "invalid_request_error" && fields["code"].String() == "model_not_found"
}

func (s *OpenAIGatewayService) recoveryCapacityRejection(ctx context.Context, c *gin.Context, account *Account, resp *http.Response, body []byte) *UpstreamFailoverError {
	f := recoveryCapacityFromContext(ctx)
	if f == nil || f.failure != nil || !account.IsOpenAIApiKey() || f.authority == "" || f.accountID != account.ID || resp.StatusCode != http.StatusTooManyRequests || isControlledExperiment(ctx) {
		return nil
	}
	r := openAIAgentRecoveryFromContext(ctx)
	if r == nil || !r.omittedHistory || r.omittedErrorHistory || r.owner != nil || r.previousResponseID != "" || len(r.items) != 0 || r.toolResult ||
		r.delivery == nil || len(r.outputs) != 0 || len(r.delivery.keys) != 0 || len(r.delivery.discoveryBindings) != 0 ||
		(account.IsTempUnschedulableEnabled() && len(matchTempUnschedulableRules(account, resp.StatusCode, body)) > 0) || !account.ShouldHandleErrorCode(resp.StatusCode) ||
		!isRecoveryCapacityRejection(resp.Header, body) {
		return nil
	}
	f.failure = &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseHeaders: resp.Header.Clone(), ResponseBody: body,
		Scope: GatewayFailureScopeRequest, RequestScopedTransient: true}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account), Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("X-Request-ID"), Kind: "failover", Message: recoveryCapacityMessage, Detail: s.opsUpstreamErrorDetail(body),
	})
	return f.failure
}
