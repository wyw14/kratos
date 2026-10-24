// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package template

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	htemplate "html/template"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"text/template"

	"github.com/ory/x/fetcher"
	"github.com/ory/x/httpx"

	"github.com/Masterminds/sprig/v3"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/pkg/errors"

	"github.com/ory/kratos/driver/config"
)

//go:embed courier/builtin/templates/*
var templates embed.FS

var Cache, _ = lru.New[string, Template](16)

// remoteVersionCache stores the content version of remote templates keyed by
// their URL, mirroring Cache's lifetime.
var remoteVersionCache, _ = lru.New[string, string](16)

type Template interface {
	Execute(wr io.Writer, data interface{}) error
}

type templateDependencies interface {
	httpx.ClientProvider
}

// courierConfigProvider is implemented by the regular template Dependencies and
// is used to resolve the configured default template locale.
type courierConfigProvider interface {
	CourierConfig() config.CourierConfigs
}

// Source identifies the template artifact that was rendered. It is used for
// offline template previews to report which template version produced the
// rendered subject/body.
type Source struct {
	// Path is the template path relative to the templates root (or the builtin
	// templates directory). For remote templates it is the configured URL.
	//
	// required: true
	Path string `json:"path"`

	// Locale is the locale variant that was rendered. It is empty when the
	// unlocalized default template was rendered.
	Locale string `json:"locale,omitempty"`

	// Fallback is true when a locale was requested but no template existed for
	// it, so a different locale (or the unlocalized default template) was
	// rendered.
	Fallback bool `json:"fallback"`

	// Remote is true when the template was loaded from a remote URL configured
	// through the courier template configuration.
	Remote bool `json:"remote"`

	// Version is the hex-encoded SHA-256 digest of the template content.
	Version string `json:"version"`
}

// RenderError wraps a template parsing or execution error (for example a
// reference to a missing variable) with diagnostics pointing at the template
// source that caused it.
type RenderError struct {
	Source
	Err error `json:"-"`
}

func (e *RenderError) Error() string {
	return fmt.Sprintf("courier template %q failed to render: %s", e.Source.Path, e.Err)
}

func (e *RenderError) Unwrap() error {
	return e.Err
}

type templateContextKey int

const (
	localeContextKey templateContextKey = iota + 1
	captureContextKey
)

// WithLocale returns a context that instructs template loaders to render the
// localized variant of a template (for example email.subject.en_US.gotmpl) for
// the given locale (e.g. "en_US").
func WithLocale(ctx context.Context, locale string) context.Context {
	return context.WithValue(ctx, localeContextKey, strings.TrimSpace(locale))
}

// LocaleFromContext returns the locale stored in the context with WithLocale,
// if any.
func LocaleFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(localeContextKey).(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// RenderCapture records the sources of all templates rendered through the
// context returned by WithRenderCapture, in render order.
type RenderCapture struct {
	mu      sync.Mutex
	sources []Source
}

func (c *RenderCapture) add(s Source) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sources = append(c.sources, s)
}

// Sources returns a copy of the captured template sources in render order.
func (c *RenderCapture) Sources() []Source {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Source, len(c.sources))
	copy(out, c.sources)
	return out
}

// WithRenderCapture returns a context that records every resolved template
// source rendered through it, together with the capture itself.
func WithRenderCapture(ctx context.Context) (context.Context, *RenderCapture) {
	c := &RenderCapture{}
	return context.WithValue(ctx, captureContextKey, c), c
}

func captureSource(ctx context.Context, s Source) {
	if c, ok := ctx.Value(captureContextKey).(*RenderCapture); ok && c != nil {
		c.add(s)
	}
}

// contentVersion returns the hex-encoded SHA-256 digest of the template
// content.
func contentVersion(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// localizedTemplateName returns the locale-specific sibling of name, e.g.
// ("recovery/valid/email.subject.gotmpl", "en_US") becomes
// "recovery/valid/email.subject.en_US.gotmpl".
func localizedTemplateName(name, locale string) string {
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext) + "." + locale + ext
}

