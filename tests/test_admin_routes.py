import unittest
from asyncio import run

from app.products.web import router as web_router


class AdminRouteCompatibilityTests(unittest.TestCase):
    def _find_route(self, path: str):
        routes = [
            route
            for route in web_router.routes
            if getattr(route, "path", None) == path
        ]
        self.assertEqual(len(routes), 1, f"expected exactly one route for {path}")
        return routes[0]

    def test_legacy_webui_entrypoints_redirect_to_admin(self):
        expected = {
            "/webui": "/admin",
            "/webui/login": "/admin/login",
            "/webui/images": "/admin/images",
        }
        for path, location in expected.items():
            with self.subTest(path=path):
                route = self._find_route(path)
                response = run(route.endpoint())
                self.assertEqual(response.status_code, 307)
                self.assertEqual(response.headers["location"], location)

    def test_admin_images_page_serves_restored_generation_ui(self):
        route = self._find_route("/admin/images")
        response = run(route.endpoint())
        html = response.body.decode()
        self.assertIn("id=\"modeSelect\"", html)
        self.assertIn("id=\"resultHistory\"", html)
        self.assertIn("id=\"clearHistoryBtn\"", html)
        self.assertIn("下载/复制与重试", html)
        self.assertIn("webui-images-prompt", html)

    def test_admin_account_page_has_visible_images_entry(self):
        route = self._find_route("/admin/account")
        response = run(route.endpoint())
        html = response.body.decode()
        self.assertIn("href=\"/admin/images\"", html)
        self.assertIn("进入生图页", html)

    def test_admin_api_router_is_mounted_before_compat_pages(self):
        admin_api_verify_index = next(
            index
            for index, route in enumerate(web_router.routes)
            if getattr(route, "path", None) == "/admin/api/verify"
        )
        images_index = next(
            index
            for index, route in enumerate(web_router.routes)
            if getattr(route, "path", None) == "/admin/images"
        )
        self.assertLess(admin_api_verify_index, images_index)


if __name__ == "__main__":
    unittest.main()
