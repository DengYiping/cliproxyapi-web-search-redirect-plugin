package main

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultSearchModel    = "gpt-5.5"
	defaultSearchProvider = "codex"
	defaultMaxSearches    = 5
)

type config struct {
	// Models lists wildcard patterns of client models whose hosted web search is redirected.
	Models []string `yaml:"models"`
	// SearchModel is the OpenAI model that runs each search with the hosted web_search tool.
	SearchModel string `yaml:"search_model"`
	// SearchProvider forces the built-in provider used for searches; empty lets the host resolve it.
	SearchProvider string `yaml:"search_provider"`
	// SearchReasoningEffort sets reasoning.effort on search requests when non-empty.
	SearchReasoningEffort string `yaml:"search_reasoning_effort"`
	// MaxSearches caps searches per client request.
	MaxSearches int `yaml:"max_searches"`
	// DebugLog logs routing decisions and searches through the host logger.
	DebugLog bool `yaml:"debug_log"`
}

func defaultConfig() config {
	return config{
		SearchModel:           defaultSearchModel,
		SearchProvider:        defaultSearchProvider,
		SearchReasoningEffort: "low",
		MaxSearches:           defaultMaxSearches,
	}
}

func parseConfig(raw []byte) (config, error) {
	cfg := defaultConfig()
	if len(raw) > 0 {
		if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
			return config{}, fmt.Errorf("parse plugin config: %w", errUnmarshal)
		}
	}
	models := make([]string, 0, len(cfg.Models))
	for _, pattern := range cfg.Models {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			models = append(models, pattern)
		}
	}
	cfg.Models = models
	cfg.SearchModel = strings.TrimSpace(cfg.SearchModel)
	if cfg.SearchModel == "" {
		cfg.SearchModel = defaultSearchModel
	}
	cfg.SearchProvider = strings.ToLower(strings.TrimSpace(cfg.SearchProvider))
	cfg.SearchReasoningEffort = strings.ToLower(strings.TrimSpace(cfg.SearchReasoningEffort))
	if cfg.MaxSearches <= 0 {
		cfg.MaxSearches = defaultMaxSearches
	}
	return cfg, nil
}

// matchesModel reports whether model matches any case-insensitive wildcard pattern.
// Only "*" is special; it matches any run of characters, including "/".
func matchesModel(patterns []string, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	for _, pattern := range patterns {
		if wildcardMatch(strings.ToLower(pattern), model) {
			return true
		}
	}
	return false
}

func wildcardMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}
	return strings.HasSuffix(s, last)
}
