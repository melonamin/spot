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
	db  *sql.DB
	hub *Hub
}

func (s *DocStore) Create(ctx context.Context, scope, collection string, data map[string]any) (Document, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Document{}, fmt.Errorf("encode document: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Document{}, fmt.Errorf("begin insert: %w", err)
	}
	defer tx.Rollback()

	var doc Document
	doc.Data = data
	err = tx.QueryRowContext(ctx, insertDocumentSQL, scope, collection, raw).
		Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt)
	if err != nil {
		return Document{}, fmt.Errorf("insert document: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Document{}, fmt.Errorf("commit insert: %w", err)
	}
	s.publishChange(ctx, "create", scope, collection, doc.ID, &doc)
	return doc, nil
}

func (s *DocStore) List(ctx context.Context, scope, collection string, limit int) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx,
		listDocumentsSQL,
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
		getDocumentSQL,
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Document{}, fmt.Errorf("begin update: %w", err)
	}
	defer tx.Rollback()

	var doc Document
	doc.Data = data
	err = tx.QueryRowContext(ctx, updateDocumentSQL, scope, collection, id, raw).
		Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("update document: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Document{}, fmt.Errorf("commit update: %w", err)
	}
	s.publishChange(ctx, "update", scope, collection, id, &doc)
	return doc, nil
}

func (s *DocStore) Delete(ctx context.Context, scope, collection, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		deleteDocumentSQL,
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete: %w", err)
	}
	s.publishChange(ctx, "delete", scope, collection, id, nil)
	return nil
}

// PurgeScope deletes every document in a scope. Used when a site is
// deleted; no realtime notifications are sent — the site's subscribers
// are going away with it.
func (s *DocStore) PurgeScope(ctx context.Context, scope string) error {
	if _, err := s.db.ExecContext(ctx, purgeScopeSQL, scope); err != nil {
		return fmt.Errorf("purge scope %s: %w", scope, err)
	}
	return nil
}

func (s *DocStore) publishChange(_ context.Context, action, scope, collection, id string, doc *Document) {
	if s.hub == nil {
		return
	}
	s.hub.Publish(scope, collection, Event{
		Type:       action,
		Collection: collection,
		ID:         id,
		Doc:        doc,
	})
}

const (
	insertDocumentSQL = `INSERT INTO documents (scope, collection, data)
		VALUES (?, ?, ?)
		RETURNING id, created_at, updated_at`

	listDocumentsSQL = `SELECT id, data, created_at, updated_at FROM documents
		WHERE scope = ? AND collection = ?
		ORDER BY created_at DESC
		LIMIT ?`

	getDocumentSQL = `SELECT id, data, created_at, updated_at FROM documents
		WHERE scope = ? AND collection = ? AND id = ?`

	updateDocumentSQL = `UPDATE documents SET data = ?4, updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now')
		WHERE scope = ?1 AND collection = ?2 AND id = ?3
		RETURNING id, created_at, updated_at`

	deleteDocumentSQL = `DELETE FROM documents WHERE scope = ? AND collection = ? AND id = ?`

	purgeScopeSQL = `DELETE FROM documents WHERE scope = ?`
)

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
