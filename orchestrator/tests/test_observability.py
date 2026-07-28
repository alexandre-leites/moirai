from __future__ import annotations

import io
import json
import logging
import unittest

from moirai.config import ConfigurationError, OrchestratorConfig
from moirai.observability import JSONFormatter


class ObservabilityTests(unittest.TestCase):
    def test_json_formatter_retains_extra_fields(self) -> None:
        stream = io.StringIO()
        handler = logging.StreamHandler(stream)
        handler.setFormatter(JSONFormatter())
        logger = logging.getLogger("moirai.test.observability")
        logger.setLevel(logging.INFO)
        logger.handlers = [handler]
        logger.propagate = False
        logger.info("runner connected", extra={"runner_id": "runner-1", "request_id": "request-1"})
        payload = json.loads(stream.getvalue())
        self.assertEqual(payload["message"], "runner connected")
        self.assertEqual(payload["level"], "INFO")
        self.assertEqual(payload["runner_id"], "runner-1")
        self.assertEqual(payload["request_id"], "request-1")
        self.assertIn("timestamp", payload)

    def test_tls_configuration_requires_certificate_and_key_together(self) -> None:
        with self.assertRaises(ConfigurationError):
            OrchestratorConfig.from_environment(
                {"LOOP_DATABASE_URL": "postgresql://localhost/db", "LOOP_GRPC_TLS_CERT_FILE": "/cert.pem"}
            )

    def test_tls_configuration_accepts_mutual_tls(self) -> None:
        config = OrchestratorConfig.from_environment(
            {
                "LOOP_DATABASE_URL": "postgresql://localhost/db",
                "LOOP_GRPC_TLS_CERT_FILE": "/cert.pem",
                "LOOP_GRPC_TLS_KEY_FILE": "/key.pem",
                "LOOP_GRPC_TLS_CLIENT_CA_FILE": "/ca.pem",
            }
        )
        self.assertEqual(config.grpc_tls_client_ca_file, "/ca.pem")
        self.assertEqual(config.metrics_bind, "0.0.0.0:9090")
