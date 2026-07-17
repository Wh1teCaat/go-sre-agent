package llm

import (
	"context"
	"fmt"
	"sync"

	"github.com/y2/go-sre-agent/internal/schema"
)

type MockProvider struct {
	mu      sync.Mutex
	actions []schema.Action
	next    int
}

func NewMockProvider(actions []schema.Action) *MockProvider {
	copied := make([]schema.Action, len(actions))
	copy(copied, actions)
	return &MockProvider{actions: copied}
}

func (p *MockProvider) Plan(context.Context, Request) (*schema.Plan, error) {
	return nil, nil
}

func (p *MockProvider) Next(context.Context, Request) (Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.next >= len(p.actions) {
		return Decision{}, fmt.Errorf("mock provider exhausted")
	}
	action := p.actions[p.next]
	p.next++
	return Decision{Action: &action}, nil
}
