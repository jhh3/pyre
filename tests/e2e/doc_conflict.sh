#!/bin/bash
# A08 at the CLI level: two writers start from the same document revision;
# one commit wins, the other gets a conflict with its proposal preserved.
#   PYRE_CONFIG=... tests/e2e/doc_conflict.sh <project>
set -uo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
P=$ROOT/bin/pyre
PRJ=$1
D=$(mktemp -d /tmp/doc-conflict.XXXX)
"$P" doc show brief --project "$PRJ" --json >"$D/head.json"
BASE=$(python3 -c "import json;print(json.load(open('$D/head.json'))['revision']['revision_id'])")
DOC=$(python3 -c "import json;print(json.load(open('$D/head.json'))['document']['document_id'])")
echo "base $BASE"
printf '# Brief\n\nWriter A: keep the API stable.\n' >"$D/a.md"
printf '# Brief\n\nWriter B: add retries.\n' >"$D/b.md"
"$P" doc edit "$DOC" --file "$D/a.md" --base "$BASE"; echo "writer A exit $?"
"$P" doc edit "$DOC" --file "$D/b.md" --base "$BASE"; echo "writer B exit $? (3 = conflict)"
"$P" doc history "$DOC"
"$P" doc proposals "$DOC"
