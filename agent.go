package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino-ext/components/model/openai"
)

// Identity headers carried from the egent's incoming request to the outbound
// Plano model call. The auth edge injects x-arch-actor-id on the way in; the
// egent forwards it so brightstaff can stamp billing.actor_id on the LLM span.
//
// Outbound target is Plano's loopback-only internal ingress (:12010), which
// trusts callers by network position plus a static x-arch-internal-key header
// (PLANO_INTERNAL_KEY) — no per-hop Oathkeeper/Talos round-trip.
const (
	actorIDHeader     = "x-arch-actor-id"
	internalKeyHeader = "x-arch-internal-key"
)

type ctxActorIDKey struct{}

// contextWithForwardedHeaders stashes the incoming actor id so the model
// client's transport can re-apply it on the outbound :12010 call.
func contextWithForwardedHeaders(ctx context.Context, actorID string) context.Context {
	return context.WithValue(ctx, ctxActorIDKey{}, actorID)
}

// forwardingTransport stamps the internal static key and the propagated actor
// id onto the outbound Plano :12010 call.
type forwardingTransport struct {
	base        http.RoundTripper
	internalKey string
}

func (t *forwardingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.internalKey != "" {
		req.Header.Set(internalKeyHeader, t.internalKey)
	}
	if v, _ := req.Context().Value(ctxActorIDKey{}).(string); v != "" {
		req.Header.Set(actorIDHeader, v)
	}
	return t.base.RoundTrip(req)
}

// newChatModel builds the single shared OpenAI-compatible ChatModel. Resolves
// the gateway/key/model exactly like egent-public-apis: PLANO_LLM_GATEWAY wins
// (route through Plano :12010); otherwise fall back to the direct provider.
func newChatModel(ctx context.Context) (*openai.ChatModel, error) {
	baseURL := os.Getenv("PLANO_LLM_GATEWAY")
	apiKey := os.Getenv("MODEL_API_KEY")

	if baseURL == "" {
		baseURL = os.Getenv("MODEL_BASE_URL")
		if baseURL == "" {
			baseURL = "https://openrouter.ai/api/v1"
		}
		if apiKey == "" {
			apiKey = os.Getenv("OPENROUTER_API_KEY")
		}
	} else {
		if apiKey == "" {
			apiKey = "EMPTY"
		}
	}

	modelName := os.Getenv("MODEL_NAME")
	if modelName == "" {
		modelName = "kawai/kawai-pro-max"
	}

	chatModel, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelName,
		APIKey:  apiKey,
		// Stamp the internal static key + forward x-arch-actor-id onto the
		// Plano :12010 (internal ingress) call.
		HTTPClient: &http.Client{Transport: &forwardingTransport{
			base:        http.DefaultTransport.(*http.Transport).Clone(),
			internalKey: os.Getenv("PLANO_INTERNAL_KEY"),
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("create chat model: %w", err)
	}
	return chatModel, nil
}

// toBaseTools converts a curated []tool.InvokableTool to the []tool.BaseTool
// slice that compose.ToolsNodeConfig.Tools requires (Go slices are invariant,
// so the subtype slice is not directly assignable).
func toBaseTools(ts []tool.InvokableTool) []tool.BaseTool {
	out := make([]tool.BaseTool, len(ts))
	for i, t := range ts {
		out[i] = t
	}
	return out
}

// newPersonaRunner wires one persona's system prompt + curated tool subset
// into a fresh ChatModelAgent + Runner. The model is shared across personas;
// only the instruction and tools differ.
func newPersonaRunner(ctx context.Context, chatModel *openai.ChatModel, p Persona, ts []tool.InvokableTool) (*adk.Runner, error) {
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        p.ID,
		Description: "egent-crew persona: " + p.ID,
		Instruction: p.SystemPrompt,
		Model:       chatModel,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: toBaseTools(ts)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create agent for %s: %w", p.ID, err)
	}
	return adk.NewRunner(ctx, adk.RunnerConfig{
		Agent: agent,
		// Drive the model via Stream() — the Plano :12000 llm_gateway wasm
		// cannot parse non-streaming provider responses.
		EnableStreaming: true,
	}), nil
}
