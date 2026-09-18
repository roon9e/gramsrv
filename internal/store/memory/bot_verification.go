package memory

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"telesrv/internal/domain"
	"telesrv/internal/store"
)

// Default page sizes applied when a caller leaves the limit unset. The
// PostgreSQL queries page with LIMIT, so an unset limit has to resolve to the
// same finite page in both backends.
const (
	defaultBotVerificationListLimit = 50
	maxBotVerificationListLimit     = 200
	// The bounds below mirror the octet_length CHECKs of 0155. They are not
	// redundant with the domain Validate methods: the domain counts runes and the
	// columns count bytes, so multi-byte text can clear validation and still
	// violate a CHECK. Guarding here is what keeps this backend from accepting
	// payloads the PostgreSQL one would reject.
	maxVerificationIconNameBytes          = 512
	maxVerifierCompanyBytes               = 512
	maxVerifierDescriptionBytes           = 280
	maxVerifierGrantReasonBytes           = 4096
	maxCustomVerificationDescriptionBytes = 4096
	maxCustomVerificationInputBytes       = 280
	maxCustomVerificationTitleBytes       = 1024
	maxCustomVerificationUsernameBytes    = 64
	maxCustomVerificationReasonBytes      = 16384
	maxCustomVerificationDecidedByBytes   = 128
	maxCustomVerificationDecisionBytes    = 4096
	maxCustomVerificationNoteBytes        = 32768
	maxCustomVerificationCorrelationBytes = 128
)

// BotVerificationStore is the in-memory implementation of
// store.BotVerificationStore. The RPC, bot and admin unit tests run against it,
// so it reproduces every invariant migrations 0155 + 0208 encode as an index, a
// CHECK or a transaction boundary, and returns the same domain errors the
// PostgreSQL backend maps its violations onto:
//
//   - verification_icons.document_id UNIQUE: the catalogue is keyed by document,
//     so a second upsert of the same document edits the entry in place.
//   - custom_verifications_org_peer_once: one mark per (organization, peer);
//     several organizations' marks on the same peer coexist, and the projection
//     picks the winner. The settings synthesis keeps 0155's bot-level view: a
//     bot's "settings" is its primary organization's.
//   - domain.MaxCustomVerificationsPerVerifier: checked per bot before a mark is
//     created, never on the update path, so an existing mark can always be
//     re-described.
//   - the projection: the peer's single visible mark is the winner among the
//     enabled organizations that mark it -- lowest display_priority, then most
//     recently granted, then newest row -- which is the operator kill switch.
//   - custom_verifications.organization_id ON DELETE CASCADE: deleting an
//     organization takes its marks with it, while applications survive as
//     history with their organization cleared.
//   - custom_verification_requests_pending_idx: one live application per
//     (organization, peer); a second one is
//     domain.ErrCustomVerificationRequestExists.
//   - the approved_at / rejected_at CHECKs: each stamp is paired with its status,
//     which is why a revocation clears approved_at as it leaves that state.
//   - the decision transaction: the apply callback runs against a state snapshot,
//     so a failing callback restores the store exactly as it was and "approved
//     implies the mark exists" holds here too.
//
// Status transitions all go through
// domain.CanTransitionCustomVerificationStatus; this file never re-implements the
// machine. Like the PostgreSQL backend it does not validate icon ids against the
// catalogue and does not refuse a grant by a disabled verifier: "may this bot
// verify?" is an RPC-edge decision made from the settings this store returns.
type BotVerificationStore struct {
	mu sync.Mutex
	// decideMu serialises decisions. A decision releases mu around its apply
	// callback -- the callback grants or revokes the mark through this very
	// store, so holding mu would deadlock -- and decideMu is what keeps two
	// decisions from interleaving inside that window.
	decideMu        sync.Mutex
	nextIconID      int64
	nextOrgID       int64
	nextMarkID      int64
	nextRequestID   int64
	icons           map[int64]domain.VerificationIcon
	iconsByDocument map[int64]int64
	organizations   map[int64]domain.VerifierOrganization
	marks           map[int64]domain.CustomVerification
	marksByPeer     map[customVerificationKey]int64
	// markCounts is the per-verifier-bot mark count the bound is checked against,
	// maintained incrementally so a verifier close to
	// domain.MaxCustomVerificationsPerVerifier does not turn every grant into a
	// full scan.
	markCounts map[int64]int
	requests   map[int64]domain.CustomVerificationRequest
	// lastNow keeps the store's clock strictly increasing, so orderings that tie
	// on a timestamp are as deterministic as they are against PostgreSQL.
	lastNow time.Time
}

// customVerificationKey is keyed by the owning organization and the peer.
type customVerificationKey struct {
	organizationID int64
	peerType       domain.PeerType
	peerID         int64
}

// NewBotVerificationStore creates an empty store. Ids start at 1 so a zero ID
// keeps meaning "no row".
func NewBotVerificationStore() *BotVerificationStore {
	return &BotVerificationStore{
		nextIconID:      1,
		nextOrgID:       1,
		nextMarkID:      1,
		nextRequestID:   1,
		icons:           make(map[int64]domain.VerificationIcon),
		iconsByDocument: make(map[int64]int64),
		organizations:   make(map[int64]domain.VerifierOrganization),
		marks:           make(map[int64]domain.CustomVerification),
		marksByPeer:     make(map[customVerificationKey]int64),
		markCounts:      make(map[int64]int),
		requests:        make(map[int64]domain.CustomVerificationRequest),
	}
}

var _ store.BotVerificationStore = (*BotVerificationStore)(nil)

// ---- icon catalogue ---------------------------------------------------------

