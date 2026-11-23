-- An unconfirmed claim also prevents resending after a process restart.
CREATE TABLE IF NOT EXISTS monthly_usage_email_deliveries (
    report_month date NOT NULL CHECK (extract(day FROM report_month) = 1),
    recipient_hash bytea NOT NULL CHECK (octet_length(recipient_hash) = 32),
    claimed_at timestamptz NOT NULL DEFAULT now(),
    accepted_at timestamptz,
    PRIMARY KEY (report_month, recipient_hash)
);

-- Manual sends have their own idempotency keys and never consume scheduled receipts.
CREATE TABLE IF NOT EXISTS monthly_usage_manual_jobs (
    request_id uuid PRIMARY KEY,
    report_month date NOT NULL CHECK (extract(day FROM report_month) = 1),
    status text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'sent', 'failed', 'uncertain')),
    total integer NOT NULL CHECK (total >= 0),
    accepted integer NOT NULL DEFAULT 0 CHECK (accepted >= 0),
    failed integer NOT NULL DEFAULT 0 CHECK (failed >= 0),
    uncertain integer NOT NULL DEFAULT 0 CHECK (uncertain >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    touched_at timestamptz NOT NULL DEFAULT now(),
    CHECK (accepted::bigint + failed + uncertain <= total),
    CHECK (status = 'running' OR accepted::bigint + failed + uncertain = total)
);

CREATE UNIQUE INDEX IF NOT EXISTS monthly_usage_manual_jobs_one_running
    ON monthly_usage_manual_jobs (status) WHERE status = 'running';
