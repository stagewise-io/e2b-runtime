import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("select-runtime-source.sh").resolve()
WORKFLOW = SCRIPT.parent.parent / ".github/workflows/klex-publish-orchestrator.yml"


class RuntimeSourceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.git("init", "-q")
        self.git("config", "user.email", "test@example.invalid")
        self.git("config", "user.name", "Test")
        self.git("commit", "--allow-empty", "-qm", "accepted runtime")
        self.accepted = self.git("rev-parse", "HEAD")
        self.git("commit", "--allow-empty", "-qm", "publisher")
        self.workflow = self.git("rev-parse", "HEAD")

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.root, text=True).strip()

    def select(self, source, **overrides):
        env = {
            **os.environ,
            "SOURCE_COMMIT": source,
            "EVENT_NAME": "workflow_dispatch",
            "WORKFLOW_REF": "refs/heads/klex/main",
            "WORKFLOW_COMMIT": self.workflow,
            **overrides,
        }
        return subprocess.run(["bash", str(SCRIPT)], cwd=self.root, env=env,
                              text=True, capture_output=True)

    def test_default_preserves_current_source(self):
        self.assertEqual(self.select("").returncode, 0)
        self.assertEqual(self.git("rev-parse", "HEAD"), self.workflow)

    def test_merged_source_is_selected_exactly(self):
        result = self.select(self.accepted)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.git("rev-parse", "HEAD"), self.accepted)

    def test_rejects_mutable_short_zero_and_shell_inputs(self):
        for source in ["main", self.accepted[:12], "0" * 40, "$(touch injected)", "-HEAD"]:
            with self.subTest(source=source):
                self.assertNotEqual(self.select(source).returncode, 0)
                self.assertEqual(self.git("rev-parse", "HEAD"), self.workflow)
        self.assertFalse((self.root / "injected").exists())

    def test_rejects_other_event_or_branch(self):
        for overrides in [{"EVENT_NAME": "pull_request"}, {"EVENT_NAME": "push"},
                          {"WORKFLOW_REF": "refs/heads/unreviewed"}]:
            self.assertNotEqual(self.select(self.accepted, **overrides).returncode, 0)
            self.assertEqual(self.git("rev-parse", "HEAD"), self.workflow)

    def test_rejects_unmerged_source(self):
        self.git("checkout", "--detach", self.accepted)
        self.git("commit", "--allow-empty", "-qm", "unmerged source")
        unmerged = self.git("rev-parse", "HEAD")
        self.git("checkout", "--detach", self.workflow)
        self.assertNotEqual(self.select(unmerged).returncode, 0)
        self.assertEqual(self.git("rev-parse", "HEAD"), self.workflow)

    def test_rejects_missing_commit(self):
        self.assertNotEqual(self.select("f" * 40).returncode, 0)

    def test_workflow_uses_selected_source_for_both_builds(self):
        workflow = WORKFLOW.read_text()
        self.assertIn("ref: ${{ needs.build.outputs.commit }}", workflow)
        self.assertIn("COMMIT_SHA=${{ steps.meta.outputs.commit }}", workflow)
        self.assertIn("COMMIT_SHA=${{ needs.build.outputs.commit }}", workflow)
        self.assertIn("EXPECTED_COMMIT: ${{ steps.meta.outputs.commit }}", workflow)
        self.assertNotIn("COMMIT_SHA=${{ github.sha }}", workflow)


if __name__ == "__main__":
    unittest.main()
