// Package bus runs an embedded NATS server with JetStream, reachable only
// in-process, and hands out KeyValue buckets backed by files under the data
// directory. Handlers watch a bucket instead of polling; writers put to it.
package bus

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Server-wide JetStream ceilings. The account view reports these as
// unlimited, so anything displaying limits should use these constants.
const (
	MaxMemory = 64 << 20
	MaxStore  = 1 << 30
)

// Bus is a running embedded server plus a client connected to it.
type Bus struct {
	srv *server.Server
	nc  *nats.Conn
	js  jetstream.JetStream
}

// Start boots the server with its store under dataDir. No TCP port is opened.
func Start(dataDir string) (*Bus, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	srv, err := server.NewServer(&server.Options{
		ServerName:         "lifestream",
		DontListen:         true,
		JetStream:          true,
		StoreDir:           dataDir,
		JetStreamMaxMemory: MaxMemory,
		JetStreamMaxStore:  MaxStore,
		NoSigs:             true,
		NoLog:              true,
	})
	if err != nil {
		return nil, fmt.Errorf("configuring embedded nats: %w", err)
	}
	srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		return nil, fmt.Errorf("embedded nats did not become ready")
	}
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		srv.Shutdown()
		return nil, fmt.Errorf("connecting in-process: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		srv.Shutdown()
		return nil, fmt.Errorf("creating jetstream client: %w", err)
	}
	return &Bus{srv: srv, nc: nc, js: js}, nil
}

// KV returns a file-backed bucket, creating it if needed.
func (b *Bus) KV(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	kv, err := b.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      bucket,
		Description: "lifestream " + bucket,
		Storage:     jetstream.FileStorage,
		History:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("creating kv bucket %q: %w", bucket, err)
	}
	return kv, nil
}

// Close disconnects the client and stops the server, waiting for it to exit.
func (b *Bus) Close() {
	b.nc.Close()
	b.srv.Shutdown()
	b.srv.WaitForShutdown()
}
