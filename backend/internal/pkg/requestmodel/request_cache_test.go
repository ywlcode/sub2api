package requestmodel

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestCandidatesBodyVersionAndIsolation(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("Content-Type", "application/json")
	body := []byte(`{"model":"public","Model":"second","session":{"model":"live"}}`)
	ResetRequestBody(req, body)
	got := FromRequestCandidates(req, "/v1/responses", body)
	require.Equal(t, []string{"public", "second"}, got)
	got[0] = "caller mutation"
	ResetRequestBody(req, body) // rereads must preserve the same cache
	require.Equal(t, []string{"public", "second"}, FromRequestCandidates(req, "/v1/responses", body))
	require.Equal(t, []string{"live"}, FromRequestCandidates(req, "/v1/live", body))
	require.True(t, ValidJSON(req, body))
	mapped := []byte(`{"model":"upstream"}`)
	ResetRequestBody(req, mapped)
	require.Equal(t, []string{"upstream"}, FromRequestCandidates(req, "/v1/responses", mapped))
	require.True(t, ValidJSON(req, mapped))
	invalid := []byte(`{"model":"broken"`)
	require.False(t, ValidJSON(req, invalid))
	require.True(t, ValidJSON(req, mapped))
	other := httptest.NewRequest("POST", "/v1/responses", nil)
	require.Equal(t, []string{"public", "second"}, FromRequestCandidates(other, "/v1/responses", body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=absent")
	require.Empty(t, FromRequestCandidates(req, "/v1/responses", mapped))
	req.Header.Set("Content-Type", "application/json")
	require.Equal(t, []string{"upstream"}, FromRequestCandidates(req, "/v1/responses", mapped))
}

func TestRequestCandidatesConcurrentReaders(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	body := []byte(`{"model":"public"}`)
	FromRequestCandidates(req, "/v1/responses", body) // publish before concurrent readers
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				got := FromRequestCandidates(req, "/v1/responses", body)
				if len(got) != 1 || got[0] != "public" || !ValidJSON(req, body) {
					t.Error("inconsistent cache")
				}
			}
		}()
	}
	wg.Wait()
}

func FuzzJSONModelCandidatesCombined(f *testing.F) {
	for _, s := range []string{`{"model":"first","Model":"last","session":{"Model":"s"}}`, `{"model":"a","model":"b"}`, `{"\u006dodel":"escaped"}`, `{"session":{"model":"a","model":"b"}}`, `null`, `{`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 1<<20 {
			t.Skip()
		}
		top, session := jsonAllModelCandidates(body)
		require.Equal(t, jsonModelCandidates(body), top)
		require.Equal(t, jsonSessionModelCandidates(body), session)
	})
}

func TestModelCandidatesDoNotBorrowMutableStrings(t *testing.T) {
	body := []byte(`{"model":"name","session":{"model":"live"}}`)
	top, session := jsonAllModelCandidates(body)
	copy(body, bytes.Repeat([]byte("x"), len(body)))
	require.Equal(t, []string{"name"}, top)
	require.Equal(t, []string{"live"}, session)
}

func BenchmarkRequestModelCandidates(b *testing.B) {
	for _, size := range []int{1, 5, 20} {
		body := []byte(`{"model":"gpt-6","input":"` + strings.Repeat("x", size<<20) + `"}`)
		for _, mode := range []string{"before", "combined", "cached_four_checks"} {
			b.Run(fmt.Sprintf("%dMB/%s", size, mode), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				for b.Loop() {
					switch mode {
					case "before":
						for range 4 {
							jsonModelCandidates(body)
							jsonSessionModelCandidates(body)
						}
					case "combined":
						for range 4 {
							FromBodyCandidates("/v1/responses", "application/json", body)
						}
					default:
						req := httptest.NewRequest("POST", "/v1/responses", nil)
						for range 4 {
							FromRequestCandidates(req, "/v1/responses", body)
						}
					}
				}
			})
		}
	}
}

func TestRequestCacheReleasesBodiesForDetachedContexts(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	body := []byte(`{"model":"one"}`)
	FromRequestCandidates(req, "/v1/responses", body)
	require.True(t, ValidJSON(req, body))
	cache := bodyCache(req)
	require.NotEmpty(t, cache.body)
	Release(req)
	require.Nil(t, cache.body)
	require.Nil(t, cache.models)
	require.True(t, cache.released)
	require.Equal(t, []string{"one"}, FromRequestCandidates(req, "/v1/responses", body))
	require.True(t, ValidJSON(req, body))
	require.Nil(t, cache.body, "late detached work cannot pin the upload again")
}

func TestRequestFieldsShareStreamAnalysisAndInvalidateOnRewrite(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	for _, test := range []struct {
		body string
		want bool
	}{
		{`{"model":"gpt","stream":true}`, true},
		{`{"model":"gpt","stream":true,"Stream":false}`, false},
		{`{"model":"gpt","stream":true,"Stream":true}`, true},
		{`{"model":"gpt","\u0073tream":true}`, true},
		{`{"model":"gpt","stream":"true"}`, false},
		{`{"model":"gpt","stream":true`, false},
		{`{"model":"gpt"}`, false},
	} {
		body := []byte(test.body)
		FromRequestCandidates(req, "/v1/responses", body)
		require.Equal(t, test.want, RequestsStream(req, body), test.body)
		require.Equal(t, test.want, RequestsStream(nil, body), test.body)
	}
	Release(req)
	require.True(t, RequestsStream(req, []byte(`{"stream":true}`)))
	require.False(t, bodyCache(req).fieldsKnown)
}
