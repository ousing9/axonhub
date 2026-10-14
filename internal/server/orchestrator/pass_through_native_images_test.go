package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
)

type passThroughTestTokenGetter struct{}

func (passThroughTestTokenGetter) Get(context.Context) (*oauth.OAuthCredentials, error) {
	return &oauth.OAuthCredentials{AccessToken: "test-token"}, nil
}

// Exercise real Codex and Images transformers with the production pass-through
// middleware. No database, account credentials, or upstream requests are needed.
func TestCodexImagePassThroughRoundTrip(t *testing.T) {
	codexOutbound, err := codex.NewOutboundTransformer(codex.Params{
		BaseURL:       "https://relay.example/codex#",
		TokenProvider: passThroughTestTokenGetter{},
	})
	require.NoError(t, err)
	channel := &biz.Channel{Channel: &ent.Channel{
		ID: 1,
		Settings: &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
		},
	}}

	for _, model := range []string{"gpt-image-2", "gpt-image-2.5-sunburst", "gpt-image-2.5-flare"} {
		for _, tt := range []struct {
			name    string
			inbound *openai.ImageInboundTransformer
			body    string
			action  string
		}{
			{
				name:    "generation",
				inbound: openai.NewImageGenerationInboundTransformer(),
				body:    `{"model":"gpt-image-2","prompt":"a blue robot","output_format":"png"}`,
				action:  "generate",
			},
			{
				name:    "json edit",
				inbound: openai.NewImageEditInboundTransformer(),
				body:    `{"model":"gpt-image-2","prompt":"make it blue","image":"data:image/png;base64,aW1hZ2U=","output_format":"png"}`,
				action:  "edit",
			},
		} {
			t.Run(model+"/"+tt.name, func(t *testing.T) {
				ctx := t.Context()
				quality := "high"
				if model == "gpt-image-2.5-sunburst" {
					quality = "xhigh"
				} else if model == "gpt-image-2.5-flare" {
					quality = "max"
				}
				body := strings.ReplaceAll(tt.body, "gpt-image-2", model)
				body = strings.TrimSuffix(body, "}") + fmt.Sprintf(`,"quality":%q}`, quality)
				llmReq, err := tt.inbound.TransformRequest(ctx, &httpclient.Request{
					Headers: http.Header{"Content-Type": {"application/json"}},
					Body:    []byte(body),
				})
				require.NoError(t, err)
				request, err := codexOutbound.TransformRequest(ctx, llmReq)
				require.NoError(t, err)
				state := &PersistenceState{
					CurrentCandidate:      &ChannelModelsCandidate{Channel: channel},
					LlmRequest:            llmReq,
					OriginalRequestStream: llmReq.Stream,
				}
				outbound := &PersistentOutboundTransformer{wrapped: codexOutbound, state: state}
				request, err = applyPassThroughRequestBody(outbound, nil).OnOutboundRawRequest(ctx, request)
				require.NoError(t, err)
				assert.False(t, state.PassThroughApplied)
				assert.Equal(t, "gpt-6-luna", gjson.GetBytes(request.Body, "model").String())
				assert.Equal(t, "image_generation", gjson.GetBytes(request.Body, "tools.0.type").String())
				assert.Equal(t, model, gjson.GetBytes(request.Body, "tools.0.model").String())
				assert.Equal(t, quality, gjson.GetBytes(request.Body, "tools.0.quality").String())
				assert.Equal(t, tt.action, gjson.GetBytes(request.Body, "tools.0.action").String())
				assert.True(t, gjson.GetBytes(request.Body, "stream").Bool())

				upstream := &httpclient.Response{
					StatusCode: http.StatusOK,
					Request:    request,
					Body:       []byte(`{"id":"resp_image","object":"response","created_at":1760000000,"status":"completed","model":"gpt-6-luna","output":[{"type":"image_generation_call","status":"completed","result":"aW1hZ2U="}]}`),
				}
				_, err = captureRawProviderResponse(outbound, nil).OnOutboundRawResponse(ctx, upstream)
				require.NoError(t, err)
				assert.Nil(t, state.RawProviderResponse)
				llmResp, err := codexOutbound.TransformResponse(ctx, upstream)
				require.NoError(t, err)
				clientResp, err := tt.inbound.TransformResponse(ctx, llmResp)
				require.NoError(t, err)
				clientResp, err = applyPassThroughResponse(outbound, nil).OnInboundRawResponse(ctx, clientResp)
				require.NoError(t, err)
				assert.Equal(t, "aW1hZ2U=", gjson.GetBytes(clientResp.Body, "data.0.b64_json").String())
				assert.False(t, gjson.GetBytes(clientResp.Body, "output").Exists())

				// A streaming image request must also retain the converted Images events.
				llmReq.Stream = lo.ToPtr(true)
				state.OriginalRequestStream = lo.ToPtr(true)
				rawStream := testHTTPStream([]*httpclient.StreamEvent{{Type: "response.completed", Data: upstream.Body}})
				captured, err := captureRawProviderStream(outbound, nil).OnOutboundRawStream(ctx, rawStream)
				require.NoError(t, err)
				require.NoError(t, captured.Close())
				assert.Nil(t, state.RawStreamCh)
				converted := testHTTPStream([]*httpclient.StreamEvent{{Type: "image_generation.completed", Data: clientResp.Body}})
				result, err := applyPassThroughStream(outbound, nil).OnInboundRawStream(ctx, converted)
				require.NoError(t, err)
				require.True(t, result.Next())
				assert.Equal(t, "image_generation.completed", result.Current().Type)
				require.NoError(t, result.Close())
			})
		}
	}
}

