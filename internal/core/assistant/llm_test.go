package assistant

import (
	"strings"
	"testing"
)

func TestParseStageRoute_ParsesProviderModel(t *testing.T) {
	route, err := parseStageRoute("MODEL_AGENT", "anthropic:claude-opus-5")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if route.Provider != "anthropic" || route.Model != "claude-opus-5" || len(route.Options) != 0 {
		t.Fatalf("got %+v", route)
	}
}

func TestParseStageRoute_ParsesQueryOptions(t *testing.T) {
	route, err := parseStageRoute("MODEL_AGENT", "anthropic:claude-opus-5?effort=high")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if route.Options["effort"] != "high" {
		t.Fatalf("options = %v", route.Options)
	}
}

func TestParseStageRoute_RejectsMissingValue(t *testing.T) {
	_, err := parseStageRoute("MODEL_AGENT", "")
	if err == nil || !strings.Contains(err.Error(), "MODEL_AGENT is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseStageRoute_RejectsMissingColon(t *testing.T) {
	_, err := parseStageRoute("MODEL_AGENT", "claude-opus-5")
	if err == nil || !strings.Contains(err.Error(), `must be "provider:model"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseStageRoute_RejectsUnknownProvider(t *testing.T) {
	_, err := parseStageRoute("MODEL_AGENT", "acme:x")
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseStageRoute_RejectsEmptyModel(t *testing.T) {
	_, err := parseStageRoute("MODEL_AGENT", "anthropic:")
	if err == nil || !strings.Contains(err.Error(), "model is empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseStageRoute_ParsesOpenAICompatibleRoute(t *testing.T) {
	route, err := parseStageRoute("MODEL_AGENT",
		"openai-compatible:mistral-small-latest?base_url=https://api.mistral.ai/v1&api_key_env=MISTRAL_API_KEY")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if route.Provider != "openai-compatible" || route.Model != "mistral-small-latest" {
		t.Fatalf("got %+v", route)
	}
	if route.Options["base_url"] != "https://api.mistral.ai/v1" || route.Options["api_key_env"] != "MISTRAL_API_KEY" {
		t.Fatalf("options = %v", route.Options)
	}
}

// Anthropic's current models reject temperature/top_p/top_k with a 400 — they
// are not merely ignored. Forwarding them would turn a stray `?temperature=0`
// in an env var into a hard request failure at runtime.
func TestParseStageRoute_StripsSamplingForAnthropicOnly(t *testing.T) {
	route, err := parseStageRoute("MODEL_AGENT", "anthropic:claude-opus-5?temperature=0.7&effort=high")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := route.Options["temperature"]; ok {
		t.Fatalf("temperature should be stripped for anthropic: %v", route.Options)
	}
	if route.Options["effort"] != "high" {
		t.Fatalf("non-sampling options must survive: %v", route.Options)
	}

	// Per-provider on purpose: openai-compatible accepts temperature, and
	// silently dropping a valid option would be its own quiet failure.
	route, err = parseStageRoute("MODEL_AGENT", "openai-compatible:m?base_url=http://x/v1&temperature=0.2")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if route.Options["temperature"] != "0.2" {
		t.Fatalf("options = %v", route.Options)
	}
}

func TestNewModel_AnthropicRequiresKeyEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	route := Route{Provider: "anthropic", Model: "claude-opus-5", Options: map[string]string{}}
	_, _, err := NewModel(route)
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewModel_OpenAICompatibleRequiresBaseURL(t *testing.T) {
	route := Route{Provider: "openai-compatible", Model: "m", Options: map[string]string{}}
	_, _, err := NewModel(route)
	if err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewModel_OpenAICompatibleNamedKeyEnvMustBeSet(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	route := Route{Provider: "openai-compatible", Model: "m", Options: map[string]string{
		"base_url": "https://api.mistral.ai/v1", "api_key_env": "MISTRAL_API_KEY",
	}}
	_, _, err := NewModel(route)
	if err == nil || !strings.Contains(err.Error(), "MISTRAL_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

// api_key_env is optional so local OpenAI-compatible servers (Ollama, vLLM)
// that don't enforce auth work without dummy credentials.
func TestNewModel_OpenAICompatibleWithoutKeyEnv(t *testing.T) {
	route := Route{Provider: "openai-compatible", Model: "llama3", Options: map[string]string{
		"base_url": "http://localhost:11434/v1",
	}}
	model, _, err := NewModel(route)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if model.Provider() != "openai-compatible" {
		t.Fatalf("provider = %q", model.Provider())
	}
}

func TestNewModel_SamplingOptionsBecomeCallOptions(t *testing.T) {
	route := Route{Provider: "openai-compatible", Model: "m", Options: map[string]string{
		"base_url": "http://x/v1", "temperature": "0.2", "top_p": "0.9", "top_k": "40",
	}}
	_, callOpts, err := NewModel(route)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(callOpts) != 3 {
		t.Fatalf("len(callOpts) = %d, want 3", len(callOpts))
	}
}

func TestNewModel_RejectsUnparseableSamplingValue(t *testing.T) {
	route := Route{Provider: "openai-compatible", Model: "m", Options: map[string]string{
		"base_url": "http://x/v1", "temperature": "abc",
	}}
	_, _, err := NewModel(route)
	if err == nil || !strings.Contains(err.Error(), "temperature") {
		t.Fatalf("err = %v", err)
	}
}

func TestDescribeStageModels(t *testing.T) {
	t.Setenv("MODEL_AGENT", "anthropic:claude-opus-5")
	desc, err := DescribeStageModels()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if desc != "agent=anthropic:claude-opus-5" {
		t.Fatalf("desc = %q", desc)
	}
}
