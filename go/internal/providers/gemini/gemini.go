// Package gemini is the gemini adapter behind the Provider interface (docs/adr/0004). Real, not a
// stub: a genuine HTTP client for the generateContent API
// (POST /v1beta/models/{model}:generateContent), translating this gateway's common
// OpenAI-Chat-Completions-shaped input into Gemini's request shape and its response back into
// providers.NormalizedChatResponse.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aeon-ai/aeon/go/internal/providers"
)

// Name identifies this adapter in ModelPolicyBundle candidates (proto/schemas/model_profile.schema.json).
const Name = "gemini"

// Provider is an alias for the shared interface (go/internal/providers) every model adapter
// implements — kept here so existing references to gemini.Provider keep working.
type Provider = providers.Provider

// Adapter implements Provider against the real Gemini generateContent API. The API key is sent as
// the x-goog-api-key header rather than the documented ?key= query parameter — Gemini supports
// both, and a secret belongs in a header, never in a URL that ends up in logs/history.
type Adapter struct {
	APIKey     string
	BaseURL    string // defaults to https://generativelanguage.googleapis.com if empty
	HTTPClient *http.Client
}

func (a *Adapter) baseURL() string {
	if a.BaseURL != "" {
		return a.BaseURL
	}
	return "https://generativelanguage.googleapis.com"
}

func (a *Adapter) httpClient() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return http.DefaultClient
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	Contents          []geminiContent `json:"contents"`
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
}

type geminiError struct {
	Error struct {
		Code    int    `json:"code"`
		Status  string `json:"status"`
		Message string `json:"message"`
	} `json:"error"`
}

// roleToGemini maps OpenAI-style roles ("user", "assistant") to Gemini's ("user", "model") — a
// "system"-role message is handled separately, extracted into systemInstruction, never passed
// through as a content role (Gemini has none for it).
func roleToGemini(role string) string {
	if role == "assistant" {
		return "model"
	}
	return role
}

// Decide translates renderedContext into a Gemini generateContent request, calls it, and returns
// providers.NormalizedChatResponse.
func (a *Adapter) Decide(ctx context.Context, renderedContext map[string]any) (map[string]any, error) {
	model, _ := renderedContext["model"].(string)
	rawMessages, _ := renderedContext["messages"].([]any)

	var systemInstruction *geminiContent
	contents := make([]geminiContent, 0, len(rawMessages))
	for _, raw := range rawMessages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		text, _ := m["content"].(string)
		if role == "system" {
			systemInstruction = &geminiContent{Parts: []geminiPart{{Text: text}}}
			continue
		}
		contents = append(contents, geminiContent{Role: roleToGemini(role), Parts: []geminiPart{{Text: text}}})
	}

	reqBody := geminiRequest{Contents: contents, SystemInstruction: systemInstruction}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("gemini: encoding request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/v1beta/models/%s:generateContent", strings.TrimRight(a.baseURL(), "/"), url.PathEscape(model))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("gemini: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", a.APIKey)

	resp, err := a.httpClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gemini: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr geminiError
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Error.Message != "" {
			return nil, fmt.Errorf("gemini: status %d: %s: %s", resp.StatusCode, apiErr.Error.Status, apiErr.Error.Message)
		}
		return nil, fmt.Errorf("gemini: status %d: %s", resp.StatusCode, string(body))
	}

	var parsed geminiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("gemini: decoding response: %w", err)
	}
	if len(parsed.Candidates) == 0 {
		return nil, fmt.Errorf("gemini: response had no candidates")
	}

	var text strings.Builder
	for _, part := range parsed.Candidates[0].Content.Parts {
		text.WriteString(part.Text)
	}

	return providers.NormalizedChatResponse(
		model, text.String(), parsed.Candidates[0].FinishReason, parsed.UsageMetadata.PromptTokenCount, parsed.UsageMetadata.CandidatesTokenCount,
	), nil
}

// CachingCapability: Gemini supports explicit context caching (create a cache, reference its
// name), not automatic prefix caching.
func (a *Adapter) CachingCapability() string { return "explicit_breakpoints" }

// CostModel: billed per token.
func (a *Adapter) CostModel() string { return "token_based" }
