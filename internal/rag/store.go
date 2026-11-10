package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// Document is a knowledge row before insertion.
type Document struct {
	Kind      string
	TaskID    string
	Title     string
	Content   string
	Metadata  map[string]any
	Embedding []float32
}

// Hit is a search result.
type Hit struct {
	ID       int64
	Kind     string
	TaskID   string
	Title    string
	Content  string
	Metadata map[string]any
	Score    float64
}

// Store is a pgvector-backed knowledge store.
type Store struct {
	pool       *pgxpool.Pool
	dim        int
	vectorType string // "vector" or "halfvec"
}

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, databaseURL string, dim int) (*Store, error) {
	if dim <= 0 || dim > 4000 {
		return nil, fmt.Errorf("invalid embedding dimension %d (expected 1..4000)", dim)
	}
	vectorType := "vector"
	if dim > 2000 {
		// pgvector HNSW indexes support up to 2000 dims for vector and 4000 for halfvec.
		vectorType = "halfvec"
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	config.MaxConns = 4
	config.ConnConfig.ConnectTimeout = 5 * time.Second

	// The vector extension must exist before pgx can register its types, so
	// bootstrap the extension with a direct connection first.
	bootstrap, err := pgx.ConnectConfig(ctx, config.ConnConfig)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if _, execErr := bootstrap.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); execErr != nil {
		_ = bootstrap.Close(ctx)
		return nil, fmt.Errorf("create vector extension: %w", execErr)
	}
	_ = bootstrap.Close(ctx)

	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{pool: pool, dim: dim, vectorType: vectorType}, nil
}

// Close closes the connection pool.
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// Ping verifies the connection.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Migrate creates the pgvector extension, table and indexes.
func (s *Store) Migrate(ctx context.Context) error {
	statements := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS knowledge (
			id BIGSERIAL PRIMARY KEY,
			kind TEXT NOT NULL,
			task_id TEXT,
			title TEXT,
			content TEXT NOT NULL,
			metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			embedding %s(%d) NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, s.vectorType, s.dim),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS knowledge_embedding_idx ON knowledge USING hnsw (embedding %s_cosine_ops)`, s.vectorType),
		`CREATE INDEX IF NOT EXISTS knowledge_kind_idx ON knowledge (kind)`,
		`CREATE INDEX IF NOT EXISTS knowledge_task_idx ON knowledge (task_id)`,
	}
	for _, statement := range statements {
		if _, err := s.pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// Insert adds documents to the store.
func (s *Store) Insert(ctx context.Context, documents []Document) error {
	for _, document := range documents {
		if len(document.Embedding) != s.dim {
			return fmt.Errorf("document %q: embedding dimension %d != %d", document.Title, len(document.Embedding), s.dim)
		}
		metadata, err := json.Marshal(document.Metadata)
		if err != nil {
			return err
		}
		var taskID any
		if document.TaskID != "" {
			taskID = document.TaskID
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO knowledge (kind, task_id, title, content, metadata, embedding)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			document.Kind, taskID, document.Title, document.Content, metadata,
			s.encode(document.Embedding),
		); err != nil {
			return fmt.Errorf("insert knowledge: %w", err)
		}
	}
	return nil
}

// DeleteByTask removes all rows for a task.
func (s *Store) DeleteByTask(ctx context.Context, taskID string) error {
	if taskID == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM knowledge WHERE task_id = $1`, taskID)
	return err
}

// DeleteBySource removes all rows for a document source.
func (s *Store) DeleteBySource(ctx context.Context, source string) error {
	if source == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM knowledge WHERE metadata->>'source' = $1`, source)
	return err
}

// Search returns the closest documents to the embedding.
func (s *Store) Search(ctx context.Context, embedding []float32, kinds []string, topK int, minScore float64) ([]Hit, error) {
	if len(embedding) != s.dim {
		return nil, fmt.Errorf("query embedding dimension %d != %d", len(embedding), s.dim)
	}
	if topK <= 0 {
		topK = 6
	}

	args := []any{s.encode(embedding)}
	where := []string{}
	if len(kinds) > 0 {
		args = append(args, kinds)
		where = append(where, fmt.Sprintf("kind = ANY($%d)", len(args)))
	}
	if minScore > 0 {
		args = append(args, minScore)
		where = append(where, fmt.Sprintf("1 - (embedding <=> $1) >= $%d", len(args)))
	}

	query := `SELECT id, kind, COALESCE(task_id, ''), COALESCE(title, ''), content, metadata,
	                 1 - (embedding <=> $1) AS score
	          FROM knowledge`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, topK)
	query += fmt.Sprintf(" ORDER BY embedding <=> $1 LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hits := []Hit{}
	for rows.Next() {
		var hit Hit
		var metadata []byte
		if err := rows.Scan(&hit.ID, &hit.Kind, &hit.TaskID, &hit.Title, &hit.Content, &metadata, &hit.Score); err != nil {
			return nil, err
		}
		if len(metadata) > 0 {
			_ = json.Unmarshal(metadata, &hit.Metadata)
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// Stats returns row counts per kind.
func (s *Store) Stats(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, count(*) FROM knowledge GROUP BY kind ORDER BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := map[string]int{}
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			return nil, err
		}
		stats[kind] = count
	}
	return stats, rows.Err()
}

// Count returns the total number of rows.
func (s *Store) Count(ctx context.Context) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) encode(values []float32) any {
	if s.vectorType == "halfvec" {
		return pgvector.NewHalfVector(values)
	}
	return pgvector.NewVector(values)
}