// UpsertVerificationIcon adds or updates a catalogue entry. document_id is the
// identity of an entry, not icon.ID, and Active travels with the payload:
// SetVerificationIconActive is the narrow path for flipping only that flag.
func (s *BotVerificationStore) UpsertVerificationIcon(_ context.Context, icon domain.VerificationIcon) (domain.VerificationIcon, error) {
	icon.Name = strings.TrimSpace(icon.Name)
	if err := icon.Validate(); err != nil {
		return domain.VerificationIcon{}, err
	}
	if len(icon.Name) > maxVerificationIconNameBytes {
		return domain.VerificationIcon{}, domain.ErrVerificationIconInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowLocked()
	if id, ok := s.iconsByDocument[icon.DocumentID]; ok {
		current := s.icons[id]
		current.OwnerBotID = icon.OwnerBotID
		current.Name = icon.Name
		current.Active = icon.Active
		current.UpdatedAt = laterVerificationTime(current.UpdatedAt, now)
		s.icons[id] = current
		return current, nil
	}
	stored := domain.VerificationIcon{
		ID:         s.nextIconID,
		DocumentID: icon.DocumentID,
		OwnerBotID: icon.OwnerBotID,
		Name:       icon.Name,
		Active:     icon.Active,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	s.nextIconID++
	s.icons[stored.ID] = stored
	s.iconsByDocument[stored.DocumentID] = stored.ID
	return stored, nil
}

// SetVerificationIconActive retires or restores an entry. Marks already granted
// with it keep rendering: the icon id is denormalised onto the mark.
func (s *BotVerificationStore) SetVerificationIconActive(_ context.Context, iconID int64, active bool) (domain.VerificationIcon, error) {
	if iconID <= 0 {
		return domain.VerificationIcon{}, domain.ErrVerificationIconNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	icon, ok := s.icons[iconID]
	if !ok {
		return domain.VerificationIcon{}, domain.ErrVerificationIconNotFound
	}
	icon.Active = active
	icon.UpdatedAt = laterVerificationTime(icon.UpdatedAt, s.nowLocked())
	s.icons[iconID] = icon
	return icon, nil
}

// VerificationIcon reads one entry by id.
func (s *BotVerificationStore) VerificationIcon(_ context.Context, iconID int64) (domain.VerificationIcon, error) {
	if iconID <= 0 {
		return domain.VerificationIcon{}, domain.ErrVerificationIconNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	icon, ok := s.icons[iconID]
	if !ok {
		return domain.VerificationIcon{}, domain.ErrVerificationIconNotFound
	}
	return icon, nil
}

// VerificationIconByDocument reads one entry by its custom emoji document id,
// which is how the admin edge resolves an icon a verifier already carries.
func (s *BotVerificationStore) VerificationIconByDocument(_ context.Context, documentID int64) (domain.VerificationIcon, error) {
	if documentID <= 0 {
		return domain.VerificationIcon{}, domain.ErrVerificationIconNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.iconsByDocument[documentID]
	if !ok {
		return domain.VerificationIcon{}, domain.ErrVerificationIconNotFound
	}
	return s.icons[id], nil
}

// ListVerificationIcons lists the catalogue, newest first.
func (s *BotVerificationStore) ListVerificationIcons(_ context.Context, activeOnly bool, limit int) ([]domain.VerificationIcon, error) {
	limit = botVerificationLimit(limit)
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := sortedBotVerificationIDs(s.icons, false)
	out := make([]domain.VerificationIcon, 0, limit)
	for _, id := range ids {
		if len(out) == limit {
			break
		}
		icon := s.icons[id]
		if activeOnly && !icon.Active {
			continue
		}
		out = append(out, icon)
	}
	return out, nil
}

// ---- verifier status --------------------------------------------------------

// The legacy bot-level view is synthesized from the bot's primary organization
// (lowest display_priority, then newest row). A bot with no organizations is
// not a verifier.

// UpsertBotVerifierSettings grants or updates verifier status through the bot's
// PRIMARY organization, keeping the 0155 bot-level API. settings.Version is the
// optimistic-locking expectation: 0 means "the bot has no organization yet", and
// a bot that already has one then reports
// domain.ErrCustomVerificationVersionConflict; a non-zero version must match the
// stored primary organization's. CreatedAt is never rewritten, so the grant date
// survives every later edit.
func (s *BotVerificationStore) UpsertBotVerifierSettings(_ context.Context, settings domain.BotVerifierSettings) (domain.BotVerifierSettings, error) {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowLocked()
	orgs := s.organizationsOfBotLocked(settings.BotID, false)
	if len(orgs) == 0 {
		if settings.Version != 0 {
			return domain.BotVerifierSettings{}, domain.ErrVerifierNotFound
		}
		org := domain.VerifierOrganization{
			ID:                         s.nextOrgID,
			VerifierBotID:              settings.BotID,
			IconDocumentID:             settings.IconDocumentID,
			CompanyName:                settings.CompanyName,
			DefaultDescription:         settings.DefaultDescription,
			CanModifyCustomDescription: settings.CanModifyCustomDescription,
			Enabled:                    settings.Enabled,
			DisplayPriority:            domain.DefaultVerifierDisplayPriority,
			GrantedBy:                  settings.GrantedBy,
			GrantReason:                settings.GrantReason,
			CreatedAt:                  now,
			UpdatedAt:                  now,
			Version:                    1,
		}
		s.nextOrgID++
		s.organizations[org.ID] = org
		return org.SettingsFor(), nil
	}
	primary := primaryOrganizationOfLocked(orgs)
	if settings.Version == 0 || settings.Version != primary.Version {
		return domain.BotVerifierSettings{}, domain.ErrCustomVerificationVersionConflict
	}
	updated := primary
	updated.IconDocumentID = settings.IconDocumentID
	updated.CompanyName = settings.CompanyName
	updated.DefaultDescription = settings.DefaultDescription
	updated.CanModifyCustomDescription = settings.CanModifyCustomDescription
	updated.Enabled = settings.Enabled
	updated.GrantedBy = settings.GrantedBy
	updated.GrantReason = settings.GrantReason
	updated.UpdatedAt = laterVerificationTime(primary.UpdatedAt, now)
	updated.Version = primary.Version + 1
	s.organizations[updated.ID] = updated
	return updated.SettingsFor(), nil
}

// SetBotVerifierEnabled flips the operator kill switch across every organization
// of the bot. Existing marks stay, but the verifier can grant nothing new and
// neither its settings nor its marks are projected, so flipping the switch back
// restores exactly what was there. Setting the flag to the value it already
// holds is a no-op and does not burn a version.
func (s *BotVerificationStore) SetBotVerifierEnabled(_ context.Context, botID int64, enabled bool) (domain.BotVerifierSettings, error) {
	if botID <= 0 {
		return domain.BotVerifierSettings{}, domain.ErrVerifierNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	orgs := s.organizationsOfBotLocked(botID, false)
	if len(orgs) == 0 {
		return domain.BotVerifierSettings{}, domain.ErrVerifierNotFound
	}
	unchanged := true
	for _, org := range orgs {
		if org.Enabled != enabled {
			unchanged = false
			break
		}
	}
	if unchanged {
		return primaryOrganizationOfLocked(orgs).SettingsFor(), nil
	}
	now := s.nowLocked()
	for _, org := range orgs {
		org.Enabled = enabled
		org.UpdatedAt = laterVerificationTime(org.UpdatedAt, now)
		org.Version++
		s.organizations[org.ID] = org
	}
	return primaryOrganizationOfLocked(s.organizationsOfBotLocked(botID, false)).SettingsFor(), nil
}

// DeleteBotVerifierSettings removes verifier status by deleting every
// organization of the bot. The organizations' marks cascade away with them,
// because a mark whose verifier no longer exists has nothing to render.
// Applications survive as history with their organization cleared.
func (s *BotVerificationStore) DeleteBotVerifierSettings(_ context.Context, botID int64) (bool, error) {
	if botID <= 0 {
		return false, domain.ErrVerifierNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	orgs := s.organizationsOfBotLocked(botID, false)
	if len(orgs) == 0 {
		return false, nil
	}
	for _, org := range orgs {
		s.deleteOrganizationLocked(org.ID)
	}
	return true, nil
}

// BotVerifierSettings reads one verifier's status (its primary organization),
// enabled or not: the caller needs the disabled row too, to render the kill
// switch and to explain BOT_VERIFIER_FORBIDDEN.
func (s *BotVerificationStore) BotVerifierSettings(_ context.Context, botID int64) (domain.BotVerifierSettings, error) {
	if botID <= 0 {
		return domain.BotVerifierSettings{}, domain.ErrVerifierNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	orgs := s.organizationsOfBotLocked(botID, false)
	if len(orgs) == 0 {
		return domain.BotVerifierSettings{}, domain.ErrVerifierNotFound
	}
	return primaryOrganizationOfLocked(orgs).SettingsFor(), nil
}

// BotVerifierSettingsBatch resolves several bots at once for the botInfo
// projection; bots without verifier status are absent from the map. Disabled
// verifiers are returned like any other row -- the Enabled field says so, and the
// projection edge decides -- which mirrors ListBotVerifiers taking enabledOnly as
// an explicit argument instead of assuming it.
func (s *BotVerificationStore) BotVerifierSettingsBatch(_ context.Context, botIDs []int64) (map[int64]domain.BotVerifierSettings, error) {
	out := make(map[int64]domain.BotVerifierSettings, len(botIDs))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range botIDs {
		if id <= 0 {
			continue
		}
		if orgs := s.organizationsOfBotLocked(id, false); len(orgs) > 0 {
			out[id] = primaryOrganizationOfLocked(orgs).SettingsFor()
		}
	}
	return out, nil
}

// ListBotVerifiers lists verifier bots for the admin panel, ordered by bot id,
// one entry per bot carrying its primary organization's settings. enabledOnly
// keeps bots whose PRIMARY organization is enabled.
func (s *BotVerificationStore) ListBotVerifiers(_ context.Context, enabledOnly bool, limit int) ([]domain.BotVerifierSettings, error) {
	limit = botVerificationLimit(limit)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.BotVerifierSettings, 0, limit)
	for _, botID := range sortedBotVerificationBotIDs(s.organizations) {
		if len(out) == limit {
			break
		}
		orgs := s.organizationsOfBotLocked(botID, false)
		if len(orgs) == 0 {
			continue
		}
		primary := primaryOrganizationOfLocked(orgs)
		if enabledOnly && !primary.Enabled {
			continue
		}
		out = append(out, primary.SettingsFor())
	}
	return out, nil
}

// ---- organizations ----------------------------------------------------------

// UpsertVerifierOrganization creates (ID == 0) or updates an organization.
// verifier_bot_id is immutable once set. Version follows the settings
// convention: 0 means "no such organization yet".
func (s *BotVerificationStore) UpsertVerifierOrganization(_ context.Context, org domain.VerifierOrganization) (domain.VerifierOrganization, error) {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowLocked()
	if org.ID == 0 {
		stored := org
		stored.ID = s.nextOrgID
		stored.CreatedAt = now
		stored.UpdatedAt = now
		stored.Version = 1
		s.nextOrgID++
		s.organizations[stored.ID] = stored
		return stored, nil
	}
	if org.Version == 0 {
		return domain.VerifierOrganization{}, domain.ErrCustomVerificationVersionConflict
	}
	current, ok := s.organizations[org.ID]
	if !ok {
		return domain.VerifierOrganization{}, domain.ErrOrganizationNotFound
	}
	if current.VerifierBotID != org.VerifierBotID {
		return domain.VerifierOrganization{}, domain.ErrVerifierSettingsInvalid
	}
	if current.Version != org.Version {
		return domain.VerifierOrganization{}, domain.ErrCustomVerificationVersionConflict
	}
	updated := org
	updated.VerifierBotID = current.VerifierBotID
	updated.CreatedAt = current.CreatedAt
	updated.UpdatedAt = laterVerificationTime(current.UpdatedAt, now)
	updated.Version = current.Version + 1
	s.organizations[updated.ID] = updated
	return updated, nil
}

// SetVerifierOrganizationEnabled flips one organization's enable bit. The mark
// rows stay; only the projection stops rendering them. A no-op when already set.
func (s *BotVerificationStore) SetVerifierOrganizationEnabled(_ context.Context, organizationID int64, enabled bool) (domain.VerifierOrganization, error) {
	if organizationID <= 0 {
		return domain.VerifierOrganization{}, domain.ErrOrganizationNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	org, ok := s.organizations[organizationID]
	if !ok {
		return domain.VerifierOrganization{}, domain.ErrOrganizationNotFound
	}
	if org.Enabled == enabled {
		return org, nil
	}
	org.Enabled = enabled
	org.UpdatedAt = laterVerificationTime(org.UpdatedAt, s.nowLocked())
	org.Version++
	s.organizations[org.ID] = org
	return org, nil
}

// DeleteVerifierOrganization removes one organization: its marks cascade away,
// and its applications keep their history with the organization cleared.
func (s *BotVerificationStore) DeleteVerifierOrganization(_ context.Context, organizationID int64) (bool, error) {
	if organizationID <= 0 {
		return false, domain.ErrOrganizationNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.organizations[organizationID]; !ok {
		return false, nil
	}
	s.deleteOrganizationLocked(organizationID)
	return true, nil
}

// VerifierOrganization reads one organization by id.
func (s *BotVerificationStore) VerifierOrganization(_ context.Context, organizationID int64) (domain.VerifierOrganization, error) {
	if organizationID <= 0 {
		return domain.VerifierOrganization{}, domain.ErrOrganizationNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	org, ok := s.organizations[organizationID]
	if !ok {
		return domain.VerifierOrganization{}, domain.ErrOrganizationNotFound
	}
	return org, nil
}

// VerifierOrganizationsByBot lists one bot's organizations ranked by
// display_priority. enabledOnly drops disabled ones.
func (s *BotVerificationStore) VerifierOrganizationsByBot(_ context.Context, verifierBotID int64, enabledOnly bool) ([]domain.VerifierOrganization, error) {
	if verifierBotID <= 0 {
		return nil, domain.ErrVerifierNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.organizationsOfBotLocked(verifierBotID, enabledOnly), nil
}

// ListVerifierOrganizations is the admin catalogue, grouped by verifier bot.
func (s *BotVerificationStore) ListVerifierOrganizations(_ context.Context, enabledOnly bool, limit int) ([]domain.VerifierOrganization, error) {
	limit = botVerificationLimit(limit)
	s.mu.Lock()
	defer s.mu.Unlock()
	orgs := make([]domain.VerifierOrganization, 0, len(s.organizations))
	for _, org := range s.organizations {
		if enabledOnly && !org.Enabled {
			continue
		}
		orgs = append(orgs, org)
	}
	sort.Slice(orgs, func(i, j int) bool {
		if orgs[i].VerifierBotID != orgs[j].VerifierBotID {
			return orgs[i].VerifierBotID < orgs[j].VerifierBotID
		}
		if orgs[i].DisplayPriority != orgs[j].DisplayPriority {
			return orgs[i].DisplayPriority < orgs[j].DisplayPriority
		}
		return orgs[i].ID < orgs[j].ID
	})
	if len(orgs) > limit {
		orgs = orgs[:limit]
	}
	return orgs, nil
}

// ---- granted marks ---------------------------------------------------------

// GrantCustomVerification creates or updates the mark a verifier root owns on a
// peer, scoped to one ORGANIZATION (mark.OrganizationID; a zero value resolves
// to the bot's primary organization). A second grant by the same organization is
// an update reported with created=false; marks of other organizations coexist and
// the projection picks the winner.
//
// mark.IconDocumentID is denormalised from the organization's settings when the
// caller leaves it unset, which is what "the icon is taken from the verifier at
// grant time" means; an explicit id is honoured.
func (s *BotVerificationStore) GrantCustomVerification(_ context.Context, mark domain.CustomVerification) (domain.CustomVerification, bool, error) {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grantLocked(mark)
}

// RevokeCustomVerification removes this verifier bot's WINNING mark from the
// peer -- the one its projection would show -- and reports whether anything was
// removed, so a repeated revoke is a no-op instead of an error.
func (s *BotVerificationStore) RevokeCustomVerification(_ context.Context, verifierBotID int64, peer domain.Peer) (bool, error) {
	if verifierBotID <= 0 || !validBotVerificationPeer(peer) {
		return false, domain.ErrCustomVerificationTargetInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokeLocked(verifierBotID, peer), nil
}

// RevokeOrganizationMark removes one organization's mark on a peer, whether or
// not it is the projection winner.
func (s *BotVerificationStore) RevokeOrganizationMark(_ context.Context, organizationID int64, peer domain.Peer) (bool, error) {
	if organizationID <= 0 || !validBotVerificationPeer(peer) {
		return false, domain.ErrCustomVerificationTargetInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := customVerificationKeyOf(organizationID, peer)
	id, found := s.marksByPeer[key]
	if !found {
		return false, nil
	}
	delete(s.marks, id)
	delete(s.marksByPeer, key)
	s.decrementMarkCountLocked(s.marks[id].VerifierBotID)
	return true, nil
}

// CustomVerification reads this verifier bot's WINNING mark on a peer, whether
// or not that verifier is currently enabled: this is the bookkeeping read, not
// the projection.
func (s *BotVerificationStore) CustomVerification(_ context.Context, verifierBotID int64, peer domain.Peer) (domain.CustomVerification, error) {
	if verifierBotID <= 0 || !validBotVerificationPeer(peer) {
		return domain.CustomVerification{}, domain.ErrCustomVerificationNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, found := s.winningMarkOfBotLocked(verifierBotID, peer)
	if !found {
		return domain.CustomVerification{}, domain.ErrCustomVerificationNotFound
	}
	return s.marks[id], nil
}

// OrganizationCustomVerification reads one organization's mark on a peer, winner
// or not.
func (s *BotVerificationStore) OrganizationCustomVerification(_ context.Context, organizationID int64, peer domain.Peer) (domain.CustomVerification, error) {
	if organizationID <= 0 || !validBotVerificationPeer(peer) {
		return domain.CustomVerification{}, domain.ErrCustomVerificationNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.marksByPeer[customVerificationKeyOf(organizationID, peer)]
	if !ok {
		return domain.CustomVerification{}, domain.ErrCustomVerificationNotFound
	}
	return s.marks[id], nil
}

// PeerVerification returns the peer's single visible mark: the winner of the tie
// between every enabled organization marking the peer.
func (s *BotVerificationStore) PeerVerification(_ context.Context, peer domain.Peer) (domain.CustomVerification, error) {
	if !validBotVerificationPeer(peer) {
		return domain.CustomVerification{}, domain.ErrCustomVerificationNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mark, found := s.projectedMarkLocked(peer)
	if !found {
		return domain.CustomVerification{}, domain.ErrCustomVerificationNotFound
	}
	return mark, nil
}

// PeerVerificationBatch resolves the projection for many peers at once. This is
// the call on the hot serialisation path, so peers without a mark are simply
// absent instead of erroring, and every peer resolves through the same
// enabled-organization winner rule PeerVerification uses.
func (s *BotVerificationStore) PeerVerificationBatch(_ context.Context, peers []domain.Peer) (map[domain.Peer]domain.CustomVerification, error) {
	out := make(map[domain.Peer]domain.CustomVerification, len(peers))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, peer := range peers {
		if !validBotVerificationPeer(peer) {
			continue
		}
		if _, done := out[peer]; done {
			continue
		}
		if mark, found := s.projectedMarkLocked(peer); found {
			out[peer] = mark
		}
	}
	return out, nil
}

// CountCustomVerifications reports how many peers a verifier bot has marked
// across all its organizations, for the per-verifier bound. Disabled verifiers
// still count their marks: the switch hides badges, it does not free quota.
func (s *BotVerificationStore) CountCustomVerifications(_ context.Context, verifierBotID int64) (int, error) {
	if verifierBotID <= 0 {
		return 0, domain.ErrCustomVerificationTargetInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.markCounts[verifierBotID], nil
}

// ListCustomVerifications is the admin listing query with keyset paging over id
// DESC (filter.BeforeID carries the last row of the previous page). Query matches
// a mark id or a peer id when it is numeric and otherwise matches the
// description case-insensitively, the only text a mark carries. filter filters by
// organization when set.
func (s *BotVerificationStore) ListCustomVerifications(_ context.Context, filter domain.CustomVerificationFilter) ([]domain.CustomVerification, error) {
	if filter.PeerType != "" && !botVerificationPeerType(filter.PeerType) {
		return nil, domain.ErrCustomVerificationTargetInvalid
	}
	limit := botVerificationLimit(filter.Limit)
	numeric, isNumeric, needle := parseBotVerificationQuery(filter.Query)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.CustomVerification, 0, limit)
	for _, id := range sortedBotVerificationIDs(s.marks, false) {
		if len(out) == limit {
			break
		}
		mark := s.marks[id]
		if filter.OrganizationID != 0 && mark.OrganizationID != filter.OrganizationID {
			continue
		}
		if filter.VerifierBotID != 0 && mark.VerifierBotID != filter.VerifierBotID {
			continue
		}
		if filter.PeerType != "" && mark.Peer.Type != filter.PeerType {
			continue
		}
		if filter.PeerID != 0 && mark.Peer.ID != filter.PeerID {
			continue
		}
		if filter.BeforeID != 0 && mark.ID >= filter.BeforeID {
			continue
		}
		if isNumeric && mark.ID != numeric && mark.Peer.ID != numeric {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(mark.Description), needle) {
			continue
		}
		out = append(out, mark)
	}
	return out, nil
}

// ---- application queue -----------------------------------------------------

// CreateCustomVerificationRequest files an application.
//
// A filed application is pending by definition, so the status is forced and any
// decision field the caller pre-filled is dropped: only
// DecideCustomVerificationRequest may write those. One live application per
// (organization, peer) is allowed, and a second one reports
// domain.ErrCustomVerificationRequestExists -- two pending rows would let two
// decisions race for one mark. A zero OrganizationID resolves to the bot's
// primary organization.
func (s *BotVerificationStore) CreateCustomVerificationRequest(_ context.Context, req domain.CustomVerificationRequest) (domain.CustomVerificationRequest, error) {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	org, err := s.resolveOrganizationLocked(req.VerifierBotID, req.OrganizationID)
	if err != nil {
		return domain.CustomVerificationRequest{}, err
	}
	req.OrganizationID = org.ID
	if _, found := s.pendingRequestLocked(org.ID, req.Peer); found {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestExists
	}
	now := s.nowLocked()
	stored := req
	stored.ID = s.nextRequestID
	stored.ApprovedAt = time.Time{}
	stored.RejectedAt = time.Time{}
	stored.CreatedAt = now
	stored.UpdatedAt = now
	stored.Version = 1
	s.nextRequestID++
	s.requests[stored.ID] = stored
	return stored, nil
}

// DecideCustomVerificationRequest moves an application through its status
// machine and keeps the mark in step with it.
//
// The whole decision is one unit: the status change, the decision metadata, the
// approved/rejected stamps and the apply callback that grants (status approved)
// or removes (status revoked) the mark. Nothing survives a callback error --
// the store is snapshotted before the callback runs and restored if it fails,
// including whatever the callback itself wrote -- which is the in-memory
// equivalent of the PostgreSQL transaction and the reason "approved without a
// mark" is not a reachable state.
//
// The version is compared first, so the loser of a race between two reviewers
// always sees domain.ErrCustomVerificationVersionConflict. A caller re-issuing a
// decision that already holds gets the request back with changed=false, no
// second stamp and no second apply -- the callback is not idempotent by
// assumption.
//
// The transition itself is domain.CanTransitionCustomVerificationStatus, never
// re-implemented here, and the resulting row is validated with
// domain.CustomVerificationRequest.Validate, which is what rejects a rejection
// with no reason (domain.ErrVerificationReasonRequired).
//
// Isolation is deliberately narrower than PostgreSQL's: decisions serialise on
// decideMu, but a concurrent reader can observe the decided row while apply is
// still running, and an unrelated write landing in that window is undone by a
// rollback. Unit tests drive this store from one goroutine; the PostgreSQL
// backend is where real isolation lives.
func (s *BotVerificationStore) DecideCustomVerificationRequest(ctx context.Context, requestID int64, version int64, status domain.CustomVerificationRequestStatus, decidedBy, reason, note string, apply func(ctx context.Context, req domain.CustomVerificationRequest) error) (domain.CustomVerificationRequest, bool, error) {
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
	s.decideMu.Lock()
	defer s.decideMu.Unlock()
	s.mu.Lock()
	current, ok := s.requests[requestID]
	if !ok {
		s.mu.Unlock()
		return domain.CustomVerificationRequest{}, false, domain.ErrCustomVerificationRequestNotFound
	}
	if current.Version != version {
		s.mu.Unlock()
		return domain.CustomVerificationRequest{}, false, domain.ErrCustomVerificationVersionConflict
	}
	if current.Status == status {
		// Already decided this way: keep the record and report that nothing moved,
		// so a retried decision cannot apply the mark twice.
		s.mu.Unlock()
		return current, false, nil
	}
	if !domain.CanTransitionCustomVerificationStatus(current.Status, status) {
		s.mu.Unlock()
		return domain.CustomVerificationRequest{}, false, domain.ErrCustomVerificationRequestInvalid
	}
	updated := customVerificationDecisionState(current, status, decidedBy, reason, note, s.nowLocked())
	if err := updated.Validate(); err != nil {
		s.mu.Unlock()
		return domain.CustomVerificationRequest{}, false, err
	}
	if err := validateCustomVerificationRequestColumns(updated); err != nil {
		s.mu.Unlock()
		return domain.CustomVerificationRequest{}, false, err
	}
	snapshot := s.snapshotLocked()
	s.requests[requestID] = updated
	// The callback grants or revokes the mark through this same store, so the
	// lock has to be released around it; decideMu still holds other decisions off.
	s.mu.Unlock()
	if customVerificationDecisionNeedsApply(status) {
		if err := apply(ctx, updated); err != nil {
			s.mu.Lock()
			s.restoreLocked(snapshot)
			s.mu.Unlock()
			return domain.CustomVerificationRequest{}, false, err
		}
	}
	return updated, true, nil
}

// CustomVerificationRequest reads one application.
func (s *BotVerificationStore) CustomVerificationRequest(_ context.Context, requestID int64) (domain.CustomVerificationRequest, error) {
	if requestID <= 0 {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.requests[requestID]
	if !ok {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestNotFound
	}
	return req, nil
}

// PendingCustomVerificationRequest returns the live application for a
// (verifier, peer) pair, resolved through the bot's primary organization. There
// is at most one, so no ordering is needed to pick it.
func (s *BotVerificationStore) PendingCustomVerificationRequest(_ context.Context, verifierBotID int64, peer domain.Peer) (domain.CustomVerificationRequest, error) {
	if verifierBotID <= 0 || !validBotVerificationPeer(peer) {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	orgs := s.organizationsOfBotLocked(verifierBotID, false)
	if len(orgs) == 0 {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestNotFound
	}
	req, found := s.pendingRequestLocked(primaryOrganizationOfLocked(orgs).ID, peer)
	if !found {
		return domain.CustomVerificationRequest{}, domain.ErrCustomVerificationRequestNotFound
	}
	return req, nil
}

// ListCustomVerificationRequests is the review-queue query with keyset paging
// over id DESC: the filter carries no timestamp cursor, and ids are monotonic, so
// id DESC is the same page order as created_at DESC without the ambiguity a mixed
// cursor would have. Query matches an application id or a peer id when it is
// numeric and otherwise prefix-matches the lowercased username snapshot.
func (s *BotVerificationStore) ListCustomVerificationRequests(_ context.Context, filter domain.CustomVerificationRequestFilter) ([]domain.CustomVerificationRequest, error) {
	for _, status := range filter.Statuses {
		if !status.Valid() {
			return nil, domain.ErrCustomVerificationRequestInvalid
		}
	}
	if filter.PeerType != "" && !botVerificationPeerType(filter.PeerType) {
		return nil, domain.ErrCustomVerificationTargetInvalid
	}
	limit := botVerificationLimit(filter.Limit)
	numeric, isNumeric, prefix := parseBotVerificationQuery(filter.Query)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.CustomVerificationRequest, 0, limit)
	for _, id := range sortedBotVerificationIDs(s.requests, false) {
		if len(out) == limit {
			break
		}
		req := s.requests[id]
		if len(filter.Statuses) > 0 && !containsCustomVerificationStatus(filter.Statuses, req.Status) {
			continue
		}
		if filter.OrganizationID != 0 && req.OrganizationID != filter.OrganizationID {
			continue
		}
		if filter.VerifierBotID != 0 && req.VerifierBotID != filter.VerifierBotID {
			continue
		}
		if filter.PeerType != "" && req.Peer.Type != filter.PeerType {
			continue
		}
		if filter.BeforeID != 0 && req.ID >= filter.BeforeID {
			continue
		}
		if isNumeric && req.ID != numeric && req.Peer.ID != numeric {
			continue
		}
		if prefix != "" && (req.PeerUsername == "" ||
			!strings.HasPrefix(strings.ToLower(req.PeerUsername), prefix)) {
			continue
		}
		out = append(out, req)
	}
	return out, nil
}

// CustomVerificationRequestsForApplicant returns an applicant's own history,
// newest first, for the verifier bot's /status command.
func (s *BotVerificationStore) CustomVerificationRequestsForApplicant(_ context.Context, applicantUserID int64, limit int) ([]domain.CustomVerificationRequest, error) {
	if applicantUserID <= 0 {
		return nil, domain.ErrCustomVerificationRequestInvalid
	}
	limit = botVerificationLimit(limit)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.CustomVerificationRequest, 0, limit)
	for _, id := range sortedBotVerificationIDs(s.requests, false) {
		if len(out) == limit {
			break
		}
		if req := s.requests[id]; req.ApplicantUserID == applicantUserID {
			out = append(out, req)
		}
	}
	return out, nil
}

// CustomVerificationRequestCounts is the queue summary by status. Statuses
// nobody is in are absent rather than zero, which a map read cannot tell apart
// anyway.
func (s *BotVerificationStore) CustomVerificationRequestCounts(_ context.Context) (map[domain.CustomVerificationRequestStatus]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[domain.CustomVerificationRequestStatus]int64, 4)
	for _, req := range s.requests {
		out[req.Status]++
	}
	return out, nil
}

// ---- locked helpers ---------------------------------------------------------

// grantLocked is the upsert both the public grant and a decision's apply
// callback end up in.
func (s *BotVerificationStore) grantLocked(mark domain.CustomVerification) (domain.CustomVerification, bool, error) {
	org, err := s.resolveOrganizationLocked(mark.VerifierBotID, mark.OrganizationID)
	if err != nil {
		return domain.CustomVerification{}, false, err
	}
	mark.OrganizationID = org.ID
	if mark.IconDocumentID <= 0 {
		mark.IconDocumentID = org.IconDocumentID
	}
	if err := mark.Validate(); err != nil {
		return domain.CustomVerification{}, false, err
	}
	now := s.nowLocked()
	key := customVerificationKeyOf(mark.OrganizationID, mark.Peer)
	if id, found := s.marksByPeer[key]; found {
		current := s.marks[id]
		current.IconDocumentID = mark.IconDocumentID
		current.Description = mark.Description
		current.GrantedByUserID = mark.GrantedByUserID
		current.UpdatedAt = laterVerificationTime(current.UpdatedAt, now)
		current.Version++
		s.marks[id] = current
		return current, false, nil
	}
	if s.markCounts[mark.VerifierBotID] >= domain.MaxCustomVerificationsPerVerifier {
		return domain.CustomVerification{}, false, domain.ErrCustomVerificationLimit
	}
	stored := mark
	stored.ID = s.nextMarkID
	stored.GrantedAt = now
	stored.CreatedAt = now
	stored.UpdatedAt = now
	stored.Version = 1
	s.nextMarkID++
	s.marks[stored.ID] = stored
	s.marksByPeer[key] = stored.ID
	s.markCounts[stored.VerifierBotID]++
	return stored, true, nil
}

// revokeLocked removes the bot's winning mark on the peer.
func (s *BotVerificationStore) revokeLocked(verifierBotID int64, peer domain.Peer) bool {
	id, found := s.winningMarkOfBotLocked(verifierBotID, peer)
	if !found {
		return false
	}
	mark := s.marks[id]
	delete(s.marks, id)
	delete(s.marksByPeer, customVerificationKeyOf(mark.OrganizationID, mark.Peer))
	s.decrementMarkCountLocked(mark.VerifierBotID)
	return true
}

// projectedMarkLocked returns the peer's single visible mark: the winner between
// the enabled organizations that mark it.
func (s *BotVerificationStore) projectedMarkLocked(peer domain.Peer) (domain.CustomVerification, bool) {
	var best domain.CustomVerification
	found := false
	for _, mark := range s.marks {
		if mark.Peer != peer {
			continue
		}
		org, ok := s.organizations[mark.OrganizationID]
		if !ok || !org.Enabled {
			continue
		}
		if !found || bvMarkBetter(mark, org, best, s.organizations[best.OrganizationID]) {
			best = mark
			found = true
		}
	}
	return best, found
}

// winningMarkOfBotLocked returns the mark this bot owns on the peer that its
// projection would show: the winner between the bot's own organizations.
func (s *BotVerificationStore) winningMarkOfBotLocked(verifierBotID int64, peer domain.Peer) (int64, bool) {
	var bestID int64
	found := false
	for _, mark := range s.marks {
		if mark.Peer != peer {
			continue
		}
		org, ok := s.organizations[mark.OrganizationID]
		if !ok || org.VerifierBotID != verifierBotID {
			continue
		}
		if !found || bvMarkBetter(mark, org, s.marks[bestID], s.organizations[s.marks[bestID].OrganizationID]) {
			bestID = mark.ID
			found = true
		}
	}
	return bestID, found
}

// bvMarkBetter is the winner tie-break shared by every projection: lower
// display_priority, then most recently granted, then newest row.
func bvMarkBetter(candidate domain.CustomVerification, candidateOrg domain.VerifierOrganization, current domain.CustomVerification, currentOrg domain.VerifierOrganization) bool {
	if candidateOrg.DisplayPriority != currentOrg.DisplayPriority {
		return candidateOrg.DisplayPriority < currentOrg.DisplayPriority
	}
	if !candidate.GrantedAt.Equal(current.GrantedAt) {
		return candidate.GrantedAt.After(current.GrantedAt)
	}
	return candidate.ID > current.ID
}

// resolveOrganizationLocked resolves an organization within a bot: zero means
// the primary, anything else must name an organization the bot hosts.
func (s *BotVerificationStore) resolveOrganizationLocked(verifierBotID, organizationID int64) (domain.VerifierOrganization, error) {
	if organizationID == 0 {
		orgs := s.organizationsOfBotLocked(verifierBotID, false)
		if len(orgs) == 0 {
			return domain.VerifierOrganization{}, domain.ErrVerifierNotFound
		}
		return primaryOrganizationOfLocked(orgs), nil
	}
	org, ok := s.organizations[organizationID]
	if !ok || org.VerifierBotID != verifierBotID {
		return domain.VerifierOrganization{}, domain.ErrOrganizationNotFound
	}
	return org, nil
}

// organizationsOfBotLocked lists a bot's organizations ranked by
// display_priority, so the first entry is always the primary.
func (s *BotVerificationStore) organizationsOfBotLocked(verifierBotID int64, enabledOnly bool) []domain.VerifierOrganization {
	orgs := make([]domain.VerifierOrganization, 0, 2)
	for _, org := range s.organizations {
		if org.VerifierBotID != verifierBotID {
			continue
		}
		if enabledOnly && !org.Enabled {
			continue
		}
		orgs = append(orgs, org)
	}
	sort.Slice(orgs, func(i, j int) bool {
		if orgs[i].DisplayPriority != orgs[j].DisplayPriority {
			return orgs[i].DisplayPriority < orgs[j].DisplayPriority
		}
		return orgs[i].ID < orgs[j].ID
	})
	return orgs
}

// primaryOrganizationOfLocked picks the primary organization of a ranked list.
func primaryOrganizationOfLocked(orgs []domain.VerifierOrganization) domain.VerifierOrganization {
	if len(orgs) == 0 {
		return domain.VerifierOrganization{}
	}
	return orgs[0]
}

// deleteOrganizationLocked removes an organization and cascades its marks,
// clearing the organization from its applications' history.
func (s *BotVerificationStore) deleteOrganizationLocked(organizationID int64) {
	if _, ok := s.organizations[organizationID]; !ok {
		return
	}
	delete(s.organizations, organizationID)
	for id, mark := range s.marks {
		if mark.OrganizationID != organizationID {
			continue
		}
		delete(s.marks, id)
		delete(s.marksByPeer, customVerificationKeyOf(organizationID, mark.Peer))
		s.decrementMarkCountLocked(mark.VerifierBotID)
	}
	for id, req := range s.requests {
		if req.OrganizationID == organizationID {
			req.OrganizationID = 0
			s.requests[id] = req
		}
	}
}

// decrementMarkCountLocked drops the per-bot mark count, removing the key when
// it reaches zero.
func (s *BotVerificationStore) decrementMarkCountLocked(botID int64) {
	if s.markCounts[botID] <= 1 {
		delete(s.markCounts, botID)
	} else {
		s.markCounts[botID]--
	}
}

func (s *BotVerificationStore) pendingRequestLocked(organizationID int64, peer domain.Peer) (domain.CustomVerificationRequest, bool) {
	for _, id := range sortedBotVerificationIDs(s.requests, true) {
		req := s.requests[id]
		if req.OrganizationID == organizationID && req.Peer == peer &&
			req.Status == domain.CustomVerificationPending {
			return req, true
		}
	}
	return domain.CustomVerificationRequest{}, false
}

// botVerificationSnapshot is the whole store, shallow-copied. Every stored value
// is a flat struct, so a map copy is a deep copy and restoring it undoes both the
// decision and whatever its callback wrote.
type botVerificationSnapshot struct {
	nextIconID      int64
	nextOrgID       int64
	nextMarkID      int64
	nextRequestID   int64
	icons           map[int64]domain.VerificationIcon
	iconsByDocument map[int64]int64
	organizations   map[int64]domain.VerifierOrganization
	marks           map[int64]domain.CustomVerification
	marksByPeer     map[customVerificationKey]int64
	markCounts      map[int64]int
	requests        map[int64]domain.CustomVerificationRequest
}

func (s *BotVerificationStore) snapshotLocked() botVerificationSnapshot {
	return botVerificationSnapshot{
		nextIconID:      s.nextIconID,
		nextOrgID:       s.nextOrgID,
		nextMarkID:      s.nextMarkID,
		nextRequestID:   s.nextRequestID,
		icons:           copyBotVerificationMap(s.icons),
		iconsByDocument: copyBotVerificationMap(s.iconsByDocument),
		organizations:   copyBotVerificationMap(s.organizations),
		marks:           copyBotVerificationMap(s.marks),
		marksByPeer:     copyBotVerificationMap(s.marksByPeer),
		markCounts:      copyBotVerificationMap(s.markCounts),
		requests:        copyBotVerificationMap(s.requests),
	}
}

func (s *BotVerificationStore) restoreLocked(snapshot botVerificationSnapshot) {
	s.nextIconID = snapshot.nextIconID
	s.nextOrgID = snapshot.nextOrgID
	s.nextMarkID = snapshot.nextMarkID
	s.nextRequestID = snapshot.nextRequestID
	s.icons = snapshot.icons
	s.iconsByDocument = snapshot.iconsByDocument
	s.organizations = snapshot.organizations
	s.marks = snapshot.marks
	s.marksByPeer = snapshot.marksByPeer
	s.markCounts = snapshot.markCounts
	s.requests = snapshot.requests
}

// nowLocked hands out strictly increasing timestamps at timestamptz resolution.
func (s *BotVerificationStore) nowLocked() time.Time {
	now := time.Now().UTC().Truncate(time.Microsecond)
	if !now.After(s.lastNow) {
		now = s.lastNow.Add(time.Microsecond)
	}
	s.lastNow = now
	return now
}

// ---- helpers ----------------------------------------------------------------

func customVerificationKeyOf(organizationID int64, peer domain.Peer) customVerificationKey {
	return customVerificationKey{
		organizationID: organizationID,
		peerType:       peer.Type,
		peerID:         peer.ID,
	}
}

// customVerificationDecisionNeedsApply reports whether a decision moves the mark
// and therefore needs the callback: approving grants it, revoking removes it,
// and a rejection never had one to move.
func customVerificationDecisionNeedsApply(status domain.CustomVerificationRequestStatus) bool {
	return status == domain.CustomVerificationApproved || status == domain.CustomVerificationRevoked
}

// customVerificationDecisionState projects a decision onto the stored row.
//
// The approved/rejected stamps are not free-form: 0155 pairs each with its
// status, so a revocation has to clear ApprovedAt as it leaves the approved
// state. The application's history of having been approved lives in the status
// itself -- revoked is reachable only from approved.
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
	next.UpdatedAt = laterVerificationTime(next.UpdatedAt, now)
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
// custom_verification_requests, so both backends reject an over-long snapshot
// the same way.
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

func containsCustomVerificationStatus(statuses []domain.CustomVerificationRequestStatus, status domain.CustomVerificationRequestStatus) bool {
	for _, item := range statuses {
		if item == status {
			return true
		}
	}
	return false
}

// sortedBotVerificationIDs orders the keys of a store map: ascending for the
// lookups that mirror a LIMIT 1 scan, descending for the listings that page
// newest first.
func sortedBotVerificationIDs[V any](items map[int64]V, ascending bool) []int64 {
	ids := make([]int64, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if ascending {
			return ids[i] < ids[j]
		}
		return ids[i] > ids[j]
	})
	return ids
}

// sortedBotVerificationBotIDs orders the distinct bots hosting organizations,
// for the per-bot legacy listings.
func sortedBotVerificationBotIDs(organizations map[int64]domain.VerifierOrganization) []int64 {
	seen := make(map[int64]struct{}, len(organizations))
	for _, org := range organizations {
		seen[org.VerifierBotID] = struct{}{}
	}
	bots := make([]int64, 0, len(seen))
	for botID := range seen {
		bots = append(bots, botID)
	}
	sort.Slice(bots, func(i, j int) bool { return bots[i] < bots[j] })
	return bots
}

func copyBotVerificationMap[K comparable, V any](items map[K]V) map[K]V {
	out := make(map[K]V, len(items))
	for key, value := range items {
		out[key] = value
	}
	return out
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
