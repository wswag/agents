package agents

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type exportedLoggingOptions struct {
	RunLog      bool   `yaml:"run_log,omitempty"`
	ContextLog  bool   `yaml:"context_log,omitempty"`
	ResponseLog bool   `yaml:"response_log,omitempty"`
	TTL         string `yaml:"ttl,omitempty"`
}

type exportedAgentDefinition struct {
	Key                  string                  `yaml:"key,omitempty"`
	Name                 string                  `yaml:"name,omitempty"`
	Version              string                  `yaml:"version,omitempty"`
	Model                string                  `yaml:"model,omitempty"`
	ReasoningEffort      string                  `yaml:"reasoning_effort,omitempty"`
	DashboardCorrelation string                  `yaml:"dashboard_correlation,omitempty"`
	ModelOptions         []ModelOption           `yaml:"model_options,omitempty"`
	Tools                []Tool                  `yaml:"tools,omitempty"`
	Output               OutputType              `yaml:"output,omitempty"`
	Image                *ImageOptions           `yaml:"image,omitempty"`
	LoggingOptions       *exportedLoggingOptions `yaml:"logging_options,omitempty"`
}

type exportedAgentOverrides exportedAgentDefinition

func validateStaticAgentKey(key string) error {
	if strings.Contains(key, "{{") || strings.Contains(key, "}}") {
		return fmt.Errorf("agent key darf keinen Template-Code enthalten")
	}
	return nil
}

func validateFrontmatterKeyTemplate(frontmatter string) error {
	for _, line := range strings.Split(frontmatter, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "key:") && (strings.Contains(trimmed, "{{") || strings.Contains(trimmed, "}}")) {
			return fmt.Errorf("agent key darf keinen Template-Code enthalten")
		}
	}
	return nil
}

func (values exportedAgentOverrides) toAgent() (Agent, error) {
	agent := Agent{
		Key: values.Key, Name: values.Name, Version: values.Version, Model: values.Model, ReasoningEffort: values.ReasoningEffort,
		DashboardCorrelation: values.DashboardCorrelation, ModelOptions: values.ModelOptions,
		Tools: values.Tools, Output: values.Output,
	}
	if values.Image != nil {
		agent.Image = *values.Image
	}
	if values.LoggingOptions != nil {
		ttl := time.Duration(0)
		if strings.TrimSpace(values.LoggingOptions.TTL) != "" {
			var err error
			ttl, err = time.ParseDuration(values.LoggingOptions.TTL)
			if err != nil {
				return Agent{}, err
			}
		}
		agent.LoggingOptions = LoggingOptions{
			RunLog: values.LoggingOptions.RunLog, ContextLog: values.LoggingOptions.ContextLog,
			ResponseLog: values.LoggingOptions.ResponseLog, TTL: ttl,
		}
	}
	return agent, nil
}

func applyAgentOverrides(agent Agent, overrides Agent) Agent {
	if overrides.Key != "" {
		agent.Key = overrides.Key
	}
	if overrides.Name != "" {
		agent.Name = overrides.Name
	}
	if overrides.Version != "" {
		agent.Version = overrides.Version
	}
	if overrides.Model != "" {
		agent.Model = overrides.Model
	}
	if overrides.DashboardCorrelation != "" {
		agent.DashboardCorrelation = overrides.DashboardCorrelation
	}
	if len(overrides.ModelOptions) > 0 {
		agent.ModelOptions = overrides.ModelOptions
	}
	if len(overrides.Tools) > 0 {
		agent.Tools = overrides.Tools
	}
	if overrides.Output != "" {
		agent.Output = overrides.Output
	}
	if overrides.Image.Model != "" {
		agent.Image.Model = overrides.Image.Model
	}
	if overrides.Image.Size != "" {
		agent.Image.Size = overrides.Image.Size
	}
	if overrides.LoggingOptions != (LoggingOptions{}) {
		agent.LoggingOptions = overrides.LoggingOptions
	}
	return agent
}

// ExportPromptBundle serializes an agent and its prompts in the combined prompt bundle format.
// Prompt content may come from inline fields, a bundle, or files in PromptFiles.FS.
func ExportPromptBundle(agent Agent) (string, error) {
	if err := validateStaticAgentKey(agent.Key); err != nil {
		return "", err
	}
	bundleSource, hasBundle, err := readAgentBundleSource(agent)
	if err != nil {
		return "", err
	}
	if hasBundle {
		frontmatterText, _, hasFrontmatter, err := splitPromptBundle(bundleSource)
		if err != nil {
			return "", err
		}
		if hasFrontmatter && strings.Contains(frontmatterText, "{{") {
			return exportTemplatedFrontmatterBundle(agent, frontmatterText)
		}
	}
	systemPrompt, inputTemplate, outputSchema, frontmatter, err := resolveAgentPrompts(agent)
	if err != nil {
		return "", fmt.Errorf("agent prompts für export auflösen: %w", err)
	}
	agent = mergeAgentDefinition(agent, frontmatter)
	definition := exportedAgentDefinition{
		Key: agent.Key, Name: agent.Name, Version: agent.Version, Model: agent.Model,
		DashboardCorrelation: agent.DashboardCorrelation, ReasoningEffort: agent.ReasoningEffort, ModelOptions: agent.ModelOptions,
		Tools: agent.Tools, Output: agent.Output,
	}
	if agent.Image != (ImageOptions{}) {
		definition.Image = &agent.Image
	}
	if agent.LoggingOptions != (LoggingOptions{}) {
		logging := &exportedLoggingOptions{
			RunLog: agent.LoggingOptions.RunLog, ContextLog: agent.LoggingOptions.ContextLog,
			ResponseLog: agent.LoggingOptions.ResponseLog,
		}
		if agent.LoggingOptions.TTL > 0 {
			logging.TTL = agent.LoggingOptions.TTL.String()
		}
		definition.LoggingOptions = logging
	}
	frontmatterYAML, err := yaml.Marshal(definition)
	if err != nil {
		return "", fmt.Errorf("agent frontmatter serialisieren: %w", err)
	}
	var bundle strings.Builder
	bundle.WriteString("---\n")
	bundle.Write(frontmatterYAML)
	bundle.WriteString("---\n")
	writePromptSection(&bundle, "system-prompt", systemPrompt)
	writePromptSection(&bundle, "input-template", inputTemplate)
	writePromptSection(&bundle, "output-schema", outputSchema)
	return bundle.String(), nil
}

