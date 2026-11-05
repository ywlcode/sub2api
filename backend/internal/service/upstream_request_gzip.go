package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

const AccountExtraRequestGzipEnabled = "request_gzip_enabled"

const upstreamRequestGzipMinBytes = 1024

// RequestGzipEnabled is opt-in for API-key HTTP relays. OAuth and signed
// provider requests must not have their wire representation changed here.
func (a *Account) RequestGzipEnabled() bool {
	if a == nil || a.Type != AccountTypeAPIKey {
		return false
	}
	enabled, _ := a.Extra[AccountExtraRequestGzipEnabled].(bool)
	return enabled
}

func ValidateRequestGzipExtra(extra map[string]any) error {
	if value, exists := extra[AccountExtraRequestGzipEnabled]; exists && value != nil {
		if _, ok := value.(bool); !ok {
			return infraerrors.BadRequest("INVALID_REQUEST_GZIP_ENABLED", "request_gzip_enabled must be a boolean")
		}
	}
	return nil
}

// Wrap the selected account, not the shared client pool: switching accounts or
// updating settings must not leak the opt-in to another request/account.
func accountRequestGzipHTTP(upstream HTTPUpstream, account *Account) HTTPUpstream {
	if !account.RequestGzipEnabled() {
		return upstream
	}
	return &requestGzipUpstream{HTTPUpstream: upstream}
}

// Admin account probes should exercise the same opt-in as gateway traffic.
func (s *AccountTestService) accountHTTPUpstream(account *Account) HTTPUpstream {
	return timedAccountTestUpstream(accountRequestGzipHTTP(s.httpUpstream, account))
}

type requestGzipUpstream struct{ HTTPUpstream }

func (u *requestGzipUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	return u.send(req, func(prepared *http.Request) (*http.Response, error) {
		return u.HTTPUpstream.Do(prepared, proxy, accountID, concurrency)
	})
}

func (u *requestGzipUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.send(req, func(prepared *http.Request) (*http.Response, error) {
		return u.HTTPUpstream.DoWithTLS(prepared, proxy, accountID, concurrency, profile)
	})
}

func (u *requestGzipUpstream) send(req *http.Request, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	prepared, err := gzipUpstreamRequest(req)
	// The transport owns the compressed body. Close the displaced original on
	// every exit, including preparation failures, just as Client.Do would.
	if (err != nil || prepared != req) && req != nil && req.Body != nil {
		defer func() { _ = req.Body.Close() }()
	}
	if err != nil {
		return nil, err
	}
	return send(prepared)
}

func gzipUpstreamRequest(req *http.Request) (*http.Request, error) {
	if req == nil || req.Body == nil || req.GetBody == nil || req.ContentLength < upstreamRequestGzipMinBytes {
		return req, nil
	}
	if req.Method != http.MethodPost && req.Method != http.MethodPut && req.Method != http.MethodPatch {
		return req, nil
	}
	if req.Header.Get("Upgrade") != "" || len(req.Trailer) != 0 {
		return req, nil
	}
	encoding := strings.TrimSpace(req.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return req, nil
	}
	mediaType, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && (!strings.HasPrefix(mediaType, "application/") || !strings.HasSuffix(mediaType, "+json"))) {
		return req, nil
	}
	if err := context.Cause(req.Context()); err != nil {
		return nil, err
	}
	// Read a separate, rewindable view. Never consume the canonical request used
	// by protocol fallbacks, retries, request hashing or billing.
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("prepare gzip upstream request: %w", err)
	}
	if body == nil {
		return nil, errors.New("prepare gzip upstream request: GetBody returned no body")
	}
	defer func() { _ = body.Close() }()
	compressed := &gzipSizeBound{limit: req.ContentLength - 1}
	writer, err := gzip.NewWriterLevel(compressed, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	_, copyErr := io.Copy(writer, &gzipContextReader{ctx: req.Context(), reader: body})
	closeErr := writer.Close()
	if err := context.Cause(req.Context()); err != nil {
		return nil, err
	}
	if errors.Is(copyErr, errGzipNoSavings) || errors.Is(closeErr, errGzipNoSavings) {
		return req, nil
	}
	if err := errors.Join(copyErr, closeErr); err != nil {
		return nil, fmt.Errorf("compress upstream request: %w", err)
	}
	data := compressed.Bytes()
	// Retain the logical body for late transport rewrites, such as timezone
	// binding after regional egress selection. Those rewrites reapply gzip.
	ctx := context.WithValue(req.Context(), upstreamRequestGzipBodyKey{}, req.GetBody)
	prepared := req.Clone(ctx)
	prepared.Body = io.NopCloser(bytes.NewReader(data))
	prepared.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	prepared.ContentLength = int64(len(data))
	prepared.TransferEncoding = nil
	prepared.Header.Set("Content-Encoding", "gzip")
	prepared.Header.Del("Content-Length")
	return prepared, nil
}

type upstreamRequestGzipBodyKey struct{}

func upstreamRequestBodyForRewrite(req *http.Request) (io.ReadCloser, bool, error) {
	getBody, compressed := req.Context().Value(upstreamRequestGzipBodyKey{}).(func() (io.ReadCloser, error))
	compressed = compressed && strings.EqualFold(req.Header.Get("Content-Encoding"), "gzip")
	if !compressed {
		getBody = req.GetBody
	}
	body, err := getBody()
	return body, compressed, err
}

func recompressRewrittenUpstreamRequest(req *http.Request) (*http.Request, error) {
	// The rewritten Body/GetBody now contain JSON. Clear the old encoding even
	// if the new body is too small or no longer benefits from compression.
	req = req.WithContext(context.WithValue(req.Context(), upstreamRequestGzipBodyKey{}, nil))
	req.Header.Del("Content-Encoding")
	req.Header.Del("Content-Length")
	prepared, err := gzipUpstreamRequest(req)
	if err != nil || prepared != req {
		_ = req.Body.Close()
	}
	return prepared, err
}

var errGzipNoSavings = errors.New("gzip does not reduce request size")

// Bound extra memory for incompressible input and keep the original request
// when compression would expand it. No uncompressed copy is allocated here.
type gzipSizeBound struct {
	bytes.Buffer
	limit int64
}

func (b *gzipSizeBound) Write(p []byte) (int, error) {
	if int64(len(p)) > b.limit-int64(b.Len()) {
		return 0, errGzipNoSavings
	}
	return b.Buffer.Write(p)
}

type gzipContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *gzipContextReader) Read(p []byte) (int, error) {
	if err := context.Cause(r.ctx); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
