package agents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"text/template"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"gopkg.in/yaml.v3"
)

// RunnerConfig contains provider configuration; agent callers only use Run.
type RunnerConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
	Logger     RunLogger
}

// Runner executes code-defined agents through the configured provider.
type Runner struct {
	client *openai.Client
	logger RunLogger
}

// ImageInput lets a parameter or context object include an image for visual tasks.
type ImageInput struct {
	MIMEType string
	Data     []byte
}

// Image is a generated image returned by an agent run.
type Image struct {
	MIMEType string
	Data     []byte
}

// Result contains the requested text and/or generated images.
type Result struct {
	Text                 string
	Images               []Image
	DashboardCorrelation string
	ResponseID           string
	ResponseIDs          []string
	PreviousResponseID   string
	Responses            []ResponseMetadata
	Usage                TokenUsage
}

// ResponseMetadata ties token usage to the response that produced it.
type ResponseMetadata struct {
	ID                   string
	PreviousResponseID   string
	DashboardCorrelation string
	Usage                TokenUsage
}

// RunOptions carries request-level cache and conversation state.
type RunOptions struct {
	DashboardCorrelation string
	PreviousResponseID   string
	ModelOverride        string
	AgentVersion         string
	Metadata             map[string]string
}

// TokenUsage summarizes token counts returned by Responses API calls.
type TokenUsage struct {
	InputTokens      int64
	CachedTokens     int64
	CacheWriteTokens int64
	OutputTokens     int64
	TotalTokens      int64
}

// NewRunner configures an agent runner. BaseURL may be a /v1 URL or an older /v1/responses URL.
func NewRunner(config RunnerConfig) *Runner {
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	baseURL := strings.TrimSpace(config.BaseURL)
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	} else if parsed, err := url.Parse(baseURL); err == nil {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/responses")
		parsed.RawQuery = ""
		baseURL = strings.TrimRight(parsed.String(), "/")
	} else {
		baseURL = strings.TrimSuffix(baseURL, "/responses")
	}
	client := openai.NewClient(option.WithAPIKey(config.APIKey), option.WithBaseURL(baseURL), option.WithHTTPClient(httpClient))
	return &Runner{client: &client, logger: config.Logger}
}

