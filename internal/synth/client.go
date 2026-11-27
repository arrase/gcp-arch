package synth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/auth/httptransport"

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
	maxVertexResponseBytes      = 8 * 1024 * 1024
	maxVertexErrorBodyBytes     = 4096
	cloudPlatformScope          = "https://www.googleapis.com/auth/cloud-platform"
)

// Schema defines a Vertex AI JSON responseSchema subset.
type Schema struct {
	Type        string             `json:"type"`
	Description string             `json:"description,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Enum        []string           `json:"enum,omitempty"`
	Nullable    bool               `json:"nullable,omitempty"`
}

// ThinkingLevel controls Gemini reasoning budget on Vertex AI.
type ThinkingLevel string

// Supported ThinkingLevel values.
const (
	ThinkingLevelMinimal ThinkingLevel = "MINIMAL"
	ThinkingLevelLow     ThinkingLevel = "LOW"
	ThinkingLevelMedium  ThinkingLevel = "MEDIUM"
	ThinkingLevelHigh    ThinkingLevel = "HIGH"
	ThinkingLevelXHigh   ThinkingLevel = "XHIGH"
)

// ParseThinkingLevel validates and normalizes a Gemini thinking level.
// An empty string returns ("", nil) so Vertex AI applies the model default.
func ParseThinkingLevel(raw string) (ThinkingLevel, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", nil
	case "minimal":
		return ThinkingLevelMinimal, nil
	case "low":
		return ThinkingLevelLow, nil
	case "medium":
		return ThinkingLevelMedium, nil
	case "high":
		return ThinkingLevelHigh, nil
	case "xhigh":
		return ThinkingLevelXHigh, nil
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
	GenerateJSON(ctx context.Context, systemPrompt, userPrompt string, schema *Schema) ([]byte, error)
}

// UsageModelClient optionally exposes token and latency telemetry alongside GenerateJSON.
type UsageModelClient interface {
	ModelClient
	GenerateJSONWithUsage(ctx context.Context, systemPrompt, userPrompt string, schema *Schema) ([]byte, UsageStats, error)
}

// VertexClientOptions configures a direct Vertex AI REST Gemini client.
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

// VertexClient executes structured JSON generation directly against the Vertex AI REST API.
type VertexClient struct {
	httpClient     *http.Client
	baseURL        string
	project        string
	location       string
	model          string
	thinkingLevel  ThinkingLevel
	attemptTimeout time.Duration
	retryBaseDelay time.Duration
}

type vertexConfig struct {
	httpClient     *http.Client
	baseURL        string
	project        string
	location       string
	model          string
	thinkingLevel  ThinkingLevel
	attemptTimeout time.Duration
	retryBaseDelay time.Duration
}

func resolveVertexConfig(opts VertexClientOptions) (vertexConfig, error) {
	project := strings.TrimSpace(opts.Project)
	if project == "" {
		return vertexConfig{}, fmt.Errorf("vertex project is required")
	}
	level, err := ParseThinkingLevel(opts.ThinkingLevel)
	if err != nil {
		return vertexConfig{}, err
	}
	location := strings.TrimSpace(opts.Location)
	if location == "" {
		location = DefaultVertexLocation
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = DefaultVertexModel
	}
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		if location == "global" {
			baseURL = "https://aiplatform.googleapis.com"
		} else {
			baseURL = "https://" + location + "-aiplatform.googleapis.com"
		}
	}
	attemptTimeout := opts.AttemptTimeout
	if attemptTimeout <= 0 {
		attemptTimeout = defaultVertexAttemptTimeout
	}
	retryDelay := opts.RetryBaseDelay
	if retryDelay <= 0 {
		retryDelay = defaultVertexRetryDelay
	}

	hc := opts.HTTPClient
	if hc == nil {
		creds := opts.Credentials
		if creds == nil {
			creds, err = credentials.DetectDefault(&credentials.DetectOptions{
				Scopes: []string{cloudPlatformScope},
			})
			if err != nil {
				return vertexConfig{}, fmt.Errorf("load Vertex AI credentials (%s): %w", cloudPlatformScope, err)
			}
		}
		hc, err = httptransport.NewClient(&httptransport.Options{
			Credentials: creds,
		})
		if err != nil {
			return vertexConfig{}, fmt.Errorf("create Vertex AI HTTP client: %w", err)
		}
	}

	return vertexConfig{
		httpClient:     hc,
		baseURL:        baseURL,
		project:        project,
		location:       location,
		model:          model,
		thinkingLevel:  level,
		attemptTimeout: attemptTimeout,
		retryBaseDelay: retryDelay,
	}, nil
}

func vertexModelURL(baseURL, project, location, model string) string {
	return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s",
		baseURL,
		url.PathEscape(project),
		url.PathEscape(location),
		url.PathEscape(model),
	)
}

// NewVertexClient initializes a Vertex AI REST Gemini client authenticated with cloud-platform scope.
func NewVertexClient(_ context.Context, opts VertexClientOptions) (*VertexClient, error) {
	cfg, err := resolveVertexConfig(opts)
	if err != nil {
		return nil, err
	}
	return &VertexClient{
		httpClient:     cfg.httpClient,
		baseURL:        cfg.baseURL,
		project:        cfg.project,
		location:       cfg.location,
		model:          cfg.model,
		thinkingLevel:  cfg.thinkingLevel,
		attemptTimeout: cfg.attemptTimeout,
		retryBaseDelay: cfg.retryBaseDelay,
	}, nil
}

type vertexGenerateRequest struct {
	SystemInstruction *vertexContent         `json:"systemInstruction,omitempty"`
	Contents          []vertexContent        `json:"contents"`
	GenerationConfig  vertexGenerationConfig `json:"generationConfig"`
}

type vertexContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []vertexPart `json:"parts"`
}

type vertexPart struct {
	Text string `json:"text,omitempty"`
}

type vertexGenerationConfig struct {
	Temperature      float64               `json:"temperature"`
	ResponseMIMEType string                `json:"responseMimeType"`
	ResponseSchema   *Schema               `json:"responseSchema,omitempty"`
	ThinkingConfig   *vertexThinkingConfig `json:"thinkingConfig,omitempty"`
}

type vertexThinkingConfig struct {
	ThinkingLevel ThinkingLevel `json:"thinkingLevel"`
}

type vertexGenerateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text    string `json:"text"`
				Thought bool   `json:"thought,omitempty"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int32 `json:"promptTokenCount"`
		CandidatesTokenCount int32 `json:"candidatesTokenCount"`
		ThoughtsTokenCount   int32 `json:"thoughtsTokenCount"`
		TotalTokenCount      int32 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

type vertexHTTPError struct {
	statusCode int
	body       string
}

func (e *vertexHTTPError) Error() string {
	if e.body != "" {
		return fmt.Sprintf("HTTP %d: %s", e.statusCode, e.body)
	}
	return fmt.Sprintf("HTTP %d", e.statusCode)
}

// GenerateJSON executes a low-temperature (0.1) structured JSON request conforming to schema.
func (c *VertexClient) GenerateJSON(ctx context.Context, systemPrompt, userPrompt string, schema *Schema) ([]byte, error) {
	raw, _, err := c.GenerateJSONWithUsage(ctx, systemPrompt, userPrompt, schema)
	return raw, err
}

// GenerateJSONWithUsage executes a low-temperature (0.1) structured JSON request and returns token/latency stats.
func (c *VertexClient) GenerateJSONWithUsage(ctx context.Context, systemPrompt, userPrompt string, schema *Schema) ([]byte, UsageStats, error) {
	reqPayload := vertexGenerateRequest{
		Contents: []vertexContent{{
			Role:  "user",
			Parts: []vertexPart{{Text: userPrompt}},
		}},
		GenerationConfig: vertexGenerationConfig{
			Temperature:      0.1,
			ResponseMIMEType: "application/json",
			ResponseSchema:   schema,
		},
	}
	if systemPrompt != "" {
		reqPayload.SystemInstruction = &vertexContent{
			Parts: []vertexPart{{Text: systemPrompt}},
		}
	}
	if c.thinkingLevel != "" {
		reqPayload.GenerationConfig.ThinkingConfig = &vertexThinkingConfig{
			ThinkingLevel: c.thinkingLevel,
		}
	}

	reqBody, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, UsageStats{}, fmt.Errorf("marshal vertex request: %w", err)
	}

	endpoint := vertexModelURL(c.baseURL, c.project, c.location, c.model) + ":generateContent"
	start := time.Now()

	var parsed vertexGenerateResponse
	for attempt := range maxVertexAttempts {
		parsed, err = c.doGenerateAttempt(ctx, endpoint, reqBody)
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

	text := extractCandidateText(parsed)
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
	if parsed.UsageMetadata != nil {
		stats.PromptTokens = parsed.UsageMetadata.PromptTokenCount
		stats.CandidateTokens = parsed.UsageMetadata.CandidatesTokenCount
		stats.ThoughtsTokens = parsed.UsageMetadata.ThoughtsTokenCount
		stats.TotalTokens = parsed.UsageMetadata.TotalTokenCount
	}
	return []byte(text), stats, nil
}

func (c *VertexClient) doGenerateAttempt(ctx context.Context, endpoint string, reqBody []byte) (vertexGenerateResponse, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, c.attemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return vertexGenerateResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return vertexGenerateResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxVertexErrorBodyBytes))
		return vertexGenerateResponse{}, &vertexHTTPError{
			statusCode: resp.StatusCode,
			body:       strings.TrimSpace(string(errBody)),
		}
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxVertexResponseBytes))
	if err != nil {
		return vertexGenerateResponse{}, err
	}
	var parsed vertexGenerateResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return vertexGenerateResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return parsed, nil
}

