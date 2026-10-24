// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package courier

import (
	"encoding/json"
	"time"

	"github.com/gofrs/uuid"
	"github.com/pkg/errors"

	"github.com/ory/herodot"
	"github.com/ory/x/stringsx"
)

// The operational status of a network's courier queue.
//
// swagger:model courierQueueStatus
type QueueStatus int

const (
	// QueueStatusRunning is the default state: workers fetch and dispatch queued messages.
	QueueStatusRunning QueueStatus = iota + 1
	// QueueStatusPaused keeps the queue intact but stops workers from fetching new messages.
	QueueStatusPaused
)

const (
	queueStatusRunningText = "running"
	queueStatusPausedText  = "paused"
)

func ToQueueStatus(str string) (QueueStatus, error) {
	switch s := stringsx.SwitchExact(str); {
	case s.AddCase(QueueStatusRunning.String()):
		return QueueStatusRunning, nil
	case s.AddCase(QueueStatusPaused.String()):
		return QueueStatusPaused, nil
	default:
		return 0, errors.WithStack(herodot.ErrBadRequest().WithWrap(s.ToUnknownCaseErr()).WithReason("Queue status is not valid"))
	}
}

func (qs QueueStatus) String() string {
	switch qs {
	case QueueStatusRunning:
		return queueStatusRunningText
	case QueueStatusPaused:
		return queueStatusPausedText
	default:
		return ""
	}
}

func (qs QueueStatus) IsValid() error {
	switch qs {
	case QueueStatusRunning, QueueStatusPaused:
		return nil
	default:
		return errors.WithStack(herodot.ErrBadRequest().WithReason("Queue status is not valid"))
	}
}

func (qs QueueStatus) MarshalJSON() ([]byte, error) {
	if err := qs.IsValid(); err != nil {
		return nil, err
	}
	return json.Marshal(qs.String())
}

func (qs *QueueStatus) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}

	s, err := ToQueueStatus(str)
	if err != nil {
		return err
	}

	*qs = s
	return nil
}

// Queue is the persisted delivery state of the courier queue for a single network.
//
// There is at most one row per network. A missing row is equivalent to the
// running state, so networks which were never paused never need a row.
//
// swagger:model courierQueue
type Queue struct {
	NID uuid.UUID `json:"-" faker:"-" db:"nid"`

	// The delivery status of the courier queue.
	//
	// required: true
	Status QueueStatus `json:"status" db:"status"`

	CreatedAt time.Time `json:"-" faker:"-" db:"created_at"`
	UpdatedAt time.Time `json:"-" faker:"-" db:"updated_at"`
}

func (Queue) TableName() string {
	return "courier_queues"
}
