// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package courier_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/gofrs/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ory/kratos/courier"
	templates "github.com/ory/kratos/courier/template/email"
	"github.com/ory/kratos/driver/config"
	"github.com/ory/kratos/pkg"
	"github.com/ory/kratos/pkg/testhelpers"
	"github.com/ory/kratos/x/events"
	"github.com/ory/x/configx"
	"github.com/ory/x/otelx"
	"github.com/ory/x/otelx/semconv"
)

func queueNewMessage(t *testing.T, c courier.Courier) uuid.UUID {
	t.Helper()
	id, err := c.QueueEmail(t.Context(), templates.NewTestStub(&templates.TestStubModel{
		To:      "test-recipient-1@example.org",
		Subject: "test-subject-1",
		Body:    "test-body-1",
	}))
	require.NoError(t, err)
	return id
}

func TestDispatchMessageWithInvalidSMTP(t *testing.T) {
	_, reg := pkg.NewRegistryDefaultWithDSN(t, "", configx.WithValues(map[string]any{
		config.ViperKeyCourierMessageRetries: 5,
		config.ViperKeyCourierSMTPURL:        "http://foo.url",
	}))

	c, err := reg.Courier(t.Context())
	require.NoError(t, err)

	t.Run("case=failed sending", func(t *testing.T) {
		id := queueNewMessage(t, c)
		message, err := reg.CourierPersister().LatestQueuedMessage(t.Context())
		require.NoError(t, err)
		require.Equal(t, id, message.ID)

		err = c.DispatchMessage(t.Context(), *message)
		// sending the email fails, because there is no SMTP server at foo.url
		require.Error(t, err)

		messages, err := reg.CourierPersister().NextMessages(t.Context(), 10)
		require.NoError(t, err)
		require.Len(t, messages, 1)
	})
}

func TestDispatchMessage(t *testing.T) {
	_, reg := pkg.NewRegistryDefaultWithDSN(t, "", configx.WithValues(map[string]any{
		config.ViperKeyCourierMessageRetries: 5,
		config.ViperKeyCourierSMTPURL:        "http://foo.url",
	}))

	c, err := reg.Courier(t.Context())
	require.NoError(t, err)
	t.Run("case=invalid channel", func(t *testing.T) {
		message := courier.Message{
			Channel:      "invalid-channel",
			Status:       courier.MessageStatusQueued,
			Type:         courier.MessageTypeEmail,
			Recipient:    testhelpers.RandomEmail(),
			Subject:      "test-subject-1",
			Body:         "test-body-1",
			TemplateType: "stub",
		}
		require.NoError(t, reg.CourierPersister().AddMessage(t.Context(), &message))
		assert.ErrorContains(t, c.DispatchMessage(t.Context(), message), "no courier channels configured for: invalid-channel")
	})
}

func TestDispatchQueue(t *testing.T) {
	_, reg := pkg.NewRegistryDefaultWithDSN(t, "", configx.WithValue(config.ViperKeyCourierMessageRetries, 1))

	c, err := reg.Courier(t.Context())
	require.NoError(t, err)
	c.FailOnDispatchError()

	id := queueNewMessage(t, c)
	require.NotEqual(t, uuid.Nil, id)

	// Fails to deliver the first time
	err = c.DispatchQueue(t.Context())
	require.Error(t, err)

	// Retry once, as we set above - still fails
	err = c.DispatchQueue(t.Context())
	require.Error(t, err)

	// Now it has been retried once, which means 2 > 1 is true and it is no longer tried
	err = c.DispatchQueue(t.Context())
	require.NoError(t, err)

	var message courier.Message
	err = reg.Persister().GetConnection(t.Context()).
		Where("status = ?", courier.MessageStatusAbandoned).
		Eager("Dispatches").
		First(&message)

	require.NoError(t, err)
	require.Equal(t, id, message.ID)

	require.Len(t, message.Dispatches, 2)
	require.Contains(t, gjson.GetBytes(message.Dispatches[0].Error, "reason").String(), "failed to send email via smtp")
	require.Contains(t, gjson.GetBytes(message.Dispatches[1].Error, "reason").String(), "failed to send email via smtp")
}