func extractCandidateText(resp vertexGenerateResponse) string {
	if len(resp.Candidates) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		if !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

func isRetryableVertexErr(err error) bool {
	var httpErr *vertexHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.statusCode == http.StatusTooManyRequests ||
			httpErr.statusCode == http.StatusServiceUnavailable ||
			httpErr.statusCode == http.StatusGatewayTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// ProbeVertexModel performs a zero-token Vertex AI model availability check via REST GET
// suitable for plugging directly into extract.VertexPreflightOptions.ModelProbe.
func ProbeVertexModel(ctx context.Context, project, location, model string) error {
	return probeVertexModelWithOptions(ctx, VertexClientOptions{
		Project:  project,
		Location: location,
		Model:    model,
	})
}

func probeVertexModelWithOptions(ctx context.Context, opts VertexClientOptions) error {
	cfg, err := resolveVertexConfig(opts)
	if err != nil {
		return err
	}
	defer cfg.httpClient.CloseIdleConnections()

	probeCtx, cancel := context.WithTimeout(ctx, probeVertexTimeout)
	defer cancel()

	endpoint := vertexModelURL(cfg.baseURL, cfg.project, cfg.location, cfg.model)
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("probe vertex model %q: %w", cfg.model, err)
	}
	resp, err := cfg.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("probe vertex model %q: %w", cfg.model, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxVertexErrorBodyBytes))
		return fmt.Errorf("probe vertex model %q: %w", cfg.model, &vertexHTTPError{
			statusCode: resp.StatusCode,
			body:       strings.TrimSpace(string(errBody)),
		})
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
