// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package courier_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ory/herodot"
	"github.com/ory/kratos/courier"
	"github.com/ory/kratos/courier/template"
	"github.com/ory/kratos/driver/config"
	"github.com/ory/kratos/pkg"
)

// resetTemplateCache ensures the package-level template cache cannot leak
// templates between preview test cases that use different template roots.
func resetTemplateCache() {
	template.Cache, _ = lru.New[string, template.Template](16)
}

// writePreviewTemplates writes name->content pairs into dir, creating parent
// directories as needed.
func writePreviewTemplates(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		fp := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(fp), 0o750))
		require.NoError(t, os.WriteFile(fp, []byte(content), 0o600))
	}
}

func findPreviewVersion(t *testing.T, p *courier.PreviewMessageDispatch, part string) courier.PreviewTemplateVersion {
	t.Helper()
	for _, v := range p.TemplateVersions {
		if v.Part == part {
			return v
		}
	}
	require.Failf(t, "template version not found", "part %q in %+v", part, p.TemplateVersions)
	return courier.PreviewTemplateVersion{}
}

func TestPreviewMessageDispatchEmail(t *testing.T) {
	ctx := context.Background()
	conf, reg := pkg.NewFastRegistryWithMocks(t)
	require.NoError(t, conf.Set(ctx, config.ViperKeyCourierTemplateDefaultLocale, ""))
	resetTemplateCache()

	c, err := reg.Courier(ctx)
	require.NoError(t, err)

	identity := map[string]interface{}{
		"id":     "identity-id",
		"traits": map[string]interface{}{"email": "bob@example.com"},
	}

	res, err := c.PreviewMessageDispatch(ctx, courier.PreviewMessageDispatchRequest{
		Channel:      "email",
		TemplateType: template.TypeRecoveryValid,
		Locale:       "",
		Identity:     identity,
		Data: map[string]interface{}{
			"to":                 "bob@example.com",
			"recovery_url":       "https://example.com/recovery?token=secret-token",
			"expires_in_minutes": 42,
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "email", res.Channel)
	assert.Equal(t, template.TypeRecoveryValid, res.TemplateType)
	assert.Equal(t, "Recover access to your account", strings.TrimSpace(res.Subject))
	assert.Contains(t, res.Body, "https://example.com/recovery?token=secret-token")
	assert.Contains(t, res.Body, "Recover access to your account by clicking")
	assert.Contains(t, res.BodyPlaintext, "https://example.com/recovery?token=secret-token")
	assert.Contains(t, res.BodyPlaintext, "expires in 42 minutes")

	require.Len(t, res.TemplateVersions, 3)
	subject := findPreviewVersion(t, res, "subject")
	assert.Equal(t, "recovery/valid/email.subject.gotmpl", subject.Path)
	assert.Empty(t, subject.Locale)
	assert.False(t, subject.Fallback)
	assert.False(t, subject.Remote)
	assert.Len(t, subject.Version, 64) // sha256 hex digest

	body := findPreviewVersion(t, res, "body")
	assert.Equal(t, "recovery/valid/email.body.gotmpl", body.Path)
	assert.Len(t, body.Version, 64)

	plaintext := findPreviewVersion(t, res, "body_plaintext")
	assert.Equal(t, "recovery/valid/email.body.plaintext.gotmpl", plaintext.Path)
	assert.Len(t, plaintext.Version, 64)

	// subject and body are different files and therefore have distinct versions
	assert.NotEqual(t, subject.Version, plaintext.Version)

	// the preview must not have queued a message
	messages, _, err := reg.CourierPersister().ListMessages(ctx, courier.ListCourierMessagesParameters{}, nil)
	require.NoError(t, err)
	assert.Empty(t, messages)
}

func TestPreviewMessageDispatchSMS(t *testing.T) {
	ctx := context.Background()
	_, reg := pkg.NewFastRegistryWithMocks(t)
	resetTemplateCache()

	c, err := reg.Courier(ctx)
	require.NoError(t, err)

	res, err := c.PreviewMessageDispatch(ctx, courier.PreviewMessageDispatchRequest{
		Channel:      "sms",
		TemplateType: template.TypeVerificationCodeValid,
		Identity:     map[string]interface{}{"traits": map[string]interface{}{"phone": "+12345678901"}},
		Data: map[string]interface{}{
			"to":                 "+12345678901",
			"verification_code":  "042424",
			"expires_in_minutes": 9,
			"request_url_domain": "example.com",
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "sms", res.Channel)
	assert.Contains(t, res.Body, "Your verification code is: 042424")
	assert.Contains(t, res.Body, "@example.com #042424")
	assert.Empty(t, res.Subject)
	assert.Empty(t, res.BodyPlaintext)

	require.Len(t, res.TemplateVersions, 1)
	v := res.TemplateVersions[0]
	assert.Equal(t, "body", v.Part)
	assert.Equal(t, "verification_code/valid/sms.body.gotmpl", v.Path)
	assert.False(t, v.Fallback)
	assert.Len(t, v.Version, 64)

	messages, _, err := reg.CourierPersister().ListMessages(ctx, courier.ListCourierMessagesParameters{}, nil)
	require.NoError(t, err)
	assert.Empty(t, messages)
}

func TestPreviewMessageDispatchLocales(t *testing.T) {
	templates := map[string]string{
		"recovery/valid/email.subject.gotmpl":              "Default Subject",
		"recovery/valid/email.subject.de_DE.gotmpl":        "Deutscher Betreff",
		"recovery/valid/email.body.gotmpl":                 "Default Body {{ .RecoveryURL }}",
		"recovery/valid/email.body.de_DE.gotmpl":           "Deutscher Body {{ .RecoveryURL }}",
		"recovery/valid/email.body.plaintext.gotmpl":       "Default plain {{ .RecoveryURL }}",
		"recovery/valid/email.body.plaintext.de_DE.gotmpl": "Deutsch plain {{ .RecoveryURL }}",
	}

	data := map[string]interface{}{
		"to":                 "bob@example.com",
		"recovery_url":       "https://example.com/recovery",
		"expires_in_minutes": 42,
	}

	request := func(t *testing.T, c courier.Courier, locale string) *courier.PreviewMessageDispatch {
		t.Helper()
		res, err := c.PreviewMessageDispatch(context.Background(), courier.PreviewMessageDispatchRequest{
			Channel:      "email",
			TemplateType: template.TypeRecoveryValid,
			Locale:       locale,
			Data:         data,
		})
		require.NoError(t, err)
		return res
	}

	t.Run("case=requested locale is rendered", func(t *testing.T) {
		ctx := context.Background()
		conf, reg := pkg.NewFastRegistryWithMocks(t)
		dir := t.TempDir()
		writePreviewTemplates(t, dir, templates)
		require.NoError(t, conf.Set(ctx, config.ViperKeyCourierTemplatesPath, dir))
		require.NoError(t, conf.Set(ctx, config.ViperKeyCourierTemplateDefaultLocale, ""))
		resetTemplateCache()

		c, err := reg.Courier(ctx)
		require.NoError(t, err)

		res := request(t, c, "de_DE")
		assert.Equal(t, "Deutscher Betreff", res.Subject)
		assert.Equal(t, "Deutscher Body https://example.com/recovery", res.Body)
		assert.Equal(t, "Deutsch plain https://example.com/recovery", res.BodyPlaintext)

		for _, v := range res.TemplateVersions {
			assert.True(t, strings.HasSuffix(v.Path, ".de_DE.gotmpl"), v.Path)
			assert.Equal(t, "de_DE", v.Locale)
			assert.False(t, v.Fallback, v.Path)
			assert.Len(t, v.Version, 64)
		}
	})

	t.Run("case=unknown locale falls back to default template", func(t *testing.T) {
		ctx := context.Background()
		conf, reg := pkg.NewFastRegistryWithMocks(t)
		dir := t.TempDir()
		writePreviewTemplates(t, dir, templates)
		require.NoError(t, conf.Set(ctx, config.ViperKeyCourierTemplatesPath, dir))
		require.NoError(t, conf.Set(ctx, config.ViperKeyCourierTemplateDefaultLocale, ""))
		resetTemplateCache()

		c, err := reg.Courier(ctx)
		require.NoError(t, err)

		res := request(t, c, "fr_FR")
		assert.Equal(t, "Default Subject", res.Subject)
		assert.Equal(t, "Default Body https://example.com/recovery", res.Body)
		assert.Equal(t, "Default plain https://example.com/recovery", res.BodyPlaintext)

		for _, v := range res.TemplateVersions {
			assert.False(t, strings.Contains(v.Path, ".fr_FR."), v.Path)
			assert.True(t, strings.HasSuffix(v.Path, ".gotmpl"), v.Path)
			assert.Empty(t, v.Locale)
			assert.True(t, v.Fallback, v.Path)
		}
	})

	t.Run("case=unknown locale falls back to configured default locale", func(t *testing.T) {
		ctx := context.Background()
		conf, reg := pkg.NewFastRegistryWithMocks(t)
		dir := t.TempDir()
		writePreviewTemplates(t, dir, templates)
		require.NoError(t, conf.Set(ctx, config.ViperKeyCourierTemplatesPath, dir))
		require.NoError(t, conf.Set(ctx, config.ViperKeyCourierTemplateDefaultLocale, "de_DE"))
		resetTemplateCache()

		c, err := reg.Courier(ctx)
		require.NoError(t, err)

		res := request(t, c, "fr_FR")
		assert.Equal(t, "Deutscher Betreff", res.Subject)

		subject := findPreviewVersion(t, res, "subject")
		assert.Equal(t, "recovery/valid/email.subject.de_DE.gotmpl", subject.Path)
		assert.Equal(t, "de_DE", subject.Locale)
		assert.True(t, subject.Fallback)
	})

	t.Run("case=configured default locale is used without explicit locale", func(t *testing.T) {
		ctx := context.Background()
		conf, reg := pkg.NewFastRegistryWithMocks(t)
		dir := t.TempDir()
		writePreviewTemplates(t, dir, templates)
		require.NoError(t, conf.Set(ctx, config.ViperKeyCourierTemplatesPath, dir))
		require.NoError(t, conf.Set(ctx, config.ViperKeyCourierTemplateDefaultLocale, "de_DE"))
		resetTemplateCache()

		c, err := reg.Courier(ctx)
		require.NoError(t, err)

		res := request(t, c, "")
		assert.Empty(t, res.Locale)
		assert.Equal(t, "Deutscher Betreff", res.Subject)

		subject := findPreviewVersion(t, res, "subject")
		assert.Equal(t, "recovery/valid/email.subject.de_DE.gotmpl", subject.Path)
		assert.Equal(t, "de_DE", subject.Locale)
		assert.True(t, subject.Fallback, "the configured default locale is a fallback")
	})
}

func TestPreviewMessageDispatchIdentityContext(t *testing.T) {
	ctx := context.Background()
	conf, reg := pkg.NewFastRegistryWithMocks(t)
	dir := t.TempDir()
	writePreviewTemplates(t, dir, map[string]string{
		"recovery/valid/email.subject.gotmpl":        `Hello {{ .Identity.traits.email }}`,
		"recovery/valid/email.body.gotmpl":           `Body {{ .RecoveryURL }}`,
		"recovery/valid/email.body.plaintext.gotmpl": `Plain {{ .RecoveryURL }}`,
	})
	require.NoError(t, conf.Set(ctx, config.ViperKeyCourierTemplatesPath, dir))
	resetTemplateCache()

	c, err := reg.Courier(ctx)
	require.NoError(t, err)

	res, err := c.PreviewMessageDispatch(ctx, courier.PreviewMessageDispatchRequest{
		Channel:      "email",
		TemplateType: template.TypeRecoveryValid,
		Identity: map[string]interface{}{
			"traits": map[string]interface{}{"email": "alice@example.com"},
		},
		Data: map[string]interface{}{"recovery_url": "https://example.com/"},
	})
	require.NoError(t, err)
	assert.Equal(t, "Hello alice@example.com", res.Subject)
}

func TestPreviewMessageDispatchHTMLEscaping(t *testing.T) {
	ctx := context.Background()
	_, reg := pkg.NewFastRegistryWithMocks(t)
	resetTemplateCache()

	c, err := reg.Courier(ctx)
	require.NoError(t, err)

	const maliciousURL = "https://example.com/<script>alert(1)</script>"

	res, err := c.PreviewMessageDispatch(ctx, courier.PreviewMessageDispatchRequest{
		Channel:      "email",
		TemplateType: template.TypeRecoveryValid,
		Data: map[string]interface{}{
			"to":                 "bob@example.com",
			"recovery_url":       maliciousURL,
			"expires_in_minutes": 42,
		},
	})
	require.NoError(t, err)

	// The HTML body is rendered with html/template and must escape attacker
	// controlled data.
	assert.NotContains(t, res.Body, "<script>")
	assert.Contains(t, res.Body, "&lt;script&gt;")

	// The plaintext body is rendered with text/template and must keep the raw
	// value untouched.
	assert.Contains(t, res.BodyPlaintext, maliciousURL)
	assert.NotContains(t, res.BodyPlaintext, "&lt;")
}

func TestPreviewMessageDispatchMissingVariableDiagnostics(t *testing.T) {
	ctx := context.Background()
	conf, reg := pkg.NewFastRegistryWithMocks(t)
	dir := t.TempDir()
	writePreviewTemplates(t, dir, map[string]string{
		"recovery/valid/email.subject.gotmpl": `{{ .DoesNotExist }}`,
	})
	require.NoError(t, conf.Set(ctx, config.ViperKeyCourierTemplatesPath, dir))
	resetTemplateCache()

	c, err := reg.Courier(ctx)
	require.NoError(t, err)

	_, err = c.PreviewMessageDispatch(ctx, courier.PreviewMessageDispatchRequest{
		Channel:      "email",
		TemplateType: template.TypeRecoveryValid,
		Data:         map[string]interface{}{},
	})
	require.Error(t, err)

	var renderErr *template.RenderError
	require.ErrorAs(t, err, &renderErr)
	assert.Equal(t, "recovery/valid/email.subject.gotmpl", renderErr.Path)
	assert.Contains(t, err.Error(), "recovery/valid/email.subject.gotmpl")
	assert.Contains(t, err.Error(), "DoesNotExist")
}

func TestPreviewMessageDispatchValidation(t *testing.T) {
	ctx := context.Background()
	_, reg := pkg.NewFastRegistryWithMocks(t)

	c, err := reg.Courier(ctx)
	require.NoError(t, err)

	t.Run("case=unknown channel", func(t *testing.T) {
		_, err := c.PreviewMessageDispatch(ctx, courier.PreviewMessageDispatchRequest{
			Channel:      "push",
			TemplateType: template.TypeRecoveryValid,
		})
		require.Error(t, err)

		var badRequest *herodot.DefaultError
		require.ErrorAs(t, err, &badRequest)
		assert.Equal(t, http.StatusBadRequest, badRequest.CodeField)
	})

	t.Run("case=template type not supported by channel", func(t *testing.T) {
		_, err := c.PreviewMessageDispatch(ctx, courier.PreviewMessageDispatchRequest{
			Channel:      "sms",
			TemplateType: template.TypeRecoveryInvalid,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "received unexpected message template type")
	})
}