func TestDispatchMessageEmitsEventWithNID(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	requestConfig := fmt.Sprintf(`{"url": "%s", "method": "POST", "body": "file://./stub/request.config.mailer.jsonnet"}`, srv.URL)

	_, reg := pkg.NewFastRegistryWithMocks(t, configx.WithValues(map[string]any{
		config.ViperKeyCourierDeliveryStrategy:  "http",
		config.ViperKeyCourierHTTPRequestConfig: requestConfig,
	}))

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	reg.SetTracer(otelx.NewNoop().WithOTLP(provider.Tracer("test")))

	c, err := reg.Courier(t.Context())
	require.NoError(t, err)

	_, err = c.QueueEmail(t.Context(), templates.NewTestStub(&templates.TestStubModel{
		To:      testhelpers.RandomEmail(),
		Subject: "test-subject",
		Body:    "test-body",
	}))
	require.NoError(t, err)

	require.NoError(t, c.DispatchQueue(t.Context()))

	ended := recorder.Ended()
	i := slices.IndexFunc(ended, func(sp sdktrace.ReadOnlySpan) bool {
		return sp.Name() == "courier.DispatchMessage"
	})
	require.GreaterOrEqual(t, i, 0, "courier.DispatchMessage span not found")

	evs := ended[i].Events()
	j := slices.IndexFunc(evs, func(ev sdktrace.Event) bool {
		return ev.Name == events.CourierMessageDispatched.String()
	})
	require.GreaterOrEqual(t, j, 0, "CourierMessageDispatched event not found on span")

	attrs := evs[j].Attributes
	k := slices.IndexFunc([]attribute.KeyValue(attrs), func(a attribute.KeyValue) bool {
		return string(a.Key) == semconv.AttributeKeyNID.String()
	})
	require.GreaterOrEqual(t, k, 0, "NID attribute not found on event")
	assert.NotEmpty(t, attrs[k].Value.AsString())
}

func TestDispatchQueuePauseResume(t *testing.T) {
	t.Parallel()

	var deliveredRequests int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&deliveredRequests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	requestConfig := fmt.Sprintf(`{"url": "%s", "method": "POST", "body": "file://./stub/request.config.mailer.jsonnet"}`, srv.URL)

	_, reg := pkg.NewFastRegistryWithMocks(t, configx.WithValues(map[string]any{
		config.ViperKeyCourierDeliveryStrategy:  "http",
		config.ViperKeyCourierHTTPRequestConfig: requestConfig,
	}))

	worker, err := reg.Courier(t.Context())
	require.NoError(t, err)
	// A second courier, constructed after the pause, models a worker that
	// restarted while the queue is paused.
	restartedWorker, err := reg.Courier(t.Context())
	require.NoError(t, err)

	assertMessageState := func(id uuid.UUID, status courier.MessageStatus, sendCount int, dispatchCount int) courier.Message {
		message, err := reg.CourierPersister().FetchMessage(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, status, message.Status)
		assert.Equal(t, sendCount, message.SendCount)
		assert.Len(t, message.Dispatches, dispatchCount)
		return *message
	}

	t.Run("case=workers skip dispatch while the queue is paused", func(t *testing.T) {
		id := queueNewMessage(t, worker)

		require.NoError(t, reg.CourierPersister().PauseQueue(t.Context()))
		// Repeated pauses are idempotent.
		require.NoError(t, reg.CourierPersister().PauseQueue(t.Context()))

		// Neither the running worker nor a freshly started worker pull messages.
		require.NoError(t, worker.DispatchQueue(t.Context()))
		require.NoError(t, restartedWorker.DispatchQueue(t.Context()))

		// The message is untouched: it stays queued, no retry is counted and no
		// dispatch attempt is recorded.
		assertMessageState(id, courier.MessageStatusQueued, 0, 0)
		assert.Zero(t, atomic.LoadInt64(&deliveredRequests))

		// New messages can still be enqueued while delivery is paused.
		require.NotEqual(t, uuid.Nil, queueNewMessage(t, worker))
		assertMessageState(id, courier.MessageStatusQueued, 0, 0)
	})

	t.Run("case=a batch fetched before the pause still completes", func(t *testing.T) {
		// Claim the oldest of the two messages that accumulated while paused.
		// This simulates a batch that was fetched just before the pause took
		// effect.
		claimed, err := reg.CourierPersister().NextMessages(t.Context(), 1)
		require.NoError(t, err)
		require.Len(t, claimed, 1)

		require.NoError(t, reg.CourierPersister().PauseQueue(t.Context()))

		// Delivery of a claimed message never consults the pause flag.
		require.NoError(t, worker.DispatchMessage(t.Context(), claimed[0]))
		assertMessageState(claimed[0].ID, courier.MessageStatusSent, 1, 0)
	})

	t.Run("case=processing resumes with the regular queue rules", func(t *testing.T) {
		// The queue is still paused from the previous case; resume it.
		require.NoError(t, reg.CourierPersister().ResumeQueue(t.Context()))
		// Repeated resumes are idempotent.
		require.NoError(t, reg.CourierPersister().ResumeQueue(t.Context()))

		before := atomic.LoadInt64(&deliveredRequests)
		require.NoError(t, worker.DispatchQueue(t.Context()))

		// Two messages were enqueued during the first case. The oldest one was
		// delivered as part of the pre-pause batch, so the remaining one is
		// delivered now that the queue runs again.
		assert.Equal(t, before+1, atomic.LoadInt64(&deliveredRequests))

		// A subsequent poll finds an empty queue.
		require.NoError(t, worker.DispatchQueue(t.Context()))
		assert.Equal(t, before+1, atomic.LoadInt64(&deliveredRequests))
	})
}
