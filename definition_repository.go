package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrDefinitionNotFound = errors.New("agent definition not found")
	ErrDefinitionExists   = errors.New("agent definition already exists")
)

// AgentDefinition is one stored revision of a combined frontmatter-and-sections document.
type AgentDefinition struct {
	Key     string
	Version string
	Content string
}

// AgentRepository stores versioned Agent objects.
type AgentRepository interface {
	Create(ctx context.Context, agent Agent) (Agent, error)
	Seed(ctx context.Context, agent Agent, nonce ...string) (Agent, error)
	Get(ctx context.Context, key string, version string) (Agent, error)
	Update(ctx context.Context, agent Agent) (Agent, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, version string) ([]Agent, error)
}

// AgentDefinitionRepository is a compatibility name for AgentRepository.
type AgentDefinitionRepository = AgentRepository

// GormAgentDefinitionRecord is the persisted representation of one definition revision.
type GormAgentDefinitionRecord struct {
	Key                string  `gorm:"primaryKey;size:128;uniqueIndex:uidx_agent_seed_idempotency"`
	Version            uint64  `gorm:"primaryKey;autoIncrement:false"`
	SeedIdempotencyKey *string `gorm:"size:64;uniqueIndex:uidx_agent_seed_idempotency"`
	Content            string  `gorm:"type:text;not null"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (GormAgentDefinitionRecord) TableName() string { return "agent_definition_versions" }

type legacyAgentDefinitionRecord struct {
	Key       string
	Content   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// GormAgentDefinitionRepository stores definitions using a caller-configured GORM database.
type GormAgentDefinitionRepository struct {
	db *gorm.DB
}

// GormAgentRepository is the preferred name for the Agent-object repository.
type GormAgentRepository = GormAgentDefinitionRepository

func NewGormAgentRepository(db *gorm.DB) *GormAgentRepository {
	return NewGormAgentDefinitionRepository(db)
}

func NewGormAgentDefinitionRepository(db *gorm.DB) *GormAgentDefinitionRepository {
	return &GormAgentDefinitionRepository{db: db}
}

// AutoMigrate creates or updates the versioned repository table schema.
func (r *GormAgentDefinitionRepository) AutoMigrate(ctx context.Context) error {
	if err := r.checkDB(); err != nil {
		return err
	}
	db := r.db.WithContext(ctx)
	if err := db.AutoMigrate(&GormAgentDefinitionRecord{}); err != nil {
		return err
	}
	if !db.Migrator().HasTable("agent_definitions") {
		return nil
	}
	var legacy []legacyAgentDefinitionRecord
	if err := db.Table("agent_definitions").Find(&legacy).Error; err != nil {
		return fmt.Errorf("read legacy agent definitions: %w", err)
	}
	for _, old := range legacy {
		record := GormAgentDefinitionRecord{
			Key: old.Key, Version: 1, Content: old.Content, CreatedAt: old.CreatedAt, UpdatedAt: old.UpdatedAt,
		}
		if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error; err != nil {
			return fmt.Errorf("migrate legacy agent definition %q: %w", old.Key, err)
		}
	}
	return nil
}

func (r *GormAgentDefinitionRepository) Create(ctx context.Context, agent Agent) (Agent, error) {
	if err := r.checkDB(); err != nil {
		return Agent{}, err
	}
	definition, err := agentDefinitionForStorage(agent)
	if err != nil {
		return Agent{}, err
	}
	key := strings.TrimSpace(definition.Key)
	var created GormAgentDefinitionRecord
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&GormAgentDefinitionRecord{}).Where("key = ?", key).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("agent definition %q: %w", key, ErrDefinitionExists)
		}
		created = GormAgentDefinitionRecord{Key: key, Version: 1, Content: definition.Content}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&created)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("agent definition %q: %w", key, ErrDefinitionExists)
		}
		return nil
	})
	if err != nil {
		return Agent{}, fmt.Errorf("create agent definition %q: %w", key, err)
	}
	return agentFromRecord(created)
}

// Seed initializes an agent once per exported definition and nonce. A new
// export or nonce appends a repository revision; repeating the same seed is a no-op.
func (r *GormAgentDefinitionRepository) Seed(ctx context.Context, agent Agent, nonces ...string) (Agent, error) {
	if err := r.checkDB(); err != nil {
		return Agent{}, err
	}
	if len(nonces) > 1 {
		return Agent{}, errors.New("at most one seed nonce may be given")
	}
	nonce := ""
	if len(nonces) == 1 {
		nonce = strings.TrimSpace(nonces[0])
	}
	definition, err := agentDefinitionForStorage(agent)
	if err != nil {
		return Agent{}, err
	}
	key := definition.Key
	idempotencyKey := seedIdempotencyKey(definition.Content, nonce)
	var seeded GormAgentDefinitionRecord
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing GormAgentDefinitionRecord
		if err := tx.Where("key = ? AND seed_idempotency_key = ?", key, idempotencyKey).First(&existing).Error; err == nil {
			seeded = existing
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var latest GormAgentDefinitionRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", key).Order("version DESC").First(&latest).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && nonce == "" && latest.Content == definition.Content {
			seeded = latest
			return nil
		}
		version := uint64(1)
		if err == nil {
			if latest.Version == ^uint64(0) {
				return fmt.Errorf("agent definition %q version overflow", key)
			}
			version = latest.Version + 1
		}
		seeded = GormAgentDefinitionRecord{
			Key: key, Version: version, SeedIdempotencyKey: &idempotencyKey, Content: definition.Content,
		}
		return tx.Create(&seeded).Error
	})
	if err != nil {
		// A concurrent retry may have inserted the same idempotency key first.
		var existing GormAgentDefinitionRecord
		if lookupErr := r.db.WithContext(ctx).Where("key = ? AND seed_idempotency_key = ?", key, idempotencyKey).First(&existing).Error; lookupErr == nil {
			return agentFromRecord(existing)
		}
		return Agent{}, fmt.Errorf("seed agent %q: %w", key, err)
	}
	return agentFromRecord(seeded)
}

func seedIdempotencyKey(export string, nonce string) string {
	exportHash := sha256.Sum256([]byte(export))
	keyHash := sha256.Sum256([]byte(hex.EncodeToString(exportHash[:]) + "\x00" + nonce))
	return hex.EncodeToString(keyHash[:])
}

// Get returns the requested revision, or the newest revision when version is empty.
func (r *GormAgentDefinitionRepository) Get(ctx context.Context, key string, version string) (Agent, error) {
	if err := r.checkDB(); err != nil {
		return Agent{}, err
	}
	query := r.db.WithContext(ctx).Where("key = ?", strings.TrimSpace(key))
	if version != "" {
		parsed, err := parseDefinitionVersion(version)
		if err != nil {
			return Agent{}, err
		}
		query = query.Where("version = ?", parsed)
	} else {
		query = query.Order("version DESC")
	}
	var record GormAgentDefinitionRecord
	if err := query.First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Agent{}, fmt.Errorf("agent definition %q version %q: %w", key, version, ErrDefinitionNotFound)
		}
		return Agent{}, fmt.Errorf("get agent definition %q: %w", key, err)
	}
	return agentFromRecord(record)
}

// Update appends a revision with the next version number.
func (r *GormAgentDefinitionRepository) Update(ctx context.Context, agent Agent) (Agent, error) {
	if err := r.checkDB(); err != nil {
		return Agent{}, err
	}
	definition, err := agentDefinitionForStorage(agent)
	if err != nil {
		return Agent{}, err
	}
	key := strings.TrimSpace(definition.Key)
	var created GormAgentDefinitionRecord
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var latest GormAgentDefinitionRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key = ?", key).Order("version DESC").First(&latest).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("agent definition %q: %w", key, ErrDefinitionNotFound)
			}
			return err
		}
		if latest.Version == ^uint64(0) {
			return fmt.Errorf("agent definition %q version overflow", key)
		}
		created = GormAgentDefinitionRecord{Key: key, Version: latest.Version + 1, Content: definition.Content}
		return tx.Create(&created).Error
	})
	if err != nil {
		return Agent{}, fmt.Errorf("append agent definition revision %q: %w", key, err)
	}
	return agentFromRecord(created)
}

// Delete removes all revisions for key.
func (r *GormAgentDefinitionRepository) Delete(ctx context.Context, key string) error {
	if err := r.checkDB(); err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	result := r.db.WithContext(ctx).Where("key = ?", key).Delete(&GormAgentDefinitionRecord{})
	if result.Error != nil {
		return fmt.Errorf("delete agent definition %q: %w", key, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("agent definition %q: %w", key, ErrDefinitionNotFound)
	}
	return nil
}

// List returns all revisions, optionally filtered to one version number.
func (r *GormAgentDefinitionRepository) List(ctx context.Context, version string) ([]Agent, error) {
	if err := r.checkDB(); err != nil {
		return nil, err
	}
	query := r.db.WithContext(ctx).Model(&GormAgentDefinitionRecord{}).Order("key ASC, version DESC")
	if version != "" {
		parsed, err := parseDefinitionVersion(version)
		if err != nil {
			return nil, err
		}
		query = query.Where("version = ?", parsed)
	}
	var records []GormAgentDefinitionRecord
	if err := query.Find(&records).Error; err != nil {
		return nil, fmt.Errorf("list agent definitions: %w", err)
	}
	agents := make([]Agent, 0, len(records))
	for _, record := range records {
		agent, err := agentFromRecord(record)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, nil
}

func (r *GormAgentDefinitionRepository) checkDB() error {
	if r == nil || r.db == nil {
		return errors.New("agent definition repository has no gorm database")
	}
	return nil
}

func agentDefinitionForStorage(agent Agent) (AgentDefinition, error) {
	content, err := ExportPromptBundle(agent)
	if err != nil {
		return AgentDefinition{}, err
	}
	sections, metadata, err := parsePromptBundle(content)
	if err != nil {
		return AgentDefinition{}, fmt.Errorf("validate exported agent bundle: %w", err)
	}
	for _, name := range []string{"system-prompt", "input-template", "output-schema"} {
		if _, exists := sections[name]; !exists {
			return AgentDefinition{}, fmt.Errorf("exported agent bundle is missing %q section", name)
		}
	}
	key := strings.TrimSpace(agent.Key)
	if key == "" {
		key = strings.TrimSpace(metadata.Key)
	}
	if key == "" {
		frontmatter, _, _, _ := splitPromptBundle(content)
		key = staticFrontmatterValue(frontmatter, "key")
	}
	if err := validateStaticAgentKey(key); err != nil {
		return AgentDefinition{}, err
	}
	if key == "" {
		return AgentDefinition{}, errors.New("agent key is required for repository storage")
	}
	if agent.Key != "" && strings.TrimSpace(agent.Key) != key {
		return AgentDefinition{}, fmt.Errorf("agent key %q does not match bundle key %q", agent.Key, key)
	}
	model := strings.TrimSpace(agent.Model)
	if model == "" {
		model = strings.TrimSpace(metadata.Model)
	}
	if model == "" {
		frontmatter, _, _, _ := splitPromptBundle(content)
		model = staticFrontmatterValue(frontmatter, "model")
	}
	frontmatter, _, _, _ := splitPromptBundle(content)
	if model == "" && !strings.Contains(frontmatter, "{{") {
		return AgentDefinition{}, errors.New("agent model is required for repository storage")
	}
	return AgentDefinition{Key: key, Content: content}, nil
}

func agentFromRecord(record GormAgentDefinitionRecord) (Agent, error) {
	_, agent, err := parseAgentDefinition(record.Content)
	if err != nil {
		_, metadata, parseErr := parsePromptBundle(record.Content)
		if parseErr != nil {
			return Agent{}, fmt.Errorf("parse stored agent %q: %w", record.Key, parseErr)
		}
		agent = metadata
	}
	if agent.Key != "" && agent.Key != record.Key {
		return Agent{}, fmt.Errorf("stored agent key %q does not match bundle key %q", record.Key, agent.Key)
	}
	agent.Key = record.Key
	agent.Version = formatDefinitionVersion(record.Version)
	agent.PromptFiles = &PromptFiles{BundleContent: record.Content}
	return agent, nil
}

func formatDefinitionVersion(version uint64) string { return fmt.Sprintf("v%d", version) }

func parseDefinitionVersion(version string) (uint64, error) {
	version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(version), "v"))
	parsed, err := strconv.ParseUint(version, 10, 64)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("invalid agent definition version %q", version)
	}
	return parsed, nil
}
