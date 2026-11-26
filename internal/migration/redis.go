package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/corematch/scribe/internal/assets"
	"github.com/corematch/scribe/internal/protocol"
	"github.com/redis/go-redis/v9"
)

var (
	ErrNotFound          = errors.New("staged migration not found")
	ErrConflict          = errors.New("staged migration conflict")
	ErrVersionConflict   = errors.New("staged migration version conflict")
	ErrInvalidTransition = errors.New("invalid staged migration transition")
	ErrCorruptStaging    = errors.New("corrupt staged migration")
	ErrPackageTooLarge   = errors.New("staged migration exceeds size limit")
)

type State string

const (
	StateStagedInRedis State = "STAGED_IN_REDIS"
	StateApplying      State = "APPLYING"
	StateCommitted     State = "COMMITTED"
	StateLocked        State = "LOCKED"
	StateReconciling   State = "RECONCILING"
	StateRollingBack   State = "ROLLING_BACK"
	StateRolledBack    State = "ROLLED_BACK"
	StateDiscarded     State = "DISCARDED"
	StateUnknown       State = "UNKNOWN"
)

type StagedAsset struct {
	assets.Asset
	Metadata map[string]string
}

type StagedMigration struct {
	MigrationID      string
	CommitDigest     string
	IdentifierHash   string
	Profile          protocol.Profile
	Skin             *StagedAsset
	Cape             *StagedAsset
	RollbackIdentity *RollbackIdentity
	State            State
	Version          int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type RollbackIdentity struct {
	CoreUserID        string `json:"core_user_id"`
	CoreUserOwnership string `json:"core_user_ownership"`
	AccountID         string `json:"account_id"`
}

type RedisOptions struct {
	TTL             time.Duration
	MaxPackageBytes int64
}

type RedisStore struct {
	client          redis.UniversalClient
	ttl             time.Duration
	maxPackageBytes int64
	now             func() time.Time
}

type stagedPayload struct {
	Version        int                 `json:"version"`
	MigrationID    string              `json:"migration_id"`
	CommitDigest   string              `json:"commit_digest"`
	IdentifierHash string              `json:"identifier_hash"`
	ProfileID      string              `json:"profile_id"`
	ProfileName    string              `json:"profile_name"`
	Skin           *stagedAssetPayload `json:"skin,omitempty"`
	Cape           *stagedAssetPayload `json:"cape,omitempty"`
}

type stagedAssetPayload struct {
	PixelSHA256 string            `json:"pixel_sha256"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

const stageScript = `
local ttl = ARGV[7]
local indexed = redis.call('GET', KEYS[2])
if indexed then
  local existing_key = 'scribe:migration:' .. indexed
  local payload = redis.call('HGET', existing_key, 'payload')
  if payload then
    local state = redis.call('HGET', existing_key, 'state')
    if state == 'STAGED_IN_REDIS' or state == 'LOCKED' then
      redis.call('PEXPIRE', existing_key, ttl)
      redis.call('PEXPIRE', KEYS[2], ttl)
    else
      redis.call('PERSIST', existing_key)
      redis.call('PERSIST', KEYS[2])
    end
  end
  if indexed == ARGV[8] then
    if payload then
      return payload
    end
    return 'MISSING:' .. indexed
  end
  return 'INDEX:' .. indexed
end
local existing = redis.call('HGET', KEYS[1], 'payload')
if existing then
  local state = redis.call('HGET', KEYS[1], 'state')
  if state == 'STAGED_IN_REDIS' or state == 'LOCKED' then
    redis.call('PEXPIRE', KEYS[1], ttl)
  else
    redis.call('PERSIST', KEYS[1])
  end
  return existing
end
redis.call('SET', KEYS[2], ARGV[8], 'PX', ttl)
redis.call('HSET', KEYS[1],
  'payload', ARGV[1],
  'state', ARGV[2],
  'version', ARGV[3],
  'created_at', ARGV[4],
  'updated_at', ARGV[4],
  'skin', ARGV[5],
  'cape', ARGV[6])
redis.call('PEXPIRE', KEYS[1], ttl)
return 0
`

const recordRollbackIdentityScript = `
local version = redis.call('HGET', KEYS[1], 'version')
if not version then
  return -1
end
if tonumber(version) ~= tonumber(ARGV[1]) then
  return 0
end
local payload = redis.call('HGET', KEYS[1], 'payload')
if not payload then
  return -3
end
local state = redis.call('HGET', KEYS[1], 'state')
if state ~= 'RECONCILING' and state ~= 'ROLLING_BACK' then
  return -3
end
local payload_valid, decoded = pcall(cjson.decode, payload)
if not payload_valid or type(decoded) ~= 'table' or decoded.migration_id ~= ARGV[4] then
  return -3
end
local current = redis.call('HGET', KEYS[1], 'rollback_identity')
if current and current ~= ARGV[2] then
  return -3
end
redis.call('HSET', KEYS[1], 'rollback_identity', ARGV[2], 'version', tonumber(ARGV[1]) + 1, 'updated_at', ARGV[3])
redis.call('PERSIST', KEYS[1])
return 1
`

const transitionScript = `
local transitions = {
  STAGED_IN_REDIS = {APPLYING=true, RECONCILING=true},
  APPLYING = {COMMITTED=true, RECONCILING=true},
  COMMITTED = {LOCKED=true},
  LOCKED = {},
  RECONCILING = {APPLYING=true, COMMITTED=true, ROLLING_BACK=true, UNKNOWN=true},
  ROLLING_BACK = {COMMITTED=true, ROLLED_BACK=true, RECONCILING=true, UNKNOWN=true},
  ROLLED_BACK = {DISCARDED=true},
  DISCARDED = {},
  UNKNOWN = {RECONCILING=true}
}
local version = redis.call('HGET', KEYS[1], 'version')
if not version then
  return -1
end
if tonumber(version) ~= tonumber(ARGV[1]) then
  return 0
end
local current = redis.call('HGET', KEYS[1], 'state')
local next_state = ARGV[2]
if not transitions[current] or not transitions[current][next_state] then
  return -2
end
local payload = redis.call('HGET', KEYS[1], 'payload')
if not payload then
  return -3
end
local payload_valid, decoded = pcall(cjson.decode, payload)
if not payload_valid or type(decoded) ~= 'table' or type(decoded.identifier_hash) ~= 'string' or
  decoded.identifier_hash == '' or decoded.migration_id ~= ARGV[5] then
  return -3
end
local index_key = 'scribe:migration:identifier:' .. decoded.identifier_hash
local indexed = redis.call('GET', index_key)
if indexed and indexed ~= ARGV[5] then
  return -3
end
if next_state == 'LOCKED' then
  redis.call('PEXPIRE', KEYS[1], ARGV[4])
  redis.call('SET', index_key, ARGV[5], 'PX', ARGV[4])
else
  redis.call('PERSIST', KEYS[1])
  if indexed then
    redis.call('PERSIST', index_key)
  else
    redis.call('SET', index_key, ARGV[5])
  end
end
redis.call('HSET', KEYS[1], 'state', next_state, 'version', tonumber(ARGV[1]) + 1, 'updated_at', ARGV[3])
return 1
`

func NewRedisStore(client redis.UniversalClient, options RedisOptions) (*RedisStore, error) {
	if client == nil {
		return nil, errors.New("Redis client is required")
	}
	if options.TTL < 0 || options.MaxPackageBytes < 0 {
		return nil, errors.New("Redis staging limits must not be negative")
	}
	if options.TTL > 0 && options.TTL < time.Millisecond {
		return nil, errors.New("Redis staging TTL must be at least one millisecond")
	}
	if options.TTL == 0 {
		options.TTL = 24 * time.Hour
	}
	if options.MaxPackageBytes == 0 {
		options.MaxPackageBytes = 16 << 20
	}
	return &RedisStore{client: client, ttl: options.TTL, maxPackageBytes: options.MaxPackageBytes, now: time.Now}, nil
}

func (s *RedisStore) Pending(ctx context.Context, identifier string) (bool, error) {
	normalized, err := protocol.NormalizeIdentifier(identifier)
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256([]byte(normalized))
	identifierHash := hex.EncodeToString(digest[:])
	migrationID, err := s.client.Get(ctx, identifierIndexKey(identifierHash)).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !validMigrationID(migrationID) {
		return false, ErrCorruptStaging
	}
	record, err := s.Load(ctx, migrationID)
	if err != nil {
		return false, err
	}
	if record.IdentifierHash != identifierHash {
		return false, ErrCorruptStaging
	}
	if record.State == StateRolledBack || record.State == StateDiscarded {
		return false, nil
	}
	return true, nil
}

func (s *RedisStore) RecordRollbackIdentity(ctx context.Context, migrationID string, expectedVersion int64, identity RollbackIdentity) (StagedMigration, error) {
	if !validMigrationID(migrationID) || expectedVersion <= 0 {
		return StagedMigration{}, errors.New("invalid rollback identity update")
	}
	if err := validateRollbackIdentity(identity); err != nil {
		return StagedMigration{}, err
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return StagedMigration{}, ErrCorruptStaging
	}
	result, err := s.client.Eval(ctx, recordRollbackIdentityScript, []string{stagingKey(migrationID)}, expectedVersion, string(encoded), s.now().UTC().Format(time.RFC3339Nano), migrationID).Int64()
	if err != nil {
		return StagedMigration{}, err
	}
	switch result {
	case -1:
		return StagedMigration{}, ErrNotFound
	case 0:
		return StagedMigration{}, ErrVersionConflict
	case -3:
		return StagedMigration{}, ErrCorruptStaging
	case 1:
		return s.Load(ctx, migrationID)
	default:
		return StagedMigration{}, ErrCorruptStaging
	}
}

func (s *RedisStore) Stage(ctx context.Context, migrationID, identifierHash string, snapshot Snapshot) (StagedMigration, bool, error) {
	if !validMigrationID(migrationID) {
		return StagedMigration{}, false, errors.New("invalid migration ID")
	}
	commitDigest, err := CommitDigest(snapshot, identifierHash)
	if err != nil {
		return StagedMigration{}, false, err
	}
	payload := stagedPayload{
		Version:        1,
		MigrationID:    migrationID,
		CommitDigest:   commitDigest,
		IdentifierHash: strings.ToLower(identifierHash),
		ProfileID:      strings.ToLower(snapshot.Profile.ID),
		ProfileName:    snapshot.Profile.Name,
	}
	skin, skinBytes, err := stageAsset(snapshot, "SKIN")
	if err != nil {
		return StagedMigration{}, false, err
	}
	cape, capeBytes, err := stageAsset(snapshot, "CAPE")
	if err != nil {
		return StagedMigration{}, false, err
	}
	payload.Skin = skin
	payload.Cape = cape
	encoded, err := json.Marshal(payload)
	if err != nil {
		return StagedMigration{}, false, fmt.Errorf("encode staged migration: %w", err)
	}
	if int64(len(encoded)+len(skinBytes)+len(capeBytes)) > s.maxPackageBytes {
		return StagedMigration{}, false, ErrPackageTooLarge
	}
	now := s.now().UTC()
	result, err := s.client.Eval(ctx, stageScript, []string{stagingKey(migrationID), identifierIndexKey(payload.IdentifierHash)}, encoded, string(StateStagedInRedis), 1, now.Format(time.RFC3339Nano), string(skinBytes), string(capeBytes), s.ttl.Milliseconds(), migrationID).Result()
	if err != nil {
		return StagedMigration{}, false, err
	}
	if existing, ok := result.(string); ok {
		if strings.HasPrefix(existing, "MISSING:") {
			return StagedMigration{}, false, ErrCorruptStaging
		}
		if strings.HasPrefix(existing, "INDEX:") {
			existingID := strings.TrimPrefix(existing, "INDEX:")
			if !validMigrationID(existingID) {
				return StagedMigration{}, false, ErrCorruptStaging
			}
			record, err := s.Load(ctx, existingID)
			if err != nil {
				return StagedMigration{}, false, err
			}
			if !strings.EqualFold(record.CommitDigest, commitDigest) {
				return StagedMigration{}, false, ErrConflict
			}
			return record, false, nil
		}
		var previous stagedPayload
		if err := json.Unmarshal([]byte(existing), &previous); err != nil {
			return StagedMigration{}, false, ErrCorruptStaging
		}
		if previous.MigrationID != migrationID {
			return StagedMigration{}, false, ErrCorruptStaging
		}
		if previous.CommitDigest != commitDigest {
			return StagedMigration{}, false, ErrConflict
		}
		record, err := s.Load(ctx, migrationID)
		return record, false, err
	}
	if createdResult, ok := result.(int64); ok && createdResult == 0 {
		return StagedMigration{
			MigrationID:    migrationID,
			CommitDigest:   commitDigest,
			IdentifierHash: strings.ToLower(identifierHash),
			Profile:        protocol.Profile{ID: payload.ProfileID, Name: payload.ProfileName},
			Skin:           stagedAsset(skin, skinBytes),
			Cape:           stagedAsset(cape, capeBytes),
			State:          StateStagedInRedis,
			Version:        1,
			CreatedAt:      now,
			UpdatedAt:      now,
		}, true, nil
	}
	return StagedMigration{}, false, ErrCorruptStaging
}

const discardScript = `
local state = redis.call('HGET', KEYS[1], 'state')
local version = redis.call('HGET', KEYS[1], 'version')
if not state or not version then
  return -1
end
if state ~= 'DISCARDED' then
  return -2
end
if tonumber(version) ~= tonumber(ARGV[1]) then
  return 0
end
local indexed = redis.call('GET', KEYS[2])
if indexed and indexed ~= ARGV[2] then
  return -3
end
redis.call('DEL', KEYS[1])
if indexed == ARGV[2] then
  redis.call('DEL', KEYS[2])
end
return 1
`

const repairIdentifierIndexScript = `
local payload = redis.call('HGET', KEYS[1], 'payload')
if not payload then
  return -1
end
local payload_valid, decoded = pcall(cjson.decode, payload)
if not payload_valid or type(decoded) ~= 'table' or type(decoded.identifier_hash) ~= 'string' or
  decoded.identifier_hash ~= ARGV[2] or decoded.migration_id ~= ARGV[1] then
  return -3
end
local indexed = redis.call('GET', KEYS[2])
if indexed then
  if indexed ~= ARGV[1] then
    return -3
  end
  return 0
end
local ttl = redis.call('PTTL', KEYS[1])
if ttl > 0 then
  redis.call('SET', KEYS[2], ARGV[1], 'PX', ttl)
else
  redis.call('SET', KEYS[2], ARGV[1])
end
return 1
`

func (s *RedisStore) List(ctx context.Context) ([]StagedMigration, error) {
	var cursor uint64
	seen := make(map[string]struct{})
	var records []StagedMigration
	for {
		keys, next, err := s.client.Scan(ctx, cursor, "scribe:migration:*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			if strings.HasPrefix(key, "scribe:migration:identifier:") {
				continue
			}
			migrationID := strings.TrimPrefix(key, "scribe:migration:")
			if !validMigrationID(migrationID) {
				return nil, ErrCorruptStaging
			}
			if _, ok := seen[migrationID]; ok {
				continue
			}
			seen[migrationID] = struct{}{}
			record, err := s.Load(ctx, migrationID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			result, err := s.client.Eval(ctx, repairIdentifierIndexScript, []string{key, identifierIndexKey(record.IdentifierHash)}, record.MigrationID, record.IdentifierHash).Int64()
			if err != nil {
				return nil, err
			}
			switch result {
			case -1:
				continue
			case -3:
				return nil, ErrCorruptStaging
			case 0, 1:
			default:
				return nil, ErrCorruptStaging
			}
			records = append(records, record)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].MigrationID < records[j].MigrationID })
	return records, nil
}

func (s *RedisStore) Load(ctx context.Context, migrationID string) (StagedMigration, error) {
	if !validMigrationID(migrationID) {
		return StagedMigration{}, errors.New("invalid migration ID")
	}
	values, err := s.client.HGetAll(ctx, stagingKey(migrationID)).Result()
	if err != nil {
		return StagedMigration{}, err
	}
	if len(values) == 0 {
		return StagedMigration{}, ErrNotFound
	}
	var payload stagedPayload
	if err := json.Unmarshal([]byte(values["payload"]), &payload); err != nil {
		return StagedMigration{}, ErrCorruptStaging
	}
	if payload.Version != 1 || payload.MigrationID != migrationID {
		return StagedMigration{}, ErrCorruptStaging
	}
	version, err := strconv.ParseInt(values["version"], 10, 64)
	if err != nil || version <= 0 {
		return StagedMigration{}, ErrCorruptStaging
	}
	state := State(values["state"])
	if !validState(state) {
		return StagedMigration{}, ErrCorruptStaging
	}
	createdAt, err := time.Parse(time.RFC3339Nano, values["created_at"])
	if err != nil {
		return StagedMigration{}, ErrCorruptStaging
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, values["updated_at"])
	if err != nil {
		return StagedMigration{}, ErrCorruptStaging
	}
	record := StagedMigration{
		MigrationID:    migrationID,
		CommitDigest:   payload.CommitDigest,
		IdentifierHash: payload.IdentifierHash,
		Profile:        protocol.Profile{ID: payload.ProfileID, Name: payload.ProfileName},
		State:          state,
		Version:        version,
		CreatedAt:      createdAt,
		UpdatedAt:      updatedAt,
	}
	record.Skin, err = loadStagedAsset(payload.Skin, values["skin"])
	if err != nil {
		return StagedMigration{}, err
	}
	record.Cape, err = loadStagedAsset(payload.Cape, values["cape"])
	if err != nil {
		return StagedMigration{}, err
	}
	if rawIdentity := values["rollback_identity"]; rawIdentity != "" {
		var identity RollbackIdentity
		if err := json.Unmarshal([]byte(rawIdentity), &identity); err != nil {
			return StagedMigration{}, ErrCorruptStaging
		}
		if err := validateRollbackIdentity(identity); err != nil {
			return StagedMigration{}, err
		}
		record.RollbackIdentity = &identity
	}
	snapshot := record.snapshot()
	digest, err := CommitDigest(snapshot, payload.IdentifierHash)
	if err != nil || digest != payload.CommitDigest {
		return StagedMigration{}, ErrCorruptStaging
	}
	return record, nil
}

func (s *RedisStore) Transition(ctx context.Context, migrationID string, expectedVersion int64, next State) (StagedMigration, error) {
	if !validMigrationID(migrationID) || expectedVersion <= 0 || !validState(next) {
		return StagedMigration{}, errors.New("invalid staged migration transition")
	}
	if next == StateRollingBack {
		return StagedMigration{}, ErrInvalidTransition
	}
	return s.transition(ctx, migrationID, expectedVersion, next)
}

func (s *RedisStore) ConfirmRollback(ctx context.Context, migrationID string, expectedVersion int64, status MigrationStatus) (StagedMigration, error) {
	if !validMigrationID(migrationID) || expectedVersion <= 0 || !strings.EqualFold(status.MigrationID, migrationID) ||
		status.Status != MigrationRolledBack || status.Result != nil {
		return StagedMigration{}, ErrInvalidResponse
	}
	if err := validateResourceInventory(status); err != nil {
		return StagedMigration{}, err
	}
	return s.transition(ctx, migrationID, expectedVersion, StateRollingBack)
}

func (s *RedisStore) transition(ctx context.Context, migrationID string, expectedVersion int64, next State) (StagedMigration, error) {
	result, err := s.client.Eval(ctx, transitionScript, []string{stagingKey(migrationID)}, expectedVersion, string(next), s.now().UTC().Format(time.RFC3339Nano), s.ttl.Milliseconds(), migrationID).Int64()
	if err != nil {
		return StagedMigration{}, err
	}
	switch result {
	case -1:
		return StagedMigration{}, ErrNotFound
	case 0:
		return StagedMigration{}, ErrVersionConflict
	case -2:
		return StagedMigration{}, ErrInvalidTransition
	case 1:
		return s.Load(ctx, migrationID)
	default:
		return StagedMigration{}, ErrCorruptStaging
	}
}

func (s *RedisStore) Discard(ctx context.Context, migrationID string, expectedVersion int64) error {
	if !validMigrationID(migrationID) || expectedVersion <= 0 {
		return errors.New("invalid staged migration discard")
	}
	record, err := s.Load(ctx, migrationID)
	if err != nil {
		return err
	}
	if record.State != StateDiscarded {
		return ErrInvalidTransition
	}
	result, err := s.client.Eval(ctx, discardScript, []string{stagingKey(migrationID), identifierIndexKey(record.IdentifierHash)}, expectedVersion, migrationID).Int64()
	if err != nil {
		return err
	}
	switch result {
	case -1:
		return ErrNotFound
	case 0:
		return ErrVersionConflict
	case -2:
		return ErrInvalidTransition
	case -3:
		return ErrCorruptStaging
	case 1:
		return nil
	default:
		return ErrCorruptStaging
	}
}

func (r StagedMigration) snapshot() Snapshot {
	snapshot := Snapshot{
		Profile: protocol.SignedProfile{ID: r.Profile.ID, Name: r.Profile.Name},
		Textures: protocol.Textures{
			ProfileID:   r.Profile.ID,
			ProfileName: r.Profile.Name,
			Textures:    map[string]protocol.Texture{},
		},
		Assets: map[string]assets.Asset{},
	}
	for name, asset := range map[string]*StagedAsset{"SKIN": r.Skin, "CAPE": r.Cape} {
		if asset == nil {
			continue
		}
		snapshot.Textures.Textures[name] = protocol.Texture{Metadata: asset.Metadata}
		snapshot.Assets[name] = asset.Asset
	}
	return snapshot
}

func stageAsset(snapshot Snapshot, name string) (*stagedAssetPayload, []byte, error) {
	texture, present := snapshot.Textures.Textures[name]
	asset, hasAsset := snapshot.Assets[name]
	if present != hasAsset {
		return nil, nil, ErrCorruptStaging
	}
	if !present {
		return nil, nil, nil
	}
	metadata, err := migrationTextureMetadata(texture.Metadata)
	if err != nil {
		return nil, nil, err
	}
	return &stagedAssetPayload{
		PixelSHA256: asset.PixelSHA256,
		Width:       asset.Width,
		Height:      asset.Height,
		Metadata:    metadata,
	}, append([]byte(nil), asset.PNG...), nil
}

func stagedAsset(payload *stagedAssetPayload, data []byte) *StagedAsset {
	if payload == nil {
		return nil
	}
	return &StagedAsset{
		Asset:    assets.Asset{PNG: append([]byte(nil), data...), PixelSHA256: payload.PixelSHA256, Width: payload.Width, Height: payload.Height},
		Metadata: cloneMetadata(payload.Metadata),
	}
}

func loadStagedAsset(payload *stagedAssetPayload, data string) (*StagedAsset, error) {
	if payload == nil {
		if data != "" {
			return nil, ErrCorruptStaging
		}
		return nil, nil
	}
	if data == "" {
		return nil, ErrCorruptStaging
	}
	return &StagedAsset{
		Asset:    assets.Asset{PNG: []byte(data), PixelSHA256: payload.PixelSHA256, Width: payload.Width, Height: payload.Height},
		Metadata: cloneMetadata(payload.Metadata),
	}, nil
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	cloned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func stagingKey(migrationID string) string {
	return "scribe:migration:" + migrationID
}

func identifierIndexKey(identifierHash string) string {
	return "scribe:migration:identifier:" + identifierHash
}

func validMigrationID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	for index, char := range id {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') && !(char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}

func validateRollbackIdentity(identity RollbackIdentity) error {
	if identity.CoreUserID == "" {
		if identity.CoreUserOwnership != "" {
			return ErrCorruptStaging
		}
	} else if identity.CoreUserOwnership != "preexisting" && identity.CoreUserOwnership != "created_by_migration" {
		return ErrCorruptStaging
	}
	if identity.AccountID != "" {
		accountID, err := strconv.ParseUint(identity.AccountID, 10, 64)
		if err != nil || accountID == 0 {
			return ErrCorruptStaging
		}
	}
	return nil
}

func validState(state State) bool {
	switch state {
	case StateStagedInRedis, StateApplying, StateCommitted, StateLocked, StateReconciling, StateRollingBack, StateRolledBack, StateDiscarded, StateUnknown:
		return true
	default:
		return false
	}
}
