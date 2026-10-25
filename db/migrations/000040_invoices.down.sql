ALTER TABLE amenity_bookings DROP COLUMN IF EXISTS invoice_id;
DROP TABLE IF EXISTS invoice_reminders;
DROP TABLE IF EXISTS invoice_counters;
DROP TABLE IF EXISTS invoice_lines;
DROP TABLE IF EXISTS invoices;
