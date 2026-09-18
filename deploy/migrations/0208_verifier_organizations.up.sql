-- Multiple organizations under one verifier bot.
--
-- 0155 made the BOT the unit of a third-party verifier: one row in
-- bot_verifier_settings and, because the wire carries exactly one
-- botVerification per peer, exactly one visible mark per peer. This migration
-- makes the ORGANIZATION the unit of identity while keeping the wire model:
-- the built-in @verifierbot (1250000013) can host several organizations, each
-- with its own icon, company name, description permission and enable bit, and a
-- peer still renders exactly one mark -- the winner.
--
-- bot_verifier_settings is replaced by verifier_organizations. A bot is a
-- verifier iff it hosts an organization; the bot's PRIMARY organization (the
-- one with the lowest display_priority, then the most recently granted) is what
-- the botInfo.verifier_settings block and the single wire mark project. Marks
-- that lose the tie stay on disk as history and surface unchanged when the
-- winner is revoked, which is the one behaviour the old one-row model could not
-- express.

-- The organization catalogue. One row is one verifiable company fronted by a
-- bot account; a bot's own identity (its profile, botInfo) is unchanged.
CREATE TABLE public.verifier_organizations (
    id bigserial PRIMARY KEY,
    -- The bot account that fronts this organization. A bot with no
    -- organizations is not a verifier.
    verifier_bot_id bigint NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    company_name text NOT NULL CHECK (octet_length(company_name) BETWEEN 1 AND 512),
    icon_document_id bigint NOT NULL CHECK (icon_document_id > 0),
    default_description text NOT NULL DEFAULT '' CHECK (octet_length(default_description) <= 512),
    -- can_modify_custom_description mirrors the TL flag of 0155: when false the
    -- organization may only apply default_description, so a per-peer description
    -- cannot be smuggled past the operator.
    can_modify_custom_description boolean NOT NULL DEFAULT false,
    enabled boolean NOT NULL DEFAULT true,
    -- display_priority ranks the organizations of one bot for the single wire
    -- slot: lower wins. Ties fall back to most recently granted, then newest row.
    display_priority integer NOT NULL DEFAULT 100 CHECK (display_priority > 0),
    granted_by text NOT NULL DEFAULT '' CHECK (octet_length(granted_by) <= 128),
    grant_reason text NOT NULL DEFAULT '' CHECK (octet_length(grant_reason) <= 4096),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    CHECK (updated_at >= created_at)
);

CREATE INDEX verifier_organizations_bot_idx
    ON public.verifier_organizations (verifier_bot_id, id);

CREATE INDEX verifier_organizations_bot_priority_idx
    ON public.verifier_organizations (verifier_bot_id, enabled, display_priority, id);

-- Every verifier bot becomes its primary organization, so nothing already
-- granted changes meaning: same company, icon, description permission, kill
-- switch and grant audit, and the same version history.
INSERT INTO public.verifier_organizations (
    verifier_bot_id, company_name, icon_document_id, default_description,
    can_modify_custom_description, enabled, granted_by, grant_reason,
    created_at, updated_at, version
)
SELECT bot_id, company_name, icon_document_id, default_description,
       can_modify_custom_description, enabled, granted_by, grant_reason,
       created_at, updated_at, version
FROM public.bot_verifier_settings;

-- Marks become organization-keyed. verifier_bot_id stays as a denormalised wire
-- issuer: an organization never moves between bots (there is no reassignment
-- path), so the copy cannot drift. The NEW uniqueness is per organization --
-- several organizations may mark one peer -- and the visible winner is picked by
-- the projection, not by squeezing out the loser at write time.
ALTER TABLE public.custom_verifications
    DROP CONSTRAINT custom_verifications_peer_once,
    DROP CONSTRAINT custom_verifications_verifier_bot_id_fkey,
    ADD COLUMN organization_id bigint REFERENCES public.verifier_organizations(id)
        ON DELETE CASCADE,
    ADD COLUMN granted_at timestamptz;

UPDATE public.custom_verifications cv
SET organization_id = vo.id,
    granted_at = cv.created_at
FROM public.verifier_organizations vo
WHERE vo.verifier_bot_id = cv.verifier_bot_id;

ALTER TABLE public.custom_verifications
    ALTER COLUMN organization_id SET NOT NULL,
    ALTER COLUMN granted_at SET NOT NULL,
    ADD CONSTRAINT custom_verifications_org_peer_once
        UNIQUE (organization_id, peer_type, peer_id);

DROP INDEX public.custom_verifications_verifier_idx;
CREATE INDEX custom_verifications_org_idx
    ON public.custom_verifications (organization_id, id DESC);

-- Applications reference the organization they were filed with. Historical rows
-- survive an organization's removal the way they survived 0155's: the
-- verifier_bot_id reference to users stays, and organization_id is cleared
-- rather than cascading the history away.
ALTER TABLE public.custom_verification_requests
    ADD COLUMN organization_id bigint REFERENCES public.verifier_organizations(id)
        ON DELETE SET NULL;

UPDATE public.custom_verification_requests r
SET organization_id = vo.id
FROM public.verifier_organizations vo
WHERE vo.verifier_bot_id = r.verifier_bot_id;

-- One live application per organization, the same single-occupancy idea 0155's
-- per-verifier index encoded. organization_id is non-null for every live row:
-- it is only nulled by an organization's own deletion, which also withdraws the
-- decision queue those applications sat in.
DROP INDEX public.custom_verification_requests_pending_idx;
CREATE UNIQUE INDEX custom_verification_requests_pending_idx
    ON public.custom_verification_requests (organization_id, peer_type, peer_id)
    WHERE status = 'pending' AND organization_id IS NOT NULL;

-- The bot-level status table is subsumed by the organizations: a bot is a
-- verifier iff it hosts (enabled) organizations.
DROP TABLE public.bot_verifier_settings;