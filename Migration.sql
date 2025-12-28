-- 1. Main Inventory Table
CREATE TABLE IF NOT EXISTS vm_inventory (
    cluster_name TEXT NOT NULL,
    vm_name      TEXT NOT NULL,
    namespace    TEXT NOT NULL,
    status       TEXT,
    node_name    TEXT,
    ip_address   TEXT,
    cpu_cores    INTEGER,
    memory_gb    TEXT,
    os_distro    TEXT,
    annotations  JSONB,
    last_seen    TIMESTAMP DEFAULT NOW(),
    PRIMARY KEY (cluster_name, vm_name, namespace)
);

-- 2. Disks Table (with Storage Class and Phase)
CREATE TABLE IF NOT EXISTS vm_disks (
    cluster_name  TEXT NOT NULL,
    vm_name       TEXT NOT NULL,
    namespace     TEXT NOT NULL,
    disk_name     TEXT NOT NULL,
    claim_name    TEXT,
    volume_type   TEXT,
    storage_class TEXT,
    capacity_gb   TEXT,
    volume_phase  TEXT,
    -- Cascading delete: if the VM is deleted from inventory, its disks are removed automatically
    FOREIGN KEY (cluster_name, vm_name, namespace) 
        REFERENCES vm_inventory(cluster_name, vm_name, namespace) ON DELETE CASCADE
);

-- 3. History Table (Audit Trail)
CREATE TABLE IF NOT EXISTS vm_history (
    id           SERIAL PRIMARY KEY,
    cluster_name TEXT NOT NULL,
    vm_name      TEXT NOT NULL,
    namespace    TEXT NOT NULL,
    status       TEXT,
    node_name    TEXT,
    action       TEXT NOT NULL, -- 'SYNC', 'DELETE'
    changed_at   TIMESTAMP DEFAULT NOW()
);

-- Index for the Deduplication logic used in the controller
CREATE INDEX IF NOT EXISTS idx_vm_history_lookup 
ON vm_history (cluster_name, vm_name, namespace, changed_at DESC);
