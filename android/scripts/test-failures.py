#!/usr/bin/env python3
"""Print failing JUnit test cases (name + first lines of the failure) from Gradle XML results."""
import glob
import sys
import xml.etree.ElementTree as ET

root = sys.argv[1] if len(sys.argv) > 1 else "app/build/test-results/testDebugUnitTest"
lines = int(sys.argv[2]) if len(sys.argv) > 2 else 8
total = failed = 0
for path in sorted(glob.glob(root + "/*.xml")):
    for case in ET.parse(path).getroot().iter("testcase"):
        total += 1
        for f in list(case.findall("failure")) + list(case.findall("error")):
            failed += 1
            print(f"FAIL {case.get('classname')}.{case.get('name')}")
            print("\n".join((f.text or f.get("message") or "").splitlines()[:lines]))
            print()
print(f"{total} tests, {failed} failed")
