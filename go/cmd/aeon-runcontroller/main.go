// Command aeon-runcontroller is the Run Controller (RUN-001): start/cancel/pause/resume/status/
// stream for a run, as a thin HTTP layer over the Temporal client. It holds no run state of its
// own — Temporal is the source of truth (docs/adr/0001) — so this service is stateless and can
// restart or scale freely.
package main

import (
	"log"
	"net/http"
	"os"

	"go.temporal.io/sdk/client"

	"github.com/aeon-ai/aeon/go/internal/api"
	"github.com/aeon-ai/aeon/go/internal/httpserver"
	"github.com/aeon-ai/aeon/go/internal/runcontroller"
)

func main() {
	if p := os.Getenv("AEON_RUNCONTROLLER_PORT"); p != "" {
		os.Setenv("AEON_PORT", p)
	} else {
		os.Setenv("AEON_PORT", "9404")
	}

	address := os.Getenv("AEON_TEMPORAL_ADDRESS")
	if address == "" {
		address = "localhost:7233"
	}
	namespace := os.Getenv("AEON_TEMPORAL_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}
	taskQueue := os.Getenv("AEON_TASK_QUEUE") // defaults to runcontroller.DefaultTaskQueue when empty

	temporalClient, err := client.Dial(client.Options{HostPort: address, Namespace: namespace})
	if err != nil {
		log.Fatalf("aeon-runcontroller: connecting to Temporal at %s: %v", address, err)
	}
	defer temporalClient.Close()

	mux := http.NewServeMux()
	handlers := &api.RunControllerHandlers{
		Controller: runcontroller.New(temporalClient, taskQueue),
	}
	handlers.Register(mux)

	srv := httpserver.New("aeon-runcontroller", mux)
	log.Printf("aeon-runcontroller starting (temporal=%s, task_queue=%s)", address, taskQueue)
	httpserver.MustListenAndServe(srv)
}
