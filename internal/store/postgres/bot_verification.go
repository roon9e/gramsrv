package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"telesrv/internal/domain"
	"telesrv/internal/store"
	"telesrv/internal/store/postgres/sqlcgen"
)

// BotVerificationStore is the PostgreSQL implementation of third-party bot
// verification (migrations 0155 + 0208): the icon catalogue, the verifier
// organizations, the granted marks and the application queue in front of them.
// 0208 replaced the one-row-per-bot bot_verifier_settings with
// verifier_organizations: a bot is a verifier through the organizations it
// hosts, and its settings block plus every projection is resolved from its
// PRIMARY organization (lowest display_priority).
//
// Four properties are load bearing and every method below exists to keep them:
//
//   - The projection is deterministic and cheap. PeerVerification and
//     PeerVerificationBatch run on every peer serialisation, so the batch form is
//     a single query for all peers (no N+1). The wire model carries one
//     BotVerification per peer, so a tie between several organizations' marks on
//     one peer resolves to a single winner: lowest display_priority, then most
//     recently granted, then newest row. Losers stay on disk and surface when the
//     winner is revoked.
//   - A disabled verifier projects nothing. Both projection reads join
//     verifier_organizations and keep only enabled organizations. The operator
//     kill switch (SetBotVerifierEnabled / SetVerifierOrganizationEnabled) flips
//     enabled off: every badge those organizations granted darkens while the rows
//     stay on disk, so flipping it back restores them unchanged.
//   - "approved implies the mark exists". DecideCustomVerificationRequest changes
//     the application status and grants (or revokes) the mark in ONE transaction:
//     the caller-supplied apply callback runs inside it and must write through
//     VerificationTxFromContext, so a callback error rolls the decision back and
//     an approved application without its mark cannot exist.
//   - Exactly one decision per application and one settings writer at a time.
//     Every mutation is guarded by WHERE ... AND version = $n on top of a
//     SELECT ... FOR UPDATE, so the loser of a race gets
//     domain.ErrCustomVerificationVersionConflict instead of silently clobbering.
//
// Status transitions are never re-implemented in SQL: they all go through
// domain.CanTransitionCustomVerificationStatus. Re-issuing a decision that
// already holds is reported as changed=false without touching the row.
//
// Two deliberate non-responsibilities: the store does not check
// icon_document_id against the catalogue (0155 has no such foreign key, and the
// admin edge picks the icon from the catalogue before it gets here), and it does
// not refuse a grant by a disabled verifier. "May this bot verify?"
// (domain.ErrVerifierForbidden / BOT_VERIFIER_FORBIDDEN) is an RPC-edge decision
// made from the settings this store returns; the store only enforces what the
// schema encodes.
type BotVerificationStore struct {
	db sqlcgen.DBTX
}

// NewBotVerificationStore builds the store on a pgx pool or transaction.
func NewBotVerificationStore(db sqlcgen.DBTX) *BotVerificationStore {
	return &BotVerificationStore{db: db}
}

var _ store.BotVerificationStore = (*BotVerificationStore)(nil)

const (
	defaultBotVerificationListLimit = 50
	maxBotVerificationListLimit     = 200
	// The bounds below mirror the octet_length CHECKs of 0155. They are not
	// redundant with the domain Validate methods: the domain counts runes and the
	// columns count bytes, so multi-byte text can clear validation and still
	// violate a CHECK. Guarding here keeps that a domain error instead of an
	// opaque constraint violation from the driver, and keeps the two backends
	// answering identically.
	maxVerificationIconNameBytes = 512
	maxVerifierCompanyBytes      = 512
	// Rune-counting domain limits use their worst-case UTF-8 byte size in SQL.
	// The final generated description may be longer than the custom-input limit.
	maxVerifierDescriptionBytes           = 4 * domain.MaxCustomVerificationDescriptionLength
	maxVerifierGrantReasonBytes           = 4096
	maxCustomVerificationDescriptionBytes = 4096
	maxCustomVerificationInputBytes       = 4 * domain.MaxCustomVerificationDescriptionLength
	maxCustomVerificationTitleBytes       = 1024
	maxCustomVerificationUsernameBytes    = 64
	maxCustomVerificationReasonBytes      = 16384
	maxCustomVerificationDecidedByBytes   = 128
	maxCustomVerificationDecisionBytes    = 4096
	maxCustomVerificationNoteBytes        = 32768
	maxCustomVerificationCorrelationBytes = 128
)

// Constraint names the store maps onto domain errors. They are the schema's
// invariants, so a race that slips past a pre-check still reports the error the
// pre-check would have.
const (
	verificationIconDocumentConstraint    = "verification_icons_document_id_key"
	customVerificationOnceConstraint      = "custom_verifications_org_peer_once"
	customVerificationRequestPendingIndex = "custom_verification_requests_pending_idx"
)

// Column projections shared by every reader of a table, in scan order.
const (
	verificationIconColumnList = `id, document_id, owner_bot_id, name, active,
       created_at, updated_at`

	verifierOrganizationColumnList = `id, verifier_bot_id, company_name,
       icon_document_id, default_description, can_modify_custom_description,
       enabled, display_priority, granted_by, grant_reason, created_at,
       updated_at, version`

	// botVerifierSettingsColumnList is a backwards-compatible name: the legacy
	// settings block is a projection of the bot's primary organization row, so
	// the two projections share the same column list and scanner.
	botVerifierSettingsColumnList = verifierOrganizationColumnList

	customVerificationColumnList = `id, organization_id, verifier_bot_id,
       peer_type, peer_id, icon_document_id, description, granted_by_user_id,
       granted_at, created_at, updated_at, version`

	customVerificationRequestColumnList = `id, verifier_bot_id, organization_id,
       applicant_user_id, peer_type, peer_id, peer_title, peer_username, reason,
       requested_description, status, decided_by, decision_reason, internal_note,
       correlation_id, created_at, updated_at, approved_at, rejected_at, version`
)

// customVerificationJoinColumns is the mark projection qualified for the
// bot_verifier_settings join the kill switch needs, kept in sync with the plain
// list by construction rather than by hand.
var customVerificationJoinColumns = prefixBotVerificationColumns(customVerificationColumnList, "cv.")

