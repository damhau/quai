-- The application role the server and the worker connect as (deploy/compose/quai.yaml): quai
-- migrate, connected as the schema's owner (quai), grants it what they need, the audit log to
-- read and append only (docs/design/09-security-and-compliance.md#audit-log).
CREATE ROLE quai_app LOGIN;
