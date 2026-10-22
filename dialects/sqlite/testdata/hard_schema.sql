-- A schema with the SQLite features that copy tools tend to get wrong, and
-- values at the edges of each storage class. All data is made up.
PRAGMA foreign_keys = ON;

CREATE TABLE "Accounts" (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL COLLATE NOCASE,
  plan TEXT NOT NULL DEFAULT 'free' CHECK (plan IN ('free', 'pro')),
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- The foreign key names the parent in another case, and the self-reference
-- names no columns, which means the primary key.
CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  account_id INTEGER NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  email VARCHAR(255) NOT NULL UNIQUE,
  referred_by INTEGER REFERENCES users,
  balance DECIMAL(12, 2),
  active BOOLEAN NOT NULL DEFAULT 1,
  born DATE,
  wakes_at TIME,
  external_id UUID,
  settings JSON,
  avatar BLOB,
  email_domain TEXT GENERATED ALWAYS AS (substr(email, instr(email, '@') + 1)) VIRTUAL,
  email_upper TEXT GENERATED ALWAYS AS (upper(email)) STORED
);

-- A reserved word, a space, mixed case and a non-ASCII name; a composite key.
CREATE TABLE "order" (
  "Order Id" INTEGER NOT NULL,
  line INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  "naïve" TEXT,
  PRIMARY KEY ("Order Id", line),
  FOREIGN KEY (user_id) REFERENCES users (id)
) WITHOUT ROWID;

CREATE TABLE order_notes (
  order_id INTEGER NOT NULL,
  line INTEGER NOT NULL,
  note TEXT,
  FOREIGN KEY (order_id, line) REFERENCES "order" ("Order Id", line)
);

CREATE TABLE measurements (
  id INTEGER PRIMARY KEY,
  reading REAL,
  raw ANY,
  tag TEXT
) STRICT;

CREATE TABLE untyped (a, b, c);

CREATE TABLE cyc_a (id INTEGER PRIMARY KEY, b_id INTEGER REFERENCES cyc_b (id));
CREATE TABLE cyc_b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES cyc_a (id));

CREATE TABLE audit (user_id INTEGER);

CREATE INDEX users_active_email ON users (email) WHERE active = 1;
CREATE INDEX users_lower_email ON users (lower(email));
CREATE UNIQUE INDEX accounts_name ON "Accounts" (name);

INSERT INTO "Accounts" (id, name, plan, created_at) VALUES
  (1, 'Acme', 'pro', '2026-01-02 03:04:05'),
  (2, 'Globex', 'free', '2026-02-03 04:05:06.500'),
  (3, 'Initech', 'free', '2026-03-04 05:06:07');
-- sqlite_sequence now says 3; the copy only needs to be past its own rows.
DELETE FROM "Accounts" WHERE id = 3;

INSERT INTO users (id, account_id, email, referred_by, balance, active, born, wakes_at, external_id, settings, avatar) VALUES
  (1, 1, 'ada@example.com', NULL, 12.5, 1, '1990-05-17', '07:30:00', '123e4567-e89b-12d3-a456-426614174000', '{"theme":"dark"}', x''),
  (2, 1, 'grace@example.com', 1, 100, 1, '19/10/1985', '24:00:00', '123E4567-E89B-12D3-A456-426614174001', NULL, zeroblob(100000)),
  (3, 2, 'linus@example.org', 1, 'n/a', 0, 0, '13:45:00.123456789', x'00112233445566778899aabbccddeeff', '[1,2,3]', x'00ff00'),
  (4, 2, 'naïve☃@example.net', NULL, -0.01, 1, '-0043-03-15', NULL, NULL, '{}', NULL),
  (5, 2, CAST(x'6100620040' AS TEXT) || 'example.com', 3, 1e20, 1, '2026-10-19', NULL, NULL, NULL, NULL);

INSERT INTO "order" ("Order Id", line, user_id, "naïve") VALUES
  (10, 1, 1, 'first'), (10, 2, 1, NULL), (11, 1, 3, 'ünïcödé');
INSERT INTO order_notes (order_id, line, note) VALUES (10, 1, 'gift'), (10, 2, NULL), (11, 1, '');

INSERT INTO measurements (id, reading, raw, tag) VALUES
  (1, 1.5, 9223372036854775807, 'int max'),
  (2, -9223372036854775808, -9223372036854775808, 'int min'),
  (3, 1.7976931348623157e308, 'text', 'max real'),
  (4, 4.9406564584124654e-324, x'deadbeef', 'smallest real'),
  (5, 1e999, 2.5, 'infinity'),
  (6, -1e999, NULL, 'minus infinity'),
  (7, 0.1, 0.30000000000000004, 'float edges');

INSERT INTO untyped (a, b, c) VALUES
  (1, 'x', x'00'), (1.0, CAST(x'fffe' AS TEXT), NULL), ('3', 3, 3.0), (NULL, '', x'');

INSERT INTO cyc_a (id, b_id) VALUES (1, NULL);
INSERT INTO cyc_b (id, a_id) VALUES (1, 1);
UPDATE cyc_a SET b_id = 1 WHERE id = 1;

-- Created after the data, as a trigger would be in a real database. If it
-- fired while the copy loads, audit would get extra rows.
CREATE TRIGGER users_audit AFTER INSERT ON users BEGIN
  INSERT INTO audit (user_id) VALUES (NEW.id);
END;

CREATE VIEW active_users AS SELECT id, email FROM users WHERE active = 1;
CREATE VIEW active_user_count AS SELECT count(*) AS n FROM active_users;
