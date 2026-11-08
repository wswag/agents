package agents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

var (
	ErrAgentNotFound = errors.New("agent not found")
	ErrRunnerNotSet  = errors.New("agent registry runner is not set")
)

// Registry combines statically registered agents with repository-backed definitions.
// Static agents take precedence when a key exists in both sources.
type Registry struct {
	mu         sync.RWMutex
	agents     map[string]Agent
	runner     *Runner
	repository AgentRepository
}

func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]Agent)}
}

// SetRunner configures the runner used by Registry.Run.
func (r *Registry) SetRunner(runner *Runner) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.runner = runner
	r.mu.Unlock()
}

// SetRepository attaches the source for dynamic agent definitions.
func (r *Registry) SetRepository(repository AgentRepository) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.repository = repository
	r.mu.Unlock()
}

// Register adds a static agent. Duplicate static keys return an error.
func (r *Registry) Register(agent Agent) error {
	if r == nil {
		return errors.New("agent registry ist nil")
	}
	if agent.Key == "" || agent.PromptFiles == nil || agent.PromptFiles.BundleContent == "" {
		_, _, _, frontmatter, err := resolveAgentPrompts(agent)
		if err != nil {
			return fmt.Errorf("agent prompts auflösen: %w", err)
		}
		agent = mergeAgentDefinition(agent, frontmatter)
	}
	key := strings.TrimSpace(agent.Key)
	if err := validateStaticAgentKey(key); err != nil {
		return err
	}
	if key == "" {
		return errors.New("agent key ist leer")
	}
	agent.Key = key
	stored := cloneAgent(agent)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agents == nil {
		r.agents = make(map[string]Agent)
	}
	if _, exists := r.agents[key]; exists {
		return fmt.Errorf("agent mit key %q ist bereits registriert", key)
	}
	r.agents[key] = stored
	return nil
}

// Get resolves a static agent first, then a repository-backed revision.
// A static key reserves that key, including when the requested version differs.
// An omitted version selects the newest repository revision for dynamic agents.
func (r *Registry) Get(ctx context.Context, key string, versions ...string) (Agent, error) {
	if r == nil {
		return Agent{}, ErrAgentNotFound
	}
	version, err := optionalVersion(versions)
	if err != nil {
		return Agent{}, err
	}
	key = strings.TrimSpace(key)
	r.mu.RLock()
	repository := r.repository
	static, hasStatic := r.agents[key]
	static = cloneAgent(static)
	r.mu.RUnlock()
	if hasStatic {
		if version == "" || sameDefinitionVersion(static.Version, version) {
			return static, nil
		}
		return Agent{}, fmt.Errorf("static agent %q version %q: %w", key, version, ErrAgentNotFound)
	}
	if repository != nil {
		agent, err := repository.Get(ctx, key, version)
		if err == nil {
			return agent, nil
		}
		if !errors.Is(err, ErrDefinitionNotFound) {
			return Agent{}, err
		}
	}
	return Agent{}, fmt.Errorf("agent %q version %q: %w", key, version, ErrAgentNotFound)
}

// Run executes the agent resolved by key using the configured runner.
func (r *Registry) Run(ctx context.Context, key string, inputObject any, options ...RunOptions) (Result, error) {
	if r == nil {
		return Result{}, ErrAgentNotFound
	}
	if len(options) > 1 {
		return Result{}, errors.New("höchstens ein RunOptions-Objekt darf übergeben werden")
	}
	var agent Agent
	var err error
	if len(options) == 1 && options[0].AgentVersion != "" {
		agent, err = r.Get(ctx, key, options[0].AgentVersion)
	} else {
		agent, err = r.Get(ctx, key)
	}
	if err != nil {
		return Result{}, err
	}
	r.mu.RLock()
	runner := r.runner
	r.mu.RUnlock()
	if runner == nil {
		return Result{}, ErrRunnerNotSet
	}
	return runner.Run(ctx, agent, inputObject, options...)
}

// List combines static agents and repository definitions, sorted by key.
// Without a version filter it returns one latest repository revision per key.
func (r *Registry) List(ctx context.Context, versions ...string) ([]Agent, error) {
	if r == nil {
		return nil, nil
	}
	version, err := optionalVersion(versions)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	repository := r.repository
	combined := make(map[string]Agent, len(r.agents))
	staticKeys := make(map[string]struct{}, len(r.agents))
	for key, agent := range r.agents {
		staticKeys[key] = struct{}{}
		if version == "" || sameDefinitionVersion(agent.Version, version) {
			combined[key] = cloneAgent(agent)
		}
	}
	r.mu.RUnlock()
	if repository != nil {
		agents, err := repository.List(ctx, version)
		if err != nil {
			return nil, err
		}
		dynamic := make(map[string]Agent, len(agents))
		for _, agent := range agents {
			if _, isStatic := staticKeys[agent.Key]; isStatic {
				continue
			}
			if version == "" {
				current, exists := dynamic[agent.Key]
				if exists {
					// Repository rows are normally ordered newest first, but explicitly compare versions.
					currentNumber, currentErr := parseDefinitionVersion(current.Version)
					candidateNumber, candidateErr := parseDefinitionVersion(agent.Version)
					if currentErr != nil || candidateErr != nil {
						return nil, fmt.Errorf("invalid agent definition version for %q", agent.Key)
					}
					if currentNumber >= candidateNumber {
						continue
					}
				}
			}
			dynamic[agent.Key] = agent
		}
		for key, agent := range dynamic {
			combined[key] = agent
		}
	}
	keys := make([]string, 0, len(combined))
	for key := range combined {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	agents := make([]Agent, 0, len(keys))
	for _, key := range keys {
		agents = append(agents, cloneAgent(combined[key]))
	}
	return agents, nil
}

func sameDefinitionVersion(left, right string) bool {
	if strings.TrimSpace(left) == strings.TrimSpace(right) {
		return true
	}
	leftNumber, leftErr := parseDefinitionVersion(left)
	rightNumber, rightErr := parseDefinitionVersion(right)
	return leftErr == nil && rightErr == nil && leftNumber == rightNumber
}

func cloneAgent(agent Agent) Agent {
	agent.ModelOptions = append([]ModelOption(nil), agent.ModelOptions...)
	agent.Tools = append([]Tool(nil), agent.Tools...)
	if agent.PromptFiles != nil {
		files := *agent.PromptFiles
		agent.PromptFiles = &files
	}
	return agent
}
