package agents

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// ImageProvider generates an image from the prompt produced by the agent's language model.
type ImageProvider interface {
	GenerateImage(ctx context.Context, request ImageProviderRequest) (Image, ImageProviderMetadata, error)
}

type ImageProviderMetadata struct {
	ResponseID string
	Usage      TokenUsage
}
type ImageProviderRequest struct {
	Prompt               string
	Model                string
	Options              map[string]any
	Agent                Agent
	Instructions         string
	Input                string
	Images               []ImageInput
	RunOptions           RunOptions
	DashboardCorrelation string
	Client               *openai.Client
}

func (r *Runner) RegisterImageProvider(name string, provider ImageProvider) error {
	if r == nil {
		return errors.New("agent runner ist nicht initialisiert")
	}
	name = strings.TrimSpace(name)
	if name == "" || provider == nil {
		return errors.New("image provider name und implementation sind erforderlich")
	}
	if r.imageProviders == nil {
		r.imageProviders = make(map[string]ImageProvider)
	}
	r.imageProviders[name] = provider
	return nil
}

// BFLImageProvider implements BFL's asynchronous image generation API.
type BFLImageProvider struct {
	APIKey     string
	HTTPClient *http.Client
	BaseURL    string
}

// OpenAIImageProvider implements OpenAI's image creation API
type OpenAIImageProvider struct{ runner *Runner }

func (p BFLImageProvider) GenerateImage(ctx context.Context, request ImageProviderRequest) (Image, ImageProviderMetadata, error) {
	if strings.TrimSpace(p.APIKey) == "" {
		return Image{}, ImageProviderMetadata{}, errors.New("BFL API-Key fehlt")
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = "https://api.bfl.ai"
	}
	image, err := generateBFLImage(ctx, client, p.APIKey, base, request.Prompt, request.Model, request.Options)
	return image, ImageProviderMetadata{}, err
}

func generateBFLImage(ctx context.Context, client *http.Client, key, base, prompt, model string, options map[string]any) (Image, error) {
	if model == "" {
		model = "flux-2-pro"
	}
	bodyOptions := map[string]any{}
	for k, v := range options {
		bodyOptions[k] = v
	}
	bodyOptions["prompt"] = prompt
	body, err := json.Marshal(bodyOptions)
	if err != nil {
		return Image{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/"+model, bytes.NewReader(body))
	if err != nil {
		return Image{}, err
	}
	req.Header.Set("x-key", key)
	req.Header.Set("content-type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Image{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Image{}, fmt.Errorf("BFL generation: HTTP %d", resp.StatusCode)
	}
	var submitted struct {
		PollingURL string `json:"polling_url"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&submitted); err != nil {
		return Image{}, err
	}
	for {
		poll, err := http.NewRequestWithContext(ctx, http.MethodGet, submitted.PollingURL, nil)
		if err != nil {
			return Image{}, err
		}
		pr, err := client.Do(poll)
		if err != nil {
			return Image{}, err
		}
		var result struct {
			Status string `json:"status"`
			Result struct {
				Sample string `json:"sample"`
			} `json:"result"`
		}
		err = json.NewDecoder(pr.Body).Decode(&result)
		pr.Body.Close()
		if err != nil {
			return Image{}, err
		}
		if result.Status == "Ready" {
			ir, err := http.NewRequestWithContext(ctx, http.MethodGet, result.Result.Sample, nil)
			if err != nil {
				return Image{}, err
			}
			got, err := client.Do(ir)
			if err != nil {
				return Image{}, err
			}
			defer got.Body.Close()
			data, err := io.ReadAll(got.Body)
			if err != nil {
				return Image{}, err
			}
			return Image{MIMEType: http.DetectContentType(data), Data: data}, nil
		}
		if result.Status == "Error" || result.Status == "Failed" || strings.Contains(result.Status, "Moderated") {
			return Image{}, fmt.Errorf("BFL generation status %q", result.Status)
		}
		select {
		case <-ctx.Done():
			return Image{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (p OpenAIImageProvider) GenerateImage(ctx context.Context, req ImageProviderRequest) (Image, ImageProviderMetadata, error) {
	image, response, err := p.generateOpenAI(ctx, req)
	if response == nil {
		return image, ImageProviderMetadata{}, err
	}
	return image, ImageProviderMetadata{ResponseID: response.ID, Usage: TokenUsage{InputTokens: response.Usage.InputTokens, CachedTokens: response.Usage.InputTokensDetails.CachedTokens, CacheWriteTokens: response.Usage.InputTokensDetails.CacheWriteTokens, OutputTokens: response.Usage.OutputTokens, TotalTokens: response.Usage.TotalTokens}}, err
}
func (p OpenAIImageProvider) generateOpenAI(ctx context.Context, req ImageProviderRequest) (Image, *responses.Response, error) {
	agent, instructions, input, images, options, dashboardCorrelation := req.Agent, req.Instructions, req.Input, req.Images, req.RunOptions, req.DashboardCorrelation
	r := p.runner
	imageModel := agent.Image.Model
	if imageModel == "" {
		imageModel = DefaultImageModel
	}
	imageOptions := map[string]any{}
	for key, value := range agent.Image.Options {
		imageOptions[key] = value
	}
	size, _ := imageOptions["size"].(string)
	if size == "" {
		size = agent.Image.Size
	}
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
	imageTool := map[string]any{"type": "image_generation", "model": imageModel, "size": size}
	quality, _ := imageOptions["quality"].(string)
	if quality == "" {
		quality = agent.Image.Quality
	}
	if quality == "" {
		quality = "low"
	}
	imageTool["quality"] = quality
	format, _ := imageOptions["output_format"].(string)
	if format == "" {
		format = agent.Image.OutputFormat
	}
	if format == "" {
		format = "png"
	}
	var mimeType string
	switch format {
	case "png":
		mimeType = "image/png"
	case "jpeg":
		mimeType = "image/jpeg"
	case "webp":
		mimeType = "image/webp"
	default:
		return Image{}, nil, fmt.Errorf("unbekanntes bildformat %q", format)
	}
	imageTool["output_format"] = format
	request := map[string]any{
		"model":        agent.Model,
		"instructions": instructions,
		"input":        []any{map[string]any{"role": "user", "content": content}},
		"tools":        []any{imageTool},
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
				return Image{MIMEType: mimeType, Data: data}, response, nil
			}
		}
	}
	return Image{}, nil, fmt.Errorf("agent %q lieferte kein bild", agent.Key)
}
