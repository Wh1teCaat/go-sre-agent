package agent

import "github.com/y2/go-sre-agent/internal/schema"

type State struct {
	Goal         string
	Observations []schema.Observation
}