// Run renders the agent templates directly from the input object, then executes the agent.
func (r *Runner) Run(ctx context.Context, agent Agent, inputObject any, options ...RunOptions) (runResult Result, runErr error) {
	if r == nil || r.client == nil {
		return Result{}, errors.New("agent runner ist nicht initialisiert")
	}
	if err := validateStaticAgentKey(agent.Key); err != nil {
		return Result{}, err
	}
	if len(options) > 1 {
		return Result{}, errors.New("höchstens ein RunOptions-Objekt darf übergeben werden")
	}
	var runOptions RunOptions
	if len(options) == 1 {
		runOptions = options[0]
	}
	var frontmatter Agent
	systemPrompt, inputTemplate, outputSchema, frontmatter, err := resolveAgentPromptsWithInput(agent, inputObject)
	if err != nil {
		return Result{}, err
	}
	agent = mergeAgentDefinition(agent, frontmatter)
	dashboardCorrelation := resolveDashboardCorrelation(agent, inputObject, runOptions)
	loggingOptions := agent.LoggingOptions
	loggingRequested := loggingOptions.RunLog || loggingOptions.ContextLog || loggingOptions.ResponseLog
	loggingEnabled := loggingRequested && r.logger != nil
	var logRecord *RunLogRecord
	if loggingEnabled {
		if loggingOptions.TTL < 0 {
			return Result{}, errors.New("agent logging TTL darf nicht negativ sein")
		}
		startedAt := time.Now().UTC()
		logRecord = &RunLogRecord{
			StartedAt: startedAt, AgentKey: agent.Key, AgentVersion: agent.Version,
			PreviousResponseID: runOptions.PreviousResponseID,
			MetadataJSON:       runMetadataJSON(agent, runOptions, dashboardCorrelation),
		}
		if loggingOptions.TTL > 0 {
			logRecord.ExpiresAt = startedAt.Add(loggingOptions.TTL)
		}
		if loggingOptions.ContextLog {
			contextJSON, marshalErr := json.Marshal(inputObject)
			if marshalErr != nil {
				defer saveRunLog(ctx, r.logger, logRecord, &runResult, &runErr, loggingOptions)
				return Result{}, fmt.Errorf("agent run context für logging serialisieren: %w", marshalErr)
			}
			logRecord.ContextJSON = string(contextJSON)
		}
		defer saveRunLog(ctx, r.logger, logRecord, &runResult, &runErr, loggingOptions)
	}
	if model := strings.TrimSpace(runOptions.ModelOverride); model != "" {
		agent.Model = model
	}
	if logRecord != nil {
		logRecord.Model = agent.Model
	}
	if strings.TrimSpace(agent.Model) == "" {
		return Result{}, errors.New("agent model ist nicht gesetzt")
	}
	if agent.Output == "" {
		agent.Output = OutputText
	}
	if agent.Output != OutputText && agent.Output != OutputImage && agent.Output != OutputBoth {
		return Result{}, fmt.Errorf("unbekannter agent output-typ %q", agent.Output)
	}
	input, err := renderAgentTemplate(agent.Key+"-input", inputTemplate, inputObject)
	if err != nil {
		return Result{}, err
	}
	if loggingOptions.ContextLog {
		contextJSON, marshalErr := json.Marshal(struct {
			Context       json.RawMessage `json:"context"`
			RenderedInput string          `json:"rendered_input"`
		}{Context: json.RawMessage(logRecord.ContextJSON), RenderedInput: input})
		if marshalErr != nil {
			return Result{}, fmt.Errorf("agent run context-log serialisieren: %w", marshalErr)
		}
		logRecord.ContextJSON = string(contextJSON)
	}
	instructions, err := renderAgentTemplate(agent.Key+"-instructions", systemPrompt+"\n\nAusgabeformat:\n"+outputSchema, inputObject)
	if err != nil {
		return Result{}, err
	}
	images := collectImages(reflect.ValueOf(inputObject))
	result := Result{PreviousResponseID: runOptions.PreviousResponseID, DashboardCorrelation: dashboardCorrelation}
	if agent.Output == OutputText || agent.Output == OutputBoth {
		text, response, err := r.runText(ctx, agent, instructions, input, images, runOptions, dashboardCorrelation)
		if err != nil {
			return Result{}, err
		}
		result.Text = text
		result.addResponse(response, runOptions.PreviousResponseID, dashboardCorrelation)
	}
	if agent.Output == OutputImage || agent.Output == OutputBoth {
		image, response, err := r.runImage(ctx, agent, instructions, input, images, runOptions, dashboardCorrelation)
		if err != nil {
			return Result{}, err
		}
		result.Images = []Image{image}
		result.addResponse(response, runOptions.PreviousResponseID, dashboardCorrelation)
	}
	return result, nil
}

func resolveAgentPrompts(agent Agent) (string, string, string, Agent, error) {
	return resolveAgentPromptsInternal(agent, nil, false)
}

func resolveAgentPromptsWithInput(agent Agent, inputObject any) (string, string, string, Agent, error) {
	return resolveAgentPromptsInternal(agent, inputObject, true)
}

