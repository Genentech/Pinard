package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// EmbeddingDim is the expected output dimension of the Rosetta model.
const EmbeddingDim = 1024

// EmbeddingError is returned on Rosetta embedding failures.
type EmbeddingError struct {
	Message string
}

func (e *EmbeddingError) Error() string { return "embed: " + e.Message }

// Embedder wraps an HTTP embedding endpoint.
//
// Environment variables:
//
//	EMBEDDING_URL   — base URL (default: https://embeddings.example.com)
//	ROSETTA_URL     — deprecated alias for EMBEDDING_URL (honored for one release)
//	ROSETTA_MODEL   — model name (default: qwen3-emb-0.6b)
type Embedder struct {
	URL   string
	Model string
	hc    *http.Client
}

// NewEmbedder creates an Embedder using environment defaults.
func NewEmbedder() *Embedder {
	embURL := os.Getenv("EMBEDDING_URL")
	if embURL == "" {
		if legacy := os.Getenv("ROSETTA_URL"); legacy != "" {
			log.Printf("[deprecation] ROSETTA_URL is deprecated; use EMBEDDING_URL instead")
			embURL = legacy
		}
	}
	if embURL == "" {
		embURL = "https://embeddings.example.com"
	}
	return &Embedder{
		URL:   strings.TrimRight(embURL, "/"),
		Model: envOr("ROSETTA_MODEL", "qwen3-emb-0.6b"),
		hc:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Embed embeds a single text string and returns a 1024-dim float vector.
func (e *Embedder) Embed(text string) ([]float64, error) {
	vecs, err := e.EmbedBatch([]string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

// EmbedBatch embeds a slice of texts. Returns one vector per text.
func (e *Embedder) EmbedBatch(texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	payload := map[string]any{
		"model": e.Model,
		"input": texts,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, e.URL+"/api/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, &EmbeddingError{Message: fmt.Sprintf("build request: %v", err)}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, &EmbeddingError{Message: fmt.Sprintf("request failed: %v", err)}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, &EmbeddingError{Message: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBody)[:min(300, len(respBody))])}
	}

	var result struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, &EmbeddingError{Message: fmt.Sprintf("parse response: %v", err)}
	}

	// Sort by index in case the API returns out-of-order.
	sort.Slice(result.Data, func(i, j int) bool {
		return result.Data[i].Index < result.Data[j].Index
	})

	out := make([][]float64, len(result.Data))
	for i, d := range result.Data {
		if len(d.Embedding) != EmbeddingDim {
			return nil, &EmbeddingError{Message: fmt.Sprintf("expected %d-dim at index %d, got %d", EmbeddingDim, i, len(d.Embedding))}
		}
		out[i] = d.Embedding
	}
	return out, nil
}


