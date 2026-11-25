package synth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"google.golang.org/genai"

	"github.com/arrase/gcp-arch/internal/extract"
)

// Default Vertex AI target settings and OAuth scope.
const (
	DefaultVertexLocation = extract.DefaultVertexLocation
	DefaultVertexModel    = extract.DefaultVertexModel
	cloudPlatformScope    = "https://www.googleapis.com/auth/cloud-platform"
)

// ModelClient abstracts structured JSON generation against Vertex AI for deterministic testing.
type ModelClient interface {
	GenerateJSON(ctx context.Context, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, error)
}

// VertexClientOptions configures a Vertex AI Gemini client.
type VertexClientOptions struct {
	Project       string
	Location      string
	Model         string
	ThinkingLevel string
	HTTPClient    *http.Client
	BaseURL       string
	Credentials   *auth.Credentials
}

// ParseThinkingLevel validates and normalizes a Gemini thinking/effort level.
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

// VertexClient executes structured JSON generation via google.golang.org/genai on Vertex AI.
type VertexClient struct {
	client        *genai.Client
	model         string
	thinkingLevel genai.ThinkingLevel
}

func newGenAIClient(ctx context.Context, opts VertexClientOptions) (*genai.Client, string, error) {
	project := strings.TrimSpace(opts.Project)
	if project == "" {
		return nil, "", fmt.Errorf("vertex project is required")
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
			return nil, "", fmt.Errorf("load Vertex AI credentials (%s): %w", cloudPlatformScope, err)
		}
	}

	cfg := &genai.ClientConfig{
		Backend:     genai.BackendVertexAI,
		Project:     project,
		Location:    location,
		Credentials: creds,
		HTTPClient:  opts.HTTPClient,
	}
	if opts.BaseURL != "" {
		cfg.HTTPOptions = genai.HTTPOptions{BaseURL: opts.BaseURL}
	}

	client, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return nil, "", fmt.Errorf("create Vertex AI genai client: %w", err)
	}
	return client, model, nil
}

// NewVertexClient initializes a Vertex AI Gemini client authenticated with cloud-platform scope.
func NewVertexClient(ctx context.Context, opts VertexClientOptions) (*VertexClient, error) {
	level, err := ParseThinkingLevel(opts.ThinkingLevel)
	if err != nil {
		return nil, err
	}
	client, model, err := newGenAIClient(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &VertexClient{client: client, model: model, thinkingLevel: level}, nil
}

// GenerateJSON executes a low-temperature (0.1) structured JSON request conforming to schema.
func (c *VertexClient) GenerateJSON(ctx context.Context, systemPrompt, userPrompt string, schema *genai.Schema) ([]byte, error) {
	temp := float32(0.1)
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(systemPrompt, genai.RoleUser),
		Temperature:       &temp,
		ResponseMIMEType:  "application/json",
		ResponseSchema:    schema,
	}
	if c.thinkingLevel != "" {
		cfg.ThinkingConfig = &genai.ThinkingConfig{
			ThinkingLevel: c.thinkingLevel,
		}
	}

	resp, err := c.client.Models.GenerateContent(ctx, c.model, genai.Text(userPrompt), cfg)
	if err != nil {
		return nil, fmt.Errorf("vertex generate content (%s): %w", c.model, err)
	}
	text := strings.TrimSpace(resp.Text())
	if text == "" {
		return nil, fmt.Errorf("vertex generate content (%s): empty JSON response", c.model)
	}
	return []byte(text), nil
}

// ProbeVertexModel performs a zero-token Vertex AI model availability check via Models.Get
// suitable for plugging directly into extract.VertexPreflightOptions.ModelProbe.
func ProbeVertexModel(ctx context.Context, project, location, model string) error {
	return probeVertexModelWithOptions(ctx, VertexClientOptions{
		Project:  project,
		Location: location,
		Model:    model,
	})
}

func probeVertexModelWithOptions(ctx context.Context, opts VertexClientOptions) error {
	client, model, err := newGenAIClient(ctx, opts)
	if err != nil {
		return err
	}
	if _, err := client.Models.Get(ctx, model, nil); err != nil {
		return fmt.Errorf("probe vertex model %q: %w", model, err)
	}
	return nil
}
