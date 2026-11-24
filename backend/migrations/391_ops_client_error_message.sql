-- Public response message, kept separate from administrative evidence.
ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS client_error_message TEXT;
