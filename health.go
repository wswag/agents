package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAIStatusURL is the official public status summary endpoint.
const OpenAIStatusURL = "https://status.openai.com/api/v2/summary.json"

// OpenAIHealth reports the published status of the Responses API.
type OpenAIHealth struct {
	Status      string
	Operational bool
}

// CheckOpenAIHealth reads the public status page. It does not authenticate or
// check connectivity to the API for a specific account or model.
func CheckOpenAIHealth(ctx context.Context, client *http.Client) (OpenAIHealth, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OpenAIStatusURL, nil)
	if err != nil {
		return OpenAIHealth{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return OpenAIHealth{}, fmt.Errorf("OpenAI status abrufen: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return OpenAIHealth{}, fmt.Errorf("OpenAI status: HTTP %d", resp.StatusCode)
	}
	var summary struct {
		Components []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"components"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&summary); err != nil {
		return OpenAIHealth{}, fmt.Errorf("OpenAI status lesen: %w", err)
	}
	for _, component := range summary.Components {
		if component.Name == "Responses" {
			if strings.TrimSpace(component.Status) == "" {
				return OpenAIHealth{}, fmt.Errorf("OpenAI Responses status ist leer")
			}
			return OpenAIHealth{Status: component.Status, Operational: component.Status == "operational"}, nil
		}
	}
	return OpenAIHealth{}, fmt.Errorf("OpenAI Responses status fehlt")
}
