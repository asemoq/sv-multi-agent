package embeddings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func embedHandler(t *testing.T, dim int) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var request struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		if request.Model != "Qwen3-Embedding-4B" {
			t.Errorf("model = %q", request.Model)
		}
		data := make([]map[string]any, len(request.Input))
		for index := range request.Input {
			vector := make([]float32, dim)
			vector[0] = float32(index + 1)
			data[index] = map[string]any{"index": index, "embedding": vector}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": request.Model, "data": data})
	})
}

func TestEmbedBatchesAndQueryPrefix(t *testing.T) {
	inputs := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		inputs = append(inputs, request.Input...)
		data := make([]map[string]any, len(request.Input))
		for index := range request.Input {
			data[index] = map[string]any{"index": index, "embedding": []float32{1, 2, 3, 4}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": request.Model, "data": data})
	}))
	defer server.Close()

	client, err := New(Options{
		BaseURL:     server.URL + "/v1",
		Model:       "Qwen3-Embedding-4B",
		Dim:         4,
		Timeout:     5 * time.Second,
		QueryPrefix: "Instruct: test\\nQuery: ",
		BatchSize:   2,
	})
	if err != nil {
		t.Fatal(err)
	}

	vectors, err := client.EmbedDocuments(context.Background(), []string{"a", "bb", "ccc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 3 || len(vectors[0]) != 4 {
		t.Fatalf("vectors = %#v", vectors)
	}

	if _, err := client.EmbedQuery(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	last := inputs[len(inputs)-1]
	if !strings.HasPrefix(last, "Instruct: test\nQuery: ") {
		t.Fatalf("query input = %q", last)
	}
}

func TestDimensionMismatch(t *testing.T) {
	server := httptest.NewServer(embedHandler(t, 3))
	defer server.Close()

	client, err := New(Options{BaseURL: server.URL + "/v1", Model: "Qwen3-Embedding-4B", Dim: 4, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.EmbedDocuments(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected dimension mismatch")
	}
}

func TestInsecureSkipVerify(t *testing.T) {
	tlsServer := httptest.NewTLSServer(embedHandler(t, 4))
	defer tlsServer.Close()

	secureClient, err := New(Options{BaseURL: tlsServer.URL + "/v1", Model: "Qwen3-Embedding-4B", Dim: 4, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secureClient.EmbedDocuments(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected TLS verification to fail")
	}

	insecureClient, err := New(Options{
		BaseURL:            tlsServer.URL + "/v1",
		Model:              "Qwen3-Embedding-4B",
		Dim:                4,
		Timeout:            5 * time.Second,
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insecureClient.EmbedDocuments(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("insecure client: %v", err)
	}
}