func TestNativeCodexImagesPassThroughRoundTrip(t *testing.T) {
	outbound, err := codex.NewOutboundTransformer(codex.Params{TokenProvider: passThroughTestTokenGetter{}})
	require.NoError(t, err)
	for _, enabled := range []bool{false, true} {
		for _, tt := range []struct {
			name, body string
			inbound    *openai.ImageInboundTransformer
			edit       bool
		}{
			{"generation", `{"model":"gpt-image-2.5-flare","prompt":"a blue robot","quality":"low","custom_field":"keep"}`, openai.NewImageGenerationInboundTransformer(), false},
			{"reference generation", `{"model":"gpt-image-2","prompt":"make it green","image":"data:image/png;base64,aW1hZ2U="}`, openai.NewImageGenerationInboundTransformer(), true},
			{"public edit", `{"model":"gpt-image-2","prompt":"make it green","image":"data:image/png;base64,aW1hZ2U="}`, openai.NewImageEditInboundTransformer(), true},
			{"multipart edit", "", openai.NewImageEditInboundTransformer(), true},
			{"codex edit", `{"model":"gpt-image-2","prompt":"make it green","images":[{"image_url":"data:image/png;base64,aW1hZ2U="}]}`, openai.NewImageEditInboundTransformer(), true},
		} {
			t.Run(fmt.Sprintf("%t/%s", enabled, tt.name), func(t *testing.T) {
				ctx := t.Context()
				rawRequest := &httpclient.Request{Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(tt.body)}
				if tt.name == "multipart edit" {
					var buffer bytes.Buffer
					writer := multipart.NewWriter(&buffer)
					require.NoError(t, writer.WriteField("model", "gpt-image-2"))
					require.NoError(t, writer.WriteField("prompt", "make it green"))
					file, err := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="image"; filename="image.png"`}, "Content-Type": {"image/png"}})
					require.NoError(t, err)
					_, err = file.Write([]byte("\x89PNG\r\n\x1a\nimage"))
					require.NoError(t, err)
					require.NoError(t, writer.Close())
					rawRequest.Body = buffer.Bytes()
					rawRequest.Headers.Set("Content-Type", writer.FormDataContentType())
				}
				req, err := tt.inbound.TransformRequest(ctx, rawRequest)
				require.NoError(t, err)
				provider, err := outbound.TransformRequest(ctx, req)
				require.NoError(t, err)
				state := &PersistenceState{CurrentCandidate: &ChannelModelsCandidate{Channel: &biz.Channel{Channel: &ent.Channel{ID: 1, Settings: &objects.ChannelSettings{PassThroughBody: lo.ToPtr(enabled)}}}}, LlmRequest: req, OriginalRequestStream: req.Stream}
				wrapped := &PersistentOutboundTransformer{wrapped: outbound, state: state}
				provider, err = applyPassThroughRequestBody(wrapped, nil).OnOutboundRawRequest(ctx, provider)
				require.NoError(t, err)
				require.Equal(t, enabled && !tt.edit, state.PassThroughApplied)
				require.Equal(t, req.Model, gjson.GetBytes(provider.Body, "model").String())
				require.False(t, gjson.GetBytes(provider.Body, "tools").Exists())
				if tt.edit {
					require.True(t, gjson.GetBytes(provider.Body, "images.0.image_url").Exists())
					require.False(t, gjson.GetBytes(provider.Body, "image").Exists())
					require.Equal(t, "application/json", provider.Headers.Get("Content-Type"))
				} else if enabled {
					require.Equal(t, "keep", gjson.GetBytes(provider.Body, "custom_field").String())
				}
				raw := &httpclient.Response{StatusCode: 200, Request: provider, Body: []byte(`{"created":123,"data":[{"b64_json":"aW1hZ2U=","generation_id":"gen-test"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)}
				_, err = captureRawProviderResponse(wrapped, nil).OnOutboundRawResponse(ctx, raw)
				require.NoError(t, err)
				normalized, err := outbound.TransformResponse(ctx, raw)
				require.NoError(t, err)
				require.Equal(t, int64(3), normalized.Usage.TotalTokens)
				result, err := tt.inbound.TransformResponse(ctx, normalized)
				require.NoError(t, err)
				result, err = applyPassThroughResponse(wrapped, nil).OnInboundRawResponse(ctx, result)
				require.NoError(t, err)
				require.Equal(t, "aW1hZ2U=", gjson.GetBytes(result.Body, "data.0.b64_json").String())
				if enabled && tt.name != "reference generation" {
					require.Equal(t, "gen-test", gjson.GetBytes(result.Body, "data.0.generation_id").String())
				}
			})
		}
	}
}

func TestCodexResponsesPassThroughRemainsEnabled(t *testing.T) {
	ctx := t.Context()
	codexOutbound, err := codex.NewOutboundTransformer(codex.Params{
		BaseURL:       "https://chatgpt.com/backend-api/codex#",
		TokenProvider: passThroughTestTokenGetter{},
	})
	require.NoError(t, err)
	llmReq := &llm.Request{
		Model:     "gpt-6-luna",
		APIFormat: llm.APIFormatOpenAIResponse,
		Stream:    lo.ToPtr(true),
		Messages:  []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("hi")}}},
		RawRequest: &httpclient.Request{
			Headers: http.Header{"Content-Type": {"application/json"}},
			Body:    []byte(`{"model":"gpt-6-luna","input":[{"role":"user","content":"hi"}],"stream":true,"store":false,"custom_client_field":{"keep":true}}`),
		},
	}
	request, err := codexOutbound.TransformRequest(ctx, llmReq)
	require.NoError(t, err)
	outbound := &PersistentOutboundTransformer{wrapped: codexOutbound, state: &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: &biz.Channel{Channel: &ent.Channel{
			ID:       1,
			Settings: &objects.ChannelSettings{PassThroughBody: lo.ToPtr(true)},
		}}},
		LlmRequest:            llmReq,
		OriginalRequestStream: llmReq.Stream,
	}}
	processed, err := applyPassThroughRequestBody(outbound, nil).OnOutboundRawRequest(ctx, request)
	require.NoError(t, err)
	require.True(t, outbound.state.PassThroughApplied)
	assert.JSONEq(t, string(llmReq.RawRequest.Body), string(processed.Body))
	assert.True(t, outbound.isPassThroughEnabled(ctx, nil), "Responses response/stream pass-through stays enabled")
}
