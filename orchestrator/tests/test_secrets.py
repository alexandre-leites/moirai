from __future__ import annotations

import base64
import os
import unittest

from moirai.persistence.secrets import (
    KEY_BYTES,
    SealedSecret,
    SecretCipher,
    SecretCipherError,
)

_KEY = base64.b64encode(bytes(range(KEY_BYTES))).decode()


class SecretCipherTests(unittest.TestCase):
    def test_a_sealed_secret_opens_back_to_the_original(self) -> None:
        cipher = SecretCipher.from_configured_key(_KEY)
        sealed = cipher.seal("ghp_a-private-repository-token")
        self.assertEqual(cipher.open(sealed), "ghp_a-private-repository-token")

    def test_the_ciphertext_does_not_contain_the_plaintext(self) -> None:
        cipher = SecretCipher.from_configured_key(_KEY)
        sealed = cipher.seal("ghp_a-private-repository-token")
        self.assertNotIn(b"ghp_", sealed.ciphertext)

    def test_sealing_twice_produces_different_ciphertext(self) -> None:
        # A fresh nonce per seal. Identical ciphertext for identical input would
        # leak that two projects share a credential.
        cipher = SecretCipher.from_configured_key(_KEY)
        first = cipher.seal("same-value")
        second = cipher.seal("same-value")
        self.assertNotEqual(first.ciphertext, second.ciphertext)
        self.assertNotEqual(first.nonce, second.nonce)
        self.assertEqual(cipher.open(first), cipher.open(second))

    def test_an_altered_ciphertext_fails_to_open(self) -> None:
        cipher = SecretCipher.from_configured_key(_KEY)
        sealed = cipher.seal("ghp_token")
        tampered = SealedSecret(
            ciphertext=bytes([sealed.ciphertext[0] ^ 0x01]) + sealed.ciphertext[1:],
            nonce=sealed.nonce,
        )
        with self.assertRaisesRegex(SecretCipherError, "could not be opened"):
            cipher.open(tampered)

    def test_a_different_key_cannot_open_the_value(self) -> None:
        sealed = SecretCipher.from_configured_key(_KEY).seal("ghp_token")
        other = SecretCipher.from_configured_key(base64.b64encode(os.urandom(KEY_BYTES)).decode())
        with self.assertRaisesRegex(SecretCipherError, "could not be opened"):
            other.open(sealed)

    def test_hex_and_base64_keys_are_both_accepted(self) -> None:
        raw = os.urandom(KEY_BYTES)
        from_base64 = SecretCipher.from_configured_key(base64.b64encode(raw).decode())
        from_hex = SecretCipher.from_configured_key(raw.hex())
        # Same key either way, so one can open what the other sealed.
        self.assertEqual(from_hex.open(from_base64.seal("shared")), "shared")

    def test_a_key_of_the_wrong_length_is_rejected(self) -> None:
        with self.assertRaisesRegex(SecretCipherError, "32 bytes"):
            SecretCipher.from_configured_key(base64.b64encode(b"too-short").decode())
        with self.assertRaisesRegex(SecretCipherError, "32 bytes"):
            SecretCipher.from_configured_key("not-a-key-at-all")

    def test_an_empty_key_is_rejected(self) -> None:
        with self.assertRaisesRegex(SecretCipherError, "empty"):
            SecretCipher.from_configured_key("   ")

    def test_an_empty_secret_is_refused_rather_than_stored(self) -> None:
        # An empty credential is a misconfiguration. Storing it would read back
        # as "configured" and then fail at the code host.
        cipher = SecretCipher.from_configured_key(_KEY)
        with self.assertRaisesRegex(SecretCipherError, "empty secret"):
            cipher.seal("")
