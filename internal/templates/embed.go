// Package templates holds every file the CLI writes. Rendered with text/template against config.Fleet.
package templates

import (
	"bytes"
	"embed"
	"text/template"
)

//go:embed files/*
var FS embed.FS

func Render(name string, data any) ([]byte, error) {
	b, err := FS.ReadFile("files/" + name)
	if err != nil {
		return nil, err
	}
	t, err := template.New(name).Parse(string(b))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