func resolveAgentPromptsInternal(agent Agent, inputObject any, renderFrontmatter bool) (string, string, string, Agent, error) {
	bundle := map[string]string{}
	var metadata Agent
	files := agent.PromptFiles
	if files != nil && files.Bundle != "" && files.BundleContent != "" {
		return "", "", "", Agent{}, errors.New("PromptFiles.Bundle und BundleContent können nicht gleichzeitig gesetzt sein")
	}
	if files != nil && (files.Bundle != "" || files.BundleContent != "") {
		content := files.BundleContent
		if files.Bundle != "" {
			var err error
			content, err = readAgentFile(files.FS, files.Bundle)
			if err != nil {
				return "", "", "", Agent{}, err
			}
		}
		var err error
		if renderFrontmatter {
			bundle, metadata, err = parsePromptBundleWithInput(content, inputObject)
		} else {
			bundle, metadata, err = parsePromptBundle(content)
		}
		if err != nil {
			return "", "", "", Agent{}, fmt.Errorf("prompt bundle parsen: %w", err)
		}
	}
	resolve := func(inline, path, section string) (string, error) {
		if path != "" {
			return readAgentFile(files.FS, path)
		}
		if inline != "" {
			return inline, nil
		}
		return bundle[section], nil
	}
	systemPrompt, err := resolve(agent.SystemPrompt, promptPath(files, func(p *PromptFiles) string { return p.SystemPrompt }), "system-prompt")
	if err != nil {
		return "", "", "", Agent{}, err
	}
	inputTemplate, err := resolve(agent.InputTemplate, promptPath(files, func(p *PromptFiles) string { return p.InputTemplate }), "input-template")
	if err != nil {
		return "", "", "", Agent{}, err
	}
	outputSchema, err := resolve(agent.OutputSchema, promptPath(files, func(p *PromptFiles) string { return p.OutputSchema }), "output-schema")
	if err != nil {
		return "", "", "", Agent{}, err
	}
	return systemPrompt, inputTemplate, outputSchema, metadata, nil
}

func mergeAgentDefinition(explicit, frontmatter Agent) Agent {
	if explicit.Key == "" {
		explicit.Key = frontmatter.Key
	}
	if explicit.Name == "" {
		explicit.Name = frontmatter.Name
	}
	if explicit.Version == "" {
		explicit.Version = frontmatter.Version
	}
	if explicit.Model == "" || (isTemplateText(explicit.Model) && frontmatter.Model != "") {
		explicit.Model = frontmatter.Model
	}
	if explicit.ReasoningEffort == "" || (isTemplateText(explicit.ReasoningEffort) && frontmatter.ReasoningEffort != "") {
		explicit.ReasoningEffort = frontmatter.ReasoningEffort
	}
	if explicit.DashboardCorrelation == "" || (isTemplateText(explicit.DashboardCorrelation) && frontmatter.DashboardCorrelation != "") {
		explicit.DashboardCorrelation = frontmatter.DashboardCorrelation
	}
	if len(explicit.ModelOptions) == 0 {
		explicit.ModelOptions = frontmatter.ModelOptions
	}
	if len(explicit.Tools) == 0 {
		explicit.Tools = frontmatter.Tools
	}
	if explicit.Output == "" {
		explicit.Output = frontmatter.Output
	}
	if explicit.Image.Model == "" {
		explicit.Image.Model = frontmatter.Image.Model
	}
	if explicit.Image.Size == "" {
		explicit.Image.Size = frontmatter.Image.Size
	}
	if explicit.LoggingOptions == (LoggingOptions{}) {
		explicit.LoggingOptions = frontmatter.LoggingOptions
	}
	return explicit
}

func isTemplateText(value string) bool {
	return strings.Contains(value, "{{") && strings.Contains(value, "}}")
}

func promptPath(files *PromptFiles, get func(*PromptFiles) string) string {
	if files == nil {
		return ""
	}
	return get(files)
}

func readAgentFile(source fs.FS, path string) (string, error) {
	if source == nil {
		return "", fmt.Errorf("agent prompt datei %q angegeben, aber PromptFiles.FS ist nil", path)
	}
	content, err := fs.ReadFile(source, path)
	if err != nil {
		return "", fmt.Errorf("agent prompt datei %q lesen: %w", path, err)
	}
	return string(content), nil
}

func parsePromptBundle(source string) (map[string]string, Agent, error) {
	return parsePromptBundleInternal(source, nil, false)
}

func parsePromptBundleWithInput(source string, inputObject any) (map[string]string, Agent, error) {
	return parsePromptBundleInternal(source, inputObject, true)
}

