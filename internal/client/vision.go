package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Vision asks the server whether model takes images. Ollama reports a
// "vision" capability (POST /api/show); OpenRouter lists each model's input
// modalities (GET /models/{slug}/endpoints). Other servers return an error,
// which callers treat as "unknown, try it".
func (c *Client) Vision(ctx context.Context, model string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var req *http.Request
	var err error
	if strings.Contains(c.BaseURL, "openrouter.ai") {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models/"+model+"/endpoints", nil)
	} else {
		body, _ := json.Marshal(map[string]string{"model": model})
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/v1")+"/api/show", bytes.NewReader(body))
	}
	if err != nil {
		return false, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var r struct {
		Capabilities []string `json:"capabilities"` // Ollama
		Data         struct {
			Architecture struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
		} `json:"data"` // OpenRouter
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return false, err
	}
	return slices.Contains(r.Capabilities, "vision") || slices.Contains(r.Data.Architecture.InputModalities, "image"), nil
}

// Embed returns embeddings for texts from an Ollama server (POST /api/embed).
func (c *Client) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"model": model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/v1")+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	defer resp.Body.Close()
	var r struct {
		Embeddings [][]float32 `json:"embeddings"`
		Error      string      `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("embed: HTTP %d", resp.StatusCode)
	}
	if r.Error != "" || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: HTTP %d %s", resp.StatusCode, r.Error)
	}
	if len(r.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed: %d embeddings for %d texts", len(r.Embeddings), len(texts))
	}
	return r.Embeddings, nil
}
