package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/openai/agentcompat"
)

// AgentCompatibilityConfig is opt-in. Native producers require both model and channel;
// portable consumers must be identified before inbound protocol conversion.
// Empty lists leave all existing requests and responses untouched.
type AgentCompatibilityConfig struct {
	NativeModels     []string `conf:"native_models" yaml:"native_models" json:"native_models"`
	NativeChannelIDs []int    `conf:"native_channel_ids" yaml:"native_channel_ids" json:"native_channel_ids"`
	PortableModels   []string `conf:"portable_models" yaml:"portable_models" json:"portable_models"`
}

// Run after Responses session history assembly, before TransformRequest would
// discard private agent_message items. This supports Chat Completions channels too.
func preparePortableAgentRequest(req *httpclient.Request, cfg AgentCompatibilityConfig) (*httpclient.Request, bool, error) {
	if req == nil || len(cfg.PortableModels) == 0 || !slices.Contains(cfg.PortableModels, gjson.GetBytes(req.Body, "model").String()) {
		return req, false, nil
	}
	body, err := agentcompat.RewriteRequest(req.Body, true)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", transformer.ErrInvalidRequest, err)
	}
	copy := *req
	copy.Body = body
	copy.Headers = req.Headers.Clone()
	copy.Headers.Del("Content-Length")
	return &copy, true, nil
}

type portableAgentResponseMiddleware struct{ pipeline.DummyMiddleware }

func (m *portableAgentResponseMiddleware) Name() string { return "codex-v2-portable-agent-response" }
func (m *portableAgentResponseMiddleware) OnInboundRawResponse(_ context.Context, resp *httpclient.Response) (*httpclient.Response, error) {
	return restoreAgentResponse(resp, true)
}
func (m *portableAgentResponseMiddleware) OnInboundRawStream(_ context.Context, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*httpclient.StreamEvent], error) {
	return &agentCompatibilityStream{Stream: stream, portable: true}, nil
}

func (p *ChatCompletionOrchestrator) WithAgentCompatibility(cfg AgentCompatibilityConfig) *ChatCompletionOrchestrator {
	c := *p
	c.agentCompatibility = cfg
	return &c
}

type agentCompatibilityMiddleware struct {
	pipeline.DummyMiddleware
	outbound *PersistentOutboundTransformer
	config   AgentCompatibilityConfig
	active   bool
	native   bool // Set once from the inbound model, before any model mapping.
	portable bool // Set once from the inbound model; portable consumers skip native injection.
}

func (m *agentCompatibilityMiddleware) Name() string { return "codex-v2-agent-compatibility" }

func (m *agentCompatibilityMiddleware) prepareRequest(_ context.Context, req *httpclient.Request) (*httpclient.Request, error) {
	m.active = false // Re-evaluate the selected channel on every retry/failover.
	ch := m.outbound.GetCurrentChannel()
	if ch == nil || req == nil || req.APIFormat != string(llm.APIFormatOpenAIResponse) {
		return req, nil
	}
	if m.portable {
		// Pass-through replays the raw input after applyTransformOptions. Honor
		// this explicit channel option on the final portable Responses body too.
		if ch.Settings == nil || !ch.Settings.TransformOptions.ReplaceDeveloperRoleWithSystem {
			return req, nil
		}
		body := req.Body
		for i, item := range gjson.GetBytes(body, "input").Array() {
			if (item.Get("type").String() == "message" || !item.Get("type").Exists()) && item.Get("role").String() == "developer" {
				var err error
				body, err = sjson.SetBytes(body, fmt.Sprintf("input.%d.role", i), "system")
				if err != nil {
					return nil, err
				}
			}
		}
		copy := *req
		copy.Body = body
		copy.Headers = req.Headers.Clone()
		copy.Headers.Del("Content-Length")
		return &copy, nil
	}
	if !m.native || !slices.Contains(m.config.NativeChannelIDs, ch.ID) {
		return req, nil
	}
	// The full private tool schema and plaintext marker must survive both directions.
	if !m.outbound.state.PassThroughApplied {
		return nil, errors.New("Codex agent compatibility requires Responses body passthrough")
	}
	body, err := agentcompat.RewriteRequest(req.Body, false)
	if err != nil {
		return nil, err
	}
	req.Body = body
	req.Headers = req.Headers.Clone()
	req.Headers.Del("Content-Length")
	m.active = true
	return req, nil
}

func (m *agentCompatibilityMiddleware) OnOutboundRawResponse(_ context.Context, resp *httpclient.Response) (*httpclient.Response, error) {
	if !m.active {
		return resp, nil
	}
	return restoreAgentResponse(resp, false)
}

func restoreAgentResponse(resp *httpclient.Response, portable bool) (*httpclient.Response, error) {
	if resp == nil || resp.StatusCode >= 400 {
		return resp, nil
	}
	body, err := agentcompat.RestoreResponse(resp.Body, portable)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(body, resp.Body) {
		return resp, nil
	}
	copy := *resp
	copy.Body = body
	copy.Headers = resp.Headers.Clone()
	copy.Headers.Del("Content-Length")
	return &copy, nil
}

func (m *agentCompatibilityMiddleware) OnOutboundRawStream(_ context.Context, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*httpclient.StreamEvent], error) {
	if !m.active {
		return stream, nil
	}
	return &agentCompatibilityStream{Stream: stream}, nil
}

type agentCompatibilityStream struct {
	streams.Stream[*httpclient.StreamEvent]
	portable bool
	current  *httpclient.StreamEvent
	err      error
}

func (s *agentCompatibilityStream) Next() bool {
	if s.err != nil || !s.Stream.Next() {
		return false
	}
	event := s.Stream.Current()
	if event == nil || len(event.Data) == 0 {
		s.current = event
		return true
	}
	body, err := agentcompat.RestoreResponse(event.Data, s.portable)
	if err != nil {
		s.err = err
		return false
	}
	copy := *event
	copy.Data = body
	s.current = &copy
	return true
}
func (s *agentCompatibilityStream) Current() *httpclient.StreamEvent { return s.current }
func (s *agentCompatibilityStream) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.Stream.Err()
}
