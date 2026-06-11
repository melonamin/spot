package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrNotFound = errors.New("document not found")

type Document struct {
	ID        string         `json:"id"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// DocStore is a schemaless document store: JSON blobs grouped into named
// collections, namespaced by scope (see scopeFor).
type DocStore struct {
	db *sql.DB
}

func (s *DocStore) Create(ctx context.Context, scope, collection string, data map[string]any) (Document, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Document{}, fmt.Errorf("encode document: %w", err)
	}
	var doc Document
	doc.Data = data
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO documents (scope, collection, data)
		 VALUES ($1, $2, $3)
		 RETURNING id, created_at, updated_at`,
		scope, collection, raw,
	).Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt)
	if err != nil {
		return Document{}, fmt.Errorf("insert document: %w", err)
	}
	return doc, nil
}

func (s *DocStore) List(ctx context.Context, scope, collection string, limit int) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, data, created_at, updated_at FROM documents
		 WHERE scope = $1 AND collection = $2
		 ORDER BY created_at DESC
		 LIMIT $3`,
		scope, collection, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()

	docs := []Document{}
	for rows.Next() {
		doc, err := scanDocument(rows.Scan)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

func (s *DocStore) Get(ctx context.Context, scope, collection, id string) (Document, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, data, created_at, updated_at FROM documents
		 WHERE scope = $1 AND collection = $2 AND id = $3`,
		scope, collection, id,
	)
	doc, err := scanDocument(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	return doc, err
}

func (s *DocStore) Update(ctx context.Context, scope, collection, id string, data map[string]any) (Document, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Document{}, fmt.Errorf("encode document: %w", err)
	}
	var doc Document
	doc.Data = data
	err = s.db.QueryRowContext(ctx,
		`UPDATE documents SET data = $4, updated_at = now()
		 WHERE scope = $1 AND collection = $2 AND id = $3
		 RETURNING id, created_at, updated_at`,
		scope, collection, id, raw,
	).Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("update document: %w", err)
	}
	return doc, nil
}

func (s *DocStore) Delete(ctx context.Context, scope, collection, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM documents WHERE scope = $1 AND collection = $2 AND id = $3`,
		scope, collection, id,
	)
	if err != nil {
		return fmt.Errorf("delete document: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete document: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanDocument(scan func(dest ...any) error) (Document, error) {
	var doc Document
	var raw []byte
	if err := scan(&doc.ID, &raw, &doc.CreatedAt, &doc.UpdatedAt); err != nil {
		return Document{}, err
	}
	if err := json.Unmarshal(raw, &doc.Data); err != nil {
		return Document{}, fmt.Errorf("decode document: %w", err)
	}
	return doc, nil
}
