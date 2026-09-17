#!/usr/bin/env python3
"""What a YAML parser says about `agents uninstall` on Hermes' config.yaml.

hermes_install.go edits config.yaml by line with no YAML parser, so nothing in
the Go tests can say what a file MEANS before and after. PyYAML can, and it
is the parser Hermes itself loads the file with. This judges every
before/after pair TestHermesDifferential writes:

  unchanged   a refusal, always safe — but if YAML reads our entry as cleanly
              present, the plan must have said something about it
  changed     the meaning of `after` must be the meaning of `before` with
              exactly our entry taken out (an emptied list and an emptied
              hooks: pruned), and every surviving line byte-identical, in order

Anything else is a finding. It started as the oracle of L3's review, where it
found 870 across 2,875 files (F2, F3 and F4 of that review); the expected
result now is FINDINGS: 0.

    HERMES_DIFFERENTIAL_OUT=/tmp/pairs.json go test ./cmd/dropin-miner -run TestHermesDifferential -count=1
    python3 cmd/dropin-miner/testdata/hermes/oracle.py /tmp/pairs.json      # needs PyYAML
"""
import copy
import json
import sys
from collections import Counter

import yaml


def load(text):
    try:
        return True, yaml.safe_load(text)
    except Exception as e:  # noqa: BLE001
        return False, str(e).splitlines()[0]


def expected(data, cmd):
    d = copy.deepcopy(data)
    if not isinstance(d, dict) or not isinstance(d.get("hooks"), dict):
        return None
    lst = d["hooks"].get("pre_tool_call")
    if not isinstance(lst, list):
        return None
    ours = [e for e in lst if e == {"command": cmd, "matcher": "terminal"}]
    if len(ours) != 1:
        return None
    lst.remove(ours[0])
    if not lst:
        del d["hooks"]["pre_tool_call"]
    if not d["hooks"]:
        del d["hooks"]
    return d


def is_subsequence(after_lines, before_lines):
    it = iter(before_lines)
    return all(any(a == b for b in it) for a in after_lines)


pairs = json.load(open(sys.argv[1]))
tally, findings = Counter(), []
for p in pairs:
    b, a, cmd = p["before"], p["after"], p["cmd"]
    okb, db = load(b)
    if a == b:
        present = okb and expected(db, cmd) is not None
        tally["left, our entry present per YAML" if present else "left, our entry not present per YAML"] += 1
        if present and "pre_tool_call hook" not in p["out"]:
            findings.append(("left SILENTLY though YAML reads our entry as live", p["name"]))
        continue
    tally["changed"] += 1
    oka, da = load(a)
    if not is_subsequence(a.splitlines(True), b.splitlines(True)):
        findings.append(("surviving lines not byte-identical", p["name"]))
    elif not okb:
        findings.append(("changed a file PyYAML cannot parse: " + str(db), p["name"]))
    elif expected(db, cmd) is None:
        findings.append(("changed a file where ours is not cleanly present per YAML", p["name"]))
    elif not oka:
        findings.append(("after does not parse: " + str(da), p["name"]))
    elif (da or {}) != (expected(db, cmd) or {}):
        findings.append(("meaning differs from before-minus-ours", p["name"]))

for k, v in sorted(tally.items()):
    print(f"{v:6d}  {k}")
print(f"{len(pairs):6d}  total")
print("FINDINGS:", len(findings))
shapes = {}
for kind, name in findings:
    shapes.setdefault(kind, set()).add(name.split("/")[1])
for kind, bodies in shapes.items():
    print("   *", kind, "->", sorted(bodies))
sys.exit(1 if findings else 0)
