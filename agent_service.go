package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// AgentService manages versioned Agent objects in an AgentRepository.
type AgentService struct {
	repository AgentRepository
}

func NewAgentService(repository AgentRepository) *AgentService {
	return &AgentService{repository: repository}
}

func (s *AgentService) Create(ctx context.Context, agent Agent) (Agent, error) {
	if err := s.checkRepository(); err != nil {
		return Agent{}, err
	}
	return s.repository.Create(ctx, agent)
}

// Seed creates or refreshes a repository agent idempotently. The optional nonce
// makes an otherwise identical seed an explicit new revision.
func (s *AgentService) Seed(ctx context.Context, agent Agent, nonce ...string) (Agent, error) {
	if err := s.checkRepository(); err != nil {
		return Agent{}, err
	}
	return s.repository.Seed(ctx, agent, nonce...)
}

func (s *AgentService) Get(ctx context.Context, key string, versions ...string) (Agent, error) {
	if err := s.checkRepository(); err != nil {
		return Agent{}, err
	}
	version, err := optionalVersion(versions)
	if err != nil {
		return Agent{}, err
	}
	return s.repository.Get(ctx, strings.TrimSpace(key), version)
}

func (s *AgentService) Update(ctx context.Context, key string, agent Agent) (Agent, error) {
	if err := s.checkRepository(); err != nil {
		return Agent{}, err
	}
	definition, err := agentDefinitionForStorage(agent)
	if err != nil {
		return Agent{}, err
	}
	key = strings.TrimSpace(key)
	if definition.Key != key {
		return Agent{}, fmt.Errorf("agent key %q does not match update key %q", definition.Key, key)
	}
	return s.repository.Update(ctx, agent)
}

func (s *AgentService) Delete(ctx context.Context, key string) error {
	if err := s.checkRepository(); err != nil {
		return err
	}
	return s.repository.Delete(ctx, strings.TrimSpace(key))
}

func (s *AgentService) List(ctx context.Context, versions ...string) ([]Agent, error) {
	if err := s.checkRepository(); err != nil {
		return nil, err
	}
	version, err := optionalVersion(versions)
	if err != nil {
		return nil, err
	}
	return s.repository.List(ctx, version)
}

// Materialize is retained as a convenience alias for Get.
func (s *AgentService) Materialize(ctx context.Context, key string, versions ...string) (Agent, error) {
	return s.Get(ctx, key, versions...)
}

func optionalVersion(versions []string) (string, error) {
	if len(versions) > 1 {
		return "", errors.New("at most one agent definition version may be given")
	}
	if len(versions) == 0 {
		return "", nil
	}
	return strings.TrimSpace(versions[0]), nil
}

func (s *AgentService) checkRepository() error {
	if s == nil || s.repository == nil {
		return errors.New("agent service has no agent repository")
	}
	return nil
}

func parseAgentDefinition(source string) (AgentDefinition, Agent, error) {
	sections, metadata, err := parsePromptBundle(source)
	if err != nil {
		return AgentDefinition{}, Agent{}, fmt.Errorf("parse combined agent definition: %w", err)
	}
	key := strings.TrimSpace(metadata.Key)
	if key == "" {
		return AgentDefinition{}, Agent{}, errors.New("combined agent definition is missing frontmatter key")
	}
	if err := validateStaticAgentKey(key); err != nil {
		return AgentDefinition{}, Agent{}, err
	}
	for _, name := range []string{"system-prompt", "input-template", "output-schema"} {
		if _, exists := sections[name]; !exists {
			return AgentDefinition{}, Agent{}, fmt.Errorf("combined agent definition is missing %q section", name)
		}
	}
	if strings.TrimSpace(metadata.Model) == "" {
		return AgentDefinition{}, Agent{}, errors.New("combined agent definition is missing frontmatter model")
	}
	metadata.Key = key
	metadata.SystemPrompt = sections["system-prompt"]
	metadata.InputTemplate = sections["input-template"]
	metadata.OutputSchema = sections["output-schema"]
	if metadata.Output == "" {
		metadata.Output = OutputText
	}
	return AgentDefinition{Key: key, Content: source}, metadata, nil
}
