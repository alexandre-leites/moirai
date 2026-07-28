import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

from moirai.config import ConfigurationError, OrchestratorConfig, read_bind, read_secret


class ConfigurationTests(unittest.TestCase):
    def test_configuration_accepts_a_direct_database_url(self) -> None:
        config = OrchestratorConfig.from_environment({"LOOP_DATABASE_URL": "postgresql://loop:secret@db/loop"})

        self.assertEqual(config.database_url, "postgresql://loop:secret@db/loop")
        self.assertEqual(config.grpc_bind, "0.0.0.0:50051")
        self.assertIsNone(config.github_token)

    def test_configuration_reads_github_token(self) -> None:
        config = OrchestratorConfig.from_environment(
            {
                "LOOP_DATABASE_URL": "postgresql://loop:secret@db/loop",
                "LOOP_GITHUB_TOKEN": "github-token",
            }
        )

        self.assertEqual(config.github_token, "github-token")

    def test_configuration_reads_trimmed_secret_file(self) -> None:
        with TemporaryDirectory() as directory:
            secret_file = Path(directory) / "database_url"
            secret_file.write_text("postgresql://loop:secret@db/loop\n", encoding="utf-8")

            config = OrchestratorConfig.from_environment(
                {"LOOP_DATABASE_URL_FILE": str(secret_file), "LOOP_GRPC_BIND": "[::1]:50052"}
            )

        self.assertEqual(config.database_url, "postgresql://loop:secret@db/loop")
        self.assertEqual(config.grpc_bind, "[::1]:50052")

    def test_secret_configuration_rejects_ambiguous_empty_and_missing_values(self) -> None:
        with self.assertRaises(ConfigurationError):
            read_secret({"LOOP_DATABASE_URL": "one", "LOOP_DATABASE_URL_FILE": "/tmp/two"}, "LOOP_DATABASE_URL")
        with self.assertRaises(ConfigurationError):
            read_secret({"LOOP_DATABASE_URL": " \n "}, "LOOP_DATABASE_URL")
        with self.assertRaises(ConfigurationError):
            read_secret({}, "LOOP_DATABASE_URL")

    def test_secret_file_must_be_a_readable_regular_file_with_a_value(self) -> None:
        with TemporaryDirectory() as directory:
            path = Path(directory)
            with self.assertRaises(ConfigurationError):
                read_secret({"LOOP_DATABASE_URL_FILE": str(path)}, "LOOP_DATABASE_URL")
            empty_file = path / "empty"
            empty_file.write_text("\n", encoding="utf-8")
            with self.assertRaises(ConfigurationError):
                read_secret({"LOOP_DATABASE_URL_FILE": str(empty_file)}, "LOOP_DATABASE_URL")

    def test_bind_validation_rejects_malformed_or_unsafe_endpoints(self) -> None:
        for bind in ("", "localhost", "localhost:0", "localhost:65536", "localhost:port", "::1:50051"):
            with self.subTest(bind=bind), self.assertRaises(ConfigurationError):
                read_bind(bind)


if __name__ == "__main__":
    unittest.main()
