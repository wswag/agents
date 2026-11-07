# agents

## CI und Releases

GitHub Actions führt `go test ./...` und `go build ./...` mit Go 1.22 auf Pull Requests und Pushes nach `main` aus. Nach einem erfolgreichen Push auf `main` erstellt go-semantic-release anhand der Commit-Nachrichten automatisch einen GitHub-Release. Dafür gelten Conventional Commits: `fix:` erhöht Patch, `feat:` erhöht Minor und `BREAKING CHANGE:` kündigt Major an.

`agents` enthält wiederverwendbare, versionierte Agentdefinitionen. Die Anwendung beschreibt Auftrag, Eingabevorlage, Ausgabeformat und Werkzeuge im Code und führt den Agenten mit einem Eingabeobjekt aus.

## Agent definieren

```go
package myagents

import "github.com/wswag/agents"

func BlogContent() agents.Agent {
	return agents.Agent{
		Key:          "blog-content",
		Name:         "Blog Content",
		Version:      "v1",
		Model:        "gpt-5.6-luna",
		Tools:        []agents.Tool{{Type: agents.ToolWebSearch}},
		SystemPrompt: "Du entwickelst redaktionelle Ideen und begründest Empfehlungen.",
	InputTemplate: `Thema: {{.Topic}}
Zielgruppe: {{.Audience}}
Ziel: {{.Goal}}`,
		OutputSchema: "Antworte auf Deutsch mit konkreten Empfehlungen.",
		Output:       agents.OutputText,
	}
}
```

## Ausführen

```go
runner := agents.NewRunner(agents.RunnerConfig{
	APIKey: os.Getenv("OPENAI_API_KEY"),
})

result, err := runner.Run(ctx, myagents.BlogContent(), map[string]any{
	"Topic":    "nachhaltige Verpackungen",
	"Audience": "B2B-Einkaufsteams",
	"Goal":     "Themenideen für einen Fachblog",
}, agents.RunOptions{
	DashboardCorrelation: "blog-content:customer-123",
	PreviousResponseID:  "resp_previous",
	ModelOverride:       "gpt-5.6-terra",
})
if err != nil {
	return err
}
fmt.Println(result.Text)
fmt.Println("response:", result.ResponseID)
fmt.Println("tokens:", result.Usage.TotalTokens, "cached:", result.Usage.CachedTokens)
```

Vorlagen greifen direkt auf das Eingabeobjekt zu, zum Beispiel mit `{{.Topic}}`. Fehlende Template-Werte werden als Fehler gemeldet.
`RunOptions` ist optional. `DashboardCorrelation` wird standardmäßig aus `Agent.Key` abgeleitet und kann in der Agentdefinition, im Eingabeobjekt oder in den Optionen überschrieben werden. Der Wert wird als `prompt_cache_key` und als Response-Metadatum gesendet; Key und Version des Agenten werden ebenfalls als Metadaten übermittelt. Zusätzliche string-basierte Metadaten lassen sich pro Lauf über `RunOptions.Metadata` übergeben und werden mit diesen Agent-Metadaten zusammengeführt. Die reservierten Metadaten-Keys `agent_key`, `agent_version` und `dashboard_correlation` werden dabei aus der Agentdefinition bzw. den Optionen gesetzt. `PreviousResponseID` setzt den Gesprächsbezug, `ModelOverride` überschreibt das Sprachmodell für diesen Lauf. `Result` gibt die Response-ID(s), `DashboardCorrelation` und Token-Nutzungsdaten (`Usage`) zurück.

## Run-Logging

Ein Agent kann das Laufprotokoll und optional den Kontext oder die Modellantwort speichern:

```go
agent := agents.Agent{
	Key:   "blog-content",
	Model: "gpt-5.6-luna",
	LoggingOptions: agents.LoggingOptions{
		RunLog:      true,
		ContextLog: true,
		ResponseLog: true,
		TTL:         30 * 24 * time.Hour,
	},
}
```

Der `RunLogger` wird einmal beim Runner konfiguriert. Ohne Logger werden die Logging-Optionen ignoriert und der Run läuft normal weiter. Das GORM-Backend legt die Tabelle `agent_run_logs` an. `ContextLog` speichert das serialisierte Eingabeobjekt samt gerenderter Eingabe, `ResponseLog` speichert Text und gegebenenfalls erzeugte Bilddaten. Jeder gespeicherte Lauf enthält Start/Ende, Agent-Key und Version, Modell, Metadaten, Response-IDs, Token-Statistiken und bei Fehlern die Fehlermeldung. Context- und Response-Daten werden nur bei expliziter Aktivierung gespeichert. Ein positives `TTL` setzt `ExpiresAt` relativ zum Laufstart; ohne TTL bleibt das Zieldatum leer. `GormRunLogger.DeleteExpired(ctx, time.Now())` entfernt abgelaufene Einträge.

