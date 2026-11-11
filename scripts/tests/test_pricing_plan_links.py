from html.parser import HTMLParser
from pathlib import Path
import runpy
import unittest
from urllib.parse import parse_qs, urlsplit


ROOT = Path(__file__).resolve().parents[2]


class PlanLinks(HTMLParser):
    def __init__(self):
        super().__init__()
        self.links = {}
        self.anchors = {}
        self.plan = None

    def handle_starttag(self, tag, attributes):
        attrs = dict(attributes)
        if tag == "article" and "data-plan" in attrs:
            self.plan = attrs["data-plan"]
            self.anchors[self.plan] = attrs.get("id")
        if tag == "a" and attrs.get("class") == "plan-cta":
            self.links[self.plan] = attrs["href"]


class PricingPlanLinkTests(unittest.TestCase):
    def test_generated_plan_links_preserve_selection_without_checkout_side_effects(self):
        generator = runpy.run_path(str(ROOT / "scripts/generate_commercial_truth.py"))
        contract = generator["load_contract"](ROOT)
        rendered = generator["render_plan_rows"](contract)
        parser = PlanLinks()
        parser.feed(rendered)
        self.assertNotIn("verified capabilities", rendered)
        self.assertEqual(set(parser.links), {plan["id"] for plan in contract["plans"]})
        for plan in contract["plans"]:
            plan_id = plan["id"]
            self.assertEqual(parser.anchors[plan_id], "plan-" + plan_id)
            target = urlsplit(parser.links[plan_id])
            if not plan["paid"]:
                self.assertEqual(target.path, "installation.html")
            elif plan["pricing"]["custom"]:
                self.assertEqual(target.scheme, "mailto")
                self.assertEqual(target.path, "hello@contextlattice.io")
            else:
                self.assertEqual(target.scheme, "https")
                self.assertEqual(target.netloc, "app.contextlattice.io")
                self.assertEqual(target.path, "/billing")
                self.assertEqual(parse_qs(target.query), {"plan": [plan_id]})


if __name__ == "__main__":
    unittest.main()
