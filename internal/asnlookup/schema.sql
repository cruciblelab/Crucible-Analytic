-- Schema for the optional ASN/country lookup module. Only needed if you
-- set asn_lookup.enabled = true in config.toml - apply this once,
-- manually, the same way and for the same reason as
-- internal/storage/schema.sql: the collector itself never runs DDL.
--
-- Plain relational table, NOT a TimescaleDB hypertable: IP ranges have no
-- time dimension to partition on (unlike traffic_snapshots).
--
-- start_addr/end_addr are INET, not BIGINT: this table holds both IPv4
-- and IPv6 ranges (BIGINT can't hold a 128-bit IPv6 address), matching
-- how the rest of this project already stores addresses (see
-- internal/storage/schema.sql's traffic_snapshots.ip) and how pgx already
-- encodes/decodes netip.Addr <-> inet without any extra code.
--
-- BREAKING CHANGE from the original (IPv4-only, BIGINT) version of this
-- table: if you applied that version already, drop and recreate it -
-- this feature is disabled by default and was only added recently, so
-- there's no expected production data to migrate.
CREATE TABLE IF NOT EXISTS ip_country_ranges (
    start_addr INET NOT NULL,
    end_addr   INET NOT NULL,
    country    TEXT NOT NULL,
    PRIMARY KEY (start_addr, end_addr)
);

-- Supports the range lookup ("largest start_addr <= this IP, then verify
-- end_addr covers it") that any external reader querying this table
-- directly - e.g. a future dashboard - would use. The collector's own
-- Resolver does not query this table per lookup; it keeps an in-memory
-- copy rebuilt on every refresh (see asnlookup.go) specifically so a
-- per-request hot path never depends on a database round trip.
CREATE INDEX IF NOT EXISTS idx_ip_country_ranges_start ON ip_country_ranges (start_addr);
