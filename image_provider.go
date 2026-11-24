package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ImageProvider generates an image from the prompt produced by the agent's language model.
type ImageProvider interface {
	GenerateImage(ctx context.Context, prompt, model string, options map[string]any) (Image, error)
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

func (p BFLImageProvider) GenerateImage(ctx context.Context, prompt, model string, options map[string]any) (Image, error) {
	if strings.TrimSpace(p.APIKey) == "" {
		return Image{}, errors.New("BFL API-Key fehlt")
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = "https://api.bfl.ai"
	}
	return generateBFLImage(ctx, client, p.APIKey, base, prompt, model, options)
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
