-- New product service: Parking Management.
-- Off by default and in no plan yet, so no society or plan changes until SuperAdmin turns it on
-- (Plans -> Edit plan -> Included services, or a society's Services tab).
INSERT INTO features (key, name, description, default_enabled)
VALUES ('parking', 'Parking Management', 'Resident and visitor parking slots', false)
ON CONFLICT (key) DO NOTHING;
