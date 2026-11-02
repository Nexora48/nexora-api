-- society_features rows for this key go with it (ON DELETE CASCADE)
UPDATE plans SET features = array_remove(features, 'parking');
DELETE FROM features WHERE key = 'parking';
