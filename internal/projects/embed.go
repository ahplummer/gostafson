package projects

import (
	"context"
	"strings"

	"github.com/ahplummer/gostafson/internal/client"
)

// OllamaEmbedder embeds with an Ollama model. nomic-embed-text expects
// "search_query: " and "search_document: " prefixes, which it adds.
type OllamaEmbedder struct {
	Client *client.Client
	Model  string
}

// Embed implements Embedder.
func (e *OllamaEmbedder) Embed(ctx context.Context, texts []string, query bool) ([][]float32, error) {
	if strings.Contains(e.Model, "nomic") {
		prefix := "search_document: "
		if query {
			prefix = "search_query: "
		}
		texts = append([]string(nil), texts...)
		for i := range texts {
			texts[i] = prefix + texts[i]
		}
	}
	return e.Client.Embed(ctx, e.Model, texts)
}
