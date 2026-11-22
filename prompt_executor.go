package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// PromptResult preserves the raw response and its run metadata for separate parsing.
type PromptResult = Result

// PromptExecutor separates execution from application-specific response parsing.
type PromptExecutor interface {
	ExecutePrompt(ctx context.Context, input any, options ...RunOptions) (PromptResult, error)
	ParseResponse(result PromptResult, destination any) error
}

// PromptExecutorFactory binds a definition and runner to an executor instance.
type PromptExecutorFactory func(*Runner, Agent) PromptExecutor

// DefaultPromptExecutor delegates execution to Runner and parses JSON responses.
type DefaultPromptExecutor struct {
	Runner *Runner
	Agent  Agent
}

func (e DefaultPromptExecutor) ExecutePrompt(ctx context.Context, input any, options ...RunOptions) (PromptResult, error) {
	return e.Runner.Run(ctx, e.Agent, input, options...)
}

func (e DefaultPromptExecutor) ParseResponse(result PromptResult, destination any) error {
	if err := json.Unmarshal([]byte(result.Text), destination); err != nil {
		return fmt.Errorf("agent response parsen: %w", err)
	}
	return nil
}

// PromptExecutor resolves an agent by key and constructs its executor. Definitions
// without a factory use DefaultPromptExecutor.
func (r *Runner) PromptExecutor(ctx context.Context, registry *Registry, key string, versions ...string) (PromptExecutor, error) {
	if r == nil || r.client == nil {
		return nil, errors.New("agent runner ist nicht initialisiert")
	}
	agent, err := registry.Get(ctx, key, versions...)
	if err != nil {
		return nil, err
	}
	if agent.PromptExecutorFactory != nil {
		executor := agent.PromptExecutorFactory(r, agent)
		if executor == nil {
			return nil, fmt.Errorf("agent %q executor factory returned nil", key)
		}
		return executor, nil
	}
	return DefaultPromptExecutor{Runner: r, Agent: agent}, nil
}

// PromptExecutor resolves an executor using the runner configured on the registry.
func (r *Registry) PromptExecutor(ctx context.Context, key string, versions ...string) (PromptExecutor, error) {
	if r == nil {
		return nil, ErrAgentNotFound
	}
	r.mu.RLock()
	runner := r.runner
	r.mu.RUnlock()
	if runner == nil {
		return nil, ErrRunnerNotSet
	}
	return runner.PromptExecutor(ctx, r, key, versions...)
}
