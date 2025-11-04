# Generic Pulsar Worker

Generic Pulsar worker with Nack-based retry functionality.

## Features

- Generic message processor interface
- Nack-based message redelivery for failed processing
- Concurrent message processing
- Dead letter queue support (planned)
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

    "github.com/your-org/your-repo/pkg/pulsar"
    "github.com/apache/pulsar-client-go/pulsar"
)

func main() {
    processor := func(ctx context.Context, msg pulsar.Message) error {
        log.Printf("Processing message: %s", string(msg.Payload()))
        return nil
    }

    config := pulsar.WorkerConfig{
        ConsumerOptions: pulsar.ConsumerOptions{
            Topic:            "my-topic",
            SubscriptionName: "my-subscription",
            Type:             pulsar.Exclusive,
        },
        Concurrency:       2,
        ProcessingTimeout: 10 * time.Second,
    }

    worker, err := pulsar.NewWorker(config, processor)
    if err != nil {
        log.Fatalf("Failed to create worker: %v", err)
    }

    pulsarClient, err := pulsar.NewClient(context.Background())
    if err != nil {
        log.Fatalf("Failed to create Pulsar client: %v", err)
    }
    defer pulsarClient.Close()

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, os.Interrupt)
    go func() {
        <-sigChan
        log.Println("Received interrupt signal, shutting down...")
        cancel()
    }()

    log.Println("Starting Pulsar worker...")
    if err := worker.Start(ctx, pulsarClient); err != nil {
        log.Fatalf("Worker failed: %v", err)
    }

    log.Println("Worker shutdown complete")
}
```

## Configuration Options

- `ClientOptions`: Pulsar client configuration (broker URL, etc.)
- `ConsumerOptions`: Consumer-specific options (topic, subscription, etc.)
- `DeadLetterTopic`: Topic to send messages that exceed max retries (optional)
- `Concurrency`: Number of goroutines to process messages (default: 1)
- `ProcessingTimeout`: Timeout for individual message processing (default: 30s)