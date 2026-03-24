import unittest

from app.services.reverse.utils.auth_probe import classify_probe


class AuthProbeTests(unittest.TestCase):
    def test_classify_probe_marks_empty_401_as_expired_token(self):
        is_token_expired, is_cloudflare = classify_probe(
            401,
            "text/plain",
            "cloudflare",
            "",
        )

        self.assertTrue(is_token_expired)
        self.assertFalse(is_cloudflare)

    def test_classify_probe_marks_challenge_as_cloudflare(self):
        is_token_expired, is_cloudflare = classify_probe(
            403,
            "text/html",
            "cloudflare",
            "<html>challenge-platform</html>",
        )

        self.assertFalse(is_token_expired)
        self.assertTrue(is_cloudflare)


if __name__ == "__main__":
    unittest.main()
