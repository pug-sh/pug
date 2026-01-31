# Generic NATS Worker

Generic NATS worker with Nak-based retry functionality.

## Features

- Generic message processor interface
- Nak-based message redelivery for failed processing
- Concurrent message processing
- Durable consumer support
- Graceful shutdown
- Health check capability

## Usage

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "time"

    "github.com/fivebitsio/cotton/pkg/nats"
    "github.com/nats-io/nats.go/jetstream"
)

func main() {
    processor := func(ctx context.Context, msg jetstream.Msg) error {
        log.Printf("Processing message: %s", string(msg.Data()))
        return nil
    }

    config := nats.WorkerConfig{
        StreamName:        "my-stream",
        ConsumerName:      "my-consumer",
        DurableName:       "my-durable-consumer",
        Concurrency:       2,
        ProcessingTimeout: 10 * time.Second,
        MaxDeliver:        3,
    }

    worker, err := nats.NewWorker(config, processor)
    if err != nil {
        log.Fatalf("Failed to create worker: %v", err)
    }

    natsClient, err := nats.New(context.Background())
    if err != nil {
        log.Fatalf("Failed to create NATS client: %v", err)
    }
    defer natsClient.Close()

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, os.Interrupt)
    go func() {
        <-sigChan
        log.Println("Received interrupt signal, shutting down...")
        cancel()
    }()

    log.Println("Starting NATS worker...")
    if err := worker.Start(ctx, natsClient); err != nil {
        log.Fatalf("Worker failed: %v", err)
    }

    log.Println("Worker shutdown complete")
}
```

## Configuration Options

- `StreamName`: Name of the NATS stream to consume from
- `ConsumerName`: Name of the consumer
- `DurableName`: Name of the durable consumer (optional, for durability)
- `Concurrency`: Number of goroutines to process messages (default: 1)
- `ProcessingTimeout`: Timeout for individual message processing (default: 30s)
- `MaxDeliver`: Maximum number of delivery attempts for a message (default: 3)
- `AckWait`: Time to wait for an acknowledgment before redelivering (default: 30s)