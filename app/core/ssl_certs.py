"""
TLS / CA bundle helpers.

Features:
- Always materialize a runtime CA bundle under an ASCII-only path to avoid
  native library path issues on Windows with non-ASCII project paths.
- Merge certifi roots with Windows system roots (ROOT/CA stores) when enabled.
- Optionally append a custom proxy/root CA file via config `proxy.custom_ca_file`.
"""

from __future__ import annotations

import os
import re
import ssl
import tempfile
from pathlib import Path
from threading import Lock
from typing import Iterable

import certifi
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization

try:
    from cryptography.hazmat.primitives.serialization.pkcs7 import (
        load_der_pkcs7_certificates,
    )
except Exception:  # pragma: no cover - depends on cryptography backend support
    load_der_pkcs7_certificates = None

from app.core.config import get_config
from app.core.logger import logger

_BUNDLE_LOCK = Lock()
_BUNDLE_SIGNATURE: tuple | None = None
_BUNDLE_PATH: str = ""
_PEM_PATTERN = re.compile(
    rb"-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----", re.DOTALL
)


def _runtime_cert_dir() -> Path:
    base = Path(os.getenv("LOCALAPPDATA") or tempfile.gettempdir()) / "grok2api" / "certs"
    base.mkdir(parents=True, exist_ok=True)
    return base


def _sha256_fingerprint(cert: x509.Certificate) -> str:
    return cert.fingerprint(hashes.SHA256()).hex()


def _parse_pem_certificates(raw: bytes) -> list[x509.Certificate]:
    certs: list[x509.Certificate] = []
    for block in _PEM_PATTERN.findall(raw):
        try:
            certs.append(x509.load_pem_x509_certificate(block))
        except Exception as exc:
            logger.debug("Skip invalid PEM certificate block: {}", exc)
    return certs


def _parse_der_or_pkcs7_certificates(raw: bytes) -> list[x509.Certificate]:
    try:
        return [x509.load_der_x509_certificate(raw)]
    except Exception:
        pass

    if load_der_pkcs7_certificates is not None:
        try:
            return list(load_der_pkcs7_certificates(raw))
        except Exception:
            pass

    return []


def _load_certificates_from_file(path: str | Path) -> list[x509.Certificate]:
    try:
        file_path = Path(path).expanduser()
        if not file_path.exists():
            logger.warning("CA file not found: {}", file_path)
            return []
        raw = file_path.read_bytes()
    except Exception as exc:
        logger.warning("Failed to read CA file {}: {}", path, exc)
        return []

    if b"-----BEGIN CERTIFICATE-----" in raw:
        return _parse_pem_certificates(raw)
    return _parse_der_or_pkcs7_certificates(raw)


def _load_windows_store_certificates(store_name: str) -> list[x509.Certificate]:
    if os.name != "nt" or not hasattr(ssl, "enum_certificates"):
        return []

    certs: list[x509.Certificate] = []
    try:
        entries = ssl.enum_certificates(store_name)
    except Exception as exc:
        logger.warning("Failed to enumerate Windows certificate store {}: {}", store_name, exc)
        return certs

    for cert_bytes, encoding_type, _trust in entries:
        try:
            if encoding_type == "x509_asn":
                certs.append(x509.load_der_x509_certificate(cert_bytes))
            elif encoding_type == "pkcs_7_asn" and load_der_pkcs7_certificates is not None:
                certs.extend(load_der_pkcs7_certificates(cert_bytes))
        except Exception as exc:
            logger.debug(
                "Skip invalid certificate from Windows store {} ({}): {}",
                store_name,
                encoding_type,
                exc,
            )
    return certs


def _append_unique_certificates(
    certificates: Iterable[x509.Certificate],
    blocks: list[bytes],
    seen: set[str],
) -> int:
    added = 0
    for cert in certificates:
        try:
            fingerprint = _sha256_fingerprint(cert)
            if fingerprint in seen:
                continue
            seen.add(fingerprint)
            blocks.append(cert.public_bytes(serialization.Encoding.PEM))
            added += 1
        except Exception as exc:
            logger.debug("Skip certificate while building CA bundle: {}", exc)
    return added


def _resolve_custom_ca_path() -> str:
    env_value = (
        os.getenv("GROK2API_CUSTOM_CA_FILE")
        or os.getenv("CUSTOM_CA_FILE")
        or ""
    )
    value = env_value.strip()
    if not value:
        value = str(get_config("proxy.custom_ca_file", "") or "").strip()
    return value


def _use_system_ca() -> bool:
    raw = os.getenv("GROK2API_USE_SYSTEM_CA") or os.getenv("USE_SYSTEM_CA")
    if raw is not None:
        return raw.strip().lower() in {"1", "true", "yes", "on", "y"}
    default = os.name == "nt"
    return bool(get_config("proxy.use_system_ca", default))


def get_combined_ca_bundle_path() -> str:
    """Return a generated CA bundle path under an ASCII-only runtime directory."""
    global _BUNDLE_SIGNATURE, _BUNDLE_PATH

    custom_ca_path = _resolve_custom_ca_path()
    custom_ca_mtime = None
    if custom_ca_path:
        try:
            custom_ca_mtime = Path(custom_ca_path).expanduser().stat().st_mtime_ns
        except OSError:
            custom_ca_mtime = None

    signature = (
        certifi.where(),
        _use_system_ca(),
        custom_ca_path,
        custom_ca_mtime,
        os.name,
    )

    with _BUNDLE_LOCK:
        if _BUNDLE_SIGNATURE == signature and _BUNDLE_PATH and Path(_BUNDLE_PATH).exists():
            return _BUNDLE_PATH

        blocks: list[bytes] = []
        seen: set[str] = set()

        certifi_certs = _load_certificates_from_file(certifi.where())
        certifi_count = _append_unique_certificates(certifi_certs, blocks, seen)

        system_count = 0
        if _use_system_ca():
            for store_name in ("ROOT", "CA"):
                system_count += _append_unique_certificates(
                    _load_windows_store_certificates(store_name), blocks, seen
                )

        custom_count = 0
        if custom_ca_path:
            custom_count = _append_unique_certificates(
                _load_certificates_from_file(custom_ca_path), blocks, seen
            )
            if custom_count == 0:
                logger.warning("No usable certificates loaded from proxy.custom_ca_file={}", custom_ca_path)

        output_path = _runtime_cert_dir() / "combined-ca.pem"
        temp_path = output_path.with_suffix(".tmp")
        temp_path.write_bytes(b"".join(blocks))
        os.replace(temp_path, output_path)

        _BUNDLE_SIGNATURE = signature
        _BUNDLE_PATH = str(output_path)

        logger.info(
            "Prepared CA bundle: path={}, certifi={}, system={}, custom={}, total={}",
            _BUNDLE_PATH,
            certifi_count,
            system_count,
            custom_count,
            len(seen),
        )
        return _BUNDLE_PATH


def create_ssl_context() -> ssl.SSLContext:
    """Create SSL context backed by the generated combined CA bundle."""
    cafile = get_combined_ca_bundle_path()
    return ssl.create_default_context(cafile=cafile)


__all__ = ["get_combined_ca_bundle_path", "create_ssl_context"]