```go
runLogger := agents.NewGormRunLogger(db)
if err := runLogger.AutoMigrate(ctx); err != nil {
	return err
}
runner := agents.NewRunner(agents.RunnerConfig{
	APIKey: os.Getenv("OPENAI_API_KEY"),
	Logger: runLogger,
})
```

Die Flags können auch in einem kombinierten Definitionsfile im Frontmatter gesetzt werden:

```yaml
logging_options:
  run_log: true
  context_log: false
  response_log: true
  ttl: 720h
```

## Registry

Agentdefinitionen lassen sich über ihren Key registrieren und später abrufen:

```go
registry := agents.NewRegistry()
registry.SetRunner(runner)
registry.SetRepository(repository)
if err := registry.Register(myagents.BlogContent()); err != nil {
	return err
}

result, err := registry.Run(ctx, "blog-content", input)
if err != nil {
	return err
}
```

Zusätzliche Metadaten können an einen einzelnen Lauf angehängt werden:

```go
result, err := registry.Run(ctx, "blog-content", input, agents.RunOptions{
	Metadata: map[string]string{
		"tenant_id": "customer-123",
		"workflow":  "weekly-summary",
	},
})
```

Doppelte statische Keys werden abgelehnt. `SetRepository` bindet zusätzlich dynamische Definitionen aus dem Repository ein. Statische Registry-Agenten haben bei einem Key-Konflikt immer Vorrang; Repository-Versionen desselben Keys werden auch bei einer Versionsabfrage nicht als Fallback verwendet. `Get` und `Run` lösen den Agenten anhand des Keys auf; für eine dynamische Revision kann `RunOptions.AgentVersion` gesetzt werden. `List` führt statische Agenten und dynamische Repository-Agenten zusammen, überspringt dabei kollidierende Repository-Keys und gibt pro dynamischem Key ohne Versionsfilter die neueste Revision zurück.

## GORM-Repository und Service

Das erste persistente Backend verwendet eine bereits konfigurierte `*gorm.DB`; der Aufrufer wählt den GORM-Dialekt und die Verbindungsparameter. `AutoMigrate` erstellt die Tabelle `agent_definition_versions` mit Key und Version als zusammengesetztem Primärschlüssel.

```go
repository := agents.NewGormAgentRepository(db)
if err := repository.AutoMigrate(ctx); err != nil {
	return err
}
service := agents.NewAgentService(repository)

agent, err := service.Seed(ctx, myagents.BlogContentAgent()) // initialisiert oder aktualisiert idempotent
if err != nil {
	return err
}

agent, err = service.Seed(ctx, myagents.BlogContentAgent(), "reapply-2026-11") // gleiche Definition bewusst erneut versionieren
if err != nil {
	return err
}

agent, err = service.Update(ctx, agent.Key, revisedAgent) // dynamisches Update hängt ebenfalls eine Version an
if err != nil {
	return err
}

latest, err := service.Get(ctx, agent.Key)             // neueste Revision
historical, err := service.Get(ctx, agent.Key, "v1") // konkrete Revision
_ = historical
if err := registry.Register(latest); err != nil {
	return err
}
```

Für eine gemeinsame Registry können `SetRunner` und `SetRepository` direkt nach der Erstellung aufgerufen werden. So lassen sich fest im Code definierte Agenten und gespeicherte Definitionen über dieselbe `Get`-, `List`- und `Run`-Schnittstelle nutzen; statische Definitionen behalten dabei den Vorrang.

Das Repository nimmt und liefert `Agent`-Objekte. Das GORM-Backend exportiert den Agenten intern mit `ExportPromptBundle` in das kombinierte Format und speichert dieses versioniert. `Seed` berechnet einen Idempotenzschlüssel aus dem Export-Hash und der optionalen Nonce: Derselbe Seed erzeugt keine weitere Revision, ein geänderter Export oder eine neue Nonce schon. `Create` setzt eine neue Definition auf `v1`; `Update` hängt bei einem bestehenden Key unabhängig davon die nächste dynamische Version an. Eine konkrete Version kann als `"v1"` oder `"1"` angegeben werden; `List(ctx, "v1")` filtert ebenfalls nach Version. `Delete` entfernt alle Revisionen eines Keys. Die Repository-Version im zurückgegebenen Agenten ersetzt die Versionsangabe aus dem Frontmatter. CRUD-Änderungen verändern bereits registrierte Agenten nicht automatisch.

Das Frontmatter wird bei `Runner.Run` ebenfalls als Go-Template mit dem direkten Eingabeobjekt gerendert. So kann etwa `model: "{{.LanguageModel}}"` das Sprachmodell anhand des Laufzeitkontexts auswählen:

