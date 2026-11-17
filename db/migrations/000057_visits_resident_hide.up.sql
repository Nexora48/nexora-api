-- A resident can remove a finished visit from their history. The row stays: the office and the gate keep their log.
ALTER TABLE visits ADD COLUMN hidden_by_resident_at timestamptz;
