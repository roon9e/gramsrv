-- Collapse organizations back into the single per-bot settings row of 0155.
-- Marks collapse to the per-peer winner that 0208's projection would show, and
-- the settings table is rebuilt from each bot's primary organization.

-- Primary organization per bot: lowest display_priority, tie-break newest row.
INSERT INTO public.bot_verifier_settings (
    bot_id, icon_document_id, company_name, default_description,
    can_modify_custom_description, enabled, granted_by, grant_reason,
    created_at, updated_at, version
)
SELECT verifier_bot_id, icon_document_id, company_name, default_description,
       can_modify_custom_description, enabled, granted_by, grant_reason,
       created_at, updated_at, version
FROM (
    SELECT DISTINCT ON (verifier_bot_id)
        verifier_bot_id, icon_document_id, company_name, default_description,
        can_modify_custom_description, enabled, granted_by, grant_reason,
        created_at, updated_at, version
    FROM public.verifier_organizations
    ORDER BY verifier_bot_id, display_priority, id
) primary_orgs;

-- Drop every mark that would not render, so restoring the peer-once constraint
-- cannot collide with the history 0208 kept.
DELETE FROM public.custom_verifications cv
WHERE NOT EXISTS (
    SELECT 1
    FROM (
        SELECT DISTINCT ON (peer_type, peer_id) id
        FROM public.custom_verifications
        ORDER BY peer_type, peer_id,
                 (SELECT display_priority FROM public.verifier_organizations
                  WHERE verifier_organizations.id = custom_verifications.organization_id),
                 granted_at DESC, id DESC
    ) winners
    WHERE winners.id = cv.id
);

ALTER TABLE public.custom_verifications
    DROP CONSTRAINT custom_verifications_org_peer_once,
    DROP COLUMN organization_id,
    DROP COLUMN granted_at,
    ADD CONSTRAINT custom_verifications_verifier_bot_id_fkey
        FOREIGN KEY (verifier_bot_id) REFERENCES public.bot_verifier_settings(bot_id)
        ON DELETE CASCADE,
    ADD CONSTRAINT custom_verifications_peer_once UNIQUE (peer_type, peer_id);

DROP INDEX public.custom_verifications_org_idx;
CREATE INDEX custom_verifications_verifier_idx
    ON public.custom_verifications (verifier_bot_id, id DESC);

ALTER TABLE public.custom_verification_requests
    DROP COLUMN organization_id;

DROP INDEX public.custom_verification_requests_pending_idx;
CREATE UNIQUE INDEX custom_verification_requests_pending_idx
    ON public.custom_verification_requests (verifier_bot_id, peer_type, peer_id)
    WHERE status = 'pending';

DROP TABLE IF EXISTS public.verifier_organizations;