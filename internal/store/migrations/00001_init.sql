-- +goose Up
-- Conventions:
--   * Primary keys are TEXT ids from store.NewID ("m_01j...").
--   * Timestamps are INTEGER unix milliseconds (UTC); NULL means "not yet".
--     Use store.Millis and store.FromMillis to convert.
--   * Booleans are INTEGER 0/1 with a NOT NULL DEFAULT.
--   * JSON documents are stored as TEXT and default to '[]' or '{}' so
--     readers never see NULL, except where NULL carries meaning (command).
--   * States are stored as the same string the Go constant and the HTTP API
--     use.

-- models: what can be deployed. name is the public model id clients send
-- in "model"; the gateway reserves "auto", so the owner rejects it.
-- Owner: deploy.
CREATE TABLE models (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    reference      TEXT NOT NULL,                 -- OCI ModelPack reference
    engine_image   TEXT NOT NULL,
    engine_args    TEXT NOT NULL DEFAULT '[]',    -- JSON array appended to the default vLLM argv
    command        TEXT,                          -- JSON array argv template; NULL means the default vLLM argv
    container_port INTEGER NOT NULL DEFAULT 8000,
    memory_mb      INTEGER NOT NULL,
    vgpus          INTEGER NOT NULL DEFAULT 0,
    auto           INTEGER NOT NULL DEFAULT 1,    -- eligible for model "auto"
    labels         TEXT NOT NULL DEFAULT '{}',    -- JSON object, read by scheduler strategies
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

-- deployments: one engine of one model pinned to one node. There is no ON
-- DELETE CASCADE on purpose: a model can only be deleted once its
-- deployments have been undeployed from Oakestra and removed.
-- UNIQUE(node_ip, host_port) is the port allocator: the owner picks the
-- lowest free port of OOA_ENGINE_PORT_RANGE for the node IP and inserts in
-- the same transaction, so two deployments can never share a host port
-- (nodes may share an IP, as with the fake).
-- Owner: deploy.
CREATE TABLE deployments (
    id           TEXT PRIMARY KEY,
    model_id     TEXT NOT NULL REFERENCES models (id),
    cluster_name TEXT NOT NULL,
    node_name    TEXT NOT NULL,
    node_ip      TEXT NOT NULL,
    host_port    INTEGER NOT NULL,
    service_name TEXT NOT NULL,                -- deterministic Oakestra microservice name
    service_id   TEXT NOT NULL DEFAULT '',     -- '' until the service exists in Oakestra
    engine_key   TEXT NOT NULL,                -- per-deployment --api-key of the engine
    state        TEXT NOT NULL,
    error        TEXT NOT NULL DEFAULT '',
    state_since  INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    UNIQUE (model_id, cluster_name, node_name),
    UNIQUE (node_ip, host_port)
);
CREATE INDEX deployments_state ON deployments (state);

-- api_keys: keys for /v1. Only the sha256 hash is kept; prefix is the
-- first characters of the secret, shown in the UI to tell keys apart.
-- Owner: auth.
CREATE TABLE api_keys (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    prefix       TEXT NOT NULL,
    hash         TEXT NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER
);

-- settings: small runtime configuration that survives restarts. Keys are
-- listed in internal/store/settings.go; values are JSON.
-- Owner: store (generic get/put helpers).
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- +goose Down
DROP TABLE settings;
DROP TABLE api_keys;
DROP TABLE deployments;
DROP TABLE models;