```yaml
model: "{{.LanguageModel}}"
```

Template-Ausdrücke in YAML-Werten müssen gequotet sein. Bei Repository-Agenten bleibt das Bundle bis zum Laufzeitaufruf erhalten, damit der Kontext beim Rendern noch verfügbar ist.

## Bilder

Ein Agent mit `Output: agents.OutputImage` liefert ein `result.Images` Element mit MIME-Type und Bildbytes. `OutputBoth` liefert Text und Bild. Für Referenzbilder kann ein Wert vom Typ `agents.ImageInput` in den Parametern oder im Kontextobjekt enthalten sein; multimodale Aufgaben können Text und Referenzbilder gemeinsam verarbeiten.

```go
type PhotoContext struct {
	Purpose string
	Photo   agents.ImageInput
}

result, err := runner.Run(ctx, photoAgent, PhotoContext{
	Purpose: "Erstelle eine sachliche Bildbeschreibung",
	Photo:   agents.ImageInput{MIMEType: "image/jpeg", Data: photoBytes},
})
```

Bildagenten können `Image.Model` und `Image.Size` in ihrer Definition festlegen. Ohne Überschreibung nutzt das Modul `agents.DefaultImageModel` (`gpt-image-2.5-flare`).

Eine vollständige Definition für eine Produktillustration steht in `examples.ProductIllustrationAgent()`. Sie kann beispielsweise so ausgeführt werden:

```go
result, err := runner.Run(ctx, examples.ProductIllustrationAgent(), map[string]any{
	"Product":    "eine wiederverwendbare Wasserflasche aus Edelstahl",
	"Style":      "minimalistische Studioillustration",
	"Background": "hellgrau",
})
if err != nil {
	return err
}
image := result.Images[0]
```

## Prompts aus Dateien einbetten

`SystemPrompt`, `InputTemplate` und `OutputSchema` können inline, als getrennte Dateien oder gemeinsam aus einem Prompt-Bundle geladen werden. Ein YAML-Frontmatter zwischen `---`-Zeilen kann außerdem `key`, `name`, `version`, `model`, `dashboard_correlation`, `model_options`, `tools`, `output` und `image` setzen. Der Body trennt die drei Textfelder mit `'''system-prompt`, `'''input-template` und `'''output-schema`; jeder Abschnitt endet mit einer eigenen Zeile `'''`.

Das Frontmatter darf Go-Template-Ausdrücke und Kontrollblöcke enthalten; `ExportPromptBundle` erhält das Frontmatter-Template. Der `key` muss statisch bleiben und darf keinen Template-Code enthalten.

Eingebettete Fence-Blöcke wie `'''yaml ... '''` innerhalb eines Abschnitts bleiben dessen Inhalt; der folgende alleinstehende `'''`-Marker schließt dann erst den Abschnitt.

`agents.ExportPromptBundle(agent)` exportiert die zusammengeführte Definition und alle drei Prompt-Abschnitte als String in genau diesem Bundle-Format. Dabei werden eingebettete Prompts, Einzeldateien und explizite Felder nach denselben Prioritätsregeln wie beim Ausführen aufgelöst.

```text
---
key: product-illustration
name: Produktillustration
version: v1
model: gpt-5.6-luna
dashboard_correlation: product-illustration
output: image
image:
  model: gpt-image-2.5-flare
  size: 1024x1024
---
'''system-prompt
Erzeuge eine schlichte Produktillustration.
'''
'''input-template
Produkt: {{.Product}}
'''
'''output-schema
Gib ein einzelnes Bild zurück.
'''
```

```go
//go:embed prompts/product-illustration/*
var productPrompts embed.FS

agent := agents.Agent{
	PromptFiles: &agents.PromptFiles{
		FS:     productPrompts,
		Bundle: "prompts/product-illustration/agent.prompt",
		// Einzeldateien überschreiben nur den gleichnamigen Bundle-Abschnitt.
		OutputSchema: "prompts/product-illustration/output.md",
	},
}
```

Die Beispieldefinition `ProductIllustrationAgent` bezieht Key, Name, Version, Modell, Ausgabeart und Bildoptionen aus dem Frontmatter. Bei Repository-Materialisierung ersetzt die gespeicherte Repository-Version den Frontmatter-Wert für `version`; andere explizite Werte in Go überschreiben Frontmatter-Werte. Einzeldateien und Inline-Inhalte überschreiben jeweils den gleichnamigen Bundle-Abschnitt. Doppelte, unbekannte oder nicht geschlossene Abschnitte sowie unbekannte Frontmatter-Felder führen zu einem Fehler.
