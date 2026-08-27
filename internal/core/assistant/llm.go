package assistant

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	aisdk "github.com/grafana/ai-sdk"
	"github.com/grafana/ai-sdk/provider"
	anthropicprovider "github.com/grafana/ai-sdk/providers/anthropic"
	openaiprovider "github.com/grafana/ai-sdk/providers/openai"
	openaicompatible "github.com/grafana/ai-sdk/providers/openai-compatible"
)

// Per-stage model selection, ported from the TS service. Each stage reads
// MODEL_<STAGE> from the environment as `provider:model` or
// `provider:model?key=value`.
//
// Only 'agent' exists today. The parser is here from the start because
// retrofitting model routing after prompts are embedded in call sites is
// painful, and splitting off a cheaper classifier or reply-writer later should
// be a config change rather than a rewrite.
var stages = []string{"agent"}

// knownProviders is ordered for the error message.
var knownProviders = []string{"anthropic", "openai", "openai-compatible"}

var samplingOptionKeys = map[string]bool{
	"temperature": true, "top_p": true, "topp": true, "top_k": true, "topk": true,
}

// Anthropic's current models REJECT sampling parameters with a 400 — they are
// not merely ignored. Forwarding every query-string option verbatim would turn
// a stray `?temperature=0` in an env var into a hard request failure at
// runtime, so sampling keys are stripped at parse time for anthropic.
//
// Deliberately per-provider rather than global: openai and openai-compatible
// accept temperature, and silently dropping a valid option would be its own
// quiet failure. A provider not listed here keeps whatever it was given.
var rejectsSampling = map[string]bool{
	"anthropic":         true,
	"openai":            false,
	"openai-compatible": false,
}

// Route is a parsed MODEL_<STAGE> value.
type Route struct {
	Provider string
	Model    string
	Options  map[string]string
}

func parseStageRoute(envKey, raw string) (Route, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return Route{}, fmt.Errorf(`%s is required (format: "provider:model" or "provider:model?key=value")`, envKey)
	}

	// Split the optional `?query` suffix off first, before parsing provider:model.
	main, query, _ := strings.Cut(value, "?")

	colon := strings.Index(main, ":")
	if colon <= 0 {
		return Route{}, fmt.Errorf(`%s must be "provider:model" (got: %q)`, envKey, value)
	}

	providerName := strings.ToLower(main[:colon])
	if _, known := rejectsSampling[providerName]; !known {
		return Route{}, fmt.Errorf("%s has unknown provider %q; known: %s",
			envKey, providerName, strings.Join(knownProviders, ", "))
	}

	model := strings.TrimSpace(main[colon+1:])
	if model == "" {
		return Route{}, fmt.Errorf("%s model is empty (got: %q)", envKey, value)
	}

	values, err := url.ParseQuery(query)
	if err != nil {
		return Route{}, fmt.Errorf("%s has an unparseable query string: %w", envKey, err)
	}
	options := map[string]string{}
	for k, vs := range values {
		if rejectsSampling[providerName] && samplingOptionKeys[strings.ToLower(k)] {
			continue
		}
		options[k] = vs[len(vs)-1]
	}

	return Route{Provider: providerName, Model: model, Options: options}, nil
}

func stageEnvKey(stage string) string { return "MODEL_" + strings.ToUpper(stage) }

// RouteForStage parses the stage's MODEL_* env var. Called at boot (fail
// fast) and by ai.DevStatus; the raw value is read here rather than through
// envconfig because the format (and the per-provider key envs it names) is
// owned by this parser, matching the TS service.
func RouteForStage(stage string) (Route, error) {
	key := stageEnvKey(stage)
	return parseStageRoute(key, os.Getenv(key))
}

// NewModel turns a parsed route into a provider model plus the per-call
// sampling options that survived the strip table. Misconfiguration (missing
// key env, missing base_url, unparseable sampling value) is a boot error
// naming the env var or option at fault.
func NewModel(route Route) (provider.LanguageModel, CallOptions, error) {
	callOpts, err := samplingCallOptions(route)
	if err != nil {
		return nil, nil, err
	}

	switch route.Provider {
	case "anthropic":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, nil, fmt.Errorf("ANTHROPIC_API_KEY is required for MODEL provider %q", route.Provider)
		}
		return anthropicprovider.New(key, route.Model), callOpts, nil

	case "openai":
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" {
			return nil, nil, fmt.Errorf("OPENAI_API_KEY is required for MODEL provider %q", route.Provider)
		}
		return openaiprovider.NewResponses(key, route.Model), callOpts, nil

	case "openai-compatible":
		baseURL := route.Options["base_url"]
		if baseURL == "" {
			return nil, nil, errors.New(`the "openai-compatible" provider requires a base_url query option (e.g. MODEL_AGENT=openai-compatible:mistral-small-latest?base_url=https://api.mistral.ai/v1&api_key_env=MISTRAL_API_KEY)`)
		}
		opts := []openaicompatible.Option{openaicompatible.WithBaseURL(baseURL)}
		// api_key_env is optional so local servers (Ollama, vLLM) that don't
		// enforce auth work without dummy credentials; when it IS named, the
		// env var it names must be set — a half-configured route should fail
		// at boot, not 401 on the first turn.
		if keyEnv := route.Options["api_key_env"]; keyEnv != "" {
			key := os.Getenv(keyEnv)
			if key == "" {
				return nil, nil, fmt.Errorf("%s is required (named by api_key_env in %s)", keyEnv, stageEnvKey("agent"))
			}
			opts = append(opts, openaicompatible.WithAPIKey(key))
		}
		return openaicompatible.New(route.Model, opts...), callOpts, nil

	default:
		return nil, nil, fmt.Errorf("unknown provider %q", route.Provider)
	}
}

// samplingCallOptions maps surviving sampling options onto ai-sdk per-call
// options. Unknown option keys are deliberately ignored (the TS service
// accepted arbitrary keys; base_url/api_key_env are consumed by NewModel).
func samplingCallOptions(route Route) (CallOptions, error) {
	var opts CallOptions
	for k, v := range route.Options {
		switch strings.ToLower(k) {
		case "temperature":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return nil, fmt.Errorf("temperature option %q is not a number", v)
			}
			opts = append(opts, aisdk.WithTemperature(f))
		case "top_p", "topp":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return nil, fmt.Errorf("top_p option %q is not a number", v)
			}
			opts = append(opts, aisdk.WithTopP(f))
		case "top_k", "topk":
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("top_k option %q is not an integer", v)
			}
			opts = append(opts, aisdk.WithTopK(n))
		}
	}
	return opts, nil
}

// DescribeStageModels renders "agent=anthropic:claude-opus-5" for boot logs,
// DevStatus, and the model field of TurnTrace. Never includes options — an
// api key env name is config, not a secret, but there's no reason to ship it
// into every trace either.
func DescribeStageModels() (string, error) {
	parts := make([]string, 0, len(stages))
	for _, stage := range stages {
		route, err := RouteForStage(stage)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("%s=%s:%s", stage, route.Provider, route.Model))
	}
	return strings.Join(parts, " "), nil
}
