"""Regenerate the issuer interop vectors in this directory.

Signs MassDB OSS claims with the MassDB SQL vendor issuer (massdb-sql
tools/license-issuer/license_issuer.py) and real OpenSSL 3, so the Go
verifier is checked against an independent implementation. The private key
is created in a temporary directory and discarded; only the public trust
manifest and the signed certificates are written here.

    python3 -I gen_issuer_vectors.py <path to license_issuer.py> <openssl 3> <output dir>

The MassDB SQL issuer only serves as an independent implementation for
these test vectors and will not get an OSS product profile (the production
OSS issuer is a separate project, plan 7.3), so this calls its internal
_sign_claims, which signs without validate_claims.
"""
import importlib.util
import json
import sys
import tempfile
from pathlib import Path

issuer_path, openssl_path, out_dir = sys.argv[1], sys.argv[2], Path(sys.argv[3])
spec = importlib.util.spec_from_file_location("license_issuer", issuer_path)
issuer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(issuer)

DEPLOYMENT = "c9b82051-a34e-455b-bff3-2f09306c6aa8"
KID = "interop-2026"
BASE = {
    "schema_version": 1, "policy_version": 1, "license_id": "oss-interop-001",
    "issuer": "MassDB Issuer", "customer_id": "客户 A", "edition": "Enterprise",
    "product": "MassDB OSS", "deployment_id": DEPLOYMENT,
    "issued_at": 1767225600, "not_before": 1767225600, "expires_at": 1798761600,
    "sequence": 1, "features": ["DATA_READ"],
    "limits": {"max_master_nodes": 3, "max_volume_servers": 10, "max_filers": 2},
}
VECTORS = {
    "issuer_valid.jws": BASE,
    "issuer_sql_product.jws": dict(BASE, product="MassDB SQL", limits={"max_fe_nodes": 3, "max_be_nodes": 10}),
}

openssl = issuer.OpenSsl(openssl_path)
with tempfile.TemporaryDirectory() as tmp:
    private, public = Path(tmp) / "private.pem", Path(tmp) / "public.pem"
    issuer.generate_keys(openssl, str(private), str(public))
    trust = out_dir / "issuer_trust.json"
    trust.unlink(missing_ok=True)
    issuer.export_trust(openssl, str(public), KID, "license", str(trust))
    for name, claims in VECTORS.items():
        target = out_dir / name
        target.unlink(missing_ok=True)
        issuer._sign_claims(openssl, claims, str(private), KID, str(target),
                            "massdb-license+jws", issuer.MAX_CERTIFICATE_BYTES)
    meta = {"kid": KID, "deployment_id": DEPLOYMENT, "claims": BASE}
    (out_dir / "issuer_valid.claims.json").write_text(json.dumps(meta, ensure_ascii=False, indent=1) + "\n")
print("ok")