func parsePromptBundleInternal(source string, inputObject any, renderFrontmatter bool) (map[string]string, Agent, error) {
	metadata := Agent{}
	source = strings.ReplaceAll(source, "\r\n", "\n")
	if strings.HasPrefix(source, "---\n") {
		end := strings.Index(source[4:], "\n---\n")
		if end < 0 {
			return nil, Agent{}, errors.New("frontmatter wurde nicht mit --- geschlossen")
		}
		frontmatterText := source[4 : 4+end]
		if err := validateFrontmatterKeyTemplate(frontmatterText); err != nil {
			return nil, Agent{}, err
		}
		if renderFrontmatter {
			var err error
			frontmatterText, err = renderAgentTemplate("agent-frontmatter", frontmatterText, inputObject)
			if err != nil {
				return nil, Agent{}, err
			}
		}
		decoder := yaml.NewDecoder(strings.NewReader(frontmatterText))
		decoder.KnownFields(true)
		var frontmatter struct {
			Key                  string        `yaml:"key"`
			Name                 string        `yaml:"name"`
			Version              string        `yaml:"version"`
			Model                string        `yaml:"model"`
			ReasoningEffort      string        `yaml:"reasoning_effort"`
			DashboardCorrelation string        `yaml:"dashboard_correlation"`
			ModelOptions         []ModelOption `yaml:"model_options"`
			Tools                []Tool        `yaml:"tools"`
			Output               OutputType    `yaml:"output"`
			Image                ImageOptions  `yaml:"image"`
			LoggingOptions       struct {
				RunLog      bool   `yaml:"run_log"`
				ContextLog  bool   `yaml:"context_log"`
				ResponseLog bool   `yaml:"response_log"`
				TTL         string `yaml:"ttl"`
			} `yaml:"logging_options"`
			AgentOverrides exportedAgentOverrides `yaml:"agent_overrides"`
		}
		if err := decoder.Decode(&frontmatter); err != nil {
			if renderFrontmatter || !strings.Contains(frontmatterText, "{{") {
				return nil, Agent{}, fmt.Errorf("frontmatter lesen: %w", err)
			}
		} else {
			loggingTTL := time.Duration(0)
			if strings.TrimSpace(frontmatter.LoggingOptions.TTL) != "" {
				var err error
				loggingTTL, err = time.ParseDuration(frontmatter.LoggingOptions.TTL)
				if err != nil {
					return nil, Agent{}, fmt.Errorf("frontmatter logging_options.ttl lesen: %w", err)
				}
			}
			metadata = Agent{
				Key: frontmatter.Key, Name: frontmatter.Name, Version: frontmatter.Version,
				Model: frontmatter.Model, ReasoningEffort: frontmatter.ReasoningEffort, DashboardCorrelation: frontmatter.DashboardCorrelation,
				ModelOptions: frontmatter.ModelOptions, Tools: frontmatter.Tools,
				Output: frontmatter.Output, Image: frontmatter.Image,
				LoggingOptions: LoggingOptions{
					RunLog: frontmatter.LoggingOptions.RunLog, ContextLog: frontmatter.LoggingOptions.ContextLog,
					ResponseLog: frontmatter.LoggingOptions.ResponseLog, TTL: loggingTTL,
				},
			}
			if err := validateStaticAgentKey(metadata.Key); err != nil {
				return nil, Agent{}, err
			}
			overrides, err := frontmatter.AgentOverrides.toAgent()
			if err != nil {
				return nil, Agent{}, fmt.Errorf("frontmatter agent_overrides lesen: %w", err)
			}
			metadata = applyAgentOverrides(metadata, overrides)
			if err := validateStaticAgentKey(metadata.Key); err != nil {
				return nil, Agent{}, err
			}
		}
		source = source[4+end+len("\n---\n"):]
	}
	sections := map[string]string{}
	var current string
	nestedFence := false
	var content strings.Builder
	for _, line := range strings.SplitAfter(source, "\n") {
		trimmed := strings.TrimSpace(line)
		if current == "" {
			if trimmed == "" {
				continue
			}
			if !strings.HasPrefix(trimmed, "'''") {
				return nil, Agent{}, fmt.Errorf("unerwarteter Inhalt außerhalb eines Abschnitts: %q", trimmed)
			}
			section := strings.TrimSpace(strings.TrimPrefix(trimmed, "'''"))
			switch section {
			case "system-prompt", "input-template", "output-schema":
			default:
				return nil, Agent{}, fmt.Errorf("unbekannter Prompt-Abschnitt %q", section)
			}
			if _, exists := sections[section]; exists {
				return nil, Agent{}, fmt.Errorf("Prompt-Abschnitt %q ist mehrfach definiert", section)
			}
			current = section
			content.Reset()
			continue
		}
		if nestedFence {
			if trimmed == "'''" {
				nestedFence = false
			}
			content.WriteString(line)
			continue
		}
		if trimmed == "'''" {
			sections[current] = strings.TrimSpace(content.String())
			current = ""
			content.Reset()
			continue
		}
		if strings.HasPrefix(trimmed, "'''") && strings.TrimSpace(strings.TrimPrefix(trimmed, "'''")) != "" {
			nestedFence = true
		}
		content.WriteString(line)
	}
	if nestedFence {
		return nil, Agent{}, fmt.Errorf("eingebetteter Prompt-Fence in Abschnitt %q wurde nicht mit ''' geschlossen", current)
	}
	if current != "" {
		return nil, Agent{}, fmt.Errorf("Prompt-Abschnitt %q wurde nicht mit ''' geschlossen", current)
	}
	return sections, metadata, nil
}

