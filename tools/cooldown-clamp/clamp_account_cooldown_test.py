#!/usr/bin/env python3
import sqlite3
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path
import tempfile

from clamp_account_cooldown import clamp_rows, parse_duration, parse_ts


class ParseTsTest(unittest.TestCase):
    def test_nanosecond_sqlite_timestamp(self) -> None:
        parsed = parse_ts("2026-08-21 04:54:14.526856759+00:00")
        self.assertIsNotNone(parsed)
        assert parsed is not None
        self.assertEqual(parsed, datetime(2026, 8, 21, 4, 54, 14, 526856, tzinfo=timezone.utc))

    def test_rfc3339_z(self) -> None:
        parsed = parse_ts("2026-08-21T04:54:14Z")
        self.assertEqual(parsed, datetime(2026, 8, 21, 4, 54, 14, tzinfo=timezone.utc))


class ClampRowsTest(unittest.TestCase):
    def test_caps_future_cooldown(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            db = Path(tmp) / "backend.db"
            conn = sqlite3.connect(db)
            conn.execute(
                "CREATE TABLE provider_accounts (id INTEGER PRIMARY KEY, cooldown_until TEXT, failure_count INTEGER, last_error TEXT, updated_at TEXT)"
            )
            conn.execute(
                "INSERT INTO provider_accounts VALUES (699, ?, 1, 'upstream status 504', '')",
                ("2026-08-21 04:54:14.526856759+00:00",),
            )
            conn.commit()
            now = datetime(2026, 8, 20, 5, 0, 0, tzinfo=timezone.utc)
            cap = now + timedelta(minutes=1)
            ids = clamp_rows(conn, cap, now)
            self.assertEqual(ids, [699])
            row = conn.execute("SELECT cooldown_until, failure_count, last_error FROM provider_accounts WHERE id=699").fetchone()
            self.assertEqual(row[1], 0)
            self.assertEqual(row[2], "")
            self.assertEqual(parse_ts(row[0]), cap)
            conn.close()


class DurationTest(unittest.TestCase):
    def test_parse_duration(self) -> None:
        self.assertEqual(parse_duration("1m"), timedelta(minutes=1))
        self.assertEqual(parse_duration("60s"), timedelta(seconds=60))


if __name__ == "__main__":
    unittest.main()
