package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/joho/godotenv"
	"github.com/yudaprama/otelcallback"
)

//go:embed personas/*.yaml
var embeddedPersonas embed.FS

// parseGRPCEndpoint resolves the OTLP gRPC endpoint (bare host:port) that the
// OpenTelemetry exporters send to. planoctl injects OTEL_TRACING_GRPC_ENDPOINT
// (e.g. "http://localhost:4317"); the gRPC client rejects a URL scheme, so it
// is stripped. Defaults to localhost:4317 (Alloy's OTLP gRPC receiver).
func parseGRPCEndpoint() string {
	ep := os.Getenv("OTEL_TRACING_GRPC_ENDPOINT")
	if ep == "" {
		return "localhost:4317"
	}
	ep = strings.TrimPrefix(ep, "https://")
	ep = strings.TrimPrefix(ep, "http://")
	return ep
}

// OpenAI-compatible types
type ChatCompletionRequest struct {
	Model       string                  `json:"model"`
	Messages    []ChatCompletionMessage `json:"messages"`
	Stream      bool                    `json:"stream,omitempty"`
	Temperature float64                 `json:"temperature,omitempty"`
	MaxTokens   int                     `json:"max_tokens,omitempty"`
}

type ChatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
}

type ChatCompletionChoice struct {
	Index        int                   `json:"index"`
	Message      ChatCompletionMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

type ChatCompletionChunk struct {
	ID      string                      `json:"id"`
	Object  string                      `json:"object"`
	Created int64                       `json:"created"`
	Model   string                      `json:"model"`
	Choices []ChatCompletionChunkChoice `json:"choices"`
}

type ChatCompletionChunkChoice struct {
	Index        int                   `json:"index"`
	Delta        ChatCompletionMessage `json:"delta"`
	FinishReason *string               `json:"finish_reason"`
}

// crew holds the per-persona runners and the fallback used when the dispatch
// header is absent or names an unknown persona.
type crew struct {
	runners  map[string]*adk.Runner
	fallback *adk.Runner
}

// runnerFor resolves the runner for a request. Plano stamps x-arch-upstream
// with the selected agent id (= persona id); unknown/absent → default persona.
func (c *crew) runnerFor(r *http.Request) *adk.Runner {
	if id := r.Header.Get("x-arch-upstream"); id != "" {
		if runner, ok := c.runners[id]; ok {
			return runner
		}
		log.Printf("no persona for x-arch-upstream=%q, using default", id)
	}
	return c.fallback
}

// resolveWorkspace chdirs the process to $CREW_WORKSPACE when set, so the
// host shell tool (run_command) and the host fs tools (lobe-local-system)
// resolve relative paths against the project root. If unset, the inherited
// cwd is used. This is a convenience scope, NOT a security boundary — see
// shell.go's SECURITY note.
func resolveWorkspace() {
	ws := os.Getenv("CREW_WORKSPACE")
	if ws == "" {
		log.Printf("CREW_WORKSPACE unset; using cwd %q", mustCwd())
		return
	}
	if err := os.Chdir(ws); err != nil {
		log.Printf("WARNING: CREW_WORKSPACE=%q chdir failed (%v); staying in %q", ws, err, mustCwd())
		return
	}
	log.Printf("workspace cwd = %s", mustCwd())
}

func mustCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "(unknown)"
	}
	return wd
}

var (
	version    string // set via ldflags at build time
	agentCrew  *crew
	agentRunOpts []adk.AgentRunOption
)

func main() {
	if exe, err := os.Executable(); err == nil {
		godotenv.Load(filepath.Join(filepath.Dir(exe), "..", ".env"))
	}
	godotenv.Load()

	port := flag.String("port", "10550", "HTTP server port")
	flag.Parse()

	resolveWorkspace()

	ctx := context.Background()

	personas, err := loadPersonas("personas", embeddedPersonas)
	if err != nil {
		log.Fatalf("load personas: %v", err)
	}

	reg, err := buildRegistry(ctx, personas)
	if err != nil {
		log.Fatalf("build tool registry: %v", err)
	}

	chatModel, err := newChatModel(ctx)
	if err != nil {
		log.Fatalf("create chat model: %v", err)
	}

	def := defaultPersona(personas)
	agentCrew = &crew{runners: map[string]*adk.Runner{}}
	for _, p := range personas {
		ts := curate(reg, p)
		runner, err := newPersonaRunner(ctx, chatModel, p, ts)
		if err != nil {
			log.Fatalf("build runner for %s: %v", p.ID, err)
		}
		agentCrew.runners[p.ID] = runner
		if p.ID == def.ID {
			agentCrew.fallback = runner
		}
		log.Printf("persona %q ready (%d tools)", p.ID, len(ts))
	}

	otelHandler, otelShutdown, otelErr := otelcallback.NewHandler(ctx, &otelcallback.Config{
		ServiceName:    "egent-crew",
		ExportEndpoint: parseGRPCEndpoint(),
		EnableTracing:  true,
		EnableMetrics:  true,
		Insecure:       true,
		SampleRate:     1.0,
	})
	if otelErr != nil {
		log.Printf("opentelemetry: init failed (telemetry disabled): %v", otelErr)
	} else {
		defer otelShutdown(context.Background())
		if otelHandler != nil {
			agentRunOpts = append(agentRunOpts, adk.WithCallbacks(otelHandler))
		}
	}

	http.HandleFunc("/v1/chat/completions", chatCompletionsHandler)
	http.HandleFunc("/health", healthHandler)

	addr := "0.0.0.0:" + *port
	log.Printf("egent-crew starting on %s with %d personas", addr, len(agentCrew.runners))
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"personas": len(agentCrew.runners),
	})
}

