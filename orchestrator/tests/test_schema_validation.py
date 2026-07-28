import unittest

from moirai.workflows.schema_validation import load_schema, validate


class SchemaValidationTests(unittest.TestCase):
    def test_loads_review_result_schema(self) -> None:
        schema = load_schema("review-result")
        self.assertEqual(schema["required"], ["verdict", "acceptanceCriteria", "findings"])

    def test_valid_review_result_has_no_errors(self) -> None:
        schema = load_schema("review-result")
        errors = validate(
            {"verdict": "approved", "acceptanceCriteria": [], "findings": []}, schema
        )
        self.assertEqual(errors, [])

    def test_missing_required_field_is_reported(self) -> None:
        schema = load_schema("review-result")
        errors = validate({"verdict": "approved", "findings": []}, schema)
        self.assertTrue(any("acceptanceCriteria" in error for error in errors))

    def test_invalid_enum_value_is_reported(self) -> None:
        schema = load_schema("review-result")
        errors = validate(
            {"verdict": "maybe", "acceptanceCriteria": [], "findings": []}, schema
        )
        self.assertTrue(any("verdict" in error for error in errors))

    def test_non_object_payload_is_reported(self) -> None:
        schema = load_schema("review-result")
        errors = validate(["not", "an", "object"], schema)
        self.assertTrue(errors)

    def test_planner_result_schema_validates_status_enum(self) -> None:
        schema = load_schema("planner-result")
        valid = validate(
            {
                "status": "ready",
                "summary": "s",
                "assumptions": [],
                "questions": [],
                "risk": "low",
                "acceptanceCriteria": [],
                "steps": [],
            },
            schema,
        )
        self.assertEqual(valid, [])
        invalid = validate({"status": "unsure"}, schema)
        self.assertTrue(invalid)


if __name__ == "__main__":
    unittest.main()
