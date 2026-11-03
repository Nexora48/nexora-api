-- App PIN: after the first OTP sign-in a resident sets a 4-6 digit PIN for that phone. Opening the app later asks for the
-- PIN (or the phone's fingerprint / face) instead of a new OTP. One PIN per user per device; wrong tries are counted here.
CREATE TABLE user_pins (
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id    text NOT NULL,
    pin_hash     text NOT NULL,
    failed       int  NOT NULL DEFAULT 0,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, device_id)
);
