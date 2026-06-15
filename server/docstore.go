package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

var ErrNotFound = errors.New("document not found")

// ErrBadQuery marks a malformed list/query request (bad field name, unknown
// operator, oversized filter). Handlers map it to 400; its message is built
// only from validated tokens, so it is safe to return to the caller.
var ErrBadQuery = errors.New("invalid query")

// ErrFieldNotNumeric is returned by Increment when the target field exists but
// holds a non-numeric value, so it cannot be incremented.
var ErrFieldNotNumeric = errors.New("field is not a number")

type Document struct {
	ID        string         `json:"id"`
	Owner     string         `json:"owner"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// Filter is one field comparison in a List query. Op is one of eq, ne, gt,
// gte, lt, lte, in; for in, Value must be a slice.
type Filter struct {
	Field string
	Op    string
	Value any
}

// ListQuery describes an optional filtered and/or sorted listing. The zero
// value (plus a Limit) reproduces List: a collection's documents newest first.
// Sort names a top-level JSON field; cursor paging via After is valid only for
// the default order.
type ListQuery struct {
	Limit int
	Owner string
	After string
	Where []Filter
	Sort  string
	Order string
}

// docFieldRe constrains JSON field names used in filters and sorts so they can
// be embedded in a json path ("$."+field) without injection risk; the path
// itself is still passed as a bound parameter.
var docFieldRe = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)*$`)

var filterOps = map[string]string{
	"eq":  "=",
	"ne":  "<>",
	"gt":  ">",
	"gte": ">=",
	"lt":  "<",
	"lte": "<=",
}

const (
	maxFilters  = 16
	maxInValues = 100
)

// rowQuerier is satisfied by both *sql.DB and *sql.Tx, letting helpers run on
// an open transaction (important under MaxOpenConns(1), where a separate
// *sql.DB query would deadlock against a held transaction).
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
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
// starts after that document in the same newest-first order. It is Query with
// no filter or custom sort.
func (s *DocStore) List(ctx context.Context, scope, collection string, limit int, owner, afterID string) ([]Document, error) {
	return s.Query(ctx, scope, collection, ListQuery{Limit: limit, Owner: owner, After: afterID})
}

