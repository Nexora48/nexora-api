ALTER TABLE society_subscriptions ADD COLUMN plan_id uuid REFERENCES plans(id);
UPDATE society_subscriptions SET plan_id = (SELECT id FROM plans WHERE key='legacy');