func (r *Result) addResponse(response *responses.Response, previousResponseID string, dashboardCorrelation string) {
	if response == nil {
		return
	}
	r.ResponseID = response.ID
	r.ResponseIDs = append(r.ResponseIDs, response.ID)
	usage := TokenUsage{
		InputTokens:      response.Usage.InputTokens,
		CachedTokens:     response.Usage.InputTokensDetails.CachedTokens,
		CacheWriteTokens: response.Usage.InputTokensDetails.CacheWriteTokens,
		OutputTokens:     response.Usage.OutputTokens,
		TotalTokens:      response.Usage.TotalTokens,
	}
	r.Responses = append(r.Responses, ResponseMetadata{ID: response.ID, PreviousResponseID: previousResponseID, DashboardCorrelation: dashboardCorrelation, Usage: usage})
	r.Usage.InputTokens += usage.InputTokens
	r.Usage.CachedTokens += usage.CachedTokens
	r.Usage.CacheWriteTokens += usage.CacheWriteTokens
	r.Usage.OutputTokens += usage.OutputTokens
	r.Usage.TotalTokens += usage.TotalTokens
}

func renderAgentTemplate(name, source string, inputObject any) (string, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(source)
	if err != nil {
		return "", fmt.Errorf("agent template %q parsen: %w", name, err)
	}
	var rendered strings.Builder
	if err := t.Execute(&rendered, inputObject); err != nil {
		return "", fmt.Errorf("agent template %q ausführen: %w", name, err)
	}
	return strings.TrimSpace(rendered.String()), nil
}

func (r *Runner) runText(ctx context.Context, agent Agent, instructions, input string, images []ImageInput, options RunOptions, dashboardCorrelation string) (string, *responses.Response, error) {
	if input == "" && len(images) == 0 {
		return "", nil, errors.New("agent input ist leer")
	}
	content := make([]responses.ResponseInputContentUnionParam, 0, len(images)+1)
	if input != "" {
		content = append(content, responses.ResponseInputContentUnionParam{OfInputText: &responses.ResponseInputTextParam{Text: input}})
	}
	for _, image := range images {
		if len(image.Data) == 0 || strings.TrimSpace(image.MIMEType) == "" {
			return "", nil, errors.New("agent image input benötigt MIMEType und Data")
		}
		dataURL := "data:" + image.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)
		content = append(content, responses.ResponseInputContentUnionParam{OfInputImage: &responses.ResponseInputImageParam{
			Detail:   responses.ResponseInputImageDetailAuto,
			ImageURL: openai.String(dataURL),
		}})
	}
	request := map[string]any{
		"model":        agent.Model,
		"instructions": instructions,
		"input":        []any{map[string]any{"role": "user", "content": content}},
		"store":        false,
	}
	addRunOptions(request, options, agent, dashboardCorrelation)
	if len(agent.Tools) > 0 {
		request["tools"] = agent.Tools
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "", nil, fmt.Errorf("agent request serialisieren: %w", err)
	}
	var params responses.ResponseNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		return "", nil, fmt.Errorf("agent request in SDK-parameter umwandeln: %w", err)
	}
	response, err := r.client.Responses.New(ctx, params)
	if err != nil {
		return "", nil, fmt.Errorf("agent %q ausführen: %w", agent.Key, err)
	}
	text := strings.TrimSpace(response.OutputText())
	if text == "" {
		return "", nil, fmt.Errorf("agent %q lieferte keinen text", agent.Key)
	}
	return text, response, nil
}

