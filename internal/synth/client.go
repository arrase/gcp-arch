package synth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"google.golang.org/genai"

	"github.com/arrase/gcp-arch/internal/extract"
)

// Default Vertex AI target settings and OAuth scope.
const (
	DefaultVertexLocation       = extract.DefaultVertexLocation
	DefaultVertexModel          = extract.DefaultVertexModel
	defaultVertexAttemptTimeout = 240 * time.Second
	defaultVertexRetryDelay     = 2 * time.Second
	probeVertexTimeout          = 30 * time.Second
	maxVertexAttempts           = 3
	cloudPlatformScope          = "https://www.googleapis.com/auth/cloud-platform"
)

// ParseThinkingLevel validates and normalizes a Gemini thinking level.
// An empty string returns ("", nil) so Vertex AI applies the model default.
func ParseThinkingLevel(raw string) (genai.ThinkingLevel, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", nil
	case "minimal":
		return genai.ThinkingLevelMinimal, nil
	case "low":
		return genai.ThinkingLevelLow, nil
	case "medium":
		return genai.ThinkingLevelMedium, nil
	case "high":
		return genai.ThinkingLevelHigh, nil
	case "xhigh":
		return genai.ThinkingLevelXhigh, nil
	default:
		return "", fmt.Errorf("invalid thinking level %q (allowed: minimal, low, medium, high, xhigh)", raw)
	}
}

// UsageStats captures Vertex AI token and latency telemetry for a synthesis call.
type UsageStats struct {
	Project         string `json:"project,omitempty"`
	Location        string `json:"location,omitempty"`
	Model           string `json:"model,omitempty"`
	DurationMs      int64  `json:"duration_ms"`
	PromptTokens    int32  `json:"prompt_tokens"`
	CandidateTokens int32  `json:"candidate_tokens"`
	ThoughtsTokens  int32  `json:"thoughts_tokens"`
	TotalTokens     int32  `json:"total_tokens"`
	ResponseBytes   int    `json:"response_bytes"`
}

// ModelClient abstracts structured JSON generation against Vertex AI for deterministic testing.
type ModelClient interface {
	GenerateJSON(ctx context.Context, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, error)
}

// UsageModelClient optionally exposes token and latency telemetry alongside GenerateJSON.
type UsageModelClient interface {
	ModelClient
	GenerateJSONWithUsage(ctx context.Context, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, UsageStats, error)
}

// VertexClientOptions configures a Vertex AI Gemini client backed by google.golang.org/genai.
type VertexClientOptions struct {
	Project        string
	Location       string
	Model          string
	ThinkingLevel  string
	AttemptTimeout time.Duration
	RetryBaseDelay time.Duration
	HTTPClient     *http.Client
	BaseURL        string
	Credentials    *auth.Credentials
}

// VertexClient executes structured JSON generation against Vertex AI using google.golang.org/genai.
type VertexClient struct {
	client         *genai.Client
	project        string
	location       string
	model          string
	thinkingLevel  genai.ThinkingLevel
	attemptTimeout time.Duration
	retryBaseDelay time.Duration
}

type vertexTarget struct {
	project  string
	location string
	model    string
}

func newGenAIClient(ctx context.Context, opts VertexClientOptions) (*genai.Client, vertexTarget, error) {
	project := strings.TrimSpace(opts.Project)
	if project == "" {
		return nil, vertexTarget{}, fmt.Errorf("vertex project is required")
	}
	location := strings.TrimSpace(opts.Location)
	if location == "" {
		location = DefaultVertexLocation
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = DefaultVertexModel
	}

	creds := opts.Credentials
	if creds == nil && opts.HTTPClient == nil {
		var err error
		creds, err = credentials.DetectDefault(&credentials.DetectOptions{
			Scopes: []string{cloudPlatformScope},
		})
		if err != nil {
			return nil, vertexTarget{}, fmt.Errorf("load Vertex AI credentials (%s): %w", cloudPlatformScope, err)
		}
	}

	cfg := &genai.ClientConfig{
		Backend:     genai.BackendVertexAI,
		Project:     project,
		Location:    location,
		Credentials: creds,
		HTTPClient:  opts.HTTPClient,
	}
	if baseURL := strings.TrimSpace(opts.BaseURL); baseURL != "" {
		cfg.HTTPOptions = genai.HTTPOptions{BaseURL: baseURL}
	}

	client, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return nil, vertexTarget{}, fmt.Errorf("create Vertex AI genai client: %w", err)
	}
	return client, vertexTarget{project: project, location: location, model: model}, nil
}

