-- Schema for the optional ASN/country lookup module. Only needed if you
-- set asn_lookup.enabled = true in config.toml - apply this once,
-- manually, the same way and for the same reason as
-- internal/storage/schema.sql: the collector itself never runs DDL.
--
-- Plain relational table, NOT a TimescaleDB hypertable: IP ranges have no
-- time dimension to partition on (unlike traffic_snapshots).
CREATE TABLE IF NOT EXISTS ip_country_ranges (
    start_addr BIGINT NOT NULL,
    end_addr   BIGINT NOT NULL,
    country    TEXT   NOT NULL,
    PRIMARY KEY (start_addr, end_addr)
);

-- Supports the range lookup ("largest start_addr <= this IP, then verify
-- end_addr covers it") that any external reader querying this table
-- directly - e.g. a future dashboard - would use. The collector's own
-- Resolver does not query this table per lookup; it keeps an in-memory
-- copy rebuilt on every refresh (see asnlookup.go) specifically so a
-- per-request hot path never depends on a database round trip.
CREATE INDEX IF NOT EXISTS idx_ip_country_ranges_start ON ip_country_ranges (start_addr);
