#!/usr/bin/env python3
"""Inject lambda/ingest-manifest-enrich.py into the CloudFormation template.

The .py file is the source of truth: it is what you edit, lint and read. The
template's inline Code:ZipFile block is generated from it, so the two can never
drift. Run with --check in CI to fail if they have.

    python3 lambda/build-template.py          # regenerate
    python3 lambda/build-template.py --check   # verify only
"""
import hashlib
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SRC = ROOT / 'lambda' / 'ingest-manifest-enrich.py'
TEMPLATE = ROOT / 'ch18-ingest-manifest-lambda.yaml'

# The .py carries @@TOKEN@@ placeholders so it stays syntactically valid Python
# and unit-testable on its own. They become CloudFormation !Sub references here.
SUBSTITUTIONS = {
    '@@UTC_TIMING_VALUE@@': '${UtcTimingValue}',
    '@@LATENCY_TARGET_MS@@': '${LatencyTargetMs}',
    '@@LATENCY_MIN_MS@@': '${LatencyMinMs}',
    '@@LATENCY_MAX_MS@@': '${LatencyMaxMs}',
}

BEGIN = '      Code:\n        ZipFile: !Sub |\n'
INDENT = ' ' * 10

# AWS::Lambda::Version only cuts a new version when its OWN properties change, and
# Lambda@Edge can only be attached to a numbered version. So a code-only edit
# leaves the edge running the previous version silently. Stamping the source hash
# into the Version description forces a new version whenever the code changes.
VERSION_DESC = re.compile(
    r'(  ManifestFunctionVersion:.*?Description: )!Sub \'[^\']*\'', re.S)


def render() -> str:
    code = SRC.read_text()
    for token, ref in SUBSTITUTIONS.items():
        if token not in code:
            sys.exit(f'error: {SRC.name} no longer contains {token}')
        code = code.replace(token, ref)

    # A literal ${...} that is not one of ours would be eaten by !Sub.
    stray = [m for m in re.findall(r'\$\{[^}]*\}', code)
             if m not in SUBSTITUTIONS.values()]
    if stray:
        sys.exit(f'error: unescaped Fn::Sub reference(s) in {SRC.name}: {stray}')

    body = ''.join(INDENT + line if line.strip() else '\n'
                   for line in code.splitlines(keepends=True))
    return BEGIN + body


def stamp_version(template: str, digest: str) -> str:
    replacement = (r"\g<1>!Sub 'source %s · latency ${LatencyTargetMs}ms'" % digest)
    updated, n = VERSION_DESC.subn(replacement, template)
    if n != 1:
        sys.exit('error: could not stamp the Lambda version description')
    return updated


def splice(template: str, block: str) -> str:
    start = template.index(BEGIN)
    # The block ends at the next line indented less than the code body.
    rest = template[start + len(BEGIN):]
    end = len(rest)
    for m in re.finditer(r'^(?! {10})\S|^ {2,8}\S', rest, re.M):
        end = m.start()
        break
    return template[:start] + block + rest[end:]


def main() -> None:
    check = '--check' in sys.argv
    template = TEMPLATE.read_text()
    digest = hashlib.sha256(SRC.read_bytes()).hexdigest()[:12]
    updated = stamp_version(splice(template, render()), digest)

    if check:
        if updated != template:
            sys.exit(f'error: {TEMPLATE.name} is stale — run: '
                     f'python3 lambda/build-template.py')
        print(f'ok: {TEMPLATE.name} matches {SRC.name}')
        return

    if updated == template:
        print(f'ok: {TEMPLATE.name} already current')
        return

    TEMPLATE.write_text(updated)
    print(f'regenerated {TEMPLATE.name} from {SRC.name} '
          f'({len(SRC.read_text().splitlines())} lines, source {digest})')


if __name__ == '__main__':
    main()
