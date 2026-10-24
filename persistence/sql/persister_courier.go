// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package sql

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/gofrs/uuid"
	"github.com/pkg/errors"

	"github.com/ory/herodot"
	"github.com/ory/kratos/courier"
	"github.com/ory/kratos/persistence/sql/update"
	"github.com/ory/pop/v6"
	"github.com/ory/x/dbal"
	"github.com/ory/x/otelx"
	keysetpagination "github.com/ory/x/pagination/keysetpagination_v2"
	"github.com/ory/x/sqlcon"
	"github.com/ory/x/uuidx"
)

var _ courier.Persister = new(Persister)

func (p *Persister) AddMessage(ctx context.Context, m *courier.Message) (err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.AddMessage")
	defer otelx.End(span, &err)

	m.NID = p.NetworkID(ctx)
	m.Status = courier.MessageStatusQueued
	return sqlcon.HandleError(p.GetConnection(ctx).Create(m)) // do not create eager to avoid identity injection.
}

func (p *Persister) ListMessages(ctx context.Context, filter courier.ListCourierMessagesParameters, opts []keysetpagination.Option) (_ []courier.Message, _ *keysetpagination.Paginator, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.ListMessages")
	defer otelx.End(span, &err)

	q := p.GetConnection(ctx).Where("nid=?", p.NetworkID(ctx))

	if filter.Status != nil {
		q = q.Where("status=?", *filter.Status)
	}

	if filter.Recipient != "" {
		q = q.Where("recipient=?", filter.Recipient)
	}

	opts = append(opts, keysetpagination.WithDefaultToken(courier.Message{}.DefaultPageToken()))
	opts = append(opts, keysetpagination.WithDefaultSize(10))
	paginator, err := keysetpagination.NewPaginator(opts...)
	if err != nil {
		return nil, nil, err
	}

	messages := make([]courier.Message, paginator.Size())
	if err := q.Scope(keysetpagination.Paginate[courier.Message](paginator)).
		All(&messages); err != nil {
		return nil, nil, sqlcon.HandleError(err)
	}

	messages, nextPage := keysetpagination.Result(messages, paginator)

	return messages, nextPage, nil
}

func (p *Persister) NextMessages(ctx context.Context, limit uint8) (messages []courier.Message, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.NextMessages")
	defer otelx.End(span, &err)

	if err := p.Transaction(ctx, func(ctx context.Context, tx *pop.Connection) error {
		q := tx.Where("nid = ? AND status = ?",
			p.NetworkID(ctx),
			courier.MessageStatusQueued,
		)
		if tx.Dialect.Name() == dbal.DriverCockroachDB {
			// On CockroachDB, exclude messages enqueued within the last second.
			// Concurrent INSERTs land at created_at ~ now; keeping them out of
			// the scanned span avoids read-write contention between this poll
			// and message enqueueing, which surfaces as transaction retries
			// (ReadWithinUncertaintyIntervalError). Skipped messages are picked
			// up by a subsequent poll. The other databases serve this query
			// from an MVCC snapshot that does not conflict with concurrent
			// inserts, so they keep the exact current behavior.
			q = q.Where("created_at < ?", time.Now().UTC().Add(-time.Second))
		}

		var m []courier.Message
		if err := q.
			Order("created_at ASC").
			Limit(int(limit)).
			All(&m); err != nil {
			return err
		}

		if len(m) == 0 {
			return sql.ErrNoRows
		}

		for i := range m {
			message := &m[i]
			message.Status = courier.MessageStatusProcessing
			if err := update.Generic(ctx, tx, p.r.Tracer(ctx).Tracer(), message, "status"); err != nil {
				return err
			}
		}

		messages = m
		return nil
	}); err != nil {
		if errors.Cause(err) == sql.ErrNoRows {
			return nil, errors.WithStack(courier.ErrQueueEmpty)
		}
		return nil, sqlcon.HandleError(err)
	}

	if len(messages) == 0 {
		return nil, errors.WithStack(courier.ErrQueueEmpty)
	}

	return messages, nil
}

func (p *Persister) LatestQueuedMessage(ctx context.Context) (_ *courier.Message, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.LatestQueuedMessage")
	defer otelx.End(span, &err)

	var m courier.Message
	if err := p.GetConnection(ctx).
		Where("nid = ? AND status = ?",
			p.NetworkID(ctx),
			courier.MessageStatusQueued,
		).
		Order("created_at DESC").
		First(&m); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.WithStack(courier.ErrQueueEmpty)
		}
		return nil, sqlcon.HandleError(err)
	}

	return &m, nil
}

