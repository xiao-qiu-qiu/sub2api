"""Generate the managed Codex overlay embedded in the Go server.

Run python tools/reauth-runtime/sync_codex_overlay.py after updating its sources.
Pass --check to verify that the committed archive is current.
"""

import argparse
import io
from pathlib import Path
from zipfile import ZIP_STORED, ZipFile, ZipInfo


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check', action='store_true')
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[2]
    output = root / 'backend/internal/reauthruntime/codex_overlay.zip'
    buffer = io.BytesIO()
    with ZipFile(buffer, 'w', ZIP_STORED) as archive:
        for name in (
            'openai_codex_diagnostics.mjs',
            'openai_codex_oauth_adapter.py',
            'openai_codex_password_flow.mjs',
        ):
            source = (root / 'tools' / name).read_bytes().replace(b'\r\n', b'\n')
            entry = ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            entry.create_system = 3
            entry.external_attr = 0o100644 << 16
            archive.writestr(entry, source)
    expected = buffer.getvalue()
    if args.check:
        if not output.is_file() or output.read_bytes() != expected:
            parser.error('stale Codex overlay: run python tools/reauth-runtime/sync_codex_overlay.py')
    else:
        output.write_bytes(expected)


if __name__ == '__main__':
    main()
