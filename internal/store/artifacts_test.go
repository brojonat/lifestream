package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestChapterOrder(t *testing.T) {
	s, _ := open(t)
	f := fx{t: t, s: s, owner: addUser(t, s, "+1").ID}
	ctx := context.Background()
	book := f.must(TypeCollectionStarted, "", CollectionStarted{Name: "postgres book", Include: "my postgres artifacts"})
	var ch []string
	for _, title := range []string{"intro", "vacuum", "upgrades"} {
		p, _ := json.Marshal(ArtifactSaved{Title: title, HTML: "<p>" + title})
		e, _, err := s.Append(ctx, Event{Owner: f.owner, Source: "dashboard", Type: TypeArtifactSaved, Payload: p})
		if err != nil {
			t.Fatal(err)
		}
		f.must(TypeLabelApplied, e.ID, LabelApplied{EventID: e.ID, CollectionID: book.ID, DefinitionVersion: 1, Reason: "postgres"})
		ch = append(ch, e.ID)
	}
	order := func() []string {
		ms, err := s.MemberEvents(ctx, book.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range ms {
			out = append(out, m.EventID)
		}
		return out
	}
	if got := order(); !slices.Equal(got, []string{ch[2], ch[1], ch[0]}) {
		t.Fatalf("unordered: newest first, got %v", got)
	}
	f.mustAt("dashboard", TypeCollectionOrdered, time.Now(), CollectionOrdered{CollectionID: book.ID, Order: []string{ch[0], ch[1]}})
	if got := order(); !slices.Equal(got, []string{ch[0], ch[1], ch[2]}) {
		t.Fatalf("ordered first, then the rest: %v", got)
	}
	for name, o := range map[string][]string{"twice": {ch[0], ch[0]}, "not a member": {book.ID}} {
		if _, err := f.at("dashboard", TypeCollectionOrdered, time.Now(), CollectionOrdered{CollectionID: book.ID, Order: o}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if got, _ := s.Undelivered(ctx, 100); slices.ContainsFunc(got, func(e Event) bool { return e.Type == TypeCollectionOrdered }) {
		t.Fatal("ordering went to the agent")
	}
}