// readTemplateBytes reads name from the override filesystem, falling back to
// the embedded builtin templates. The returned builtin flag reports whether
// the builtin fallback was used.
func readTemplateBytes(filesystem fs.FS, name string) (b []byte, builtin bool, err error) {
	if filesystem != nil {
		if f, oerr := filesystem.Open(name); oerr == nil {
			defer func() { _ = f.Close() }()
			b, rerr := io.ReadAll(f)
			if rerr != nil {
				return nil, false, errors.WithStack(rerr)
			}
			return b, false, nil
		}
	}

	f, err := templates.Open(path.Join("courier/builtin/templates", filepath.ToSlash(name)))
	if err != nil {
		return nil, false, errors.WithStack(err)
	}
	defer func() { _ = f.Close() }()

	b, err = io.ReadAll(f)
	if err != nil {
		return nil, true, errors.WithStack(err)
	}
	return b, true, nil
}

func parseTemplate(name string, b []byte, builtin, html bool) (Template, error) {
	if t, found := Cache.Get(name); found {
		return t, nil
	}

	var tpl Template
	if html {
		funcs := sprig.HermeticHtmlFuncMap()
		if builtin {
			funcs = sprig.HtmlFuncMap()
		}
		t, err := htemplate.New(filepath.Base(name)).Funcs(funcs).Parse(string(b))
		if err != nil {
			return nil, errors.WithStack(err)
		}
		tpl = t
	} else {
		funcs := sprig.HermeticTxtFuncMap()
		if builtin {
			funcs = sprig.TxtFuncMap()
		}
		t, err := template.New(filepath.Base(name)).Funcs(funcs).Parse(string(b))
		if err != nil {
			return nil, errors.WithStack(err)
		}
		tpl = t
	}

	_ = Cache.Add(name, tpl)
	return tpl, nil
}

func loadBuiltInTemplate(filesystem fs.FS, name string, html bool) (Template, error) {
	if t, found := Cache.Get(name); found {
		return t, nil
	}

	file, err := filesystem.Open(name)
	if err != nil {
		// try to fallback to bundled templates
		var fallbackErr error
		file, fallbackErr = templates.Open(path.Join("courier/builtin/templates", filepath.ToSlash(name)))
		if fallbackErr != nil {
			// return original error from os.DirFS
			return nil, errors.WithStack(err)
		}
	}

	defer func() { _ = file.Close() }()

	var b bytes.Buffer
	if _, err := io.Copy(&b, file); err != nil {
		return nil, errors.WithStack(err)
	}

	var tpl Template
	if html {
		t, err := htemplate.New(name).Funcs(sprig.HtmlFuncMap()).Parse(b.String())
		if err != nil {
			return nil, errors.WithStack(err)
		}
		tpl = t
	} else {
		t, err := template.New(name).Funcs(sprig.TxtFuncMap()).Parse(b.String())
		if err != nil {
			return nil, errors.WithStack(err)
		}
		tpl = t
	}

	_ = Cache.Add(name, tpl)
	return tpl, nil
}

func fetchRemoteTemplate(ctx context.Context, d templateDependencies, url string, html bool) (t Template, version string, err error) {
	if t, found := Cache.Get(url); found {
		version, _ := remoteVersionCache.Get(url)
		return t, version, nil
	}

	f := fetcher.NewFetcher(fetcher.WithClient(d.HTTPClient(ctx)))
	bb, err := f.FetchContext(ctx, url)
	if err != nil {
		return nil, "", errors.WithStack(err)
	}
	b := bb.Bytes()

	if html {
		t, err = htemplate.New(url).Funcs(sprig.HermeticHtmlFuncMap()).Parse(string(b))
		if err != nil {
			return nil, "", errors.WithStack(err)
		}
	} else {
		t, err = template.New(url).Funcs(sprig.HermeticTxtFuncMap()).Parse(string(b))
		if err != nil {
			return nil, "", errors.WithStack(err)
		}
	}
	version = contentVersion(b)
	Cache.Add(url, t)
	remoteVersionCache.Add(url, version)

	return t, version, nil
}

func loadRemoteTemplate(ctx context.Context, d templateDependencies, url string, html bool) (t Template, err error) {
	t, _, err = fetchRemoteTemplate(ctx, d, url, html)
	return t, err
}

