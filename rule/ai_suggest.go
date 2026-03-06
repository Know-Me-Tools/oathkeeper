package rule

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	aiCache = &suggestionCache{
		store: make(map[string]cacheEntry),
	}
	httpClient = &http.Client{
		Timeout: 5 * time.Second, // Max time we'll wait so proxy isn't permanently stalled
	}
)

type cacheEntry struct {
	suggestion string
	expiresAt  time.Time
}

type suggestionCache struct {
	sync.RWMutex
	store map[string]cacheEntry
}

func (c *suggestionCache) get(key string) (string, bool) {
	c.RLock()
	defer c.RUnlock()
	entry, ok := c.store[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return "", false
	}
	return entry.suggestion, true
}

func (c *suggestionCache) set(key string, suggestion string) {
	c.Lock()
	defer c.Unlock()

	// Periodically cleanup cache if it gets too large (simple eviction)
	if len(c.store) > 1000 {
		now := time.Now()
		for k, v := range c.store {
			if now.After(v.expiresAt) {
				delete(c.store, k)
			}
		}
	}

	c.store[key] = cacheEntry{
		suggestion: suggestion,
		expiresAt:  time.Now().Add(1 * time.Hour),
	}
}

const systemPrompt = `You are an expert devops engineer debugging Ory Oathkeeper access rules.
Oathkeeper acts as a proxy that matches incoming HTTP requests to a specific Access Rule.
An Access Rule contains:
- id: A unique string identifier.
- match.url: A pattern used to match the request URL. It supports <.*> for regex or <*> for glob.
- match.methods: An array of HTTP methods (e.g., ["GET", "POST"]).

CRITICAL RULES:
1. Every incoming request must match EXACTLY ONE rule.
2. If zero rules match, the request is rejected (No Rule Matched).
3. If multiple rules match, the request is rejected (Duplicate Route Detected).

Your task is to analyze the active rules configuration, the incoming request, and the specific failure, and provide a clear, concise instruction on how to fix the rule configuration. Provide the exact JSON or YAML snippet to modify, add, or delete. Be direct and avoid conversational filler.`

// SuggestFix asks OpenAI for a fix suggestion based on conflicting or missing rules.
// It returns an empty string if AI is disabled, unconfigured, or if an error occurs.
func SuggestFix(errorType string, requestMethod string, requestURL string, conflictingRules interface{}, activeRules interface{}) string {
	useAI := os.Getenv("USE_AI")
	apiKey := os.Getenv("OPENAI_API_KEY")

	// Only run if explicitly enabled and key is present
	if (useAI != "true" && useAI != "1") || apiKey == "" {
		return ""
	}

	apiUrl := os.Getenv("OPENAI_API_URL")
	if apiUrl == "" {
		apiUrl = "https://api.openai.com/v1/chat/completions"
	}
	model := os.Getenv("OPENAI_API_MODEL")
	if model == "" {
		model = "gpt-5.2" // Fallback to user-requested default
	}

	conflictingJSON, _ := json.MarshalIndent(conflictingRules, "", "  ")
	activeJSON, _ := json.MarshalIndent(activeRules, "", "  ")

	var userPromptBuilder strings.Builder
	userPromptBuilder.WriteString(fmt.Sprintf("ERROR: %s\n", errorType))
	userPromptBuilder.WriteString(fmt.Sprintf("Request: %s %s\n\n", requestMethod, requestURL))

	if errorType == "Duplicate Route Detected" {
		userPromptBuilder.WriteString("Conflicting Rules:\n")
		userPromptBuilder.WriteString(string(conflictingJSON))
		userPromptBuilder.WriteString("\n\nAll Active Rules:\n")
		userPromptBuilder.WriteString(string(activeJSON))
		userPromptBuilder.WriteString("\n\nHow should the configuration be updated to resolve this conflict so only one rule matches?")
	} else { // No Rule Matched
		userPromptBuilder.WriteString("All Active Rules:\n")
		userPromptBuilder.WriteString(string(activeJSON))
		userPromptBuilder.WriteString("\n\nPlease provide a complete new rule configuration (as a JSON snippet) to add that will accommodate this missing route. Use the existing active rules as a template to determine the appropriate authenticators, authorizer, mutators, and upstream URL structure for this new route.")
	}

	userPrompt := userPromptBuilder.String()

	hash := sha256.Sum256([]byte(userPrompt + model))
	cacheKey := hex.EncodeToString(hash[:])

	if cached, ok := aiCache.get(cacheKey); ok {
		return cached
	}

	reqBody := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": 0.2, // Low temp for deterministic config suggestions
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return ""
	}

	req, err := http.NewRequest("POST", apiUrl, bytes.NewBuffer(jsonBody))
	if err != nil {
		return ""
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "" // Silently fail to not disrupt proxy error
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	var openAIResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(bodyBytes, &openAIResp); err != nil {
		return ""
	}

	if len(openAIResp.Choices) > 0 {
		suggestion := strings.TrimSpace(openAIResp.Choices[0].Message.Content)
		if suggestion != "" {
			suggestion = fmt.Sprintf("\n\n--- AI Fix Suggestion ---\n%s", suggestion)
			aiCache.set(cacheKey, suggestion)
			return suggestion
		}
	}

	return ""
}