func exportTemplatedFrontmatterBundle(agent Agent, rawFrontmatter string) (string, error) {
	systemPrompt, inputTemplate, outputSchema, frontmatter, err := resolveAgentPrompts(agent)
	if err != nil {
		return "", fmt.Errorf("agent prompts für export auflösen: %w", err)
	}
	agent = mergeAgentDefinition(agent, frontmatter)
	if agent.Key == "" {
		agent.Key = staticFrontmatterValue(rawFrontmatter, "key")
	}
	if err := validateStaticAgentKey(agent.Key); err != nil {
		return "", err
	}
	overrides := exportedAgentOverrides(agentToExportedDefinition(agent))
	overridesYAML, err := yaml.Marshal(overrides)
	if err != nil {
		return "", fmt.Errorf("agent overrides serialisieren: %w", err)
	}
	var bundle strings.Builder
	bundle.WriteString("---\n")
	bundle.WriteString(strings.TrimSpace(rawFrontmatter))
	bundle.WriteByte('\n')
	if !strings.Contains(rawFrontmatter, "agent_overrides:") && strings.TrimSpace(string(overridesYAML)) != "{}" {
		bundle.WriteString("agent_overrides:\n")
		for _, line := range strings.Split(strings.TrimSuffix(string(overridesYAML), "\n"), "\n") {
			bundle.WriteString("  " + line + "\n")
		}
	}
	bundle.WriteString("---\n")
	writePromptSection(&bundle, "system-prompt", systemPrompt)
	writePromptSection(&bundle, "input-template", inputTemplate)
	writePromptSection(&bundle, "output-schema", outputSchema)
	return bundle.String(), nil
}

func agentToExportedDefinition(agent Agent) exportedAgentDefinition {
	definition := exportedAgentDefinition{
		Key: agent.Key, Name: agent.Name, Version: agent.Version, Model: agent.Model,
		DashboardCorrelation: agent.DashboardCorrelation, ReasoningEffort: agent.ReasoningEffort, ModelOptions: agent.ModelOptions,
		Tools: agent.Tools, Output: agent.Output,
	}
	if agent.Image != (ImageOptions{}) {
		definition.Image = &agent.Image
	}
	if agent.LoggingOptions != (LoggingOptions{}) {
		logging := &exportedLoggingOptions{
			RunLog: agent.LoggingOptions.RunLog, ContextLog: agent.LoggingOptions.ContextLog,
			ResponseLog: agent.LoggingOptions.ResponseLog,
		}
		if agent.LoggingOptions.TTL > 0 {
			logging.TTL = agent.LoggingOptions.TTL.String()
		}
		definition.LoggingOptions = logging
	}
	return definition
}

func readAgentBundleSource(agent Agent) (string, bool, error) {
	files := agent.PromptFiles
	if files == nil || (files.Bundle == "" && files.BundleContent == "") {
		return "", false, nil
	}
	if files.Bundle != "" && files.BundleContent != "" {
		return "", false, fmt.Errorf("PromptFiles.Bundle und BundleContent können nicht gleichzeitig gesetzt sein")
	}
	if files.BundleContent != "" {
		return files.BundleContent, true, nil
	}
	content, err := readAgentFile(files.FS, files.Bundle)
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

func splitPromptBundle(source string) (frontmatter string, body string, hasFrontmatter bool, err error) {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	if !strings.HasPrefix(source, "---\n") {
		return "", source, false, nil
	}
	end := strings.Index(source[4:], "\n---\n")
	if end < 0 {
		return "", "", false, fmt.Errorf("frontmatter wurde nicht mit --- geschlossen")
	}
	return source[4 : 4+end], source[4+end+len("\n---\n"):], true, nil
}

func staticFrontmatterValue(frontmatter string, key string) string {
	for _, line := range strings.Split(frontmatter, "\n") {
		trimmed := strings.TrimSpace(line)
		prefix := key + ":"
		if strings.HasPrefix(trimmed, prefix) {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
			value = strings.Trim(value, "\"'")
			if !strings.Contains(value, "{{") && !strings.Contains(value, "}}") {
				return value
			}
		}
	}
	return ""
}

func writePromptSection(bundle *strings.Builder, name string, content string) {
	bundle.WriteString("'''" + name + "\n")
	bundle.WriteString(strings.TrimSpace(content))
	bundle.WriteString("\n'''\n")
}