func (r *Runner) runImage(ctx context.Context, agent Agent, instructions, input string, images []ImageInput, options RunOptions, dashboardCorrelation string) (Image, *responses.Response, error) {
	prompt := strings.TrimSpace(instructions + "\n\n" + input)
	if prompt == "" {
		return Image{}, nil, errors.New("agent image prompt ist leer")
	}
	imageModel := agent.Image.Model
	if imageModel == "" {
		imageModel = DefaultImageModel
	}
	size := agent.Image.Size
	if size == "" {
		size = "1024x1024"
	}
	// The Responses image tool handles both text-to-image and reference-image editing.
	content := make([]responses.ResponseInputContentUnionParam, 0, len(images)+1)
	if input != "" {
		content = append(content, responses.ResponseInputContentUnionParam{OfInputText: &responses.ResponseInputTextParam{Text: input}})
	}
	for _, image := range images {
		if len(image.Data) == 0 || strings.TrimSpace(image.MIMEType) == "" {
			return Image{}, nil, errors.New("agent image input benötigt MIMEType und Data")
		}
		dataURL := "data:" + image.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)
		content = append(content, responses.ResponseInputContentUnionParam{OfInputImage: &responses.ResponseInputImageParam{
			Detail: responses.ResponseInputImageDetailAuto, ImageURL: openai.String(dataURL),
		}})
	}
	request := map[string]any{
		"model":        agent.Model,
		"instructions": instructions,
		"input":        []any{map[string]any{"role": "user", "content": content}},
		"tools":        []any{map[string]any{"type": "image_generation", "model": imageModel, "size": size}},
		"store":        false,
	}
	addRunOptions(request, options, agent, dashboardCorrelation)
	body, err := json.Marshal(request)
	if err != nil {
		return Image{}, nil, fmt.Errorf("agent image request serialisieren: %w", err)
	}
	var params responses.ResponseNewParams
	if err := json.Unmarshal(body, &params); err != nil {
		return Image{}, nil, fmt.Errorf("agent image request in SDK-parameter umwandeln: %w", err)
	}
	response, err := r.client.Responses.New(ctx, params)
	if err != nil {
		return Image{}, nil, fmt.Errorf("agent %q bildgenerierung: %w", agent.Key, err)
	}
	for _, item := range response.Output {
		if item.Type == "image_generation_call" {
			image := item.AsImageGenerationCall()
			if image.Result != "" {
				data, err := base64.StdEncoding.DecodeString(image.Result)
				if err != nil {
					return Image{}, nil, fmt.Errorf("agent bild dekodieren: %w", err)
				}
				return Image{MIMEType: "image/png", Data: data}, response, nil
			}
		}
	}
	return Image{}, nil, fmt.Errorf("agent %q lieferte kein bild", agent.Key)
}

func addRunOptions(request map[string]any, options RunOptions, agent Agent, dashboardCorrelation string) {
	if dashboardCorrelation != "" {
		request["prompt_cache_key"] = dashboardCorrelation
	}
	metadata := make(map[string]string, len(options.Metadata)+3)
	for key, value := range options.Metadata {
		metadata[key] = value
	}
	// Keep SDK-owned classification fields authoritative if callers use the same keys.
	if agent.Key != "" {
		metadata["agent_key"] = agent.Key
	}
	if agent.Version != "" {
		metadata["agent_version"] = agent.Version
	}
	if dashboardCorrelation != "" {
		metadata["dashboard_correlation"] = dashboardCorrelation
	}
	if len(metadata) > 0 {
		request["metadata"] = metadata
	}
	if options.PreviousResponseID != "" {
		request["previous_response_id"] = options.PreviousResponseID
	}
}

