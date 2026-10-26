import copy
import unittest

from supervised_runtime_release import release_plan


class ReleasePlanTests(unittest.TestCase):
    def setUp(self):
        self.env = {
            "ENVIRONMENT": "preview",
            "REPOSITORY_URLS": '{"orchestrator":"347282156621.dkr.ecr.eu-central-1.amazonaws.com/stagewise-e2b-preview-e2b-orchestrator"}',
            "BUNDLE_DIGEST": "sha256:" + "1" * 64,
            "SUPERVISOR_DIGEST": "sha256:" + "2" * 64,
            "BUNDLE_COMMIT": "3" * 40,
            "SUPERVISOR_COMMIT": "4" * 40,
            "ASSEMBLY_COMMIT": "5" * 40,
            "BUNDLE_VERSION": "0.16.1+klex." + "3" * 12,
        }

    def test_deterministic_identity(self):
        first = release_plan(self.env)
        self.assertEqual(first, release_plan(copy.deepcopy(self.env)))
        self.assertIn(":supervised-", first["image"])
        self.assertEqual(first["runtimeCommit"], self.env["BUNDLE_COMMIT"])
        self.assertTrue(first["bundleImage"].endswith("@" + self.env["BUNDLE_DIGEST"]))

    def test_every_input_is_part_of_identity(self):
        first = release_plan(self.env)["image"]
        for name in (
            "BUNDLE_DIGEST",
            "SUPERVISOR_DIGEST",
            "SUPERVISOR_COMMIT",
            "ASSEMBLY_COMMIT",
        ):
            with self.subTest(name=name):
                env = {**self.env, name: self.env[name][:-1] + "6"}
                self.assertNotEqual(first, release_plan(env)["image"])
        env = {
            **self.env,
            "BUNDLE_COMMIT": "6" * 40,
            "BUNDLE_VERSION": "0.16.1+klex." + "6" * 12,
        }
        self.assertNotEqual(first, release_plan(env)["image"])
        env = {**self.env, "BUNDLE_VERSION": "0.16.2+klex." + "3" * 12}
        self.assertNotEqual(first, release_plan(env)["image"])

    def test_production_is_cell_local(self):
        env = {
            **self.env,
            "ENVIRONMENT": "production",
            "REPOSITORY_URLS": self.env["REPOSITORY_URLS"]
            .replace("347282156621", "925213028165")
            .replace("preview", "production"),
        }
        plan = release_plan(env)
        self.assertEqual(plan["cell"], "production-machines")
        self.assertIn("925213028165", plan["supervisorImage"])
        self.assertNotEqual(plan["image"], release_plan(self.env)["image"])

    def test_invalid_inputs_fail_closed(self):
        cases = [
            ("ENVIRONMENT", "unknown"),
            ("ENVIRONMENT", "production"),
            ("BUNDLE_VERSION", "0.16.1+klex.wrong"),
            ("REPOSITORY_URLS", '{"orchestrator":"evil.example/input"}'),
        ]
        for name in ("BUNDLE_DIGEST", "SUPERVISOR_DIGEST"):
            cases.extend(
                (name, value)
                for value in (
                    "latest",
                    "sha256:" + "0" * 64,
                    "sha256:" + "g" * 64,
                    "sha256:" + "1" * 63,
                )
            )
        for name in ("BUNDLE_COMMIT", "SUPERVISOR_COMMIT", "ASSEMBLY_COMMIT"):
            cases.extend((name, value) for value in ("main", "0" * 40, "1" * 39))
        for name, value in cases:
            with self.subTest(name=name, value=value):
                with self.assertRaises(ValueError):
                    release_plan({**self.env, name: value})


if __name__ == "__main__":
    unittest.main()
