package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/openai/agentcompat"
)

// AgentCompatibilityConfig enables task delivery to the configured portable models.
// All Responses passthrough producers use the same stable tool catalog, independent
// of channel IDs. An empty model list leaves requests and responses untouched.
type AgentCompatibilityConfig struct {
	PortableModels []string `conf:"portable_models" yaml:"portable_models" json:"portable_models"`
}

// Run after Responses session history assembly, before TransformRequest would
// discard private agent_message items. This supports Chat Completions channels too.
func preparePortableAgentRequest(req *httpclient.Request, cfg AgentCompatibilityConfig) (*httpclient.Request, bool, error) {
	if req == nil || len(cfg.PortableModels) == 0 || !slices.Contains(cfg.PortableModels, gjson.GetBytes(req.Body, "model").String()) {
		return req, false, nil
	}
	body, err := agentcompat.RewriteRequest(req.Body, true, cfg.PortableModels)
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
	return restoreAgentResponse(resp, true, nil)
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
	// The full private tool schema and plaintext marker must survive both directions.
	// Other protocol paths retain their existing behavior, without new requirements.
	if len(m.config.PortableModels) == 0 || !m.outbound.state.PassThroughApplied {
		return req, nil
	}
	body, err := agentcompat.RewriteRequest(req.Body, false, m.config.PortableModels)
	if err != nil {
		return nil, err
	}
	m.active = !bytes.Equal(body, req.Body) || gjson.GetBytes(req.Body, "previous_response_id").String() != ""
	if bytes.Equal(body, req.Body) {
		return req, nil
	}
	copy := *req
	copy.Body = body
	copy.Headers = req.Headers.Clone()
	copy.Headers.Del("Content-Length")
	return &copy, nil
}

func (m *agentCompatibilityMiddleware) OnOutboundRawResponse(_ context.Context, resp *httpclient.Response) (*httpclient.Response, error) {
	if !m.active {
		return resp, nil
	}
	return restoreAgentResponse(resp, false, m.config.PortableModels)
}

func restoreAgentResponse(resp *httpclient.Response, portable bool, portableModels []string) (*httpclient.Response, error) {
	if resp == nil || resp.StatusCode >= 400 {
		return resp, nil
	}
	body, err := agentcompat.RestoreResponse(resp.Body, portable, portableModels)
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
	return &agentCompatibilityStream{Stream: stream, portableModels: m.config.PortableModels}, nil
}

// agentCompatibilityStream restores portable tool calls and supplies the phase
// metadata DeepSeek omits. Portable assistant messages start as commentary so
// their text can stream immediately; only the message's done event is held
// until a following tool call or terminal response identifies its final phase.
type agentCompatibilityStream struct {
	streams.Stream[*httpclient.StreamEvent]
	portable       bool
	portableModels []string
	current        *httpclient.StreamEvent
	err            error
	sourceDone     bool
	pending        *portableMessageBuffer
	queued         []*httpclient.StreamEvent
}

type portableMessageBuffer struct {
	// Only the message done event is buffered. Added and text delta events are
	// already visible to the client with a provisional commentary phase.
	events    []*httpclient.StreamEvent
	itemID    string
	donePhase string
}

func (s *agentCompatibilityStream) Next() bool {
	for len(s.queued) == 0 && !s.sourceDone && s.err == nil {
		if !s.Stream.Next() {
			s.sourceDone = true
			if s.pending != nil {
				s.flushPending("final_answer")
			}
			break
		}
		raw := s.Stream.Current()
		event, err := s.restoreEvent(raw)
		if err != nil {
			if s.pending != nil {
				s.flushPending("final_answer")
			}
			s.err = err
			break
		}
		s.consume(event)
	}
	if len(s.queued) == 0 {
		return false
	}
	s.current = s.queued[0]
	s.queued = s.queued[1:]
	return true
}

func (s *agentCompatibilityStream) restoreEvent(event *httpclient.StreamEvent) (*httpclient.StreamEvent, error) {
	if event == nil || len(event.Data) == 0 {
		return event, nil
	}
	body, err := agentcompat.RestoreResponse(event.Data, s.portable, s.portableModels)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(body, event.Data) {
		return event, nil
	}
	copy := *event
	copy.Data = body
	return &copy, nil
}