func prefixBotVerificationColumns(list, alias string) string {
	parts := strings.Split(list, ",")
	for i, part := range parts {
		parts[i] = alias + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

// botVerificationNow is the single clock for the store. Timestamps are truncated
// to the timestamptz resolution so a value written here reads back identically.
func botVerificationNow() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// ---- icon catalogue ---------------------------------------------------------

// UpsertVerificationIcon adds or updates a catalogue entry.
//
// document_id is the identity of an entry, not icon.ID: the catalogue exists to
// name real custom emoji documents, and the operator addresses them by document.
// Active travels with the payload, so an editor that means to keep an entry
// retired has to say so; SetVerificationIconActive is the narrow path for
// flipping only that flag.
func (s *BotVerificationStore) UpsertVerificationIcon(ctx context.Context, icon domain.VerificationIcon) (domain.VerificationIcon, error) {
	if s == nil || s.db == nil {
		return domain.VerificationIcon{}, fmt.Errorf("bot verification store is not configured")
	}
	icon.Name = strings.TrimSpace(icon.Name)
	if err := icon.Validate(); err != nil {
		return domain.VerificationIcon{}, err
	}
	if len(icon.Name) > maxVerificationIconNameBytes {
		return domain.VerificationIcon{}, domain.ErrVerificationIconInvalid
	}
	now := botVerificationNow()
	stored, err := scanVerificationIcon(s.db.QueryRow(ctx, `
INSERT INTO verification_icons (
  document_id, owner_bot_id, name, active, created_at, updated_at
) VALUES ($1,$2,$3,$4,$5,$5)
ON CONFLICT ON CONSTRAINT `+verificationIconDocumentConstraint+` DO UPDATE
SET owner_bot_id = EXCLUDED.owner_bot_id,
    name = EXCLUDED.name,
    active = EXCLUDED.active,
    updated_at = GREATEST(verification_icons.updated_at, EXCLUDED.updated_at)
RETURNING `+verificationIconColumnList,
		icon.DocumentID, icon.OwnerBotID, icon.Name, icon.Active, now,
	))
	if err != nil {
		return domain.VerificationIcon{}, fmt.Errorf("upsert verification icon: %w", err)
	}
	return stored, nil
}

// SetVerificationIconActive retires or restores an entry. Marks already granted
// with it keep rendering: the icon id is denormalised onto the mark.
func (s *BotVerificationStore) SetVerificationIconActive(ctx context.Context, iconID int64, active bool) (domain.VerificationIcon, error) {
	if s == nil || s.db == nil {
		return domain.VerificationIcon{}, fmt.Errorf("bot verification store is not configured")
	}
	if iconID <= 0 {
		return domain.VerificationIcon{}, domain.ErrVerificationIconNotFound
	}
	icon, err := scanVerificationIcon(s.db.QueryRow(ctx, `
UPDATE verification_icons
SET active = $2, updated_at = GREATEST(updated_at, $3)
WHERE id = $1
RETURNING `+verificationIconColumnList, iconID, active, botVerificationNow()))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.VerificationIcon{}, domain.ErrVerificationIconNotFound
	}
	if err != nil {
		return domain.VerificationIcon{}, fmt.Errorf("set verification icon active: %w", err)
	}
	return icon, nil
}

// VerificationIcon reads one entry by id.
func (s *BotVerificationStore) VerificationIcon(ctx context.Context, iconID int64) (domain.VerificationIcon, error) {
	if s == nil || s.db == nil {
		return domain.VerificationIcon{}, fmt.Errorf("bot verification store is not configured")
	}
	if iconID <= 0 {
		return domain.VerificationIcon{}, domain.ErrVerificationIconNotFound
	}
	icon, err := scanVerificationIcon(s.db.QueryRow(ctx, `
SELECT `+verificationIconColumnList+`
FROM verification_icons
WHERE id = $1`, iconID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.VerificationIcon{}, domain.ErrVerificationIconNotFound
	}
	if err != nil {
		return domain.VerificationIcon{}, fmt.Errorf("get verification icon: %w", err)
	}
	return icon, nil
}

// VerificationIconByDocument reads one entry by its custom emoji document id,
// which is how the admin edge resolves an icon a verifier already carries.
func (s *BotVerificationStore) VerificationIconByDocument(ctx context.Context, documentID int64) (domain.VerificationIcon, error) {
	if s == nil || s.db == nil {
		return domain.VerificationIcon{}, fmt.Errorf("bot verification store is not configured")
	}
	if documentID <= 0 {
		return domain.VerificationIcon{}, domain.ErrVerificationIconNotFound
	}
	icon, err := scanVerificationIcon(s.db.QueryRow(ctx, `
SELECT `+verificationIconColumnList+`
FROM verification_icons
WHERE document_id = $1`, documentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.VerificationIcon{}, domain.ErrVerificationIconNotFound
	}
	if err != nil {
		return domain.VerificationIcon{}, fmt.Errorf("get verification icon by document: %w", err)
	}
	return icon, nil
}

// ListVerificationIcons lists the catalogue, newest first. The order is the tail
// of verification_icons_active_idx, so the activeOnly form is an index-only walk.
func (s *BotVerificationStore) ListVerificationIcons(ctx context.Context, activeOnly bool, limit int) ([]domain.VerificationIcon, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("bot verification store is not configured")
	}
	limit = botVerificationLimit(limit)
	rows, err := s.db.Query(ctx, `
SELECT `+verificationIconColumnList+`
FROM verification_icons
WHERE NOT $1::boolean OR active
ORDER BY id DESC
LIMIT $2`, activeOnly, limit)
	if err != nil {
		return nil, fmt.Errorf("list verification icons: %w", err)
	}
	defer rows.Close()
	out := make([]domain.VerificationIcon, 0, limit)
	for rows.Next() {
		icon, err := scanVerificationIcon(rows)
		if err != nil {
			return nil, fmt.Errorf("scan verification icon: %w", err)
		}
		out = append(out, icon)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate verification icons: %w", err)
	}
	return out, nil
}

// ---- verifier status --------------------------------------------------------

// The legacy bot-level view is synthesized from the bot's primary organization
// (lowest display_priority, then newest row). A bot with no organizations is
// not a verifier.

// UpsertBotVerifierSettings grants or updates verifier status through the bot's
// PRIMARY organization. The 0155 bot-level API is deliberately kept: an
// operator/verifierbot that only ever knows one verifier per bot keeps working,
// and the built-in @verifierbot becomes a bot with (so far) one organization.
//
// settings.Version is the optimistic-locking expectation: 0 means "the bot has
// no organization yet", and a bot that already has one then reports
// domain.ErrCustomVerificationVersionConflict; a non-zero version must match the
// stored primary organization's. created_at is never rewritten, so the grant
// date survives every later edit.
func (s *BotVerificationStore) UpsertBotVerifierSettings(ctx context.Context, settings domain.BotVerifierSettings) (domain.BotVerifierSettings, error) {
	if s == nil || s.db == nil {
		return domain.BotVerifierSettings{}, fmt.Errorf("bot verification store is not configured")
	}
	settings.CompanyName = strings.TrimSpace(settings.CompanyName)
	settings.DefaultDescription = strings.TrimSpace(settings.DefaultDescription)
	settings.GrantedBy = strings.TrimSpace(settings.GrantedBy)
	settings.GrantReason = strings.TrimSpace(settings.GrantReason)
	if err := settings.Validate(); err != nil {
		return domain.BotVerifierSettings{}, err
	}
	if settings.Version < 0 || !botVerifierSettingsColumnsFit(settings) {
		return domain.BotVerifierSettings{}, domain.ErrVerifierSettingsInvalid
	}
	var stored domain.BotVerifierSettings
	err := withTx(ctx, s.db, "upsert bot verifier settings", func(tx pgx.Tx) error {
		orgs, err := lockBotOrganizationsTx(ctx, tx, settings.BotID)
		if err != nil {
			return err
		}
		if len(orgs) == 0 {
			// Granting a fresh verifier creates its primary organization at the
			// default rank, exactly the shape the old settings row had.
			now := botVerificationNow()
			inserted, err := scanVerifierOrganization(tx.QueryRow(ctx, `
INSERT INTO verifier_organizations (
  verifier_bot_id, company_name, icon_document_id, default_description,
  can_modify_custom_description, enabled, display_priority, granted_by,
  grant_reason, created_at, updated_at, version
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10,1)
RETURNING `+verifierOrganizationColumnList,
				settings.BotID, settings.CompanyName, settings.IconDocumentID,
				settings.DefaultDescription, settings.CanModifyCustomDescription,
				settings.Enabled, domain.DefaultVerifierDisplayPriority,
				settings.GrantedBy, settings.GrantReason, now,
			))
			if err != nil {
				return fmt.Errorf("insert verifier organization: %w", err)
			}
			stored = inserted.SettingsFor()
			return nil
		}
		primary := primaryOrganizationOf(orgs)
		if settings.Version == 0 || settings.Version != primary.Version {
			return domain.ErrCustomVerificationVersionConflict
		}
		updated, err := scanVerifierOrganization(tx.QueryRow(ctx, `
UPDATE verifier_organizations
SET icon_document_id = $3,
    company_name = $4,
    default_description = $5,
    can_modify_custom_description = $6,
    enabled = $7,
    granted_by = $8,
    grant_reason = $9,
    version = version + 1,
    updated_at = GREATEST(updated_at, $10)
WHERE id = $1 AND version = $2
RETURNING `+verifierOrganizationColumnList,
			primary.ID, settings.Version, settings.IconDocumentID,
			settings.CompanyName, settings.DefaultDescription,
			settings.CanModifyCustomDescription, settings.Enabled,
			settings.GrantedBy, settings.GrantReason, botVerificationNow(),
		))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCustomVerificationVersionConflict
		}
		if err != nil {
			return fmt.Errorf("update bot verifier settings: %w", err)
		}
		stored = updated.SettingsFor()
		return nil
	})
	if err != nil {
		return domain.BotVerifierSettings{}, err
	}
	return stored, nil
}

// SetBotVerifierEnabled flips the operator kill switch across every organization
// of the bot, which is what "disable this verifier" means now that one bot can
// front several organizations. Existing marks stay on disk, but the bot can
// grant nothing new and neither its settings nor its marks are projected, so
// flipping the switch back restores exactly what was there. Setting the flag to
// the value it already holds is a no-op and does not burn a version.
func (s *BotVerificationStore) SetBotVerifierEnabled(ctx context.Context, botID int64, enabled bool) (domain.BotVerifierSettings, error) {
	if s == nil || s.db == nil {
		return domain.BotVerifierSettings{}, fmt.Errorf("bot verification store is not configured")
	}
	if botID <= 0 {
		return domain.BotVerifierSettings{}, domain.ErrVerifierNotFound
	}
	var stored domain.BotVerifierSettings
	err := withTx(ctx, s.db, "set bot verifier enabled", func(tx pgx.Tx) error {
		orgs, err := lockBotOrganizationsTx(ctx, tx, botID)
		if err != nil {
			return err
		}
		if len(orgs) == 0 {
			return domain.ErrVerifierNotFound
		}
		unchanged := true
		for _, org := range orgs {
			if org.Enabled != enabled {
				unchanged = false
				break
			}
		}
		if unchanged {
			stored = primaryOrganizationOf(orgs).SettingsFor()
			return nil
		}
		if _, err := tx.Exec(ctx, `
UPDATE verifier_organizations
SET enabled = $2, version = version + 1, updated_at = GREATEST(updated_at, $3)
WHERE verifier_bot_id = $1`, botID, enabled, botVerificationNow()); err != nil {
			return fmt.Errorf("set bot verifier enabled: %w", err)
		}
		reloaded, err := scanVerifierOrganization(tx.QueryRow(ctx, `
SELECT `+verifierOrganizationColumnList+`
FROM verifier_organizations
WHERE verifier_bot_id = $1
ORDER BY display_priority, id
LIMIT 1`, botID))
		if err != nil {
			return fmt.Errorf("reload primary verifier organization: %w", err)
		}
		stored = reloaded.SettingsFor()
		return nil
	})
	if err != nil {
		return domain.BotVerifierSettings{}, err
	}
	return stored, nil
}

// DeleteBotVerifierSettings removes verifier status by deleting every
// organization of the bot. The organizations' marks cascade away with them
// (custom_verifications.organization_id ON DELETE CASCADE), because a mark whose
// verifier no longer exists has nothing to render. Applications survive: their
// organization_id is cleared (ON DELETE SET NULL) and the applicant
// reference stays, so the review history is preserved.
func (s *BotVerificationStore) DeleteBotVerifierSettings(ctx context.Context, botID int64) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("bot verification store is not configured")
	}
	if botID <= 0 {
		return false, domain.ErrVerifierNotFound
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM verifier_organizations WHERE verifier_bot_id = $1`, botID)
	if err != nil {
		return false, fmt.Errorf("delete verifier organizations: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// BotVerifierSettings reads one verifier's status (its primary organization),
// enabled or not: the caller needs the disabled row too, to render the kill
// switch and to explain BOT_VERIFIER_FORBIDDEN.
func (s *BotVerificationStore) BotVerifierSettings(ctx context.Context, botID int64) (domain.BotVerifierSettings, error) {
	if s == nil || s.db == nil {
		return domain.BotVerifierSettings{}, fmt.Errorf("bot verification store is not configured")
	}
	if botID <= 0 {
		return domain.BotVerifierSettings{}, domain.ErrVerifierNotFound
	}
	settings, err := scanBotVerifierSettings(s.db.QueryRow(ctx, `
SELECT `+verifierOrganizationColumnList+`
FROM verifier_organizations
WHERE verifier_bot_id = $1
ORDER BY display_priority, id
LIMIT 1`, botID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.BotVerifierSettings{}, domain.ErrVerifierNotFound
	}
	if err != nil {
		return domain.BotVerifierSettings{}, fmt.Errorf("get bot verifier settings: %w", err)
	}
	return settings, nil
}

// BotVerifierSettingsBatch resolves several bots in one round trip for the
// botInfo projection; bots without verifier status are absent from the map.
// Disabled verifiers are returned like any other row -- the Enabled field says
// so, and the projection edge decides -- which mirrors ListBotVerifiers taking
// enabledOnly as an explicit argument instead of assuming it.
func (s *BotVerificationStore) BotVerifierSettingsBatch(ctx context.Context, botIDs []int64) (map[int64]domain.BotVerifierSettings, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("bot verification store is not configured")
	}
	ids := make([]int64, 0, len(botIDs))
	seen := make(map[int64]struct{}, len(botIDs))
	for _, id := range botIDs {
		if id <= 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	out := make(map[int64]domain.BotVerifierSettings, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
SELECT DISTINCT ON (o.verifier_bot_id) `+verifierOrganizationColumnList+`
FROM verifier_organizations o
WHERE o.verifier_bot_id = ANY($1::bigint[])
ORDER BY o.verifier_bot_id, o.display_priority, o.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("batch bot verifier settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		org, err := scanVerifierOrganization(rows)
		if err != nil {
			return nil, fmt.Errorf("scan bot verifier organization: %w", err)
		}
		out[org.VerifierBotID] = org.SettingsFor()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate bot verifier organizations: %w", err)
	}
	return out, nil
}

// ListBotVerifiers lists verifier bots for the admin panel, one entry per bot
// carrying the primary organization's settings, ordered by bot id. enabledOnly
// keeps bots whose PRIMARY organization is enabled.
func (s *BotVerificationStore) ListBotVerifiers(ctx context.Context, enabledOnly bool, limit int) ([]domain.BotVerifierSettings, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("bot verification store is not configured")
	}
	limit = botVerificationLimit(limit)
	rows, err := s.db.Query(ctx, `
SELECT p.*
FROM (
  SELECT DISTINCT ON (o.verifier_bot_id) `+verifierOrganizationColumnList+`
  FROM verifier_organizations o
  ORDER BY o.verifier_bot_id, o.display_priority, o.id
) p
WHERE NOT $1::boolean OR p.enabled
ORDER BY p.verifier_bot_id
LIMIT $2`, enabledOnly, limit)
	if err != nil {
		return nil, fmt.Errorf("list bot verifiers: %w", err)
	}
	defer rows.Close()
	out := make([]domain.BotVerifierSettings, 0, limit)
	for rows.Next() {
		org, err := scanVerifierOrganization(rows)
		if err != nil {
			return nil, fmt.Errorf("scan bot verifier organization: %w", err)
		}
		out = append(out, org.SettingsFor())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate bot verifier organizations: %w", err)
	}
	return out, nil
}

// ---- organizations ----------------------------------------------------------

// UpsertVerifierOrganization creates (ID == 0) or updates an organization.
// verifier_bot_id is immutable once set: marks and the wire projection hang off
// the bot, so an update that would move the organization to another bot is
// rejected. Version follows the settings convention: 0 means "no such
// organization yet", and an edit must carry the stored version otherwise.
func (s *BotVerificationStore) UpsertVerifierOrganization(ctx context.Context, org domain.VerifierOrganization) (domain.VerifierOrganization, error) {
	if s == nil || s.db == nil {
		return domain.VerifierOrganization{}, fmt.Errorf("bot verification store is not configured")
	}
	org.CompanyName = strings.TrimSpace(org.CompanyName)
	org.DefaultDescription = strings.TrimSpace(org.DefaultDescription)
	org.GrantedBy = strings.TrimSpace(org.GrantedBy)
	org.GrantReason = strings.TrimSpace(org.GrantReason)
	if err := org.Validate(); err != nil {
		return domain.VerifierOrganization{}, err
	}
	if org.Version < 0 {
		return domain.VerifierOrganization{}, domain.ErrVerifierSettingsInvalid
	}
	if org.ID == 0 {
		now := botVerificationNow()
		inserted, err := scanVerifierOrganization(s.db.QueryRow(ctx, `
INSERT INTO verifier_organizations (
  verifier_bot_id, company_name, icon_document_id, default_description,
  can_modify_custom_description, enabled, display_priority, granted_by,
  grant_reason, created_at, updated_at, version
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10,1)
RETURNING `+verifierOrganizationColumnList,
			org.VerifierBotID, org.CompanyName, org.IconDocumentID,
			org.DefaultDescription, org.CanModifyCustomDescription,
			org.Enabled, org.DisplayPriority, org.GrantedBy, org.GrantReason, now,
		))
		if err != nil {
			return domain.VerifierOrganization{}, fmt.Errorf("insert verifier organization: %w", err)
		}
		return inserted, nil
	}
	if org.Version == 0 {
		return domain.VerifierOrganization{}, domain.ErrCustomVerificationVersionConflict
	}
	var stored domain.VerifierOrganization
	err := withTx(ctx, s.db, "upsert verifier organization", func(tx pgx.Tx) error {
		current, err := lockVerifierOrganizationTx(ctx, tx, org.ID)
		if err != nil {
			return err
		}
		if current.VerifierBotID != org.VerifierBotID {
			return domain.ErrVerifierSettingsInvalid
		}
		if current.Version != org.Version {
			return domain.ErrCustomVerificationVersionConflict
		}
		updated, err := scanVerifierOrganization(tx.QueryRow(ctx, `
UPDATE verifier_organizations
SET company_name = $3,
    icon_document_id = $4,
    default_description = $5,
    can_modify_custom_description = $6,
    enabled = $7,
    display_priority = $8,
    granted_by = $9,
    grant_reason = $10,
    version = version + 1,
    updated_at = GREATEST(updated_at, $11)
WHERE id = $1 AND version = $2
RETURNING `+verifierOrganizationColumnList,
			org.ID, org.Version, org.CompanyName, org.IconDocumentID,
			org.DefaultDescription, org.CanModifyCustomDescription, org.Enabled,
			org.DisplayPriority, org.GrantedBy, org.GrantReason, botVerificationNow(),
		))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCustomVerificationVersionConflict
		}
		if err != nil {
			return fmt.Errorf("update verifier organization: %w", err)
		}
		stored = updated
		return nil
	})
	if err != nil {
		return domain.VerifierOrganization{}, err
	}
	return stored, nil
}

// SetVerifierOrganizationEnabled flips one organization's enable bit. The mark
// rows stay on disk; only the projection stops rendering them. Setting the flag
// to its current value is a no-op.
func (s *BotVerificationStore) SetVerifierOrganizationEnabled(ctx context.Context, organizationID int64, enabled bool) (domain.VerifierOrganization, error) {
	if s == nil || s.db == nil {
		return domain.VerifierOrganization{}, fmt.Errorf("bot verification store is not configured")
	}
	if organizationID <= 0 {
		return domain.VerifierOrganization{}, domain.ErrOrganizationNotFound
	}
	var stored domain.VerifierOrganization
	err := withTx(ctx, s.db, "set verifier organization enabled", func(tx pgx.Tx) error {
		current, err := lockVerifierOrganizationTx(ctx, tx, organizationID)
		if err != nil {
			return err
		}
		if current.Enabled == enabled {
			stored = current
			return nil
		}
		updated, err := scanVerifierOrganization(tx.QueryRow(ctx, `
UPDATE verifier_organizations
SET enabled = $2, version = version + 1, updated_at = GREATEST(updated_at, $3)
WHERE id = $1
RETURNING `+verifierOrganizationColumnList, organizationID, enabled, botVerificationNow()))
		if err != nil {
			return fmt.Errorf("set verifier organization enabled: %w", err)
		}
		stored = updated
		return nil
	})
	if err != nil {
		return domain.VerifierOrganization{}, err
	}
	return stored, nil
}

// DeleteVerifierOrganization removes one organization. Its marks cascade away;
// applications it staged keep their history with organization_id cleared.
func (s *BotVerificationStore) DeleteVerifierOrganization(ctx context.Context, organizationID int64) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("bot verification store is not configured")
	}
	if organizationID <= 0 {
		return false, domain.ErrOrganizationNotFound
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM verifier_organizations WHERE id = $1`, organizationID)
	if err != nil {
		return false, fmt.Errorf("delete verifier organization: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// VerifierOrganization reads one organization by id.
func (s *BotVerificationStore) VerifierOrganization(ctx context.Context, organizationID int64) (domain.VerifierOrganization, error) {
	if s == nil || s.db == nil {
		return domain.VerifierOrganization{}, fmt.Errorf("bot verification store is not configured")
	}
	if organizationID <= 0 {
		return domain.VerifierOrganization{}, domain.ErrOrganizationNotFound
	}
	org, err := scanVerifierOrganization(s.db.QueryRow(ctx, `
SELECT `+verifierOrganizationColumnList+`
FROM verifier_organizations
WHERE id = $1`, organizationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.VerifierOrganization{}, domain.ErrOrganizationNotFound
	}
	if err != nil {
		return domain.VerifierOrganization{}, fmt.Errorf("get verifier organization: %w", err)
	}
	return org, nil
}

// VerifierOrganizationsByBot lists one bot's organizations ranked by
// display_priority, so the first entry is always the primary. enabledOnly drops
// disabled ones.
func (s *BotVerificationStore) VerifierOrganizationsByBot(ctx context.Context, verifierBotID int64, enabledOnly bool) ([]domain.VerifierOrganization, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("bot verification store is not configured")
	}
	if verifierBotID <= 0 {
		return nil, domain.ErrVerifierNotFound
	}
	rows, err := s.db.Query(ctx, `
SELECT `+verifierOrganizationColumnList+`
FROM verifier_organizations
WHERE verifier_bot_id = $1 AND (NOT $2::boolean OR enabled)
ORDER BY display_priority, id`, verifierBotID, enabledOnly)
	if err != nil {
		return nil, fmt.Errorf("list verifier organizations: %w", err)
	}
	defer rows.Close()
	out := make([]domain.VerifierOrganization, 0, 4)
	for rows.Next() {
		org, err := scanVerifierOrganization(rows)
		if err != nil {
			return nil, fmt.Errorf("scan verifier organization: %w", err)
		}
		out = append(out, org)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate verifier organizations: %w", err)
	}
	return out, nil
}

// ListVerifierOrganizations is the admin catalogue, grouped by verifier bot.
func (s *BotVerificationStore) ListVerifierOrganizations(ctx context.Context, enabledOnly bool, limit int) ([]domain.VerifierOrganization, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("bot verification store is not configured")
	}
	limit = botVerificationLimit(limit)
	rows, err := s.db.Query(ctx, `
SELECT `+verifierOrganizationColumnList+`
FROM verifier_organizations
WHERE NOT $1::boolean OR enabled
ORDER BY verifier_bot_id, display_priority, id
LIMIT $2`, enabledOnly, limit)
	if err != nil {
		return nil, fmt.Errorf("list verifier organizations: %w", err)
	}
	defer rows.Close()
	out := make([]domain.VerifierOrganization, 0, limit)
	for rows.Next() {
		org, err := scanVerifierOrganization(rows)
		if err != nil {
			return nil, fmt.Errorf("scan verifier organization: %w", err)
		}
		out = append(out, org)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate verifier organizations: %w", err)
	}
	return out, nil
}

// ---- granted marks ---------------------------------------------------------

// GrantCustomVerification creates or updates the mark a verifier bot owns on a
// peer, scoped to one ORGANIZATION (mark.OrganizationID; a zero value resolves
// to the bot's primary organization).
//
// custom_verifications_org_peer_once makes (organization, peer) the identity of
// a mark. A repeat by the same organization updates in place; marks of other
// organizations of the same bot coexist and the projection picks the winner, so
// unlike 0155 nothing is squeezed out at write time.
//
// The bot's organizations are locked first: the primary row is the existence
// check (domain.ErrVerifierNotFound) and the whole set is the serialisation
// point that makes the per-verifier bound real. Two concurrent grants by one bot
// queue up, so the count they check cannot go stale between the check and the
// insert and domain.MaxCustomVerificationsPerVerifier cannot be overshot.
//
// mark.IconDocumentID is denormalised from the organization's settings when the
// caller leaves it unset, which is what "the icon is taken from the verifier at
// grant time" means; an explicit id is honoured, so re-issuing a historical mark
// keeps its original icon. granted_at records when the mark won: it is set on a
// fresh grant and preserved on an in-place update, and feeds the winner
// tie-break (newest granted wins).
func (s *BotVerificationStore) GrantCustomVerification(ctx context.Context, mark domain.CustomVerification) (domain.CustomVerification, bool, error) {
	if s == nil || s.db == nil {
		return domain.CustomVerification{}, false, fmt.Errorf("bot verification store is not configured")
	}
	mark.Description = strings.TrimSpace(mark.Description)
	if mark.VerifierBotID <= 0 || !validBotVerificationPeer(mark.Peer) {
		return domain.CustomVerification{}, false, domain.ErrCustomVerificationTargetInvalid
	}
	if mark.GrantedByUserID < 0 {
		return domain.CustomVerification{}, false, domain.ErrCustomVerificationTargetInvalid
	}
	if len(mark.Description) > maxCustomVerificationDescriptionBytes {
		return domain.CustomVerification{}, false, domain.ErrCustomVerificationRequestInvalid
	}
	var stored domain.CustomVerification
	created := false
	err := withTx(ctx, s.db, "grant custom verification", func(tx pgx.Tx) error {
		orgs, err := lockBotOrganizationsTx(ctx, tx, mark.VerifierBotID)
		if err != nil {
			return err
		}
		if len(orgs) == 0 {
			return domain.ErrVerifierNotFound
		}
		org, err := resolveOrganizationOf(orgs, mark.OrganizationID)
		if err != nil {
			return err
		}
		mark.OrganizationID = org.ID
		if mark.IconDocumentID <= 0 {
			mark.IconDocumentID = org.IconDocumentID
		}
		if err := mark.Validate(); err != nil {
			return err
		}
		existed := true
		switch _, err := customVerificationTx(ctx, tx, org.ID, mark.Peer, true); {
		case err == nil:
		case errors.Is(err, domain.ErrCustomVerificationNotFound):
			existed = false
		default:
			return err
		}
		if !existed {
			count, err := countCustomVerificationsTx(ctx, tx, mark.VerifierBotID)
			if err != nil {
				return err
			}
			if count >= domain.MaxCustomVerificationsPerVerifier {
				return domain.ErrCustomVerificationLimit
			}
		}
		now := botVerificationNow()
		upserted, err := scanCustomVerification(tx.QueryRow(ctx, `
INSERT INTO custom_verifications (
  organization_id, verifier_bot_id, peer_type, peer_id, icon_document_id,
  description, granted_by_user_id, granted_at, created_at, updated_at, version
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,$8,1)
ON CONFLICT ON CONSTRAINT `+customVerificationOnceConstraint+` DO UPDATE
SET organization_id = EXCLUDED.organization_id,
    verifier_bot_id = EXCLUDED.verifier_bot_id,
    icon_document_id = EXCLUDED.icon_document_id,
    description = EXCLUDED.description,
    granted_by_user_id = EXCLUDED.granted_by_user_id,
    granted_at = CASE
      WHEN custom_verifications.organization_id = EXCLUDED.organization_id
        THEN custom_verifications.granted_at
      ELSE EXCLUDED.granted_at
    END,
    created_at = CASE
      WHEN custom_verifications.organization_id = EXCLUDED.organization_id
        THEN custom_verifications.created_at
      ELSE EXCLUDED.created_at
    END,
    version = custom_verifications.version + 1,
    updated_at = GREATEST(custom_verifications.updated_at, EXCLUDED.updated_at)
RETURNING `+customVerificationColumnList,
			mark.OrganizationID, mark.VerifierBotID, string(mark.Peer.Type), mark.Peer.ID,
			mark.IconDocumentID, mark.Description, mark.GrantedByUserID, now,
		))
		if err != nil {
			return fmt.Errorf("grant custom verification: %w", err)
		}
		stored = upserted
		created = !existed
		return nil
	})
	if err != nil {
		return domain.CustomVerification{}, false, err
	}
	return stored, created, nil
}

// RevokeCustomVerification removes this verifier bot's WINNING mark from the
// peer -- the one its projection would show -- and reports whether anything was
// removed, so a repeated revoke is a no-op instead of an error. When the bot
// hosts several organizations, the winner's mark goes, exposing the next
// organization's mark as the rendered badge, which is the "revoked the badge the
// peer sees" behaviour.
func (s *BotVerificationStore) RevokeCustomVerification(ctx context.Context, verifierBotID int64, peer domain.Peer) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("bot verification store is not configured")
	}
	if verifierBotID <= 0 || !validBotVerificationPeer(peer) {
		return false, domain.ErrCustomVerificationTargetInvalid
	}
	tag, err := s.db.Exec(ctx, `
DELETE FROM custom_verifications
WHERE peer_type = $2 AND peer_id = $3
  AND id = (
    SELECT cv.id
    FROM custom_verifications cv
    JOIN verifier_organizations o ON o.id = cv.organization_id
    WHERE o.verifier_bot_id = $1 AND cv.peer_type = $2 AND cv.peer_id = $3
    ORDER BY o.display_priority, cv.granted_at DESC, cv.id DESC
    LIMIT 1
  )`,
		verifierBotID, string(peer.Type), peer.ID)
	if err != nil {
		return false, fmt.Errorf("revoke custom verification: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RevokeOrganizationMark removes one organization's mark on a peer, whether or
// not it is the projection winner. This is the precise path for admin revokes
// and for clearing a sold organization's mark so the peer's slot frees up.
func (s *BotVerificationStore) RevokeOrganizationMark(ctx context.Context, organizationID int64, peer domain.Peer) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("bot verification store is not configured")
	}
	if organizationID <= 0 || !validBotVerificationPeer(peer) {
		return false, domain.ErrCustomVerificationTargetInvalid
	}
	tag, err := s.db.Exec(ctx, `
DELETE FROM custom_verifications
WHERE organization_id = $1 AND peer_type = $2 AND peer_id = $3`,
		organizationID, string(peer.Type), peer.ID)
	if err != nil {
		return false, fmt.Errorf("revoke organization mark: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// CustomVerification reads this verifier bot's WINNING mark on a peer (the one
// rendered under the bot), whether or not that verifier is currently enabled:
// this is the bookkeeping read, not the projection.
func (s *BotVerificationStore) CustomVerification(ctx context.Context, verifierBotID int64, peer domain.Peer) (domain.CustomVerification, error) {
	if s == nil || s.db == nil {
		return domain.CustomVerification{}, fmt.Errorf("bot verification store is not configured")
	}
	if verifierBotID <= 0 || !validBotVerificationPeer(peer) {
		return domain.CustomVerification{}, domain.ErrCustomVerificationNotFound
	}
	mark, err := scanCustomVerification(s.db.QueryRow(ctx, `
SELECT `+customVerificationJoinColumns+`
FROM custom_verifications cv
JOIN verifier_organizations o ON o.id = cv.organization_id
WHERE o.verifier_bot_id = $1 AND cv.peer_type = $2 AND cv.peer_id = $3
ORDER BY o.display_priority, cv.granted_at DESC, cv.id DESC
LIMIT 1`, verifierBotID, string(peer.Type), peer.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CustomVerification{}, domain.ErrCustomVerificationNotFound
	}
	if err != nil {
		return domain.CustomVerification{}, fmt.Errorf("get verifier custom verification: %w", err)
	}
	return mark, nil
}

// OrganizationCustomVerification reads one organization's mark on a peer, winner
// or not.
func (s *BotVerificationStore) OrganizationCustomVerification(ctx context.Context, organizationID int64, peer domain.Peer) (domain.CustomVerification, error) {
	if s == nil || s.db == nil {
		return domain.CustomVerification{}, fmt.Errorf("bot verification store is not configured")
	}
	return customVerificationTx(ctx, s.db, organizationID, peer, false)
}

// PeerVerification returns the mark a peer is rendered with: the winner of the
// tie between every enabled organization marking the peer.
func (s *BotVerificationStore) PeerVerification(ctx context.Context, peer domain.Peer) (domain.CustomVerification, error) {
	if s == nil || s.db == nil {
		return domain.CustomVerification{}, fmt.Errorf("bot verification store is not configured")
	}
	if !validBotVerificationPeer(peer) {
		return domain.CustomVerification{}, domain.ErrCustomVerificationNotFound
	}
	mark, err := scanCustomVerification(s.db.QueryRow(ctx, `
SELECT `+customVerificationJoinColumns+`
FROM custom_verifications cv
JOIN verifier_organizations o ON o.id = cv.organization_id AND o.enabled
WHERE cv.peer_type = $1 AND cv.peer_id = $2
ORDER BY o.display_priority, cv.granted_at DESC, cv.id DESC
LIMIT 1`, string(peer.Type), peer.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CustomVerification{}, domain.ErrCustomVerificationNotFound
	}
	if err != nil {
		return domain.CustomVerification{}, fmt.Errorf("get peer verification: %w", err)
	}
	return mark, nil
}

// PeerVerificationBatch resolves the projection for many peers at once.
//
// This is the call on the hot serialisation path, so it is ONE query for the
// whole batch: DISTINCT ON (peer_type, peer_id) with the same enabled-organization
// winner ordering PeerVerification applies per peer, and peers without a mark are
// simply absent instead of erroring. Sending N queries here would put a per-peer
// round trip on every dialog list.
func (s *BotVerificationStore) PeerVerificationBatch(ctx context.Context, peers []domain.Peer) (map[domain.Peer]domain.CustomVerification, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("bot verification store is not configured")
	}
	types, ids := botVerificationPeerArrays(peers)
	out := make(map[domain.Peer]domain.CustomVerification, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
SELECT DISTINCT ON (cv.peer_type, cv.peer_id) `+customVerificationJoinColumns+`
FROM custom_verifications cv
JOIN verifier_organizations o ON o.id = cv.organization_id AND o.enabled
WHERE (cv.peer_type, cv.peer_id) IN (SELECT * FROM unnest($1::text[], $2::bigint[]))
ORDER BY cv.peer_type, cv.peer_id, o.display_priority, cv.granted_at DESC, cv.id DESC`, types, ids)
	if err != nil {
		return nil, fmt.Errorf("batch peer verification: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		mark, err := scanCustomVerification(rows)
		if err != nil {
			return nil, fmt.Errorf("scan peer verification: %w", err)
		}
		out[mark.Peer] = mark
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate peer verifications: %w", err)
	}
	return out, nil
}

// CountCustomVerifications reports how many peers a verifier bot has marked
// across all its organizations, for the per-verifier bound. Disabled verifiers
// still count their marks: the switch hides badges, it does not free quota.
func (s *BotVerificationStore) CountCustomVerifications(ctx context.Context, verifierBotID int64) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("bot verification store is not configured")
	}
	if verifierBotID <= 0 {
		return 0, domain.ErrCustomVerificationTargetInvalid
	}
	return countCustomVerificationsTx(ctx, s.db, verifierBotID)
}

// ListCustomVerifications is the admin listing query with keyset paging.
//
// Paging is keyset over id DESC (filter.BeforeID carries the last row of the
// previous page), which is the tail of custom_verifications_org_idx and of
// custom_verifications_peer_idx. Query matches a mark id or a peer id when it is
// numeric and otherwise matches the description case-insensitively, the only
// text a mark carries. When the filter names an OrganizationID, only that
// organization's marks are listed.
func (s *BotVerificationStore) ListCustomVerifications(ctx context.Context, filter domain.CustomVerificationFilter) ([]domain.CustomVerification, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("bot verification store is not configured")
	}
	if filter.PeerType != "" && !botVerificationPeerType(filter.PeerType) {
		return nil, domain.ErrCustomVerificationTargetInvalid
	}
	limit := botVerificationLimit(filter.Limit)
	numeric, isNumeric, needle := parseBotVerificationQuery(filter.Query)
	rows, err := s.db.Query(ctx, `
SELECT `+customVerificationColumnList+`
FROM custom_verifications
WHERE ($1 = 0 OR organization_id = $1)
  AND ($2 = 0 OR verifier_bot_id = $2)
  AND ($3 = '' OR peer_type = $3)
  AND ($4 = 0 OR peer_id = $4)
  AND ($5 = 0 OR id < $5)
  AND (
    NOT $6::boolean
    OR ($7::boolean AND (id = $8::bigint OR peer_id = $8::bigint))
    OR (NOT $7::boolean AND lower(description) LIKE '%' || $9::text || '%')
  )
ORDER BY id DESC
LIMIT $10`,
		filter.OrganizationID, filter.VerifierBotID, string(filter.PeerType),
		filter.PeerID, filter.BeforeID, isNumeric || needle != "", isNumeric,
		numeric, escapeLike(needle), limit)
	if err != nil {
		return nil, fmt.Errorf("list custom verifications: %w", err)
	}
	defer rows.Close()
	out := make([]domain.CustomVerification, 0, limit)
	for rows.Next() {
		mark, err := scanCustomVerification(rows)
		if err != nil {
			return nil, fmt.Errorf("scan custom verification: %w", err)
		}
		out = append(out, mark)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate custom verifications: %w", err)
	}
	return out, nil
}

// ---- application queue -----------------------------------------------------

// CreateCustomVerificationRequest files an application.
//
// A filed application is pending by definition, so the status is forced and any
// decision field the caller pre-filled is dropped: only
// DecideCustomVerificationRequest may write those.
// custom_verification_requests_pending_idx allows one live application per
// (organization, peer), and a second one reports
// domain.ErrCustomVerificationRequestExists -- two pending rows would let two
// decisions race for one mark. A zero OrganizationID in the request is resolved
// to the verifier bot's primary organization.
func (s *BotVerificationStore) CreateCustomVerificationRequest(ctx context.Context, req domain.CustomVerificationRequest) (domain.CustomVerificationRequest, error) {
	if s == nil || s.db == nil {
		return domain.CustomVerificationRequest{}, fmt.Errorf("bot verification store is not configured")
	}
	req = normalizeCustomVerificationRequest(req)
	if req.Status == "" {
		req.Status = domain.CustomVerificationPending
	}
	if req.Status != domain.CustomVerificationPending {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestInvalid
	}
	req.DecidedBy = ""
	req.DecisionReason = ""
	if err := req.Validate(); err != nil {
		return domain.CustomVerificationRequest{}, err
	}
	if err := validateCustomVerificationRequestColumns(req); err != nil {
		return domain.CustomVerificationRequest{}, err
	}
	org, err := resolveOrganizationForBot(ctx, s.db, req.VerifierBotID, req.OrganizationID)
	if err != nil {
		return domain.CustomVerificationRequest{}, err
	}
	req.OrganizationID = org.ID
	now := botVerificationNow()
	stored, err := scanCustomVerificationRequest(s.db.QueryRow(ctx, `
INSERT INTO custom_verification_requests (
  verifier_bot_id, organization_id, applicant_user_id, peer_type, peer_id,
  peer_title, peer_username, reason, requested_description, status,
  internal_note, correlation_id, created_at, updated_at, version
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending',$10,$11,$12,$12,1)
RETURNING `+customVerificationRequestColumnList,
		req.VerifierBotID, req.OrganizationID, req.ApplicantUserID,
		string(req.Peer.Type), req.Peer.ID, req.PeerTitle, req.PeerUsername,
		req.Reason, req.RequestedDescription, req.InternalNote,
		req.CorrelationID, now,
	))
	if err != nil {
		if isUniqueConstraint(err, customVerificationRequestPendingIndex) {
			return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestExists
		}
		return domain.CustomVerificationRequest{}, fmt.Errorf("insert custom verification request: %w", err)
	}
	return stored, nil
}

// DecideCustomVerificationRequest moves an application through its status
// machine and keeps the mark in step with it.
//
// The whole decision is one transaction: the status change, the decision
// metadata, the approved_at/rejected_at stamps and the apply callback that
// grants (status approved) or removes (status revoked) the mark. apply is handed
// a context carrying this transaction, so it must write through
// VerificationTxFromContext -- for example
// postgres.NewBotVerificationStore(tx).GrantCustomVerification. A callback that
// fails rolls the status change back with it, which is why "approved without a
// mark" is not a reachable state; a callback that ignores the handle would write
// on its own connection and survive that rollback.
//
// Order of checks is the deterministic part of two reviewers acting at once: the
// version is compared first, so the loser always sees
// domain.ErrCustomVerificationVersionConflict. A caller re-issuing a decision
// that already holds gets the request back with changed=false, no second stamp
// and no second apply -- the callback is not idempotent by assumption.
//
// The transition itself is domain.CanTransitionCustomVerificationStatus, never
// re-implemented here, and the resulting row is validated with
// domain.CustomVerificationRequest.Validate, which is what rejects a rejection
// with no reason (domain.ErrVerificationReasonRequired).
func (s *BotVerificationStore) DecideCustomVerificationRequest(ctx context.Context, requestID int64, version int64, status domain.CustomVerificationRequestStatus, decidedBy, reason, note string, apply func(ctx context.Context, req domain.CustomVerificationRequest) error) (domain.CustomVerificationRequest, bool, error) {
	if s == nil || s.db == nil {
		return domain.CustomVerificationRequest{}, false, fmt.Errorf("bot verification store is not configured")
	}
	decidedBy = strings.TrimSpace(decidedBy)
	reason = strings.TrimSpace(reason)
	note = strings.TrimSpace(note)
	if requestID <= 0 || version <= 0 {
		return domain.CustomVerificationRequest{}, false, domain.ErrCustomVerificationRequestInvalid
	}
	if !status.Valid() || status == domain.CustomVerificationPending {
		// Pending is where an application starts, not a decision anybody makes.
		return domain.CustomVerificationRequest{}, false, domain.ErrCustomVerificationRequestInvalid
	}
	if customVerificationDecisionNeedsApply(status) && apply == nil {
		// Deciding without a way to move the mark is exactly the state this store
		// exists to make impossible.
		return domain.CustomVerificationRequest{}, false, fmt.Errorf("custom verification decision %q requires an apply callback", status)
	}
	var stored domain.CustomVerificationRequest
	changed := false
	err := withTx(ctx, s.db, "decide custom verification request", func(tx pgx.Tx) error {
		current, err := lockCustomVerificationRequestTx(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if current.Version != version {
			return domain.ErrCustomVerificationVersionConflict
		}
		if current.Status == status {
			// Already decided this way: keep the record and report that nothing
			// moved, so a retried decision cannot apply the mark twice.
			stored = current
			return nil
		}
		if !domain.CanTransitionCustomVerificationStatus(current.Status, status) {
			return domain.ErrCustomVerificationRequestInvalid
		}
		now := botVerificationNow()
		next := customVerificationDecisionState(current, status, decidedBy, reason, note, now)
		if err := next.Validate(); err != nil {
			return err
		}
		if err := validateCustomVerificationRequestColumns(next); err != nil {
			return err
		}
		updated, err := scanCustomVerificationRequest(tx.QueryRow(ctx, `
UPDATE custom_verification_requests
SET status = $3,
    decided_by = $4,
    decision_reason = $5,
    internal_note = $6,
    approved_at = $7,
    rejected_at = $8,
    version = version + 1,
    updated_at = GREATEST(updated_at, $9)
WHERE id = $1 AND version = $2
RETURNING `+customVerificationRequestColumnList,
			requestID, version, string(next.Status), next.DecidedBy,
			next.DecisionReason, next.InternalNote,
			botVerificationTimeArg(next.ApprovedAt),
			botVerificationTimeArg(next.RejectedAt), now,
		))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCustomVerificationVersionConflict
		}
		if err != nil {
			return fmt.Errorf("decide custom verification request: %w", err)
		}
		if customVerificationDecisionNeedsApply(status) {
			if err := apply(verificationTxContext(ctx, tx), updated); err != nil {
				return err
			}
		}
		stored = updated
		changed = true
		return nil
	})
	if err != nil {
		return domain.CustomVerificationRequest{}, false, err
	}
	return stored, changed, nil
}

// CustomVerificationRequest reads one application.
func (s *BotVerificationStore) CustomVerificationRequest(ctx context.Context, requestID int64) (domain.CustomVerificationRequest, error) {
	if s == nil || s.db == nil {
		return domain.CustomVerificationRequest{}, fmt.Errorf("bot verification store is not configured")
	}
	if requestID <= 0 {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestNotFound
	}
	req, err := scanCustomVerificationRequest(s.db.QueryRow(ctx, `
SELECT `+customVerificationRequestColumnList+`
FROM custom_verification_requests
WHERE id = $1`, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestNotFound
	}
	if err != nil {
		return domain.CustomVerificationRequest{}, fmt.Errorf("get custom verification request: %w", err)
	}
	return req, nil
}

// PendingCustomVerificationRequest returns the live application for a
// (verifier, peer) pair, resolved through the bot's primary organization. The
// partial unique index guarantees there is at most one per organization, so no
// ordering is needed to pick it.
func (s *BotVerificationStore) PendingCustomVerificationRequest(ctx context.Context, verifierBotID int64, peer domain.Peer) (domain.CustomVerificationRequest, error) {
	if s == nil || s.db == nil {
		return domain.CustomVerificationRequest{}, fmt.Errorf("bot verification store is not configured")
	}
	if verifierBotID <= 0 || !validBotVerificationPeer(peer) {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestNotFound
	}
	org, err := resolveOrganizationForBot(ctx, s.db, verifierBotID, 0)
	if err != nil {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestNotFound
	}
	req, err := scanCustomVerificationRequest(s.db.QueryRow(ctx, `
SELECT `+customVerificationRequestColumnList+`
FROM custom_verification_requests
WHERE organization_id = $1 AND peer_type = $2 AND peer_id = $3
  AND status = 'pending'
LIMIT 1`, org.ID, string(peer.Type), peer.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestNotFound
	}
	if err != nil {
		return domain.CustomVerificationRequest{}, fmt.Errorf("get pending custom verification request: %w", err)
	}
	return req, nil
}

// ListCustomVerificationRequests is the review-queue query with keyset paging.
//
// Paging is keyset over id DESC: the filter carries no timestamp cursor, and ids
// are monotonic, so id DESC is the same page order as created_at DESC without
// the ambiguity a mixed cursor would have. Query matches an application id or a
// peer id when it is numeric and otherwise prefix-matches the lowercased
// username snapshot, the same split the official verification queue uses.
func (s *BotVerificationStore) ListCustomVerificationRequests(ctx context.Context, filter domain.CustomVerificationRequestFilter) ([]domain.CustomVerificationRequest, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("bot verification store is not configured")
	}
	statuses := make([]string, 0, len(filter.Statuses))
	for _, status := range filter.Statuses {
		if !status.Valid() {
			return nil, domain.ErrCustomVerificationRequestInvalid
		}
		statuses = append(statuses, string(status))
	}
	if filter.PeerType != "" && !botVerificationPeerType(filter.PeerType) {
		return nil, domain.ErrCustomVerificationTargetInvalid
	}
	limit := botVerificationLimit(filter.Limit)
	numeric, isNumeric, prefix := parseBotVerificationQuery(filter.Query)
	rows, err := s.db.Query(ctx, `
SELECT `+customVerificationRequestColumnList+`
FROM custom_verification_requests
WHERE (cardinality($1::text[]) = 0 OR status = ANY($1::text[]))
  AND ($2 = 0 OR organization_id = $2)
  AND ($3 = 0 OR verifier_bot_id = $3)
  AND ($4 = '' OR peer_type = $4)
  AND ($5 = 0 OR id < $5)
  AND (
    NOT $6::boolean
    OR ($7::boolean AND (id = $8::bigint OR peer_id = $8::bigint))
    OR (
      NOT $7::boolean
      AND peer_username <> ''
      AND lower(peer_username) LIKE $9::text || '%'
    )
  )
ORDER BY id DESC
LIMIT $10`,
		statuses, filter.OrganizationID, filter.VerifierBotID,
		string(filter.PeerType), filter.BeforeID, isNumeric || prefix != "",
		isNumeric, numeric, escapeLike(prefix), limit)
	if err != nil {
		return nil, fmt.Errorf("list custom verification requests: %w", err)
	}
	defer rows.Close()
	out := make([]domain.CustomVerificationRequest, 0, limit)
	for rows.Next() {
		req, err := scanCustomVerificationRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("scan custom verification request: %w", err)
		}
		out = append(out, req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate custom verification requests: %w", err)
	}
	return out, nil
}

// CustomVerificationRequestsForApplicant returns an applicant's own history,
// newest first, for the verifier bot's /status command.
func (s *BotVerificationStore) CustomVerificationRequestsForApplicant(ctx context.Context, applicantUserID int64, limit int) ([]domain.CustomVerificationRequest, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("bot verification store is not configured")
	}
	if applicantUserID <= 0 {
		return nil, domain.ErrCustomVerificationRequestInvalid
	}
	limit = botVerificationLimit(limit)
	rows, err := s.db.Query(ctx, `
SELECT `+customVerificationRequestColumnList+`
FROM custom_verification_requests
WHERE applicant_user_id = $1
ORDER BY id DESC
LIMIT $2`, applicantUserID, limit)
	if err != nil {
		return nil, fmt.Errorf("list applicant custom verification requests: %w", err)
	}
	defer rows.Close()
	out := make([]domain.CustomVerificationRequest, 0, limit)
	for rows.Next() {
		req, err := scanCustomVerificationRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("scan applicant custom verification request: %w", err)
		}
		out = append(out, req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applicant custom verification requests: %w", err)
	}
	return out, nil
}

// CustomVerificationRequestCounts is the queue summary by status. Statuses
// nobody is in are absent rather than zero, which a map read cannot tell apart
// anyway.
func (s *BotVerificationStore) CustomVerificationRequestCounts(ctx context.Context) (map[domain.CustomVerificationRequestStatus]int64, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("bot verification store is not configured")
	}
	rows, err := s.db.Query(ctx, `
SELECT status, count(*)
FROM custom_verification_requests
GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("count custom verification requests: %w", err)
	}
	defer rows.Close()
	out := make(map[domain.CustomVerificationRequestStatus]int64, 4)
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scan custom verification request count: %w", err)
		}
		out[domain.CustomVerificationRequestStatus(status)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate custom verification request counts: %w", err)
	}
	return out, nil
}

// ---- helpers ----------------------------------------------------------------

func scanVerificationIcon(row pgx.Row) (domain.VerificationIcon, error) {
	var icon domain.VerificationIcon
	if err := row.Scan(&icon.ID, &icon.DocumentID, &icon.OwnerBotID, &icon.Name,
		&icon.Active, &icon.CreatedAt, &icon.UpdatedAt); err != nil {
		return domain.VerificationIcon{}, err
	}
	icon.CreatedAt = icon.CreatedAt.UTC()
	icon.UpdatedAt = icon.UpdatedAt.UTC()
	return icon, nil
}

func scanVerifierOrganization(row pgx.Row) (domain.VerifierOrganization, error) {
	var org domain.VerifierOrganization
	if err := row.Scan(&org.ID, &org.VerifierBotID, &org.CompanyName,
		&org.IconDocumentID, &org.DefaultDescription,
		&org.CanModifyCustomDescription, &org.Enabled, &org.DisplayPriority,
		&org.GrantedBy, &org.GrantReason, &org.CreatedAt, &org.UpdatedAt,
		&org.Version); err != nil {
		return domain.VerifierOrganization{}, err
	}
	org.CreatedAt = org.CreatedAt.UTC()
	org.UpdatedAt = org.UpdatedAt.UTC()
	return org, nil
}

// scanBotVerifierSettings adapts a PRIMARY organization row to the legacy
// bot-level settings view.
func scanBotVerifierSettings(row pgx.Row) (domain.BotVerifierSettings, error) {
	org, err := scanVerifierOrganization(row)
	if err != nil {
		return domain.BotVerifierSettings{}, err
	}
	return org.SettingsFor(), nil
}

// lockBotOrganizationsTx reads and locks every organization of a bot in id
// order (stable lock order, so concurrent grants of the same bot cannot deadlock
// against each other). The set is the serialisation point every grant and every
// settings mutation by that bot goes through, which is what makes the
// per-verifier bound hold under concurrency. An empty slice means the bot is not
// a verifier; callers decide what that is worth.
func lockBotOrganizationsTx(ctx context.Context, tx pgx.Tx, botID int64) ([]domain.VerifierOrganization, error) {
	rows, err := tx.Query(ctx, `
SELECT `+verifierOrganizationColumnList+`
FROM verifier_organizations
WHERE verifier_bot_id = $1
ORDER BY id
FOR UPDATE`, botID)
	if err != nil {
		return nil, fmt.Errorf("lock verifier organizations: %w", err)
	}
	defer rows.Close()
	var orgs []domain.VerifierOrganization
	for rows.Next() {
		org, err := scanVerifierOrganization(rows)
		if err != nil {
			return nil, fmt.Errorf("scan locked verifier organization: %w", err)
		}
		orgs = append(orgs, org)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate locked verifier organizations: %w", err)
	}
	return orgs, nil
}

// lockVerifierOrganizationTx reads one organization for mutation.
func lockVerifierOrganizationTx(ctx context.Context, tx pgx.Tx, organizationID int64) (domain.VerifierOrganization, error) {
	org, err := scanVerifierOrganization(tx.QueryRow(ctx, `
SELECT `+verifierOrganizationColumnList+`
FROM verifier_organizations
WHERE id = $1
FOR UPDATE`, organizationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.VerifierOrganization{}, domain.ErrOrganizationNotFound
	}
	if err != nil {
		return domain.VerifierOrganization{}, fmt.Errorf("lock verifier organization: %w", err)
	}
	return org, nil
}

// primaryOrganizationOf picks the primary organization of a bot's locked set:
// lowest display_priority, then newest row. The caller guarantees a non-empty
// slice.
func primaryOrganizationOf(orgs []domain.VerifierOrganization) domain.VerifierOrganization {
	primary := orgs[0]
	for _, org := range orgs[1:] {
		if org.DisplayPriority < primary.DisplayPriority ||
			(org.DisplayPriority == primary.DisplayPriority && org.ID < primary.ID) {
			primary = org
		}
	}
	return primary
}

// resolveOrganizationOf resolves a mark's organization within a bot's locked
// set: zero means the primary, anything else must name an organization the bot
// hosts.
func resolveOrganizationOf(orgs []domain.VerifierOrganization, organizationID int64) (domain.VerifierOrganization, error) {
	if organizationID == 0 {
		return primaryOrganizationOf(orgs), nil
	}
	for _, org := range orgs {
		if org.ID == organizationID {
			return org, nil
		}
	}
	return domain.VerifierOrganization{}, domain.ErrOrganizationNotFound
}

// primaryOrganization reads the bot's primary organization without locking.
func primaryOrganization(ctx context.Context, db sqlcgen.DBTX, botID int64) (domain.VerifierOrganization, error) {
	org, err := scanVerifierOrganization(db.QueryRow(ctx, `
SELECT `+verifierOrganizationColumnList+`
FROM verifier_organizations
WHERE verifier_bot_id = $1
ORDER BY display_priority, id
LIMIT 1`, botID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.VerifierOrganization{}, domain.ErrVerifierNotFound
	}
	if err != nil {
		return domain.VerifierOrganization{}, fmt.Errorf("get primary verifier organization: %w", err)
	}
	return org, nil
}

// resolveOrganizationForBot resolves a request's organization: zero means the
// bot's primary organization, and a non-zero id must name an organization the
// bot hosts.
func resolveOrganizationForBot(ctx context.Context, db sqlcgen.DBTX, botID int64, organizationID int64) (domain.VerifierOrganization, error) {
	if organizationID == 0 {
		return primaryOrganization(ctx, db, botID)
	}
	org, err := scanVerifierOrganization(db.QueryRow(ctx, `
SELECT `+verifierOrganizationColumnList+`
FROM verifier_organizations
WHERE id = $1 AND verifier_bot_id = $2`, organizationID, botID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.VerifierOrganization{}, domain.ErrOrganizationNotFound
	}
	if err != nil {
		return domain.VerifierOrganization{}, fmt.Errorf("get verifier organization for bot: %w", err)
	}
	return org, nil
}

// customVerificationRow adapts the mark projection onto the domain type: the
// peer arrives as (text, bigint) rather than as a domain.Peer.
type customVerificationRow struct {
	mark     domain.CustomVerification
	peerType string
}

func (r *customVerificationRow) dest() []any {
	return []any{
		&r.mark.ID, &r.mark.OrganizationID, &r.mark.VerifierBotID, &r.peerType,
		&r.mark.Peer.ID, &r.mark.IconDocumentID, &r.mark.Description,
		&r.mark.GrantedByUserID, &r.mark.GrantedAt, &r.mark.CreatedAt,
		&r.mark.UpdatedAt, &r.mark.Version,
	}
}

func (r *customVerificationRow) value() domain.CustomVerification {
	mark := r.mark
	mark.Peer.Type = domain.PeerType(r.peerType)
	mark.GrantedAt = mark.GrantedAt.UTC()
	mark.CreatedAt = mark.CreatedAt.UTC()
	mark.UpdatedAt = mark.UpdatedAt.UTC()
	return mark
}

func scanCustomVerification(row pgx.Row) (domain.CustomVerification, error) {
	var r customVerificationRow
	if err := row.Scan(r.dest()...); err != nil {
		return domain.CustomVerification{}, err
	}
	return r.value(), nil
}

// customVerificationTx reads one organization's mark on a peer, optionally
// locking it so a concurrent grant of the same pair serialises behind this one.
func customVerificationTx(ctx context.Context, db sqlcgen.DBTX, organizationID int64, peer domain.Peer, forUpdate bool) (domain.CustomVerification, error) {
	query := `
SELECT ` + customVerificationColumnList + `
FROM custom_verifications
WHERE organization_id = $1 AND peer_type = $2 AND peer_id = $3`
	if forUpdate {
		query += `
FOR UPDATE`
	}
	mark, err := scanCustomVerification(db.QueryRow(ctx, query,
		organizationID, string(peer.Type), peer.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CustomVerification{}, domain.ErrCustomVerificationNotFound
	}
	if err != nil {
		return domain.CustomVerification{}, fmt.Errorf("get custom verification: %w", err)
	}
	return mark, nil
}

func countCustomVerificationsTx(ctx context.Context, db sqlcgen.DBTX, verifierBotID int64) (int, error) {
	var count int
	if err := db.QueryRow(ctx, `
SELECT count(*) FROM custom_verifications cv
JOIN verifier_organizations o ON o.id = cv.organization_id
WHERE o.verifier_bot_id = $1`,
		verifierBotID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count custom verifications: %w", err)
	}
	return count, nil
}

// customVerificationRequestRow adapts the application projection: the enum
// columns arrive as text, approved_at / rejected_at / organization_id are
// nullable (organization_id is cleared by an organization's deletion).
type customVerificationRequestRow struct {
	req        domain.CustomVerificationRequest
	peerType   string
	status     string
	orgID      *int64
	approvedAt *time.Time
	rejectedAt *time.Time
}

func (r *customVerificationRequestRow) dest() []any {
	return []any{
		&r.req.ID, &r.req.VerifierBotID, &r.orgID, &r.req.ApplicantUserID,
		&r.peerType, &r.req.Peer.ID, &r.req.PeerTitle, &r.req.PeerUsername,
		&r.req.Reason, &r.req.RequestedDescription, &r.status, &r.req.DecidedBy,
		&r.req.DecisionReason, &r.req.InternalNote, &r.req.CorrelationID,
		&r.req.CreatedAt, &r.req.UpdatedAt, &r.approvedAt, &r.rejectedAt,
		&r.req.Version,
	}
}

func (r *customVerificationRequestRow) value() domain.CustomVerificationRequest {
	req := r.req
	if r.orgID != nil {
		req.OrganizationID = *r.orgID
	}
	req.Peer.Type = domain.PeerType(r.peerType)
	req.Status = domain.CustomVerificationRequestStatus(r.status)
	req.CreatedAt = req.CreatedAt.UTC()
	req.UpdatedAt = req.UpdatedAt.UTC()
	if r.approvedAt != nil {
		req.ApprovedAt = r.approvedAt.UTC()
	}
	if r.rejectedAt != nil {
		req.RejectedAt = r.rejectedAt.UTC()
	}
	return req
}

func scanCustomVerificationRequest(row pgx.Row) (domain.CustomVerificationRequest, error) {
	var r customVerificationRequestRow
	if err := row.Scan(r.dest()...); err != nil {
		return domain.CustomVerificationRequest{}, err
	}
	return r.value(), nil
}

// lockCustomVerificationRequestTx reads the application for mutation. FOR UPDATE
// plus the version guard on the following UPDATE is what serialises two
// reviewers: the second one blocks here and then sees the bumped version.
func lockCustomVerificationRequestTx(ctx context.Context, tx pgx.Tx, requestID int64) (domain.CustomVerificationRequest, error) {
	req, err := scanCustomVerificationRequest(tx.QueryRow(ctx, `
SELECT `+customVerificationRequestColumnList+`
FROM custom_verification_requests
WHERE id = $1
FOR UPDATE`, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestNotFound
	}
	if err != nil {
		return domain.CustomVerificationRequest{}, fmt.Errorf("lock custom verification request: %w", err)
	}
	return req, nil
}

// customVerificationDecisionNeedsApply reports whether a decision moves the mark
// and therefore needs the callback: approving grants it, revoking removes it,
// and a rejection never had one to move.
func customVerificationDecisionNeedsApply(status domain.CustomVerificationRequestStatus) bool {
	return status == domain.CustomVerificationApproved || status == domain.CustomVerificationRevoked
}

// customVerificationDecisionState projects a decision onto the stored row.
//
// The approved_at / rejected_at stamps are not free-form: 0155 pairs each with
// its status ("(status = 'approved') = (approved_at IS NOT NULL)"), so a
// revocation has to clear approved_at as it leaves the approved state. The
// application's history of having been approved lives in the status itself --
// revoked is reachable only from approved.
func customVerificationDecisionState(current domain.CustomVerificationRequest, status domain.CustomVerificationRequestStatus, decidedBy, reason, note string, now time.Time) domain.CustomVerificationRequest {
	next := current
	next.Status = status
	next.DecidedBy = decidedBy
	next.DecisionReason = reason
	next.InternalNote = note
	next.ApprovedAt = time.Time{}
	next.RejectedAt = time.Time{}
	switch status {
	case domain.CustomVerificationApproved:
		next.ApprovedAt = now
	case domain.CustomVerificationRejected:
		next.RejectedAt = now
	}
	next.Version = current.Version + 1
	if now.After(next.UpdatedAt) {
		next.UpdatedAt = now
	}
	return next
}

func normalizeCustomVerificationRequest(req domain.CustomVerificationRequest) domain.CustomVerificationRequest {
	req.PeerTitle = strings.TrimSpace(req.PeerTitle)
	req.PeerUsername = domain.NormalizeUsername(req.PeerUsername)
	req.Reason = strings.TrimSpace(req.Reason)
	req.RequestedDescription = strings.TrimSpace(req.RequestedDescription)
	req.DecidedBy = strings.TrimSpace(req.DecidedBy)
	req.DecisionReason = strings.TrimSpace(req.DecisionReason)
	req.InternalNote = strings.TrimSpace(req.InternalNote)
	req.CorrelationID = strings.TrimSpace(req.CorrelationID)
	return req
}

// botVerifierSettingsColumnsFit reports whether the verifier text fits the
// columns in bytes, which the rune-counting domain Validate cannot answer.
func botVerifierSettingsColumnsFit(settings domain.BotVerifierSettings) bool {
	return len(settings.CompanyName) <= maxVerifierCompanyBytes &&
		len(settings.DefaultDescription) <= maxVerifierDescriptionBytes &&
		len(settings.GrantReason) <= maxVerifierGrantReasonBytes
}

// validateCustomVerificationRequestColumns guards the octet_length CHECKs on
// custom_verification_requests, so an over-long snapshot is a domain error
// rather than a constraint violation from the driver.
func validateCustomVerificationRequestColumns(req domain.CustomVerificationRequest) error {
	if len(req.PeerTitle) > maxCustomVerificationTitleBytes ||
		len(req.PeerUsername) > maxCustomVerificationUsernameBytes ||
		len(req.Reason) > maxCustomVerificationReasonBytes ||
		len(req.RequestedDescription) > maxCustomVerificationInputBytes ||
		len(req.DecidedBy) > maxCustomVerificationDecidedByBytes ||
		len(req.DecisionReason) > maxCustomVerificationDecisionBytes ||
		len(req.InternalNote) > maxCustomVerificationNoteBytes ||
		len(req.CorrelationID) > maxCustomVerificationCorrelationBytes {
		return domain.ErrCustomVerificationRequestInvalid
	}
	return nil
}

// botVerificationTimeArg keeps a zero time out of a NOT NULL-paired column: the
// schema wants NULL, and pgx would otherwise write year 1.
func botVerificationTimeArg(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at.UTC()
}

// botVerificationPeerArrays turns the batch input into the two parallel arrays
// the single projection query unnests, dropping unverifiable peers and
// duplicates so one peer cannot cost two rows.
func botVerificationPeerArrays(peers []domain.Peer) ([]string, []int64) {
	types := make([]string, 0, len(peers))
	ids := make([]int64, 0, len(peers))
	seen := make(map[domain.Peer]struct{}, len(peers))
	for _, peer := range peers {
		if !validBotVerificationPeer(peer) {
			continue
		}
		if _, dup := seen[peer]; dup {
			continue
		}
		seen[peer] = struct{}{}
		types = append(types, string(peer.Type))
		ids = append(ids, peer.ID)
	}
	return types, ids
}

// validBotVerificationPeer mirrors the peer_type CHECK: only users and channels
// carry a third-party mark.
func validBotVerificationPeer(peer domain.Peer) bool {
	return botVerificationPeerType(peer.Type) && peer.ID > 0
}

func botVerificationPeerType(peerType domain.PeerType) bool {
	return peerType == domain.PeerTypeUser || peerType == domain.PeerTypeChannel
}

func botVerificationLimit(limit int) int {
	if limit <= 0 {
		return defaultBotVerificationListLimit
	}
	if limit > maxBotVerificationListLimit {
		return maxBotVerificationListLimit
	}
	return limit
}

// parseBotVerificationQuery splits an admin search term into its two shapes: a
// number addresses a row id or a peer id, anything else is text. Telegram
// usernames never start with a digit, so the two shapes cannot collide.
func parseBotVerificationQuery(query string) (numeric int64, isNumeric bool, text string) {
	query = strings.TrimSpace(query)
	query = strings.TrimPrefix(query, "@")
	query = strings.TrimSpace(query)
	if query == "" {
		return 0, false, ""
	}
	if id, err := strconv.ParseInt(query, 10, 64); err == nil && id > 0 {
		return id, true, ""
	}
	return 0, false, strings.ToLower(query)
}
