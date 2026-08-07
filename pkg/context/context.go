// Package context provides a client for context.dev's live web data API.
//
// It fetches structured information from URLs and web queries, returning
// clean text suitable for injecting into LLM prompts. This is the "live
// web data" requirement for the hackathon.
package context

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client fetches live web data from context.dev.
type Client struct {
	apiKey string
	client *http.Client
}

// New creates a new context.dev client.
// If apiKey is empty, reads CONTEXT_API_KEY from the environment.
func New(apiKey string) (*Client, error) {
	if apiKey == "" {
		apiKey = os.Getenv("CONTEXT_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("CONTEXT_API_KEY not set")
	}
	return &Client{
		apiKey: apiKey,
		client: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// FetchResult contains the structured data returned by context.dev.
type FetchResult struct {
	Title       string `json:"title"`
	Content     string `json:"content"`
	URL         string `json:"url"`
	Description string `json:"description"`
	SiteName    string `json:"site_name"`
	Favicon     string `json:"favicon"`
}

// FetchURL retrieves structured content from a URL via context.dev.
func (c *Client) FetchURL(url string) (*FetchResult, error) {
	body := map[string]string{"url": url}
	b, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", "https://api.context.dev/v1/fetch", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("context.dev fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("context.dev: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result FetchResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("context.dev parse: %w", err)
	}
	return &result, nil
}

// SearchResult is a single search result from context.dev.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Search queries context.dev for web results.
func (c *Client) Search(query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 5
	}
	body := map[string]any{
		"query": query,
		"limit": limit,
	}
	b, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", "https://api.context.dev/v1/search", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("context.dev search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("context.dev: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var results struct {
		Results []SearchResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, fmt.Errorf("context.dev parse: %w", err)
	}
	return results.Results, nil
}

// FetchAndSummarize fetches a URL and returns a concise summary suitable
// for LLM context injection. Truncates to maxChars (0 = no limit).
func (c *Client) FetchAndSummarize(url string, maxChars int) (string, error) {
	result, err := c.FetchURL(url)
	if err != nil {
		return "", err
	}

	var parts []string
	if result.Title != "" {
		parts = append(parts, fmt.Sprintf("Title: %s", result.Title))
	}
	if result.SiteName != "" {
		parts = append(parts, fmt.Sprintf("Source: %s", result.SiteName))
	}
	if result.Description != "" {
		parts = append(parts, fmt.Sprintf("Description: %s", result.Description))
	}
	if result.Content != "" {
		parts = append(parts, fmt.Sprintf("Content:\n%s", result.Content))
	}

	summary := strings.Join(parts, "\n\n")
	if maxChars > 0 && len(summary) > maxChars {
		summary = summary[:maxChars] + "..."
	}
	return summary, nil
}

// SearchAndSummarize searches for a query and returns a formatted summary
// of the top results, suitable for LLM context injection.
func (c *Client) SearchAndSummarize(query string, limit int) (string, error) {
	results, err := c.Search(query, limit)
	if err != nil {
		return "", err
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("Web search results for: %q\n", query))
	for i, r := range results {
		parts = append(parts, fmt.Sprintf("%d. %s\n   %s\n   %s", i+1, r.Title, r.URL, r.Snippet))
	}
	return strings.Join(parts, "\n\n"), nil
}

// LLMContext builds a prompt-ready context string from a URL or search query.
// If the input looks like a URL, it fetches the page. Otherwise, it searches.
func (c *Client) LLMContext(input string, maxChars int) (string, error) {
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		return c.FetchAndSummarize(input, maxChars)
	}
	return c.SearchAndSummarize(input, 5)
}