func loadTemplate(filesystem fs.FS, name, pattern string, html bool) (Template, error) {
	if t, found := Cache.Get(name); found {
		return t, nil
	}

	matches, _ := fs.Glob(filesystem, name)
	// make sure the file exists in the fs, otherwise fallback to built in templates
	if matches == nil {
		return loadBuiltInTemplate(filesystem, name, html)
	}

	glob := name
	if pattern != "" {
		// pattern matching is used when we have more than one gotmpl for different use cases, such as i18n support
		// e.g. some_template/template_name* will match some_template/template_name.body.en_US.gotmpl
		matches, _ = fs.Glob(filesystem, pattern)
		// set the glob string to match patterns
		if matches != nil {
			glob = pattern
		}
	}

	var tpl Template
	if html {
		t, err := htemplate.New(filepath.Base(name)).Funcs(sprig.HermeticHtmlFuncMap()).ParseFS(filesystem, glob)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		tpl = t
	} else {
		t, err := template.New(filepath.Base(name)).Funcs(sprig.HermeticTxtFuncMap()).ParseFS(filesystem, glob)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		tpl = t
	}

	_ = Cache.Add(name, tpl)
	return tpl, nil
}

// localeCandidates returns the locales to try when resolving a localized
// template: the explicitly requested locale first, followed by the configured
// default locale (de-duplicated).
func localeCandidates(ctx context.Context, d templateDependencies, requested string) []string {
	var locales []string
	if requested != "" {
		locales = append(locales, requested)
	}
	if cp, ok := d.(courierConfigProvider); ok {
		if fallback := strings.TrimSpace(cp.CourierConfig().CourierTemplateDefaultLocale(ctx)); fallback != "" && fallback != requested {
			locales = append(locales, fallback)
		}
	}
	return locales
}

// resolveTemplate loads the best matching template for the locale carried in
// the context and reports its Source. Resolution order is: the requested
// locale, the configured default locale, and finally the unlocalized default
// template.
func resolveTemplate(ctx context.Context, d templateDependencies, filesystem fs.FS, name, pattern string, html bool) (Template, Source, error) {
	requested := LocaleFromContext(ctx)
	locales := localeCandidates(ctx, d, requested)

	for i, loc := range locales {
		lname := localizedTemplateName(name, loc)
		b, builtin, err := readTemplateBytes(filesystem, lname)
		if err != nil {
			// the locale variant does not exist - try the next candidate
			continue
		}

		tpl, err := parseTemplate(lname, b, builtin, html)
		if err != nil {
			return nil, Source{Path: lname, Locale: loc}, err
		}

		// Only a hit on the explicitly requested first candidate is not a
		// fallback. Hits on the configured default locale (or a request that
		// fell through) are reported as fallbacks.
		fallback := !(i == 0 && requested != "")
		return tpl, Source{
			Path:     lname,
			Locale:   loc,
			Fallback: fallback,
			Version:  contentVersion(b),
		}, nil
	}

	tpl, err := loadTemplate(filesystem, name, pattern, html)
	if err != nil {
		return nil, Source{Path: name}, err
	}

	src := Source{
		Path:     name,
		Fallback: len(locales) > 0,
	}
	if b, _, rerr := readTemplateBytes(filesystem, name); rerr == nil {
		src.Version = contentVersion(b)
	}
	return tpl, src, nil
}

func render(ctx context.Context, d templateDependencies, filesystem fs.FS, name, pattern string, model interface{}, remoteURL string, html bool) (string, Source, error) {
	var (
		tpl Template
		src Source
		err error
	)

	if remoteURL != "" {
		var version string
		tpl, version, err = fetchRemoteTemplate(ctx, d, remoteURL, html)
		src = Source{Path: remoteURL, Remote: true, Version: version}
	} else {
		tpl, src, err = resolveTemplate(ctx, d, filesystem, name, pattern, html)
	}
	if err != nil {
		return "", src, &RenderError{Source: src, Err: err}
	}

	var buf bytes.Buffer
	if err = tpl.Execute(&buf, model); err != nil {
		return "", src, &RenderError{Source: src, Err: err}
	}
	return buf.String(), src, nil
}

func LoadText(ctx context.Context, d templateDependencies, filesystem fs.FS, name, pattern string, model interface{}, remoteURL string) (string, error) {
	rendered, src, err := render(ctx, d, filesystem, name, pattern, model, remoteURL, false)
	captureSource(ctx, src)
	return rendered, err
}

func LoadHTML(ctx context.Context, d templateDependencies, filesystem fs.FS, name, pattern string, model interface{}, remoteURL string) (string, error) {
	rendered, src, err := render(ctx, d, filesystem, name, pattern, model, remoteURL, true)
	captureSource(ctx, src)
	return rendered, err
}