// Query lists documents with optional field filters and a sort. Field names
// are validated and JSON paths are passed as bound parameters, so neither a
// filter nor a sort can inject SQL. Cursor paging (After) applies only to the
// default newest-first order.
func (s *DocStore) Query(ctx context.Context, scope, collection string, q ListQuery) ([]Document, error) {
	var sb strings.Builder
	sb.WriteString(`SELECT id, owner, data, created_at, updated_at FROM documents WHERE scope = ? AND collection = ?`)
	args := []any{scope, collection}
	if q.Owner != "" {
		sb.WriteString(" AND owner = ?")
		args = append(args, q.Owner)
	}
	whereSQL, whereArgs, err := buildWhere(q.Where)
	if err != nil {
		return nil, err
	}
	sb.WriteString(whereSQL)
	args = append(args, whereArgs...)

	order, err := sortOrder(q.Order)
	if err != nil {
		return nil, err
	}
	if q.Sort != "" {
		if !docFieldRe.MatchString(q.Sort) {
			return nil, fmt.Errorf("%w: %q is not a valid sort field", ErrBadQuery, q.Sort)
		}
		sb.WriteString(" ORDER BY json_extract(data, ?) " + order + ", id DESC")
		args = append(args, "$."+q.Sort)
	} else {
		// Default order is by creation time. Honor an explicit asc/desc and keep
		// cursor paging working in whichever direction was requested.
		cmp := "<"
		if order == "ASC" {
			cmp = ">"
		}
		if q.After != "" {
			sb.WriteString(fmt.Sprintf(` AND (
			created_at %[1]s (SELECT created_at FROM documents WHERE scope = ? AND collection = ? AND id = ?)
			OR (created_at = (SELECT created_at FROM documents WHERE scope = ? AND collection = ? AND id = ?) AND id %[1]s ?)
		)`, cmp))
			args = append(args, scope, collection, q.After, scope, collection, q.After, q.After)
		}
		sb.WriteString(" ORDER BY created_at " + order + ", id " + order)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	sb.WriteString(" LIMIT ?")
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
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

// Count returns how many documents match the owner filter and where clause.
func (s *DocStore) Count(ctx context.Context, scope, collection, owner string, where []Filter) (int, error) {
	var sb strings.Builder
	sb.WriteString(`SELECT count(*) FROM documents WHERE scope = ? AND collection = ?`)
	args := []any{scope, collection}
	if owner != "" {
		sb.WriteString(" AND owner = ?")
		args = append(args, owner)
	}
	whereSQL, whereArgs, err := buildWhere(where)
	if err != nil {
		return 0, err
	}
	sb.WriteString(whereSQL)
	args = append(args, whereArgs...)

	var n int
	if err := s.db.QueryRowContext(ctx, sb.String(), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count documents: %w", err)
	}
	return n, nil
}

// GetMany returns the documents with the given ids that exist in the scope,
// newest first. Missing ids are omitted rather than erroring.
func (s *DocStore) GetMany(ctx context.Context, scope, collection string, ids []string) ([]Document, error) {
	return s.getMany(ctx, scope, collection, ids, "")
}

// GetManyOwned returns only documents owned by owner from the requested ids.
func (s *DocStore) GetManyOwned(ctx context.Context, scope, collection string, ids []string, owner string) ([]Document, error) {
	return s.getMany(ctx, scope, collection, ids, owner)
}

func (s *DocStore) getMany(ctx context.Context, scope, collection string, ids []string, owner string) ([]Document, error) {
	if len(ids) == 0 {
		return []Document{}, nil
	}
	var sb strings.Builder
	sb.WriteString(`SELECT id, owner, data, created_at, updated_at FROM documents WHERE scope = ? AND collection = ? AND id IN (`)
	args := []any{scope, collection}
	for i, id := range ids {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("?")
		args = append(args, id)
	}
	sb.WriteString(")")
	if owner != "" {
		sb.WriteString(" AND owner = ?")
		args = append(args, owner)
	}
	sb.WriteString(" ORDER BY created_at DESC, id DESC")

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("get documents: %w", err)
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

// buildWhere turns validated filters into a SQL fragment and its bound args.
// Every JSON path is a parameter and every operator comes from a fixed table,
// so the fragment carries no caller-controlled SQL text.
func buildWhere(filters []Filter) (string, []any, error) {
	if len(filters) == 0 {
		return "", nil, nil
	}
	if len(filters) > maxFilters {
		return "", nil, fmt.Errorf("%w: too many filters (max %d)", ErrBadQuery, maxFilters)
	}
	var sb strings.Builder
	var args []any
	for _, f := range filters {
		if !docFieldRe.MatchString(f.Field) {
			return "", nil, fmt.Errorf("%w: %q is not a valid field name", ErrBadQuery, f.Field)
		}
		path := "$." + f.Field
		switch f.Op {
		case "in":
			vals, ok := f.Value.([]any)
			if !ok {
				return "", nil, fmt.Errorf("%w: in on %q needs an array", ErrBadQuery, f.Field)
			}
			if len(vals) > maxInValues {
				return "", nil, fmt.Errorf("%w: in on %q exceeds %d values", ErrBadQuery, f.Field, maxInValues)
			}
			if len(vals) == 0 {
				sb.WriteString(" AND 0") // IN () matches nothing
				continue
			}
			sb.WriteString(" AND json_extract(data, ?) IN (")
			args = append(args, path)
			for i, v := range vals {
				if !isFilterScalar(v) {
					return "", nil, fmt.Errorf("%w: in on %q needs scalar values", ErrBadQuery, f.Field)
				}
				if i > 0 {
					sb.WriteString(",")
				}
				sb.WriteString("?")
				args = append(args, normalizeFilterValue(v))
			}
			sb.WriteString(")")
		case "eq", "ne":
			if f.Value == nil {
				sb.WriteString(" AND json_extract(data, ?) IS ")
				if f.Op == "ne" {
					sb.WriteString("NOT ")
				}
				sb.WriteString("NULL")
				args = append(args, path)
				continue
			}
			if _, isSlice := f.Value.([]any); isSlice {
				return "", nil, fmt.Errorf("%w: use in for array values on %q", ErrBadQuery, f.Field)
			}
			if !isFilterScalar(f.Value) {
				return "", nil, fmt.Errorf("%w: object values are not supported on %q", ErrBadQuery, f.Field)
			}
			sb.WriteString(" AND json_extract(data, ?) " + filterOps[f.Op] + " ?")
			args = append(args, path, normalizeFilterValue(f.Value))
		case "gt", "gte", "lt", "lte":
			if f.Value == nil {
				return "", nil, fmt.Errorf("%w: %s on %q needs a non-null value", ErrBadQuery, f.Op, f.Field)
			}
			if !isFilterScalar(f.Value) {
				return "", nil, fmt.Errorf("%w: %s on %q needs a scalar", ErrBadQuery, f.Op, f.Field)
			}
			sb.WriteString(" AND json_extract(data, ?) " + filterOps[f.Op] + " ?")
			args = append(args, path, normalizeFilterValue(f.Value))
		default:
			return "", nil, fmt.Errorf("%w: unknown operator %q", ErrBadQuery, f.Op)
		}
	}
	return sb.String(), args, nil
}

// sortOrder maps an optional asc/desc request onto the SQL keyword, defaulting
// to DESC (newest/highest first). An unrecognized value is a bad query so the
// parameter is never silently ignored.
func sortOrder(order string) (string, error) {
	switch {
	case order == "" || strings.EqualFold(order, "desc"):
		return "DESC", nil
	case strings.EqualFold(order, "asc"):
		return "ASC", nil
	default:
		return "", fmt.Errorf("%w: order must be asc or desc", ErrBadQuery)
	}
}

func isFilterScalar(v any) bool {
	switch v.(type) {
	case []any, map[string]any:
		return false
	default:
		return true
	}
}

// normalizeFilterValue maps a JSON bool to the 1/0 integer json_extract yields
// for JSON true/false, so equality against a boolean field works.
func normalizeFilterValue(v any) any {
	if b, ok := v.(bool); ok {
		if b {
			return 1
		}
		return 0
	}
	return v
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

// Increment atomically adds by to a numeric JSON field.
func (s *DocStore) Increment(ctx context.Context, scope, collection, id, field string, by float64) (Document, error) {
	return s.increment(ctx, scope, collection, id, "", field, by)
}

// IncrementOwned increments a field only when the document belongs to owner.
func (s *DocStore) IncrementOwned(ctx context.Context, scope, collection, id, owner, field string, by float64) (Document, error) {
	return s.increment(ctx, scope, collection, id, owner, field, by)
}

// increment adds by to a numeric (or absent, treated as 0) JSON field in a
// single statement, so concurrent counters never lose updates. A field holding
// a non-numeric value yields ErrFieldNotNumeric; a missing document or owner
// mismatch yields ErrNotFound.
func (s *DocStore) increment(ctx context.Context, scope, collection, id, owner, field string, by float64) (Document, error) {
	if !docFieldRe.MatchString(field) {
		return Document{}, fmt.Errorf("%w: %q is not a valid field name", ErrBadQuery, field)
	}
	if math.IsNaN(by) || math.IsInf(by, 0) {
		return Document{}, fmt.Errorf("%w: increment amount must be a finite number", ErrBadQuery)
	}
	path := "$." + field
	if by == 0 {
		// A zero delta changes nothing: return the current document without a
		// write or a realtime event, while keeping the same ownership and
		// numeric-field guarantees as a real increment (404 / 409).
		var doc Document
		var raw []byte
		err := s.db.QueryRowContext(ctx, incrementReadSQL, scope, collection, id, path, owner).
			Scan(&doc.ID, &doc.Owner, &raw, &doc.CreatedAt, &doc.UpdatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return Document{}, classifyIncrementMiss(ctx, s.db, scope, collection, id, owner, path)
		}
		if err != nil {
			return Document{}, fmt.Errorf("read document: %w", err)
		}
		if err := json.Unmarshal(raw, &doc.Data); err != nil {
			return Document{}, fmt.Errorf("decode document: %w", err)
		}
		return doc, nil
	}
	// Store an in-range whole amount as an integer so counters stay integers in
	// JSON; larger finite whole numbers stay float64 to avoid int64 overflow.
	var amount any = by
	const (
		minInt64Float          = -9223372036854775808.0
		maxInt64ExclusiveFloat = 9223372036854775808.0
	)
	if by == math.Trunc(by) && by >= minInt64Float && by < maxInt64ExclusiveFloat {
		amount = int64(by)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Document{}, fmt.Errorf("begin increment: %w", err)
	}
	defer tx.Rollback()

	var doc Document
	var raw []byte
	err = tx.QueryRowContext(ctx, incrementDocumentSQL, scope, collection, id, path, amount, owner).
		Scan(&doc.ID, &doc.Owner, &raw, &doc.CreatedAt, &doc.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// No row matched: the document is missing/owned by someone else, or the
		// field is non-numeric (the type guard in the statement excluded it).
		// Classify on the same transaction to give a useful error.
		return Document{}, classifyIncrementMiss(ctx, tx, scope, collection, id, owner, path)
	}
	if err != nil {
		return Document{}, fmt.Errorf("increment document: %w", err)
	}
	if err := json.Unmarshal(raw, &doc.Data); err != nil {
		return Document{}, fmt.Errorf("decode document: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Document{}, fmt.Errorf("commit increment: %w", err)
	}
	s.publishChange("update", scope, collection, id, &doc)
	return doc, nil
}

// classifyIncrementMiss explains why an increment matched no row.
func classifyIncrementMiss(ctx context.Context, q rowQuerier, scope, collection, id, owner, path string) error {
	var typ sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT json_type(data, ?) FROM documents
		 WHERE scope = ? AND collection = ? AND id = ? AND (? = '' OR owner = ?)`,
		path, scope, collection, id, owner, owner,
	).Scan(&typ)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("classify increment miss: %w", err)
	}
	// The document exists. A present, non-numeric field is why it was rejected;
	// a null/absent field would have matched, so anything else is an ownership
	// mismatch, which we report as not found (as Update/Delete do).
	if typ.Valid && typ.String != "integer" && typ.String != "real" {
		return ErrFieldNotNumeric
	}
	return ErrNotFound
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

	// The type guard makes a non-numeric field match no row, which increment
	// distinguishes from a missing document; a null/absent field is treated as
	// 0 by the coalesce.
	incrementDocumentSQL = `UPDATE documents
		SET data = json_set(data, ?4, coalesce(json_extract(data, ?4), 0) + ?5),
		    updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now')
		WHERE scope = ?1 AND collection = ?2 AND id = ?3 AND (?6 = '' OR owner = ?6)
		  AND (json_type(data, ?4) IS NULL OR json_type(data, ?4) IN ('integer', 'real'))
		RETURNING id, owner, data, created_at, updated_at`

	// incrementReadSQL mirrors incrementDocumentSQL's WHERE (owner filter and
	// numeric-field guard) but only reads, so a zero-delta increment returns the
	// current document with the same 404/409 contract and without a write.
	incrementReadSQL = `SELECT id, owner, data, created_at, updated_at FROM documents
		WHERE scope = ?1 AND collection = ?2 AND id = ?3 AND (?5 = '' OR owner = ?5)
		  AND (json_type(data, ?4) IS NULL OR json_type(data, ?4) IN ('integer', 'real'))`

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