func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, "messages cannot be empty", http.StatusBadRequest)
		return
	}

	runner := agentCrew.runnerFor(r)
	query := buildConversationQuery(req.Messages)

	ctx := contextWithForwardedHeaders(r.Context(), r.Header.Get(actorIDHeader))
	iter := runner.Query(ctx, query, agentRunOpts...)

	if req.Stream {
		handleStreamingResponse(w, req, iter)
	} else {
		handleNonStreamingResponse(w, req, iter)
	}
}

// buildConversationQuery formats the full conversation history so the agent
// has multi-turn context. When there is only a single user message, it is
// returned as-is for zero overhead.
func buildConversationQuery(messages []ChatCompletionMessage) string {
	if len(messages) == 1 {
		return messages[0].Content
	}

	var b strings.Builder
	for _, m := range messages {
		switch m.Role {
		case "system":
			// system prompt is already in the agent instruction
		case "user":
			fmt.Fprintf(&b, "User: %s\n", m.Content)
		case "assistant":
			fmt.Fprintf(&b, "Assistant: %s\n", m.Content)
		}
	}
	return b.String()
}

func handleNonStreamingResponse(w http.ResponseWriter, req ChatCompletionRequest, iter *adk.AsyncIterator[*adk.AgentEvent]) {
	var finalContent strings.Builder

	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			http.Error(w, fmt.Sprintf("agent error: %v", event.Err), http.StatusInternalServerError)
			return
		}
		if event.Output != nil && event.Output.MessageOutput != nil {
			msg, err := event.Output.MessageOutput.GetMessage()
			if err != nil {
				log.Printf("get message error: %v", err)
				continue
			}
			if msg.Role == schema.Assistant && msg.Content != "" {
				finalContent.WriteString(msg.Content)
			}
		}
	}

	resp := ChatCompletionResponse{
		ID:      generateID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []ChatCompletionChoice{
			{
				Index: 0,
				Message: ChatCompletionMessage{
					Role:    "assistant",
					Content: finalContent.String(),
				},
				FinishReason: "stop",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleStreamingResponse(w http.ResponseWriter, req ChatCompletionRequest, iter *adk.AsyncIterator[*adk.AgentEvent]) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	writer := bufio.NewWriter(w)
	requestID := generateID()

	for {
		event, ok := iter.Next()
		if !ok {
			finishReason := "stop"
			chunk := ChatCompletionChunk{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   req.Model,
				Choices: []ChatCompletionChunkChoice{
					{
						Index:        0,
						Delta:        ChatCompletionMessage{},
						FinishReason: &finishReason,
					},
				},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(writer, "data: %s\n\n", data)
			fmt.Fprintf(writer, "data: [DONE]\n\n")
			writer.Flush()
			flusher.Flush()
			break
		}

		if event.Err != nil {
			log.Printf("agent error: %v", event.Err)
			errChunk := ChatCompletionChunk{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   req.Model,
				Choices: []ChatCompletionChunkChoice{
					{
						Index: 0,
						Delta: ChatCompletionMessage{
							Role:    "assistant",
							Content: fmt.Sprintf("\n[Error: %v]", event.Err),
						},
					},
				},
			}
			errData, _ := json.Marshal(errChunk)
			fmt.Fprintf(writer, "data: %s\n\n", errData)
			writer.Flush()
			flusher.Flush()
			break
		}

		if event.Output != nil && event.Output.MessageOutput != nil {
			msg, err := event.Output.MessageOutput.GetMessage()
			if err != nil {
				log.Printf("get message error: %v", err)
				continue
			}
			if msg.Role == schema.Assistant && msg.Content != "" {
				chunk := ChatCompletionChunk{
					ID:      requestID,
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   req.Model,
					Choices: []ChatCompletionChunkChoice{
						{
							Index: 0,
							Delta: ChatCompletionMessage{
								Role:    "assistant",
								Content: msg.Content,
							},
							FinishReason: nil,
						},
					},
				}
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(writer, "data: %s\n\n", data)
				writer.Flush()
				flusher.Flush()
			}
		}
	}
}

func generateID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}
