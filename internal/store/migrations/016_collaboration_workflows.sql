-- Context is acknowledged only after the existing history/accounting/process
-- finalization gate. A new participant/provider thread starts from zero.
ALTER TABLE collaboration_participants ADD COLUMN context_seq INTEGER NOT NULL DEFAULT 0 CHECK(context_seq >= 0);
ALTER TABLE collaboration_participants ADD COLUMN context_provider TEXT NOT NULL DEFAULT '';

-- Server-derived evidence is immutable for one snapshot/configuration. Review
-- follow-up questions and handoff repair reuse the exact same evidence.
CREATE TABLE collaboration_review_evidence (
    run_id TEXT NOT NULL REFERENCES collaborations(id) ON DELETE CASCADE,
    snapshot_id TEXT NOT NULL,
    config_version INTEGER NOT NULL,
    evidence_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(run_id,snapshot_id,config_version)
);
