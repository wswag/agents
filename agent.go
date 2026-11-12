package agents

import (
	"io/fs"
	"time"
)

// Agent describes a reusable, specialized AI agent with a versioned definition.
type Agent struct {
	Key                  string
	Name                 string
	Version              string
	Model                string
	ReasoningEffort      string
	DashboardCorrelation string
	ModelOptions         []ModelOption
	Tools                []Tool
	SystemPrompt         string
	InputTemplate        string
	OutputSchema         string
	Output               OutputType
	Image                ImageOptions
	LoggingOptions       LoggingOptions
	PromptFiles          *PromptFiles
}

// LoggingOptions controls which parts of an agent run are persisted.
// Context and response data are stored only when their respective flags are enabled.
type LoggingOptions struct {
	RunLog      bool
	ContextLog  bool
	ResponseLog bool
	TTL         time.Duration
}

// PromptFiles loads agent text and templates from an embedded or other fs.FS filesystem.
type PromptFiles struct {
	FS            fs.FS
	Bundle        string
	BundleContent string
	SystemPrompt  string
	InputTemplate string
	OutputSchema  string
}

// OutputType selects the result shape produced by a defined agent.
type OutputType string

const (
	OutputText  OutputType = "text"
	OutputImage OutputType = "image"
	OutputBoth  OutputType = "both"
)

// ImageOptions configures image generation for agents with OutputImage or OutputBoth.
type ImageOptions struct {
	Model string
	Size  string
}

// ModelOption beschreibt ein vom Betreiber explizit freigegebenes Modell.
type ModelOption struct {
	ID   string
	Name string
}

// Tool beschreibt ein vom jeweiligen AI-Provider unterstütztes Agentenwerkzeug.
type Tool struct {
	Type string
}

const ToolWebSearch = "web_search"

// DefaultImageModel is the module default for agents that request image output.
const DefaultImageModel = "gpt-image-2.5-flare"
