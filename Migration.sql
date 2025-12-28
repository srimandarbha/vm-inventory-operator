CREATE TABLE IF NOT EXISTS vm_history (
    id SERIAL PRIMARY KEY,
    vm_name TEXT NOT NULL,
    namespace TEXT NOT NULL,
    cluster_name TEXT NOT NULL,
    action TEXT NOT NULL, -- e.g., 'CREATE', 'UPDATE', 'DELETE'
    annotations JSONB,
    changed_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS vm_inventory (
    id SERIAL PRIMARY KEY,
    vm_name TEXT NOT NULL,
    namespace TEXT NOT NULL,
    cluster_name TEXT NOT NULL,
    annotations JSONB, -- JSONB is faster for searching/updating than plain JSON
    last_synced TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    -- This constraint is critical for the "ON CONFLICT" logic to work
    UNIQUE (vm_name, namespace)
);
