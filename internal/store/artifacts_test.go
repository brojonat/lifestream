package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestArtifacts(t *testing.T) {
	s, _ := open(t)
	owner := addUser(t, s, "+1").ID
	ctx := context.Background()
	save := func(a ArtifactSaved) error {
		b, _ := json.Marshal(a)
		_, _, err := s.Append(ctx, Event{Owner: owner, Source: "dashboard", Type: TypeArtifactSaved, Payload: b})
		return err
	}
	html := "<!doctype html><title>Postgres</title><p>" + strings.Repeat("é", 100)
	for _, a := range []ArtifactSaved{
		{Title: "Postgres upgrades", HTML: html},
		{Title: "Vacuum, explained", URL: "https://claude.ai/public/artifacts/0b7e1d2c-aaaa-bbbb-cccc-000000000000"},
	} {
		if err := save(a); err != nil {
			t.Fatal(err)
		}
	}
	for name, a := range map[string]ArtifactSaved{
		"no title":    {HTML: "<p>x"},
		"nothing":     {Title: "x"},
		"other host":  {Title: "x", URL: "https://evil.example/claude.ai"},
		"http":        {Title: "x", URL: "http://claude.ai/public/artifacts/1"},
		"lookalike":   {Title: "x", URL: "https://claude.ai.evil.example/a"},
		"userinfo":    {Title: "x", URL: "https://me@claude.ai/a"},
		"blank html":  {Title: "x", HTML: "  "},
		"javascript:": {Title: "x", URL: "javascript:alert(1)"},
	} {
		var inv *InvalidEventError
		if err := save(a); !errors.As(err, &inv) {
			t.Errorf("%s: want InvalidEventError, got %v", name, err)
		}
	}
	list, err := s.Artifacts(ctx, owner, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Title != "Vacuum, explained" || list[0].HTMLSize != 0 || list[1].HTMLSize != len(html) || list[1].URL != "" {
		t.Fatalf("artifacts: %+v (html is %d bytes)", list, len(html))
	}
}
