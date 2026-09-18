package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestNativeImagesPreserveModelAndBypassResponsesTransport(t *testing.T) {
	for _, transport := range []string{"", responses.TransportWebSocket} {
		for _, model := range []string{"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
			t.Run(transport+"/"+model, func(t *testing.T) {
				token := testAccessTokenWithAccountID(t)
				outbound, err := NewOutboundTransformer(Params{BaseURL: "wss://chatgpt.com/backend-api/codex#", Transport: transport, TokenProvider: staticTokenGetter{creds: &oauth.OAuthCredentials{AccessToken: token}}})
				require.NoError(t, err)
				source := &llm.Request{Model: model, RequestType: llm.RequestTypeImage, APIFormat: llm.APIFormatOpenAIImageGeneration, Image: &llm.ImageRequest{Prompt: "a blue robot", Quality: "low", Size: "1024x1024", N: lo.ToPtr(int64(1))}}
				req, err := outbound.TransformRequest(t.Context(), source)
				require.NoError(t, err)
				require.Equal(t, "https://chatgpt.com/backend-api/codex/images/generations", req.URL)
				require.Equal(t, model, gjson.GetBytes(req.Body, "model").String())
				require.Equal(t, "a blue robot", gjson.GetBytes(req.Body, "prompt").String())
				require.Equal(t, "low", gjson.GetBytes(req.Body, "quality").String())
				require.False(t, gjson.GetBytes(req.Body, "tools").Exists())
				require.False(t, gjson.GetBytes(req.Body, "stream").Exists())
				require.Equal(t, "application/json", req.Headers.Get("Accept"))
				require.Equal(t, testChatAccountID, req.Headers.Get("Chatgpt-Account-Id"))
				require.Equal(t, token, req.Auth.APIKey)
				require.Equal(t, llm.APIFormatOpenAIImageGeneration.String(), req.APIFormat)
				require.Nil(t, source.Stream)
				mock := &mockCodexExecutor{}
				_, err = outbound.CustomizeExecutor(mock).Do(context.Background(), req)
				require.NoError(t, err)
				require.Equal(t, int32(1), mock.doCalls.Load())
				require.Zero(t, mock.doStreamCalls.Load())
				raw := &httpclient.Response{StatusCode: 200, Request: req, Body: []byte(`{"created":123,"data":[{"b64_json":"aW1hZ2U="}],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`)}
				result, err := outbound.TransformResponse(t.Context(), raw)
				require.NoError(t, err)
				require.Equal(t, model, result.Model)
				require.Equal(t, "aW1hZ2U=", result.Image.Data[0].B64JSON)
				require.Equal(t, int64(30), result.Usage.TotalTokens)
				raw.StatusCode = http.StatusBadRequest
				_, err = outbound.TransformResponse(t.Context(), raw)
				require.Error(t, err)
			})
		}
	}
}

func TestNativeImageEditUsesCodexJSON(t *testing.T) {
	outbound, err := NewOutboundTransformer(Params{TokenProvider: staticTokenGetter{creds: &oauth.OAuthCredentials{AccessToken: "test"}}})
	require.NoError(t, err)
	source := &llm.Request{Model: "gpt-image-2", RequestType: llm.RequestTypeImage, APIFormat: llm.APIFormatOpenAIImageEdit, Image: &llm.ImageRequest{Prompt: "make it green", Images: [][]byte{[]byte("image one"), []byte("image two")}, InputFidelity: "high", OutputFormat: "png"}}
	req, err := outbound.TransformRequest(t.Context(), source)
	require.NoError(t, err)
	require.Equal(t, "https://chatgpt.com/backend-api/codex/images/edits", req.URL)
	require.Equal(t, "application/json", req.Headers.Get("Content-Type"))
	require.Len(t, gjson.GetBytes(req.Body, "images").Array(), 2)
	require.Contains(t, gjson.GetBytes(req.Body, "images.0.image_url").String(), "base64,aW1hZ2Ugb25l")
	require.Equal(t, "high", gjson.GetBytes(req.Body, "input_fidelity").String())
	require.Equal(t, "png", gjson.GetBytes(req.Body, "output_format").String())
	require.False(t, gjson.GetBytes(req.Body, "image").Exists())
	require.False(t, outbound.AllowPassThroughBody(t.Context(), source, req))
	require.Nil(t, source.Stream)
	source.Image.Mask = []byte("mask")
	_, err = outbound.TransformRequest(t.Context(), source)
	require.ErrorContains(t, err, "masked image edits are not supported")
	source.Image.Mask = nil
	source.Image.Images = nil
	_, err = outbound.TransformRequest(t.Context(), source)
	require.Error(t, err)
}

func TestNativeImageExecutorPropagatesHTTPFailure(t *testing.T) {
	// Error conversion remains owned by the HTTP executor; no SSE aggregation
	// may turn an upstream failure into an empty successful Images response.
	outbound, err := NewOutboundTransformer(Params{TokenProvider: staticTokenGetter{creds: &oauth.OAuthCredentials{AccessToken: "test"}}})
	require.NoError(t, err)
	raw := &httpclient.Error{StatusCode: 429, Body: json.RawMessage(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`)}
	converted := outbound.TransformError(t.Context(), raw)
	require.NotNil(t, converted)
	require.Contains(t, converted.Error(), "rate limited")
}

func TestNativeImagesHTTPRoundTrip(t *testing.T) {
	for _, status := range []int{200, 400} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/images/generations", r.URL.Path)
				require.Equal(t, "application/json", r.Header.Get("Accept"))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status == 200 {
					_, _ = w.Write([]byte(`{"created":123,"data":[{"b64_json":"aW1hZ2U="}]}`))
				} else {
					_, _ = w.Write([]byte(`{"error":{"message":"unsupported image model","type":"invalid_request_error"}}`))
				}
			}))
			defer server.Close()
			outbound, err := NewOutboundTransformer(Params{BaseURL: "ws" + strings.TrimPrefix(server.URL, "http"), Transport: responses.TransportWebSocket, TokenProvider: staticTokenGetter{creds: &oauth.OAuthCredentials{AccessToken: "test"}}})
			require.NoError(t, err)
			outbound.official = true // Exercise official routing against a local fake HTTP upstream.
			request, err := outbound.TransformRequest(t.Context(), &llm.Request{Model: "gpt-image-2", RequestType: llm.RequestTypeImage, APIFormat: llm.APIFormatOpenAIImageGeneration, Image: &llm.ImageRequest{Prompt: "a robot"}})
			require.NoError(t, err)
			raw, err := outbound.CustomizeExecutor(httpclient.NewHttpClientWithClient(server.Client())).Do(t.Context(), request)
			if status == 400 {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			normalized, err := outbound.TransformResponse(t.Context(), raw)
			require.NoError(t, err)
			require.Equal(t, "aW1hZ2U=", normalized.Image.Data[0].B64JSON)
		})
	}
}
