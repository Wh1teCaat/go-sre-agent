package report

import (
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/trace"
)

type Input struct {
	Goal      string
	Diagnosis schema.Diagnosis
	Plan      schema.Plan
	Trace     []trace.Entry
}
