package assistant

import (
	aisdk "github.com/grafana/ai-sdk"
)

// CallOptions are ai-sdk per-call options resolved at boot from the model
// route (sampling parameters). A named type so service.go never has to import
// the alpha SDK — ai-sdk imports stay confined to loop.go and llm.go.
type CallOptions []aisdk.StreamOption