func (p *Persister) SetMessageStatus(ctx context.Context, id uuid.UUID, ms courier.MessageStatus) (err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.SetMessageStatus")
	defer otelx.End(span, &err)

	count, err := p.GetConnection(ctx).RawQuery(
		"UPDATE courier_messages SET status = ? WHERE id = ? AND nid = ?",
		ms,
		id,
		p.NetworkID(ctx),
	).ExecWithCount()
	if err != nil {
		return sqlcon.HandleError(err)
	}

	if count == 0 {
		return errors.WithStack(sqlcon.ErrNoRows())
	}

	return nil
}

func (p *Persister) IncrementMessageSendCount(ctx context.Context, id uuid.UUID) (err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.IncrementMessageSendCount")
	defer otelx.End(span, &err)

	count, err := p.GetConnection(ctx).RawQuery(
		"UPDATE courier_messages SET send_count = send_count + 1 WHERE id = ? AND nid = ?",
		id,
		p.NetworkID(ctx),
	).ExecWithCount()
	if err != nil {
		return sqlcon.HandleError(err)
	}

	if count == 0 {
		return errors.WithStack(sqlcon.ErrNoRows())
	}

	return nil
}

func (p *Persister) FetchMessage(ctx context.Context, msgID uuid.UUID) (_ *courier.Message, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.FetchMessage")
	defer otelx.End(span, &err)

	var message courier.Message
	if err := p.GetConnection(ctx).
		Where("id = ? AND nid = ?", msgID, p.NetworkID(ctx)).
		Eager().
		First(&message); err != nil {
		return nil, sqlcon.HandleError(err)
	}

	return &message, nil
}

func (p *Persister) PauseQueue(ctx context.Context) error {
	return p.setQueueStatus(ctx, courier.QueueStatusPaused)
}

func (p *Persister) ResumeQueue(ctx context.Context) error {
	return p.setQueueStatus(ctx, courier.QueueStatusRunning)
}

// setQueueStatus stores the queue status for the current network as an
// idempotent upsert. Repeating the same status is a no-op that does not even
// touch the row's timestamps, so pausing a paused queue (and resuming a running
// queue) are fully idempotent.
func (p *Persister) setQueueStatus(ctx context.Context, status courier.QueueStatus) (err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.setQueueStatus")
	defer otelx.End(span, &err)

	nid := p.NetworkID(ctx)
	now := time.Now().UTC()
	conn := p.GetConnection(ctx)

	var stmt string
	var args []interface{}
	switch conn.Dialect.Name() {
	case dbal.DriverMySQL:
		stmt = `
INSERT INTO courier_queues (nid, status, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  status = VALUES(status),
  updated_at = IF(courier_queues.status = VALUES(status), courier_queues.updated_at, VALUES(updated_at))`
		args = []interface{}{nid, status, now, now}
	default:
		// PostgreSQL, CockroachDB and SQLite all support the same
		// INSERT ... ON CONFLICT (nid) DO UPDATE syntax.
		stmt = `
INSERT INTO courier_queues (nid, status, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (nid) DO UPDATE SET
  status = EXCLUDED.status,
  updated_at = CASE WHEN courier_queues.status = EXCLUDED.status
                    THEN courier_queues.updated_at ELSE EXCLUDED.updated_at END`
		args = []interface{}{nid, status, now, now}
	}

	if _, err := conn.RawQuery(stmt, args...).ExecWithCount(); err != nil {
		return sqlcon.HandleError(err)
	}

	return nil
}

func (p *Persister) GetQueueStatus(ctx context.Context) (_ courier.QueueStatus, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetQueueStatus")
	defer otelx.End(span, &err)

	var q courier.Queue
	switch err := p.GetConnection(ctx).RawQuery(
		"SELECT nid, status, created_at, updated_at FROM courier_queues WHERE nid = ?",
		p.NetworkID(ctx),
	).First(&q); {
	case err == nil:
		return q.Status, nil
	case errors.Is(err, sql.ErrNoRows):
		// A network that has never been paused is running.
		return courier.QueueStatusRunning, nil
	default:
		return 0, sqlcon.HandleError(err)
	}
}

func (p *Persister) RecordDispatch(ctx context.Context, msgID uuid.UUID, status courier.CourierMessageDispatchStatus, err error) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.RecordDispatch")
	defer otelx.End(span, &err)

	dispatch := courier.MessageDispatch{
		ID:        uuidx.NewV4(),
		MessageID: msgID,
		Status:    status,
		NID:       p.NetworkID(ctx),
	}

	if err != nil {
		// We use herodot as a carrier for the error's data
		her := herodot.ToDefaultError(err, "")
		content, mErr := json.Marshal(her)
		if mErr != nil {
			return errors.WithStack(mErr)
		}
		dispatch.Error = content
	}

	if err := p.GetConnection(ctx).Create(&dispatch); err != nil {
		return sqlcon.HandleError(err)
	}

	return nil
}
