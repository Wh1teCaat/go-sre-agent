package llm

import (
	"context"
	"fmt"

	"github.com/y2/go-sre-agent/internal/schema"
)

type MockProvider struct {
	actions []schema.Action
	next    int
}

func NewMockProvider(actions []schema.Action) *MockProvider {
	return &MockProvider{actions: actions}
}

func (p *MockProvider) Plan(context.Context, Request) (*schema.Plan, error) {
	return nil, nil
}

func (p *MockProvider) Next(context.Context, Request) (Decision, error) {
	if p.next >= len(p.actions) {
		return Decision{}, fmt.Errorf("mock provider exhausted")
	}
	action := p.actions[p.next]
	p.next++
	return Decision{Action: &action}, nil
}