// NewVertexClient initializes a Vertex AI Gemini client authenticated with cloud-platform scope.
func NewVertexClient(ctx context.Context, opts VertexClientOptions) (*VertexClient, error) {
	level, err := ParseThinkingLevel(opts.ThinkingLevel)
	if err != nil {
		return nil, err
	}
	client, target, err := newGenAIClient(ctx, opts)
	if err != nil {
		return nil, err
	}

	attemptTimeout := opts.AttemptTimeout
	if attemptTimeout <= 0 {
		attemptTimeout = defaultVertexAttemptTimeout
	}
	retryDelay := opts.RetryBaseDelay
	if retryDelay <= 0 {
		retryDelay = defaultVertexRetryDelay
	}

	return &VertexClient{
		client:         client,
		project:        target.project,
		location:       target.location,
		model:          target.model,
		thinkingLevel:  level,
		attemptTimeout: attemptTimeout,
		retryBaseDelay: retryDelay,
	}, nil
}

// GenerateJSON executes a low-temperature (0.1) structured JSON request conforming to schema.
func (c *VertexClient) GenerateJSON(ctx context.Context, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, error) {
	raw, _, err := c.GenerateJSONWithUsage(ctx, systemPrompt, userPrompt, schema)
	return raw, err
}

// GenerateJSONWithUsage executes a low-temperature (0.1) structured JSON request and returns token/latency stats.
func (c *VertexClient) GenerateJSONWithUsage(ctx context.Context, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, UsageStats, error) {
	temp := float32(0.1)
	cfg := &genai.GenerateContentConfig{
		Temperature:      &temp,
		ResponseMIMEType: "application/json",
		ResponseSchema:   schema,
	}
	if systemPrompt != "" {
		cfg.SystemInstruction = genai.NewContentFromText(systemPrompt, genai.RoleUser)
	}
	if c.thinkingLevel != "" {
		cfg.ThinkingConfig = &genai.ThinkingConfig{
			ThinkingLevel: c.thinkingLevel,
		}
	}

	contents := genai.Text(userPrompt)
	start := time.Now()

	var (
		resp *genai.GenerateContentResponse
		err  error
	)
	for attempt := range maxVertexAttempts {
		resp, err = c.doGenerateAttempt(ctx, contents, cfg)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, UsageStats{}, fmt.Errorf("vertex generate content (%s): %w", c.model, err)
		}
		if attempt < maxVertexAttempts-1 && isRetryableVertexErr(err) {
			backoff := c.retryBaseDelay << attempt
			select {
			case <-ctx.Done():
				return nil, UsageStats{}, fmt.Errorf("vertex generate content (%s): %w", c.model, ctx.Err())
			case <-time.After(backoff):
				continue
			}
		}
		return nil, UsageStats{}, fmt.Errorf("vertex generate content (%s): %w", c.model, err)
	}
	durMs := time.Since(start).Milliseconds()

	text := strings.TrimSpace(resp.Text())
	if text == "" {
		return nil, UsageStats{}, fmt.Errorf("vertex generate content (%s): empty JSON response", c.model)
	}

	stats := UsageStats{
		Project:       c.project,
		Location:      c.location,
		Model:         c.model,
		DurationMs:    durMs,
		ResponseBytes: len(text),
	}
	if resp.UsageMetadata != nil {
		stats.PromptTokens = resp.UsageMetadata.PromptTokenCount
		stats.CandidateTokens = resp.UsageMetadata.CandidatesTokenCount
		stats.ThoughtsTokens = resp.UsageMetadata.ThoughtsTokenCount
		stats.TotalTokens = resp.UsageMetadata.TotalTokenCount
	}
	return []byte(text), stats, nil
}

func (c *VertexClient) doGenerateAttempt(ctx context.Context, contents []*genai.Content, cfg *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, c.attemptTimeout)
	defer cancel()
	return c.client.Models.GenerateContent(attemptCtx, c.model, contents, cfg)
}

func isRetryableVertexErr(err error) bool {
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusTooManyRequests ||
			apiErr.Code == http.StatusServiceUnavailable ||
			apiErr.Code == http.StatusGatewayTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// ProbeVertexModel performs a zero-token Vertex AI model availability check via genai Models.Get
// suitable for plugging directly into extract.VertexPreflightOptions.ModelProbe.
func ProbeVertexModel(ctx context.Context, project, location, model string) error {
	return probeVertexModelWithOptions(ctx, VertexClientOptions{
		Project:  project,
		Location: location,
		Model:    model,
	})
}

func probeVertexModelWithOptions(ctx context.Context, opts VertexClientOptions) error {
	client, target, err := newGenAIClient(ctx, opts)
	if err != nil {
		return err
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeVertexTimeout)
	defer cancel()

	if _, err := client.Models.Get(probeCtx, target.model, nil); err != nil {
		return fmt.Errorf("probe vertex model %q: %w", target.model, err)
	}
	return nil
}
