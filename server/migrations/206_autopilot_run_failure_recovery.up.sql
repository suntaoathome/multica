ALTER TABLE autopilot_run
    ADD COLUMN failure_class TEXT,
    ADD COLUMN recovery_action TEXT;
