package sourcelog

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"time"
)

const maxResponseLimit = 100

// ResponseMeta describes a captured upstream response without its body, so
// listings stay small.
type ResponseMeta struct {
	ID          int64  `json:"id"`
	Source      string `json:"source"`
	Subject     string `json:"subject"`
	URL         string `json:"url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	FetchedAt   int64  `json:"fetched_at"` // unix seconds
	Size        int    `json:"size"`       // uncompressed bytes
}

const responseMetaColumns = `id, source, subject, url, status, content_type, fetched_at, size`

type rowScanner interface{ Scan(dest ...any) error }

func scanMeta(r rowScanner, extra ...any) (ResponseMeta, error) {
	var m ResponseMeta
	err := r.Scan(append([]any{&m.ID, &m.Source, &m.Subject, &m.URL, &m.Status, &m.ContentType, &m.FetchedAt, &m.Size}, extra...)...)
	return m, err
}

// Responses lists captures newest first. before is inclusive and a zero
// before means now; an empty subject matches every subject of the source.
// Both shapes are served by the (source, subject, fetched_at) index.
func (l *Log) Responses(ctx context.Context, source, subject string, before time.Time, limit int) ([]ResponseMeta, error) {
	if before.IsZero() {
		before = l.now()
	}
	limit = clamp(limit, 1, maxResponseLimit)
	q := `SELECT ` + responseMetaColumns + ` FROM responses WHERE source = ? AND fetched_at <= ?`
	args := []any{source, before.Unix()}
	if subject != "" {
		q += ` AND subject = ?`
		args = append(args, subject)
	}
	q += ` ORDER BY fetched_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResponseMeta{}
	for rows.Next() {
		m, err := scanMeta(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Response returns one capture with its body decompressed; found is false
// for an unknown id.
func (l *Log) Response(ctx context.Context, id int64) (ResponseMeta, []byte, bool, error) {
	var blob []byte
	m, err := scanMeta(l.db.QueryRowContext(ctx,
		`SELECT `+responseMetaColumns+`, body FROM responses WHERE id = ?`, id), &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return ResponseMeta{}, nil, false, nil
	}
	if err != nil {
		return ResponseMeta{}, nil, false, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return ResponseMeta{}, nil, false, err
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		return ResponseMeta{}, nil, false, err
	}
	return m, body, true, nil
}
