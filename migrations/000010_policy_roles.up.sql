-- Policy-centric access model (ADR-0010): an agent's access is decided by its
-- governing policy's permitted roles. policy_roles maps a policy to the roles
-- whose members may see and run agents governed by that policy. An agent with
-- no governing policy is generally accessible to any member holding agents.run;
-- an all_agents role still sees every agent.
--
-- This supersedes the role_agents / entitlements access mechanisms. Those
-- tables are RETAINED (not dropped) here so the admin UI and roles API keep
-- compiling and running; they are deprecated and consulted by nothing for
-- access. A later cleanup migration removes them once their UI usage is gone.
BEGIN;

CREATE TABLE policy_roles (
    policy_id UUID NOT NULL REFERENCES policies (id) ON DELETE CASCADE,
    role_id   UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    PRIMARY KEY (policy_id, role_id)
);

-- Reverse lookup: "which policies permit this role" — the access query joins
-- from a member's reachable roles to the policies (and thus agents) they may
-- see (ListPoliciesForRoles / ListAccessibleAgentIDs).
CREATE INDEX policy_roles_role_id_idx ON policy_roles (role_id);

COMMIT;
