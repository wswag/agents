package agents

import (
	"embed"

	ai "github.com/wswag/agents"
)

//go:embed prompts/product-illustration/*
var productIllustrationPrompts embed.FS

// BlogContentAgent definiert den festen Use-Case für Blog-Content.
func BlogContentAgent() ai.Agent {
	return ai.Agent{
		// Key: stabiler technischer Identifier; gültig sind kleingeschriebene ASCII-Slugs.
		Key: "blog-content",
		// Name: sichtbare, menschenlesbare Use-Case-Bezeichnung.
		Name: "Blog Content",
		// Version: bei jeder fachlichen Prompt-Änderung erhöhen; gültig sind Werte wie "v1" oder "v2".
		Version: "v3",
		// Model: fest gepflegtes Responses-Modell; gültige projektseitige Werte sind z. B. "gpt-5.6-luna".
		Model: "gpt-5.6-luna",
		// Tools: ausschließlich Websuche mit ai.ToolWebSearch; weitere oder nutzerdefinierte Tools sind unzulässig.
		Tools: []ai.Tool{{Type: ai.ToolWebSearch}},
		// SystemPrompt: fester Auftrag für Content-Themen, Perspektiven und belastbare Aussagen.
		SystemPrompt: `
Dein Use-Case ist Blog Content. Entwickle Themen, Blickwinkel, Gliederungen und
redaktionelle Prioritäten Nutze Websuche nur zur Überprüfung oder Ergänzung öffentlich belegbarer Fakten.`,
		// OutputSchema: freier deutscher Beratungstext mit nachvollziehbaren Empfehlungen.
		OutputSchema: "Antworte auf Deutsch. Leite Empfehlungen sichtbar aus Evidenz, Webrecherche und Unsicherheit her.",
		// InputTemplate greift direkt auf Felder des Eingabeobjekts zu.
		InputTemplate: `Entwickle Ideen für das Thema {{.Topic}}.
Zielgruppe: {{.Audience}}
Ziel: {{.Goal}}`,
		Output: ai.OutputText,
	}
}

// ProductIllustrationAgent definiert einen Agenten für Produktillustrationen.
func ProductIllustrationAgent() ai.Agent {
	return ai.Agent{
		PromptFiles: &ai.PromptFiles{
			FS:           productIllustrationPrompts,
			Bundle:       "prompts/product-illustration/agent.prompt.md",
			OutputSchema: "prompts/product-illustration/output.md",
		},
	}
}
