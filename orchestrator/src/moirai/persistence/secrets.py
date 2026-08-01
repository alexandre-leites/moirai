"""Authenticated encryption for credentials the control plane stores.

Per-project GitHub tokens and SSH private keys are the first values Moirai keeps
that are neither hashed nor write-once: they have to come back out to be used.
AES-GCM rather than a bare cipher because it authenticates as well as encrypts --
a row edited in the database fails to open instead of decrypting to something
the orchestrator would then send to a code host.

The key is deployment configuration (``LOOP_SECRET_KEY`` / ``_FILE``), read the
same way as every other secret. It is deliberately not derived from anything: a
key that can be recomputed from the database is not a key.
"""

from __future__ import annotations

import base64
import binascii
import os
from dataclasses import dataclass

from cryptography.exceptions import InvalidTag
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

# AES-256, and the nonce size AES-GCM is specified for. A nonce must never
# repeat under one key; 96 random bits is the standard construction.
KEY_BYTES = 32
NONCE_BYTES = 12


class SecretCipherError(RuntimeError):
    """The key is unusable, or a stored value could not be opened."""


@dataclass(frozen=True)
class SealedSecret:
    ciphertext: bytes
    nonce: bytes


class SecretCipher:
    def __init__(self, key: bytes) -> None:
        if len(key) != KEY_BYTES:
            raise SecretCipherError(
                f"secret key must be {KEY_BYTES} bytes, got {len(key)}"
            )
        self._aead = AESGCM(key)

    @classmethod
    def from_configured_key(cls, value: str) -> SecretCipher:
        """Builds a cipher from the configured key, base64 or hex encoded.

        Both encodings are accepted because both are what people reach for --
        `openssl rand -base64 32` and `openssl rand -hex 32`. Anything else is
        rejected rather than silently truncated or padded into 32 bytes.
        """
        text = value.strip()
        if not text:
            raise SecretCipherError("secret key is empty")
        for decoded in (_from_base64(text), _from_hex(text)):
            if decoded is not None and len(decoded) == KEY_BYTES:
                return cls(decoded)
        raise SecretCipherError(
            f"secret key must decode to {KEY_BYTES} bytes from base64 or hex; "
            "generate one with `openssl rand -base64 32`"
        )

    def seal(self, plaintext: str) -> SealedSecret:
        if not plaintext:
            raise SecretCipherError("refusing to seal an empty secret")
        nonce = os.urandom(NONCE_BYTES)
        ciphertext = self._aead.encrypt(nonce, plaintext.encode("utf-8"), None)
        return SealedSecret(ciphertext=ciphertext, nonce=nonce)

    def open(self, sealed: SealedSecret) -> str:
        """Returns the plaintext, or raises if the value was altered.

        An InvalidTag means the ciphertext, the nonce, or the key is not the one
        the value was sealed with. It is reported as a failure to open rather
        than distinguished further: telling a caller *which* of the three is
        wrong tells an attacker the same thing.
        """
        try:
            plaintext = self._aead.decrypt(sealed.nonce, sealed.ciphertext, None)
        except InvalidTag as error:
            raise SecretCipherError(
                "stored secret could not be opened: it was encrypted with a "
                "different key, or the stored value has been altered"
            ) from error
        return plaintext.decode("utf-8")


def _from_base64(text: str) -> bytes | None:
    """Decodes strict base64, or None when the text is not base64.

    `validate=True` matters: without it, base64 silently discards characters
    outside the alphabet, so a hex key would decode to the wrong bytes instead
    of being rejected and handed to the hex decoder.
    """
    try:
        return base64.b64decode(text, validate=True)
    except (binascii.Error, ValueError):
        return None


def _from_hex(text: str) -> bytes | None:
    try:
        return bytes.fromhex(text)
    except ValueError:
        return None
