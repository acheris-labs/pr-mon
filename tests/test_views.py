import unittest
from datetime import UTC, datetime

from pr_mon.views import format_time

NOW = datetime(2026, 9, 16, 12, 0, 0, tzinfo=UTC)


def local(iso):
    return datetime.fromisoformat(iso).astimezone().strftime("%Y-%m-%d %H:%M")


class FormatTimeTest(unittest.TestCase):
    def check(self, iso, age):
        self.assertEqual(format_time(iso, NOW), f"{local(iso)} ({age})")

    def test_seconds(self):
        self.check("2026-09-16T11:59:30Z", "just now")

    def test_minutes(self):
        self.check("2026-09-16T11:55:00Z", "5m ago")

    def test_hours(self):
        self.check("2026-09-16T09:10:00Z", "2h ago")

    def test_days(self):
        self.check("2026-09-13T12:00:00Z", "3d ago")

    def test_future_clock_skew(self):
        self.check("2026-09-16T12:00:05Z", "just now")


if __name__ == "__main__":
    unittest.main()
