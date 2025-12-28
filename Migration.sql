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

##########################

-- 1. Main Inventory Table: Represents the current "Source of Truth"
-- Each VM is uniquely identified by the combination of Cluster, Namespace, and Name.
CREATE TABLE IF NOT EXISTS vm_inventory (
    cluster_name TEXT NOT NULL,
    vm_name      TEXT NOT NULL,
    namespace    TEXT NOT NULL,
    status       TEXT,           -- PrintableStatus (Running, Stopped, Migrating)
    node_name    TEXT,           -- The OpenShift Node hosting the VM
    ip_address   TEXT,           -- Primary IP from Guest Agent/VMI
    cpu_cores    INTEGER,        -- CPU Cores requested
    memory_gb    TEXT,           -- Memory string (e.g., "4Gi")
    os_distro    TEXT,           -- Operating System label
    annotations  JSONB,          -- Metadata for custom filtering
    last_seen    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    
    PRIMARY KEY (cluster_name, vm_name, namespace)
);

-- 2. Disks Table: Tracks one-to-many relationship (VM can have multiple disks)
-- Uses a Foreign Key to the main table with CASCADE DELETE.
CREATE TABLE IF NOT EXISTS vm_disks (
    id           SERIAL PRIMARY KEY,
    cluster_name TEXT NOT NULL,
    vm_name      TEXT NOT NULL,
    namespace    TEXT NOT NULL,
    disk_name    TEXT NOT NULL,  -- Name within the VM spec
    claim_name   TEXT,           -- The actual PVC or DataVolume name
    volume_type  TEXT,           -- (pvc, containerDisk, dataVolume, hostDisk)
    last_updated TIMESTAMP DEFAULT CURRENT_TIMESTAMP,

    CONSTRAINT fk_vm 
        FOREIGN KEY (cluster_name, vm_name, namespace) 
        REFERENCES vm_inventory(cluster_name, vm_name, namespace) 
        ON DELETE CASCADE
);

-- 3. History Table: The Audit Log for every state change
-- Unlike inventory, this has no primary key on VM identity because it stores duplicates over time.
CREATE TABLE IF NOT EXISTS vm_history (
    id           SERIAL PRIMARY KEY,
    cluster_name TEXT NOT NULL,
    vm_name      TEXT NOT NULL,
    namespace    TEXT NOT NULL,
    status       TEXT,
    node_name    TEXT,
    action       TEXT NOT NULL,  -- (CREATE, UPDATE, DELETE, SYNC)
    changed_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- 4. Performance Indexes
-- These ensure that looking up disks or history for a specific VM is instantaneous.
CREATE INDEX IF NOT EXISTS idx_vm_disks_lookup ON vm_disks (cluster_name, vm_name, namespace);
CREATE INDEX IF NOT EXISTS idx_vm_history_lookup ON vm_history (cluster_name, vm_name, namespace);
CREATE INDEX IF NOT EXISTS idx_vm_history_time ON vm_history (changed_at);

SRE VIEW
    CREATE OR REPLACE VIEW vm_full_report AS
SELECT 
    i.cluster_name,
    i.namespace,
    i.vm_name,
    i.status,
    i.node_name,
    i.ip_address,
    i.cpu_cores,
    i.memory_gb,
    COUNT(d.id) AS total_disks,
    STRING_AGG(d.claim_name, ', ') AS pvc_list
FROM 
    vm_inventory i
LEFT JOIN 
    vm_disks d ON i.cluster_name = d.cluster_name 
               AND i.vm_name = d.vm_name 
               AND i.namespace = d.namespace
GROUP BY 
    i.cluster_name, i.namespace, i.vm_name, i.status, i.node_name, i.ip_address, i.cpu_cores, i.memory_gb;
##########################################


ALTER TABLE vm_inventory 
ADD COLUMN node_name TEXT,
ADD COLUMN status TEXT,
ADD COLUMN cpu_cores INTEGER,
ADD COLUMN memory_gb TEXT,
ADD COLUMN os_distro TEXT,
ADD COLUMN ip_address TEXT;

-- Do the same for history if you want to track changes in status/resources
ALTER TABLE vm_history 
ADD COLUMN node_name TEXT,
ADD COLUMN status TEXT,
ADD COLUMN ip_address TEXT;