func runMetadataJSON(agent Agent, options RunOptions, dashboardCorrelation string) string {
	metadata := make(map[string]string, len(options.Metadata)+3)
	for key, value := range options.Metadata {
		metadata[key] = value
	}
	if agent.Key != "" {
		metadata["agent_key"] = agent.Key
	}
	if agent.Version != "" {
		metadata["agent_version"] = agent.Version
	}
	if dashboardCorrelation != "" {
		metadata["dashboard_correlation"] = dashboardCorrelation
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func saveRunLog(ctx context.Context, logger RunLogger, record *RunLogRecord, result *Result, runErr *error, options LoggingOptions) {
	if record == nil || logger == nil {
		return
	}
	record.EndedAt = time.Now().UTC()
	if responseIDs, err := json.Marshal(result.ResponseIDs); err != nil {
		*runErr = errors.Join(*runErr, fmt.Errorf("agent run response ids serialisieren: %w", err))
	} else {
		record.ResponseIDsJSON = string(responseIDs)
	}
	record.InputTokens = result.Usage.InputTokens
	record.CachedTokens = result.Usage.CachedTokens
	record.CacheWriteTokens = result.Usage.CacheWriteTokens
	record.OutputTokens = result.Usage.OutputTokens
	record.TotalTokens = result.Usage.TotalTokens
	if options.ResponseLog {
		responseJSON, err := json.Marshal(struct {
			Text   string  `json:"text,omitempty"`
			Images []Image `json:"images,omitempty"`
		}{Text: result.Text, Images: result.Images})
		if err != nil {
			*runErr = errors.Join(*runErr, fmt.Errorf("agent run response-log serialisieren: %w", err))
		} else {
			record.ResponseJSON = string(responseJSON)
		}
	}
	if *runErr != nil {
		record.ErrorMessage = (*runErr).Error()
	}
	if err := logger.SaveRun(ctx, *record); err != nil {
		*runErr = errors.Join(*runErr, fmt.Errorf("agent run log speichern: %w", err))
	}
}

func resolveDashboardCorrelation(agent Agent, inputObject any, options RunOptions) string {
	if value := strings.TrimSpace(options.DashboardCorrelation); value != "" {
		return value
	}
	if value := dashboardCorrelationFromInput(reflect.ValueOf(inputObject)); value != "" {
		return value
	}
	if value := strings.TrimSpace(agent.DashboardCorrelation); value != "" {
		return value
	}
	return strings.TrimSpace(agent.Key)
}

func dashboardCorrelationFromInput(value reflect.Value) string {
	if !value.IsValid() {
		return ""
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return ""
		}
		return dashboardCorrelationFromInput(value.Elem())
	}
	switch value.Kind() {
	case reflect.Map:
		keyType := value.Type().Key()
		if keyType.Kind() != reflect.String {
			return ""
		}
		key := reflect.ValueOf("DashboardCorrelation").Convert(keyType)
		field := value.MapIndex(key)
		if field.IsValid() && field.Kind() == reflect.String {
			return strings.TrimSpace(field.String())
		}
	case reflect.Struct:
		field := value.FieldByName("DashboardCorrelation")
		if field.IsValid() && field.Kind() == reflect.String && field.CanInterface() {
			return strings.TrimSpace(field.String())
		}
	}
	return ""
}

func collectImages(value reflect.Value) []ImageInput {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		return collectImages(value.Elem())
	}
	if value.CanInterface() {
		if image, ok := value.Interface().(ImageInput); ok {
			return []ImageInput{image}
		}
	}
	var found []ImageInput
	switch value.Kind() {
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			found = append(found, collectImages(iter.Value())...)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			found = append(found, collectImages(value.Index(i))...)
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if value.Field(i).CanInterface() {
				found = append(found, collectImages(value.Field(i))...)
			}
		}
	}
	return found
}
