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
	Owner     string         `json:"owner"`
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

// Create stores a new document. owner is the stable identity key of the
// visitor who created it (see actorKey); it is "" when the request could
// not be attributed to an identity, which keeps open, resolver-less
// installs working.
func (s *DocStore) Create(ctx context.Context, scope, collection, owner string, data map[string]any) (Document, error) {
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
	doc.Owner = owner
	doc.Data = data
	err = tx.QueryRowContext(ctx, insertDocumentSQL, scope, collection, owner, raw).
		Scan(&doc.ID, &doc.CreatedAt, &doc.UpdatedAt)
	if err != nil {
		return Document{}, fmt.Errorf("insert document: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Document{}, fmt.Errorf("commit insert: %w", err)
	}
	s.publishChange("create", scope, collection, doc.ID, &doc)
	return doc, nil
}

// List returns a collection's documents, newest first. When owner is
// non-empty, only documents stamped with that owner key are returned; an
// empty owner returns every document in the scope. afterID, when present,
// starts after that document in the same newest-first order.
func (s *DocStore) List(ctx context.Context, scope, collection string, limit int, owner, afterID string) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx,
		listDocumentsSQL,
		scope, collection, owner, limit, afterID,
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
	return s.update(ctx, scope, collection, id, "", data)
}

// UpdateOwned updates a document only when it belongs to owner. It returns
// ErrNotFound for missing documents and owner mismatches so callers do not
// leak another visitor's document IDs.
func (s *DocStore) UpdateOwned(ctx context.Context, scope, collection, id, owner string, data map[string]any) (Document, error) {
	return s.update(ctx, scope, collection, id, owner, data)
}

func (s *DocStore) update(ctx context.Context, scope, collection, id, owner string, data map[string]any) (Document, error) {
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
	err = tx.QueryRowContext(ctx, updateDocumentSQL, scope, collection, id, raw, owner).
		Scan(&doc.ID, &doc.Owner, &doc.CreatedAt, &doc.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("update document: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Document{}, fmt.Errorf("commit update: %w", err)
	}
	s.publishChange("update", scope, collection, id, &doc)
	return doc, nil
}

func (s *DocStore) Delete(ctx context.Context, scope, collection, id string) error {
	return s.delete(ctx, scope, collection, id, "")
}

// DeleteOwned deletes a document only when it belongs to owner. It returns
// ErrNotFound for missing documents and owner mismatches.
func (s *DocStore) DeleteOwned(ctx context.Context, scope, collection, id, owner string) error {
	return s.delete(ctx, scope, collection, id, owner)
}

func (s *DocStore) delete(ctx context.Context, scope, collection, id, owner string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		deleteDocumentSQL,
		scope, collection, id, owner,
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
	s.publishChange("delete", scope, collection, id, nil)
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

func (s *DocStore) publishChange(action, scope, collection, id string, doc *Document) {
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
	insertDocumentSQL = `INSERT INTO documents (scope, collection, owner, data)
		VALUES (?, ?, ?, ?)
		RETURNING id, created_at, updated_at`

	listDocumentsSQL = `SELECT id, owner, data, created_at, updated_at FROM documents
		WHERE scope = ?1 AND collection = ?2 AND (?3 = '' OR owner = ?3)
		  AND (?5 = '' OR (
			created_at < (SELECT created_at FROM documents WHERE scope = ?1 AND collection = ?2 AND id = ?5)
			OR (
				created_at = (SELECT created_at FROM documents WHERE scope = ?1 AND collection = ?2 AND id = ?5)
				AND id < ?5
			)
		  ))
		ORDER BY created_at DESC, id DESC
		LIMIT ?4`

	getDocumentSQL = `SELECT id, owner, data, created_at, updated_at FROM documents
		WHERE scope = ? AND collection = ? AND id = ?`

	updateDocumentSQL = `UPDATE documents SET data = ?4, updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now')
		WHERE scope = ?1 AND collection = ?2 AND id = ?3 AND (?5 = '' OR owner = ?5)
		RETURNING id, owner, created_at, updated_at`

	deleteDocumentSQL = `DELETE FROM documents WHERE scope = ?1 AND collection = ?2 AND id = ?3 AND (?4 = '' OR owner = ?4)`

	purgeScopeSQL = `DELETE FROM documents WHERE scope = ?`
)

func scanDocument(scan func(dest ...any) error) (Document, error) {
	var doc Document
	var raw []byte
	if err := scan(&doc.ID, &doc.Owner, &raw, &doc.CreatedAt, &doc.UpdatedAt); err != nil {
		return Document{}, err
	}
	if err := json.Unmarshal(raw, &doc.Data); err != nil {
		return Document{}, fmt.Errorf("decode document: %w", err)
	}
	return doc, nil
}
