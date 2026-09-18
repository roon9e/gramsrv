package store

import (
	"context"

	"telesrv/internal/domain"
)

// BotVerificationStore owns third-party verification: the icon catalogue, the
// verifier organizations, the granted marks and the application queue in front
// of them.
//
// A bot is a verifier through the organizations it hosts (verifier_organizations,
// replacing the old one-row-per-bot settings). The wire model carries exactly one
// BotVerification per peer, so every projection resolves a deterministic winner:
// the organization with the lowest display_priority, then the most recently
// granted mark. Losers stay on disk and surface when the winner is revoked.
//
// Reads on the projection path (PeerVerification / PeerVerificationBatch) are on
// every peer serialisation, so they must be cheap.
type BotVerificationStore interface {
	// --- icon catalogue ---

	// UpsertVerificationIcon adds or updates a catalogue entry by document id.
	UpsertVerificationIcon(ctx context.Context, icon domain.VerificationIcon) (domain.VerificationIcon, error)
	// SetVerificationIconActive retires or restores an entry. Marks already granted
	// with it keep rendering: the icon id is denormalised onto the mark.
	SetVerificationIconActive(ctx context.Context, iconID int64, active bool) (domain.VerificationIcon, error)
	// VerificationIcon reads one entry by id.
	VerificationIcon(ctx context.Context, iconID int64) (domain.VerificationIcon, error)
	// VerificationIconByDocument reads one entry by its custom emoji document id.
	VerificationIconByDocument(ctx context.Context, documentID int64) (domain.VerificationIcon, error)
	// ListVerificationIcons lists the catalogue, newest first.
	ListVerificationIcons(ctx context.Context, activeOnly bool, limit int) ([]domain.VerificationIcon, error)

	// --- verifier status ---

	// The legacy bot-level methods are kept under the same names: each bot's
	// "settings" is synthesised from its PRIMARY organization (lowest
	// display_priority). A bot without organizations is not a verifier.
	//
	// UpsertBotVerifierSettings grants or updates verifier status through the
	// bot's primary organization. Optimistic locking on the stored version keeps
	// two operators from clobbering each other. A bot with no organization
	// receives one at domain.DefaultVerifierDisplayPriority.
	UpsertBotVerifierSettings(ctx context.Context, settings domain.BotVerifierSettings) (domain.BotVerifierSettings, error)
	// SetBotVerifierEnabled flips the operator kill switch on every organization
	// of the bot. Existing marks stay, but the verifier can grant nothing new and
	// its settings stop being projected.
	SetBotVerifierEnabled(ctx context.Context, botID int64, enabled bool) (domain.BotVerifierSettings, error)
	// DeleteBotVerifierSettings removes verifier status by deleting every
	// organization of the bot; the organizations' marks cascade away with them,
	// because a mark whose verifier no longer exists has nothing to render.
	DeleteBotVerifierSettings(ctx context.Context, botID int64) (bool, error)
	// BotVerifierSettings reads one verifier's status (primary organization),
	// enabled or not.
	BotVerifierSettings(ctx context.Context, botID int64) (domain.BotVerifierSettings, error)
	// BotVerifierSettingsBatch resolves several bots in one round trip for the
	// botInfo projection; bots without verifier status are absent from the map.
	BotVerifierSettingsBatch(ctx context.Context, botIDs []int64) (map[int64]domain.BotVerifierSettings, error)
	// ListBotVerifiers lists verifier bots (their primary organization) for the
	// admin panel.
	ListBotVerifiers(ctx context.Context, enabledOnly bool, limit int) ([]domain.BotVerifierSettings, error)

	// --- organizations ---

	// UpsertVerifierOrganization creates an organization (ID == 0) or updates one
	// with optimistic locking on its version. Its verifier_bot_id is immutable
	// once set.
	UpsertVerifierOrganization(ctx context.Context, org domain.VerifierOrganization) (domain.VerifierOrganization, error)
	// SetVerifierOrganizationEnabled flips one organization's enable bit.
	SetVerifierOrganizationEnabled(ctx context.Context, organizationID int64, enabled bool) (domain.VerifierOrganization, error)
	// DeleteVerifierOrganization removes one organization: its marks cascade away,
	// and the organization of its applications is cleared so review history stays.
	DeleteVerifierOrganization(ctx context.Context, organizationID int64) (bool, error)
	// VerifierOrganization reads one organization by id.
	VerifierOrganization(ctx context.Context, organizationID int64) (domain.VerifierOrganization, error)
	// VerifierOrganizationsByBot lists one bot's organizations, ranked by
	// display_priority ascending. When enabledOnly, disabled organizations are
	// filtered out.
	VerifierOrganizationsByBot(ctx context.Context, verifierBotID int64, enabledOnly bool) ([]domain.VerifierOrganization, error)
	// ListVerifierOrganizations is the admin catalogue, grouped by verifier bot.
	ListVerifierOrganizations(ctx context.Context, enabledOnly bool, limit int) ([]domain.VerifierOrganization, error)

	// --- granted marks ---

	// GrantCustomVerification creates or updates the peer's mark within one
	// organization (domain.CustomVerification.OrganizationID). A zero
	// OrganizationID is resolved to the verifier's primary organization. The icon
	// and description are taken from the organization's settings at grant time and
	// the caller has already resolved the description through
	// organization.DescriptionFor. Marks of different organizations coexist; the
	// projection picks the winner.
	GrantCustomVerification(ctx context.Context, mark domain.CustomVerification) (domain.CustomVerification, bool, error)
	// RevokeCustomVerification removes the verifier's WINNING mark from the peer
	// (the one the projection would show) and reports whether anything was
	// removed, so a repeated revoke is a no-op. When the verifier hosts multiple
	// organizations, the winner's mark is the one removed, exposing the next
	// organization's mark.
	RevokeCustomVerification(ctx context.Context, verifierBotID int64, peer domain.Peer) (bool, error)
	// RevokeOrganizationMark removes one organization's mark on a peer, whether it
	// is the projection winner or not.
	RevokeOrganizationMark(ctx context.Context, organizationID int64, peer domain.Peer) (bool, error)
	// CustomVerification reads the verifier's winning mark on a peer.
	CustomVerification(ctx context.Context, verifierBotID int64, peer domain.Peer) (domain.CustomVerification, error)
	// OrganizationCustomVerification reads one organization's mark on a peer,
	// winner or not.
	OrganizationCustomVerification(ctx context.Context, organizationID int64, peer domain.Peer) (domain.CustomVerification, error)
	// PeerVerification returns the peer's single visible mark: the winner of the
	// enabled organizations' tie. Missing marks report
	// domain.ErrCustomVerificationNotFound.
	PeerVerification(ctx context.Context, peer domain.Peer) (domain.CustomVerification, error)
	// PeerVerificationBatch resolves the projection for many peers at once. This is
	// the call on the hot serialisation path, so peers without a mark are simply
	// absent instead of erroring.
	PeerVerificationBatch(ctx context.Context, peers []domain.Peer) (map[domain.Peer]domain.CustomVerification, error)
	// CountCustomVerifications reports how many peers a verifier bot has marked
	// across all its organizations, for the per-verifier bound.
	CountCustomVerifications(ctx context.Context, verifierBotID int64) (int, error)
	// ListCustomVerifications is the admin listing query with keyset paging.
	ListCustomVerifications(ctx context.Context, filter domain.CustomVerificationFilter) ([]domain.CustomVerification, error)

	// --- application queue ---

	// CreateCustomVerificationRequest files an application, resolving a zero
	// OrganizationID to the verifier's primary organization. A pending application
	// on the same (organization, peer) reports domain.ErrCustomVerificationRequestExists.
	CreateCustomVerificationRequest(ctx context.Context, req domain.CustomVerificationRequest) (domain.CustomVerificationRequest, error)
	// DecideCustomVerificationRequest moves an application through its status
	// machine. approve=true grants the mark in the same transaction through the
	// supplied callback, so an approved application can never exist without its
	// mark; revoke removes it the same way.
	DecideCustomVerificationRequest(ctx context.Context, requestID int64, version int64, status domain.CustomVerificationRequestStatus, decidedBy, reason, note string, apply func(ctx context.Context, req domain.CustomVerificationRequest) error) (domain.CustomVerificationRequest, bool, error)
	// CustomVerificationRequest reads one application.
	CustomVerificationRequest(ctx context.Context, requestID int64) (domain.CustomVerificationRequest, error)
	// PendingCustomVerificationRequest returns the live application for a
	// (verifier, peer) pair, if any.
	PendingCustomVerificationRequest(ctx context.Context, verifierBotID int64, peer domain.Peer) (domain.CustomVerificationRequest, error)
	// ListCustomVerificationRequests is the review-queue query with keyset paging.
	ListCustomVerificationRequests(ctx context.Context, filter domain.CustomVerificationRequestFilter) ([]domain.CustomVerificationRequest, error)
	// CustomVerificationRequestsForApplicant returns an applicant's own history for
	// the verifier bot's /status command.
	CustomVerificationRequestsForApplicant(ctx context.Context, applicantUserID int64, limit int) ([]domain.CustomVerificationRequest, error)
	// CustomVerificationRequestCounts is the queue summary by status.
	CustomVerificationRequestCounts(ctx context.Context) (map[domain.CustomVerificationRequestStatus]int64, error)
}