func (s *agentCompatibilityStream) consume(event *httpclient.StreamEvent) {
	if !s.portable || event == nil || len(event.Data) == 0 {
		s.queued = append(s.queued, event)
		return
	}
	data := event.Data
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		if s.pending != nil {
			s.flushPending("final_answer")
		}
		s.queued = append(s.queued, event)
		return
	}
	typ := gjson.GetBytes(data, "type").String()
	if s.pending == nil {
		if typ == "response.output_item.added" && portableAssistantMessageWithoutPhase(data) {
			s.startPortableMessage(event)
			return
		}
		s.queued = append(s.queued, event)
		return
	}
	if typ == "response.output_item.added" && portableToolItem(data) {
		s.flushPending("commentary")
		s.queued = append(s.queued, event)
		return
	}
	if typ == "response.completed" || typ == "response.failed" || typ == "response.incomplete" || typ == "error" {
		s.flushPending("final_answer")
		s.queued = append(s.queued, event)
		return
	}
	if typ == "response.output_item.done" {
		if portableMessageItem(data) && samePortableItem(s.pending.itemID, gjson.GetBytes(data, "item.id").String()) {
			s.pending.events = append(s.pending.events, event)
			s.pending.donePhase = portablePhase(data)
			return
		}
		if portableToolItem(data) {
			s.flushPending("commentary")
			s.queued = append(s.queued, event)
			return
		}
	}
	if typ == "response.output_text.delta" || typ == "response.output_text.done" {
		if samePortableItem(s.pending.itemID, gjson.GetBytes(data, "item_id").String()) {
			s.queued = append(s.queued, event)
			return
		}
	}
	// A new output item closes an otherwise well-formed message. Keep its
	// explicit done phase when available; otherwise let the terminal event
	// decide below.
	if typ == "response.output_item.added" {
		s.flushPending("final_answer")
		if portableAssistantMessageWithoutPhase(data) {
			s.startPortableMessage(event)
			return
		}
	}
	s.queued = append(s.queued, event)
}

func (s *agentCompatibilityStream) startPortableMessage(event *httpclient.StreamEvent) {
	if event == nil {
		return
	}
	updated, err := s.setMessagePhase(event, "commentary")
	if err == nil {
		event = updated
	}
	s.pending = &portableMessageBuffer{itemID: gjson.GetBytes(event.Data, "item.id").String()}
	s.queued = append(s.queued, event)
}

func (s *agentCompatibilityStream) flushPending(defaultPhase string) {
	if s.pending == nil {
		return
	}
	phase := defaultPhase
	if s.pending.donePhase != "" {
		phase = s.pending.donePhase
	}
	for _, event := range s.pending.events {
		if event == nil {
			continue
		}
		if portablePhase(event.Data) == "" {
			if updated, err := s.setMessagePhase(event, phase); err == nil {
				event = updated
			}
		}
		s.queued = append(s.queued, event)
	}
	s.pending = nil
}

func (s *agentCompatibilityStream) setMessagePhase(event *httpclient.StreamEvent, phase string) (*httpclient.StreamEvent, error) {
	body, err := sjson.SetBytes(event.Data, "item.phase", phase)
	if err != nil {
		return nil, err
	}
	copy := *event
	copy.Data = body
	return &copy, nil
}

func portableMessageItem(data []byte) bool {
	item := gjson.GetBytes(data, "item")
	return item.Get("type").String() == "message" && item.Get("role").String() == "assistant"
}

func portableAssistantMessageWithoutPhase(data []byte) bool {
	if !portableMessageItem(data) {
		return false
	}
	phase := gjson.GetBytes(data, "item.phase")
	return !phase.Exists() || phase.Type == gjson.Null
}

func portablePhase(data []byte) string {
	phase := gjson.GetBytes(data, "item.phase")
	if !phase.Exists() || phase.Type == gjson.Null {
		return ""
	}
	return phase.String()
}

func portableToolItem(data []byte) bool {
	typ := gjson.GetBytes(data, "item.type").String()
	return typ != "" && (strings.HasSuffix(typ, "_call") || typ == "function_call")
}

func samePortableItem(expected, actual string) bool {
	return expected == "" || actual == "" || expected == actual
}

func (s *agentCompatibilityStream) Current() *httpclient.StreamEvent { return s.current }
func (s *agentCompatibilityStream) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.Stream.Err()
}
