-- Identity: teams, members, and the many-to-many between them. Members are the
-- human principals (mapped from an upstream IdP subject); teams group members
-- for entitlement and budget scoping.
BEGIN;

-- Coarse authorization role carried on the member record.
CREATE TYPE member_role AS ENUM ('admin', 'member', 'auditor');

CREATE TABLE teams (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

CREATE INDEX teams_org_id_idx ON teams (org_id);

CREATE TABLE members (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    -- Subject claim from the upstream identity provider; unique within an org.
    idp_subject  TEXT NOT NULL,
    email        TEXT NOT NULL,
    display_name TEXT,
    role         member_role NOT NULL DEFAULT 'member',
    active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, idp_subject)
);

CREATE INDEX members_org_id_idx ON members (org_id);

-- Composite-PK join table linking members to teams.
CREATE TABLE team_memberships (
    team_id   UUID NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    member_id UUID NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    added_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, member_id)
);

CREATE INDEX team_memberships_member_id_idx ON team_memberships (member_id);

COMMIT;
