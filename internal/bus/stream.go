package bus

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// JS exposes the JetStream client for publishing and reading streams.
func (b *Bus) JS() jetstream.JetStream { return b.js }

// Stream returns a file-backed stream for the subjects, creating or updating
// it. Messages older than maxAge are dropped.
func (b *Bus) Stream(ctx context.Context, name string, subjects []string, maxAge time.Duration) (jetstream.Stream, error) {
	s, err := b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        name,
		Description: "lifestream " + name,
		Subjects:    subjects,
		Storage:     jetstream.FileStorage,
		Retention:   jetstream.LimitsPolicy,
		MaxAge:      maxAge,
		Discard:     jetstream.DiscardOld,
	})
	if err != nil {
		return nil, fmt.Errorf("creating stream %q: %w", name, err)
	}
	return s, nil
}
