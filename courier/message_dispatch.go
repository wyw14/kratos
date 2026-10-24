// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package courier

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/pkg/errors"

	"github.com/ory/herodot"

	"github.com/ory/kratos/courier/template"
	"github.com/ory/x/sqlxx"
)

// swagger:enum CourierMessageDispatchStatus
type CourierMessageDispatchStatus string

const (
	CourierMessageDispatchStatusFailed  CourierMessageDispatchStatus = "failed"
	CourierMessageDispatchStatusSuccess CourierMessageDispatchStatus = "success"
)

// MessageDispatch represents an attempt of sending a courier message
// It contains the status of the attempt (failed or successful) and the error if any occured
//
// swagger:model messageDispatch
type MessageDispatch struct {
	// The ID of this message dispatch
	// required: true
	ID uuid.UUID `json:"id" db:"id"`

	// The ID of the message being dispatched
	// required: true
	MessageID uuid.UUID `json:"message_id" db:"message_id"`

	// The status of this dispatch
	// Either "failed" or "success"
	// required: true
	Status CourierMessageDispatchStatus `json:"status" db:"status"`

	// An optional error
	Error sqlxx.JSONRawMessage `json:"error,omitempty" db:"error"`

	// CreatedAt is a helper struct field for gobuffalo.pop.
	// required: true
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	// UpdatedAt is a helper struct field for gobuffalo.pop.
	// required: true
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`

	NID uuid.UUID `json:"-" db:"nid"`
}

func (MessageDispatch) TableName() string {
	return "courier_message_dispatches"
}

// PreviewMessageDispatchRequest contains the input for an offline courier
// message template preview.
//
// swagger:model previewMessageDispatchRequest
type PreviewMessageDispatchRequest struct {
	// Channel is the message channel to preview: "email" or "sms".
	//
	// required: true
	Channel string `json:"channel"`

	// TemplateType is the courier template type to render.
	//
	// required: true
	TemplateType template.TemplateType `json:"template_type"`

	// Locale is the locale (for example "en_US") used to render localized
	// templates. If it is empty the configured courier.template_default_locale
	// is used.
	Locale string `json:"locale,omitempty"`

	// Identity is the identity context exposed to the template as .Identity.
	Identity map[string]interface{} `json:"identity,omitempty"`

	// Data holds additional template variables exposed to the template.
	Data map[string]interface{} `json:"data,omitempty"`
}

// PreviewTemplateVersion describes the version of a single rendered template
// part.
//
// swagger:model previewTemplateVersion
type PreviewTemplateVersion struct {
	// Part is the template part: "subject", "body" or "body_plaintext".
	//
	// required: true
	Part string `json:"part"`

	// Path is the resolved template path, or the remote URL for remotely
	// configured templates.
	//
	// required: true
	Path string `json:"path"`

	// Locale is the locale variant that was rendered. It is empty when the
	// unlocalized default template was rendered.
	Locale string `json:"locale,omitempty"`

	// Fallback is true when a locale was requested but the template only
	// existed for a different locale or the unlocalized default.
	Fallback bool `json:"fallback"`

	// Remote is true when the template was loaded from a configured remote URL.
	Remote bool `json:"remote"`

	// Version is the hex-encoded SHA-256 digest of the template content.
	//
	// required: true
	Version string `json:"version"`
}

// PreviewMessageDispatch is the result of rendering a courier message
// template offline. It is never enqueued or sent.
//
// swagger:model previewMessageDispatch
type PreviewMessageDispatch struct {
	// required: true
	Channel string `json:"channel"`

	// required: true
	TemplateType template.TemplateType `json:"template_type"`

	// Locale is the locale that was requested for the preview. The resolved
	// locale (including fallbacks) is reported per entry in TemplateVersions.
	//
	// required: true
	Locale string `json:"locale"`

	// Subject is the rendered message subject (email only).
	Subject string `json:"subject,omitempty"`

	// Body is the rendered HTML body for email messages and the rendered
	// message body for SMS messages.
	//
	// required: true
	Body string `json:"body"`

	// BodyPlaintext is the rendered plaintext email body.
	BodyPlaintext string `json:"body_plaintext,omitempty"`

	// TemplateVersions reports the path, locale and content version of every
	// template part that was rendered.
	//
	// required: true
	TemplateVersions []PreviewTemplateVersion `json:"template_versions"`
}

// PreviewMessageDispatch renders a courier message template offline. It
// returns the rendered subject and body (and, for email, the plaintext body)
// together with the path and content version of every rendered template part.
//
// In contrast to QueueEmail, QueueSMS and DispatchMessage, this method never
// persists, enqueues or sends a message. Errors caused by templates that
// reference missing variables include the resolved template path in the
// returned *template.RenderError.
func (c *courier) PreviewMessageDispatch(ctx context.Context, req PreviewMessageDispatchRequest) (*PreviewMessageDispatch, error) {
	msgType, err := ToMessageType(strings.ToLower(strings.TrimSpace(req.Channel)))
	if err != nil {
		return nil, err
	}

	locale := strings.TrimSpace(req.Locale)

	model := make(map[string]interface{}, len(req.Data)+1)
	for k, v := range req.Data {
		model[k] = v
	}
	if req.Identity != nil {
		model[identityContextKey] = req.Identity
	}

	raw, err := json.Marshal(model)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	renderCtx, capture := template.WithRenderCapture(template.WithLocale(ctx, locale))
	msg := Message{TemplateType: req.TemplateType, TemplateData: raw}

	result := &PreviewMessageDispatch{
		Channel:      msgType.String(),
		TemplateType: req.TemplateType,
		Locale:       locale,
	}

	switch msgType {
	case MessageTypeEmail:
		t, err := c.newEmailTemplateFromMessage(c.deps, msg)
		if err != nil {
			return nil, err
		}

		subject, err := t.EmailSubject(renderCtx)
		if err != nil {
			return nil, err
		}

		body, err := t.EmailBody(renderCtx)
		if err != nil {
			return nil, err
		}

		plaintext, err := t.EmailBodyPlaintext(renderCtx)
		if err != nil {
			return nil, err
		}

		result.Subject = subject
		result.Body = body
		result.BodyPlaintext = plaintext
	case MessageTypeSMS:
		t, err := NewSMSTemplateFromMessage(c.deps, msg)
		if err != nil {
			return nil, err
		}

		body, err := t.SMSBody(renderCtx)
		if err != nil {
			return nil, err
		}

		result.Body = body
	default:
		return nil, errors.WithStack(herodot.ErrBadRequest().WithReasonf("Message channel %q is not supported for template previews.", req.Channel))
	}

	for _, src := range capture.Sources() {
		result.TemplateVersions = append(result.TemplateVersions, PreviewTemplateVersion{
			Part:     templatePart(src.Path),
			Path:     src.Path,
			Locale:   src.Locale,
			Fallback: src.Fallback,
			Remote:   src.Remote,
			Version:  src.Version,
		})
	}

	return result, nil
}

const identityContextKey = "identity"

// templatePart derives the template part label from a resolved template path.
func templatePart(p string) string {
	switch base := filepath.Base(p); {
	case strings.Contains(base, "subject"):
		return "subject"
	case strings.Contains(base, "plaintext"):
		return "body_plaintext"
	default:
		return "body"
	}
}
